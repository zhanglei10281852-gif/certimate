package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/samber/lo"

	"github.com/certimate-go/certimate/internal/app"
	"github.com/certimate-go/certimate/internal/domain"
	"github.com/certimate-go/certimate/internal/repository"
	"github.com/certimate-go/certimate/internal/workflow/engine"
	"github.com/certimate-go/certimate/pkg/logging"
	xenv "github.com/certimate-go/certimate/pkg/utils/env"
)

var envMaxWorkers = 1
var envShutdownTimeoutSeconds = 30

func init() {
	envMaxWorkers = xenv.GetOrDefaultInt("CERTIMATE_WORKFLOW_MAX_WORKERS", runtime.GOMAXPROCS(0))
	if envMaxWorkers <= 0 {
		envMaxWorkers = max(1, runtime.NumCPU())
	}

	envShutdownTimeoutSeconds = xenv.GetOrDefaultInt("CERTIMATE_WORKFLOW_SHUTDOWN_TIMEOUT_SECONDS", envShutdownTimeoutSeconds)
	if envShutdownTimeoutSeconds <= 0 {
		envShutdownTimeoutSeconds = 30
	}
}

type WorkflowDispatcher interface {
	GetStatistics() Statistics

	Bootup(ctx context.Context) error
	Shutdown(ctx context.Context) error
	Start(ctx context.Context, workflowId string, runId string) error
	Cancel(ctx context.Context, runId string) error

	// ShutdownWorkflow closes the ownership of a single workflow: it rejects
	// new runs, withdraws queued runs, cancels in-flight executions (and their
	// child processes) and waits until all of its execution goroutines have
	// converged. It is idempotent and safe to retry.
	ShutdownWorkflow(ctx context.Context, workflowId string) error
	// ReleaseWorkflow clears the in-memory close fence after the workflow
	// record has been committed to deletion.
	ReleaseWorkflow(workflowId string)
}

type Statistics struct {
	Concurrency      int
	PendingRunIds    []string
	ProcessingRunIds []string
}

type workflowDispatcher struct {
	booted      bool
	concurrency int

	taskMtx          sync.RWMutex
	pendingRunQueue  []*taskInfo
	processingTasks  map[string]*taskInfo // Key: RunId, removed as soon as a slot is freed
	activeTasks      map[string]*taskInfo // Key: RunId, removed only once the execution goroutine has fully exited
	closingWorkflows map[string]struct{} // Key: WorkflowId

	workflowRepo    workflowRepository
	workflowRunRepo workflowRunRepository
	workflowLogRepo workflowLogRepository

	syslog *slog.Logger
}

var _ WorkflowDispatcher = (*workflowDispatcher)(nil)

func (wd *workflowDispatcher) GetStatistics() Statistics {
	wd.taskMtx.RLock()
	defer wd.taskMtx.RUnlock()

	stats := Statistics{
		Concurrency:      wd.concurrency,
		PendingRunIds:    make([]string, 0),
		ProcessingRunIds: make([]string, 0),
	}
	for _, pendingTask := range wd.pendingRunQueue {
		stats.PendingRunIds = append(stats.PendingRunIds, pendingTask.RunId)
	}
	for _, processingRunId := range wd.processingTasks {
		stats.ProcessingRunIds = append(stats.ProcessingRunIds, processingRunId.RunId)
	}

	return stats
}

func (wd *workflowDispatcher) Bootup(ctx context.Context) error {
	if wd.booted {
		return fmt.Errorf("could not re-bootup")
	}

	wd.taskMtx.Lock()
	defer wd.taskMtx.Unlock()

	if err := wd.workflowRunRepo.ResetStatusIfHanging(ctx); err != nil {
		return err
	}

	wd.booted = true

	return nil
}

func (wd *workflowDispatcher) Shutdown(ctx context.Context) error {
	if !wd.booted {
		return fmt.Errorf("could not re-shutdown")
	}

	wd.taskMtx.Lock()
	defer wd.taskMtx.Unlock()

	for runId, task := range wd.processingTasks {
		task.cancel()
		delete(wd.processingTasks, runId)
	}

	wd.booted = false
	wd.pendingRunQueue = make([]*taskInfo, 0)
	wd.processingTasks = make(map[string]*taskInfo)
	wd.activeTasks = make(map[string]*taskInfo)
	wd.closingWorkflows = make(map[string]struct{})
	return nil
}

