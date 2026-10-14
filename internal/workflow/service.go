package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
}

func NewWorkflowService(workflowRepo workflowRepository, workflowRunRepo workflowRunRepository) *WorkflowService {
	srv := &WorkflowService{
		dispatcher: dispatcher.GetSingletonDispatcher(),

		workflowRepo:    workflowRepo,
		workflowRunRepo: workflowRunRepo,
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

	if err := s.dispatcher.Start(ctx, workflowRun.Id); err != nil {
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

// SaveDraft 基于客户端读取到的草稿代际保存草稿。
// 版本冲突时透传 ErrWorkflowVersionConflict，不做静默合并或服务端重试。
func (s *WorkflowService) SaveDraft(ctx context.Context, req *dtos.WorkflowSaveDraftReq) (*domain.Workflow, error) {
	if req.Graph == nil {
		return nil, domain.ErrInvalidParams
	}
	if req.BaseDraftVersion < 0 {
		return nil, domain.ErrInvalidParams
	}

	workflow, err := s.workflowRepo.SaveDraft(ctx, req.WorkflowId, req.Graph, req.BaseDraftVersion)
	if err != nil {
		return nil, err
	}

	return workflow, nil
}

// PublishDraft 原子发布对应草稿代：校验草稿版本、图与触发配置，
// 成功提交后才把新的触发方式/定时表达式同步到定时调度；失败时调度与新运行仍使用上一份完整发布版本。
func (s *WorkflowService) PublishDraft(ctx context.Context, req *dtos.WorkflowPublishDraftReq) (*domain.Workflow, error) {
	if req.BaseDraftVersion < 0 {
		return nil, domain.ErrInvalidParams
	}

	workflow, err := s.workflowRepo.PublishDraft(ctx, req.WorkflowId, req.BaseDraftVersion)
	if err != nil {
		return nil, err
	}

	// 仅在事务提交成功后切换定时任务，保证不会出现"新调度 + 旧快照"的中间状态
	s.syncSchedule(workflow)

	return workflow, nil
}

// RollbackDraft 基于客户端读取到的草稿代际回滚到当前已发布内容。
func (s *WorkflowService) RollbackDraft(ctx context.Context, req *dtos.WorkflowRollbackDraftReq) (*domain.Workflow, error) {
	if req.BaseDraftVersion < 0 {
		return nil, domain.ErrInvalidParams
	}

	workflow, err := s.workflowRepo.RollbackDraft(ctx, req.WorkflowId, req.BaseDraftVersion)
	if err != nil {
		return nil, err
	}

	s.syncSchedule(workflow)

	return workflow, nil
}

// syncSchedule 依据已提交的持久状态重建该工作流的定时任务：
// 停用或手动触发时移除任务，启用且定时触发时按最新表达式（重新）注册。
func (s *WorkflowService) syncSchedule(workflow *domain.Workflow) {
	scheduler := app.GetScheduler()
	jobKey := buildPbJobKey(workflow.Id)

	if !workflow.Enabled || workflow.Trigger != domain.WorkflowTriggerTypeScheduled {
		scheduler.Remove(jobKey)
		return
	}

	if err := registerWorkflowJob(s, workflow.Id, workflow.TriggerCron); err != nil {
		// 表达式已在发布事务中校验通过，此处失败只可能是暂时故障；
		// 重启时 InitSchedule 会依据持久状态重建任务，不影响已提交快照的完整性。
		app.GetLogger().Error(fmt.Sprintf("failed to sync cron job after workflow #%s commit", workflow.Id), slog.Any("error", err))
	}
}

func (s *WorkflowService) Shutdown(ctx context.Context) {
	s.dispatcher.Shutdown(ctx)
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
