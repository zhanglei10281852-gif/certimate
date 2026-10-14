package workflow

import (
	"context"

	"github.com/pocketbase/pocketbase/core"

	"github.com/certimate-go/certimate/internal/app"
	"github.com/certimate-go/certimate/internal/domain"
)

func registerWorkflowRecordEvents() {
	pb := app.GetApp()
	pb.OnRecordCreateRequest(domain.CollectionNameWorkflow).BindFunc(func(e *core.RecordRequestEvent) error {
		if err := e.Next(); err != nil {
			return err
		}

		if err := onWorkflowRecordCreateOrUpdate(e.Request.Context(), e.App, e.Record); err != nil {
			app.GetLogger().Error(err.Error())
			return err
		}

		return nil
	})
	pb.OnRecordUpdateRequest(domain.CollectionNameWorkflow).BindFunc(func(e *core.RecordRequestEvent) error {
		if err := e.Next(); err != nil {
			return err
		}

		if err := onWorkflowRecordCreateOrUpdate(e.Request.Context(), e.App, e.Record); err != nil {
			app.GetLogger().Error(err.Error())
			return err
		}

		return nil
	})
	pb.OnRecordDeleteRequest(domain.CollectionNameWorkflow).BindFunc(func(e *core.RecordRequestEvent) error {
		// 删除被视为资源所有权的关闭过程，必须在记录真正提交删除之前完成：
		// 阻止新运行、撤出等待队列、停止当前执行及子进程，并等待所有权收敛。
		// 任一步骤失败都中止删除并返回错误，保留可重试的完整状态。
		if err := thisSvcInst().OnWorkflowDeleting(e.Request.Context(), e.Record.Id); err != nil {
			app.GetLogger().Error(err.Error())
			return err
		}

		// 所有权收敛后才提交数据库删除（级联清理运行、输出与日志）
		if err := e.Next(); err != nil {
			return err
		}

		// 删除成功后释放内存关闭边界
		thisSvcInst().OnWorkflowDeleted(e.Record.Id)
		return nil
	})
}

func onWorkflowRecordCreateOrUpdate(_ context.Context, _ core.App, record *core.Record) error {
	scheduler := app.GetScheduler()

	// 处于删除关闭流程中的工作流，不得被更新请求重新恢复定时任务
	if record.GetString("lifecycle") == domain.WorkflowLifecycleClosing.String() {
		scheduler.Remove(buildPbJobKey(record.Id))
		return nil
	}

	// 向数据库插入/更新时，同时更新定时任务
	enabled := record.GetBool("enabled")
	trigger := record.GetString("trigger")
	triggerCron := record.GetString("triggerCron")

	// 如果非定时触发或未启用，移除定时任务
	if !enabled || trigger != domain.WorkflowTriggerTypeScheduled.String() {
		scheduler.Remove(buildPbJobKey(record.Id))
		return nil
	}

	// 反之，重新添加定时任务
	if err := registerWorkflowJob(thisSvcInst(), record.Id, triggerCron); err != nil {
		return err
	}

	return nil
}