func (wd *workflowDispatcher) Start(ctx context.Context, workflowId string, runId string) error {
	wd.taskMtx.Lock()
	defer wd.taskMtx.Unlock()

	if !wd.booted {
		return fmt.Errorf("workflow dispatcher is not booted")
	}

	// Close fence: once the ownership of a workflow is closing, no further
	// generation may enter the queue, regardless of who requested the run.
	if _, closing := wd.closingWorkflows[workflowId]; closing {
		return fmt.Errorf("workflow #%s is closing, the new run #%s is rejected", workflowId, runId)
	}

	if _, exists := wd.processingTasks[runId]; exists {
		return fmt.Errorf("workflow run %s is already processing", runId)
	}

	for _, pendingTask := range wd.pendingRunQueue {
		if pendingTask.RunId == runId {
			return fmt.Errorf("workflow run %s is already in the queue", runId)
		}
	}

	wd.pendingRunQueue = append(wd.pendingRunQueue, &taskInfo{WorkflowId: workflowId, RunId: runId})
	go func() { wd.tryNextAsync() }()

	return nil
}

func (wd *workflowDispatcher) ShutdownWorkflow(ctx context.Context, workflowId string) error {
	// 1. Raise the close fence, withdraw queued generations and cancel every
	//    in-flight execution atomically. Cancelling here (before any slow
	//    database work) guarantees that no node can cross the close boundary
	//    and publish a certificate/output or trigger a deploy/notification.
	wd.taskMtx.Lock()
	wd.closingWorkflows[workflowId] = struct{}{}

	inflightTasks := make([]*taskInfo, 0)
	for _, task := range wd.activeTasks {
		if task.WorkflowId == workflowId {
			inflightTasks = append(inflightTasks, task)
			task.cancel()
		}
	}

	// Withdraw every queued generation of this workflow. Their database status
	// is corrected by the cancel sweep below, so they can never be dispatched.
	wd.pendingRunQueue = lo.Filter(wd.pendingRunQueue, func(task *taskInfo, _ int) bool {
		return task.WorkflowId != workflowId
	})
	wd.taskMtx.Unlock()

	// 2. Database-side fence: mark every still active run as canceled. This
	//    also covers generations whose row was inserted but that were rejected
	//    by the in-memory fence above.
	canceledRuns, err := wd.workflowRunRepo.CancelActiveByWorkflowId(ctx, workflowId)
	if err != nil {
		return fmt.Errorf("workflow #%s: failed to cancel active runs while closing: %w", workflowId, err)
	}
	for _, run := range canceledRuns {
		wd.syslog.Info(fmt.Sprintf("workflow #%s's run #%s was canceled because the workflow is closing", workflowId, run.Id))
	}

	// 3. Wait for ownership convergence. The wait is intentionally detached
	//    from the request context so a client disconnect cannot split the
	//    state halfway, but it is bounded to avoid blocking forever.
	waitCtx, waitCancel := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(envShutdownTimeoutSeconds)*time.Second)
	defer waitCancel()

	for _, task := range inflightTasks {
		select {
		case <-task.done:
		case <-waitCtx.Done():
			runningRunIds := wd.activeRunIds(workflowId)
			return fmt.Errorf("workflow #%s: timed out waiting for %d active run(s) to stop [%s]; the deletion has been aborted and can be retried",
				workflowId, len(runningRunIds), strings.Join(runningRunIds, ","))
		}
	}

	// 4. Final sweep: any active row left means a generation slipped in
	//    concurrently; abort rather than committing a split state.
	leftRuns, err := wd.workflowRunRepo.CancelActiveByWorkflowId(ctx, workflowId)
	if err != nil {
		return fmt.Errorf("workflow #%s: failed to verify active runs while closing: %w", workflowId, err)
	}
	if len(leftRuns) > 0 {
		runningRunIds := lo.Map(leftRuns, func(run *domain.WorkflowRun, _ int) string { return run.Id })
		return fmt.Errorf("workflow #%s: %d active run(s) are still present [%s]; the deletion has been aborted and can be retried",
			workflowId, len(runningRunIds), strings.Join(runningRunIds, ","))
	}

	return nil
}

