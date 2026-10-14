package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pocketbase/dbx"

	"github.com/certimate-go/certimate/internal/app"
	"github.com/certimate-go/certimate/internal/domain"
	"github.com/certimate-go/certimate/internal/domain/dtos"
	"github.com/certimate-go/certimate/internal/settings"
	"github.com/certimate-go/certimate/internal/workflow/dispatcher"
)

type WorkflowService struct {
	dispatcher dispatcher.WorkflowDispatcher

	workflowRepo    workflowRepository
	workflowRunRepo workflowRunRepository
	certificateRepo certificateRepository
	deleteLocks     sync.Map // Key: WorkflowId, Value: *sync.Mutex
}

func NewWorkflowService(workflowRepo workflowRepository, workflowRunRepo workflowRunRepository, certificateRepo certificateRepository) *WorkflowService {
	srv := &WorkflowService{
		dispatcher: dispatcher.GetSingletonDispatcher(),

		workflowRepo:    workflowRepo,
		workflowRunRepo: workflowRunRepo,
		certificateRepo: certificateRepo,
	}
	return srv
}

func (s *WorkflowService) InitSchedule(ctx context.Context) error {
	// 每日清理工作流运行历史
	app.GetScheduler().MustAdd("cleanupWorkflowHistoryRuns", "0 0 * * *", func() {
		s.cleanupHistoryRuns(context.Background())
	})

	// 初始化工作流调度器
	if err := s.dispatcher.Bootup(ctx); err != nil {
		panic(err)
	}

	// 注册工作流后台任务
	{
		workflows, err := s.workflowRepo.ListEnabledScheduled(ctx)
		if err != nil {
			return err
		}

		var errs []error
		for _, workflow := range workflows {
			if err := registerWorkflowJob(s, workflow.Id, workflow.TriggerCron); err != nil {
				errs = append(errs, err)
			}
		}
		if len(errs) > 0 {
			return errors.Join(errs...)
		}
	}

	return nil
}

func (s *WorkflowService) GetStatistics(ctx context.Context) (*dtos.WorkflowStatisticsResp, error) {
	stats := s.dispatcher.GetStatistics()
	return &dtos.WorkflowStatisticsResp{
		Concurrency:      stats.Concurrency,
		PendingRunIds:    stats.PendingRunIds,
		ProcessingRunIds: stats.ProcessingRunIds,
	}, nil
}

func (s *WorkflowService) StartRun(ctx context.Context, req *dtos.WorkflowStartRunReq) (*dtos.WorkflowStartRunResp, error) {
	workflow, err := s.workflowRepo.GetById(ctx, req.WorkflowId)
	if err != nil {
		return nil, err
	}

	// 关闭边界：正在删除的工作流拒绝一切新的手动/定时运行
	if workflow.IsClosing() {
		return nil, fmt.Errorf("workflow #%s is being deleted, starting a new run is not allowed", workflow.Id)
	}

	if req.RunTrigger == domain.WorkflowTriggerTypeManual && (workflow.LastRunStatus == domain.WorkflowRunStatusTypePending || workflow.LastRunStatus == domain.WorkflowRunStatusTypeProcessing) {
		return nil, fmt.Errorf("workflow is already pending or processing")
	} else if workflow.GraphContent == nil {
		return nil, fmt.Errorf("workflow graph content is empty")
	} else if err := workflow.GraphContent.Verify(); err != nil {
		return nil, fmt.Errorf("workflow graph content is invalid: %w", err)
	}

	workflowRun := &domain.WorkflowRun{
		WorkflowId: workflow.Id,
		Status:     domain.WorkflowRunStatusTypePending,
		Trigger:    req.RunTrigger,
		StartedAt:  time.Now(),
		Graph:      workflow.GraphContent.Clone(),
	}
	if resp, err := s.workflowRunRepo.Save(ctx, workflowRun); err != nil {
		return nil, err
	} else {
		workflowRun = resp
	}

	if err := s.dispatcher.Start(ctx, workflow.Id, workflowRun.Id); err != nil {
		// 关闭竞态：运行记录已落库但被关闭边界拒绝入队，将其收敛为已取消，
		// 避免留下悬挂的排队代际
		workflowRun.Status = domain.WorkflowRunStatusTypeCanceled
		workflowRun.EndedAt = time.Now()
		if _, saveErr := s.workflowRunRepo.Save(context.WithoutCancel(ctx), workflowRun); saveErr != nil {
			app.GetLogger().Error(fmt.Sprintf("failed to cancel rejected run #%s of closing workflow #%s", workflowRun.Id, workflow.Id), slog.Any("error", saveErr))
		}
		return nil, err
	}

	return &dtos.WorkflowStartRunResp{RunId: workflowRun.Id}, nil
}

func (s *WorkflowService) CancelRun(ctx context.Context, req *dtos.WorkflowCancelRunReq) (*dtos.WorkflowCancelRunResp, error) {
	workflow, err := s.workflowRepo.GetById(ctx, req.WorkflowId)
	if err != nil {
		return nil, err
	}

	workflowRun, err := s.workflowRunRepo.GetById(ctx, req.RunId)
	if err != nil {
		return nil, err
	} else if workflowRun.WorkflowId != workflow.Id {
		return nil, fmt.Errorf("workflow run not found")
	} else if workflowRun.Status != domain.WorkflowRunStatusTypePending && workflowRun.Status != domain.WorkflowRunStatusTypeProcessing {
		return nil, fmt.Errorf("workflow run is not pending or processing")
	}

	if err := s.dispatcher.Cancel(ctx, workflowRun.Id); err != nil {
		return nil, err
	}

	return &dtos.WorkflowCancelRunResp{}, nil
}

func (s *WorkflowService) Shutdown(ctx context.Context) {
	s.dispatcher.Shutdown(ctx)
}

// acquireDeleteLock returns a release function for the per-workflow lock that
// serializes concurrent deletion attempts of the same workflow.
func (s *WorkflowService) acquireDeleteLock(workflowId string) func() {
	mu, _ := s.deleteLocks.LoadOrStore(workflowId, &sync.Mutex{})
	lock := mu.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

// OnWorkflowDeleting is the ownership shutdown process that MUST complete
// successfully before the workflow record is deleted. It persists the close
// fence, stops future scheduled triggers, preserves certificate provenance
// and drains every queued or in-flight run (including child processes).
//
// It is idempotent. If any stage fails it returns a diagnosable error and the
// workflow is left in a consistent, fully retryable state (persisted as
// "closing", cron removed and new runs blocked) rather than half-deleted.
func (s *WorkflowService) OnWorkflowDeleting(ctx context.Context, workflowId string) error {
	// 与删除串行：工作流已被并发删除时直接放行，交由后续删除流程返回不存在
	if _, err := s.workflowRepo.GetById(ctx, workflowId); err != nil {
		if domain.IsRecordNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("workflow #%s: failed to load before closing: %w", workflowId, err)
	}

	unlock := s.acquireDeleteLock(workflowId)
	defer unlock()

	// The drain is intentionally detached from the HTTP request context so a
	// client disconnect cannot leave the deletion half-applied.
	bgCtx := context.WithoutCancel(ctx)

	workflow, err := s.workflowRepo.GetById(bgCtx, workflowId)
	if err != nil {
		if domain.IsRecordNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("workflow #%s: failed to load before closing: %w", workflowId, err)
	}

	// 1. 持久化关闭边界：阻止重启后的调度恢复以及一切新的手动/定时运行
	if err := s.workflowRepo.MarkClosing(bgCtx, workflowId); err != nil {
		return fmt.Errorf("workflow #%s: failed to mark as closing: %w", workflowId, err)
	}

	// 2. 撤出定时任务，杜绝新的定时触发
	app.GetScheduler().Remove(buildPbJobKey(workflowId))

	// 3. 为按关系策略保留的证书补齐来源快照，避免删除后成为无法解释的孤立记录
	if err := s.backfillCertificateSnapshots(bgCtx, workflow); err != nil {
		return err
	}

	// 4. 撤出等待队列、取消当前执行及其子进程，并等待活动所有权收敛
	if err := s.dispatcher.ShutdownWorkflow(bgCtx, workflowId); err != nil {
		return err
	}

	return nil
}

// OnWorkflowDeleted releases the in-memory close fence after the workflow
// record (and its cascade data) has been deleted successfully.
func (s *WorkflowService) OnWorkflowDeleted(workflowId string) {
	s.dispatcher.ReleaseWorkflow(workflowId)
}

// backfillCertificateSnapshots writes a provenance snapshot to every retained
// certificate that still belongs to the workflow and does not have one yet, so
// that nulling workflow/run relations on deletion does not orphan it.
func (s *WorkflowService) backfillCertificateSnapshots(ctx context.Context, workflow *domain.Workflow) error {
	certificates, err := s.certificateRepo.ListByWorkflowId(ctx, workflow.Id)
	if err != nil {
		return fmt.Errorf("workflow #%s: failed to list certificates for source snapshot: %w", workflow.Id, err)
	}
	if len(certificates) == 0 {
		return nil
	}

	runs, err := s.workflowRunRepo.ListByWorkflowId(ctx, workflow.Id)
	if err != nil {
		return fmt.Errorf("workflow #%s: failed to list runs for source snapshot: %w", workflow.Id, err)
	}

	changed := 0
	for _, certificate := range certificates {
		if certificate.SourceSnapshot != nil && certificate.SourceSnapshot.WorkflowId != "" {
			continue
		}

		snapshot := &domain.CertificateSourceSnapshot{
			WorkflowId:   workflow.Id,
			WorkflowName: workflow.Name,
			RunId:        certificate.WorkflowRunId,
			NodeId:       certificate.WorkflowNodeId,
		}

		if run := findWorkflowRun(runs, certificate.WorkflowRunId); run != nil {
			snapshot.RunTrigger = run.Trigger.String()
			if run.Graph != nil && certificate.WorkflowNodeId != "" {
				if node, ok := run.Graph.GetNodeById(certificate.WorkflowNodeId); ok {
					snapshot.NodeName = node.Data.Name
				}
			}
		}

		certificate.SourceSnapshot = snapshot
		if _, err := s.certificateRepo.Save(ctx, certificate); err != nil {
			return fmt.Errorf("workflow #%s: failed to persist source snapshot for certificate #%s: %w", workflow.Id, certificate.Id, err)
		}
		changed++
	}

	if changed > 0 {
		app.GetLogger().Info(fmt.Sprintf("workflow #%s: source snapshot backfilled for %d certificate(s)", workflow.Id, changed))
	}

	return nil
}

func findWorkflowRun(runs []*domain.WorkflowRun, runId string) *domain.WorkflowRun {
	if runId == "" {
		return nil
	}
	for _, run := range runs {
		if run.Id == runId {
			return run
		}
	}
	return nil
}

func (s *WorkflowService) cleanupHistoryRuns(ctx context.Context) error {
	globalSettingsForPersistence := settings.GetGlobalSettingsForPersistence()
	if globalSettingsForPersistence.WorkflowRunsRetentionMaxDays != 0 {
		ret, err := s.workflowRunRepo.DeleteWithExprs(ctx,
			dbx.NewExp(fmt.Sprintf("status!='%s'", domain.WorkflowRunStatusTypePending)),
			dbx.NewExp(fmt.Sprintf("status!='%s'", domain.WorkflowRunStatusTypeProcessing)),
			dbx.NewExp(fmt.Sprintf("endedAt<DATETIME('now', '-%d days')", globalSettingsForPersistence.WorkflowRunsRetentionMaxDays)),
		)
		if err != nil {
			app.GetLogger().Error("failed to delete workflow history runs", slog.Any("error", err))
			return err
		}

		if ret > 0 {
			app.GetLogger().Info(fmt.Sprintf("cleanup %d workflow history runs", ret))
		}
	}

	return nil
}