func (wd *workflowDispatcher) ReleaseWorkflow(workflowId string) {
	wd.taskMtx.Lock()
	defer wd.taskMtx.Unlock()

	delete(wd.closingWorkflows, workflowId)
}

func (wd *workflowDispatcher) activeRunIds(workflowId string) []string {
	wd.taskMtx.RLock()
	defer wd.taskMtx.RUnlock()

	runIds := make([]string, 0)
	for _, task := range wd.activeTasks {
		if task.WorkflowId == workflowId {
			runIds = append(runIds, task.RunId)
		}
	}
	slices.Sort(runIds)
	return runIds
}

func (wd *workflowDispatcher) Cancel(ctx context.Context, runId string) error {
	wd.taskMtx.Lock()
	defer wd.taskMtx.Unlock()

	workflowRun, err := wd.workflowRunRepo.GetById(ctx, runId)
	if err != nil {
		return err
	} else if workflowRun.Status != domain.WorkflowRunStatusTypePending && workflowRun.Status != domain.WorkflowRunStatusTypeProcessing {
		return fmt.Errorf("workrun #%s is already completed", workflowRun.Id)
	}

	workflow, err := wd.workflowRepo.GetById(ctx, workflowRun.WorkflowId)
	if err != nil {
		return err
	}

	workflowRun.Status = domain.WorkflowRunStatusTypeCanceled
	if workflow.LastRunId == workflowRun.Id {
		_, err := wd.workflowRunRepo.SaveWithCascading(ctx, workflowRun)
		if err != nil {
			return err
		}
	} else {
		_, err := wd.workflowRunRepo.Save(ctx, workflowRun)
		if err != nil {
			return err
		}
	}

	if task, exists := wd.processingTasks[runId]; exists {
		task.cancel()
		delete(wd.processingTasks, runId)

		wd.syslog.Info(fmt.Sprintf("workrun #%s was canceled", task.RunId))
	}

	for i, pendingTask := range wd.pendingRunQueue {
		if pendingTask.RunId == runId {
			wd.pendingRunQueue = append(wd.pendingRunQueue[:i], wd.pendingRunQueue[i+1:]...)
			break
		}
	}

	go func() { wd.tryNextAsync() }()

	return nil
}

func (wd *workflowDispatcher) tryExecuteAsync(task *taskInfo) {
	var workflow *domain.Workflow
	var workflowRun *domain.WorkflowRun
	var err error

	// 捕获 panic
	defer func() {
		if r := recover(); r != nil {
			wd.syslog.Error(fmt.Sprintf("workflow dispatcher panic: %v", r), slog.String("workflowId", task.WorkflowId), slog.String("runId", task.RunId))
			slog.Error(fmt.Sprintf("workflow dispatcher panic: %v, stack trace: %s", r, string(debug.Stack())), slog.String("workflowId", task.WorkflowId), slog.String("runId", task.RunId))

			if workflowRun != nil {
				workflowRun.Status = domain.WorkflowRunStatusTypeFailed
				workflowRun.EndedAt = time.Now()
				workflowRun.Error = fmt.Sprintf("workflow dispatcher panic: %v", r)
				if _, err := wd.workflowRunRepo.SaveWithCascading(context.Background(), workflowRun); err != nil {
					log.Default().Println("failed to save workflow run after panic", slog.Any("error", err))
				}
			}
		}
	}()

	// 尝试继续执行等待队列中的任务；activeTasks 与 done 仅在执行协程完全
	// 退出（含所有收尾回调）后才释放/关闭，ShutdownWorkflow 据此等待所有权收敛
	defer func() {
		wd.taskMtx.Lock()
		delete(wd.processingTasks, task.RunId)
		delete(wd.activeTasks, task.RunId)
		wd.taskMtx.Unlock()

		go func() { wd.tryNextAsync() }()

		close(task.done)
	}()

	// 查询运行实体，并级联更新状态
	if workflowRun, err = wd.workflowRunRepo.GetById(task.ctx, task.RunId); err != nil {
		if !(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			wd.syslog.Error(fmt.Sprintf("failed to get workrun #%s record", task.RunId), slog.Any("error", err))
		}
		return
	} else {
		if workflowRun.Status == domain.WorkflowRunStatusTypePending {
			workflowRun.Status = domain.WorkflowRunStatusTypeProcessing
			wd.workflowRunRepo.SaveWithCascading(task.ctx, workflowRun)
		} else {
			// WTF? That should be impossible!
			return
		}
	}

	// 查询工作流实体
	workflow, err = wd.workflowRepo.GetById(task.ctx, workflowRun.WorkflowId)
	if err != nil {
		wd.syslog.Error(fmt.Sprintf("failed to get workflow #%s record", workflowRun.WorkflowId), slog.Any("error", err))
		return
	}

	// 关闭边界：工作流已进入删除流程时，旧代际不得继续执行，直接落库为取消
	if workflow.IsClosing() {
		workflowRun.Status = domain.WorkflowRunStatusTypeCanceled
		workflowRun.EndedAt = time.Now()
		if _, err := wd.workflowRunRepo.SaveWithCascading(context.Background(), workflowRun); err != nil {
			wd.syslog.Error(fmt.Sprintf("failed to cancel workrun #%s of closing workflow #%s", task.RunId, workflow.Id), slog.Any("error", err))
		}
		return
	}

	// 初始化工作流引擎
	logsBuf := make(domain.WorkflowLogs, 0)
	we := engine.NewWorkflowEngine()
	we.OnEnd(func(ctx context.Context) error {
		if errmsg := logsBuf.ErrorString(); errmsg == "" {
			workflowRun.Status = domain.WorkflowRunStatusTypeSucceeded
			workflowRun.EndedAt = time.Now()
		} else {
			workflowRun.Status = domain.WorkflowRunStatusTypeFailed
			workflowRun.EndedAt = time.Now()
			workflowRun.Error = errmsg
		}
		wd.workflowRunRepo.SaveWithCascading(task.ctx, workflowRun)

		return nil
	})
	we.OnError(func(ctx context.Context, err error) error {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			workflowRun.Status = domain.WorkflowRunStatusTypeCanceled
			wd.workflowRunRepo.SaveWithCascading(context.Background(), workflowRun)
		} else {
			workflowRun.Status = domain.WorkflowRunStatusTypeFailed
			workflowRun.EndedAt = time.Now()
			workflowRun.Error = err.Error()
			wd.workflowRunRepo.SaveWithCascading(task.ctx, workflowRun)
		}

		return nil
	})
	we.OnNodeError(func(ctx context.Context, node *engine.Node, err error) error {
		if errors.Is(err, engine.ErrTerminated) || errors.Is(err, engine.ErrBlocksException) {
			return nil
		}

		log := domain.WorkflowLog{}
		log.WorkflowId = task.WorkflowId
		log.RunId = task.RunId
		log.NodeId = node.Id
		log.NodeName = node.Data.Name
		log.TimestampMilli = time.Now().UnixMilli()
		log.Level = int32(slog.LevelError)
		log.Message = err.Error()
		log.CreatedAt = time.Now()
		logsBuf = append(logsBuf, log)

		if _, err := wd.workflowLogRepo.Save(ctx, &log); err != nil {
			wd.syslog.Error(err.Error())
		}

		return nil
	})
	we.OnNodeLogging(func(ctx context.Context, node *engine.Node, record logging.Record) error {
		log := domain.WorkflowLog{}
		log.WorkflowId = task.WorkflowId
		log.RunId = task.RunId
		log.NodeId = node.Id
		log.NodeName = node.Data.Name
		log.TimestampMilli = record.Time.UnixMilli()
		log.Level = int32(record.Level)
		log.Message = record.Message
		log.Data = record.Data()
		log.CreatedAt = time.Now()
		logsBuf = append(logsBuf, log)

		if _, err := wd.workflowLogRepo.Save(ctx, &log); err != nil {
			wd.syslog.Error(err.Error())
		}

		return nil
	})

	// 执行工作流
	wd.syslog.Info(fmt.Sprintf("workflow #%s's run #%s started", task.WorkflowId, task.RunId))
	we.Invoke(task.ctx, engine.WorkflowExecution{
		WorkflowId:          workflow.Id,
		WorkflowName:        workflow.Name,
		WorkflowDescription: workflow.Description,
		RunId:               workflowRun.Id,
		RunTrigger:          workflowRun.Trigger,
		RunAt:               workflowRun.StartedAt,
		Graph:               workflowRun.Graph,
	})
	wd.syslog.Info(fmt.Sprintf("workflow #%s's run #%s stopped", task.WorkflowId, task.RunId))
}

func (wd *workflowDispatcher) tryNextAsync() {
	wd.taskMtx.Lock()
	defer wd.taskMtx.Unlock()

	if !wd.booted {
		return
	}

	// Iterate over a snapshot because queued entries may be withdrawn while we
	// make repository calls (e.g. by a concurrent workflow shutdown).
	for _, pendingTask := range slices.Clone(wd.pendingRunQueue) {
		workflowRun, err := wd.workflowRunRepo.GetById(context.Background(), pendingTask.RunId)
		if err != nil {
			wd.syslog.Error(fmt.Sprintf("failed to get workrun #%s record", pendingTask.RunId), slog.Any("error", err))
			continue
		}

		// 关闭边界：工作流已进入删除流程时，撤出该工作流的排队代际，
		// 其数据库状态由 ShutdownWorkflow 的取消扫描负责纠正
		if _, closing := wd.closingWorkflows[workflowRun.WorkflowId]; closing {
			wd.syslog.Warn(fmt.Sprintf("workflow #%s's run #%s is withdrawn, because the workflow is closing", workflowRun.WorkflowId, workflowRun.Id))
			wd.pendingRunQueue = lo.Filter(wd.pendingRunQueue, func(t *taskInfo, _ int) bool { return t.WorkflowId != workflowRun.WorkflowId })
			continue
		}

		// 排队记录已不是 pending（例如已被取消），丢弃陈旧的队列项
		if workflowRun.Status != domain.WorkflowRunStatusTypePending {
			wd.pendingRunQueue = lo.Filter(wd.pendingRunQueue, func(t *taskInfo, _ int) bool { return t.RunId != pendingTask.RunId })
			continue
		}

		var hasSameWorkflowTask bool // 相同 Workflow 的任务同一时间只能有一个 Run 在执行
		for _, processingTask := range wd.processingTasks {
			if processingTask.WorkflowId == workflowRun.WorkflowId {
				hasSameWorkflowTask = true
				break
			}
		}

		if hasSameWorkflowTask {
			wd.syslog.Warn(fmt.Sprintf("workflow #%s's run #%s is pending, because tasks that belonging to the same workflow already exists", workflowRun.WorkflowId, workflowRun.Id))
			continue
		}

		if len(wd.processingTasks) >= wd.concurrency && wd.concurrency > 0 {
			wd.syslog.Warn(fmt.Sprintf("workflow #%s's run #%s is pending, because the maximum concurrency (limit: %d) has been reached", workflowRun.WorkflowId, workflowRun.Id, wd.concurrency))
			continue
		}

		// 二次确认队列项仍在，避免与撤出/取消竞争
		if !lo.ContainsBy(wd.pendingRunQueue, func(t *taskInfo) bool { return t.RunId == pendingTask.RunId }) {
			continue
		}

		ctxRun, ctxCancel := context.WithCancel(context.Background())
		task := &taskInfo{
			WorkflowId: workflowRun.WorkflowId,
			RunId:      workflowRun.Id,
			ctx:        ctxRun,
			cancel:     ctxCancel,
			done:       make(chan struct{}),
		}
		wd.pendingRunQueue = lo.Filter(wd.pendingRunQueue, func(t *taskInfo, _ int) bool { return t.RunId != pendingTask.RunId })
		wd.processingTasks[pendingTask.RunId] = task
		wd.activeTasks[pendingTask.RunId] = task
		wd.syslog.Info(fmt.Sprintf("workflow #%s's run #%s is being dispatched ...", task.WorkflowId, task.RunId))

		go func() { wd.tryExecuteAsync(task) }()
		return
	}
}

func newWorkflowDispatcher() WorkflowDispatcher {
	return &workflowDispatcher{
		concurrency: envMaxWorkers,

		pendingRunQueue:  make([]*taskInfo, 0),
		processingTasks:  make(map[string]*taskInfo),
		activeTasks:      make(map[string]*taskInfo),
		closingWorkflows: make(map[string]struct{}),

		workflowRepo:    repository.NewWorkflowRepository(),
		workflowRunRepo: repository.NewWorkflowRunRepository(),
		workflowLogRepo: repository.NewWorkflowLogRepository(),

		syslog: app.GetLogger(),
	}
}
