package workflow

import (
	"context"
	"net/http"
	"strings"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"github.com/certimate-go/certimate/internal/app"
	"github.com/certimate-go/certimate/internal/domain"
)

// workflowVersionGuardedFields 只能通过带代际校验的专用接口（草稿保存/发布/回滚）修改，
// 禁止通过通用记录更新接口直接写入，否则陈旧页面或手工请求会绕过乐观并发控制。
var workflowVersionGuardedFields = map[string]struct{}{
	"trigger":        {},
	"triggerCron":    {},
	"graphDraft":     {},
	"graphContent":   {},
	"hasDraft":       {},
	"hasContent":     {},
	"draftVersion":   {},
	"contentVersion": {},
}

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
		// 在通用更新接口落库前拦截受保护字段：编排内容只能走 /api/workflows/:id/{draft,publish,rollback}
		if err := guardWorkflowVersionedFields(e); err != nil {
			return err
		}

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
		if err := e.Next(); err != nil {
			return err
		}

		if err := onWorkflowRecordDelete(e.Request.Context(), e.App, e.Record); err != nil {
			app.GetLogger().Error(err.Error())
			return err
		}

		return nil
	})
}

// guardWorkflowVersionedFields 检查通用记录更新请求是否携带了受保护字段，
// 包括 PocketBase 的 +/- 修饰写法（如 graphDraft+）。命中时返回可识别的冲突结果且不落库。
func guardWorkflowVersionedFields(e *core.RecordRequestEvent) error {
	info, err := e.RequestInfo()
	if err != nil {
		return apis.NewBadRequestError("Failed to read request data.", err)
	}

	for key := range info.Body {
		normalized := strings.TrimLeft(key, "+")
		normalized = strings.TrimRight(normalized, "+-")
		if _, ok := workflowVersionGuardedFields[normalized]; ok {
			return apis.NewApiError(
				http.StatusConflict,
				"workflow version conflict: the workflow graph, trigger and their versions can only be modified through the versioned draft/publish/rollback APIs, please reload and try again",
				map[string]any{"code": "WORKFLOW_VERSION_CONFLICT", "field": normalized},
			)
		}
	}

	return nil
}

func onWorkflowRecordCreateOrUpdate(_ context.Context, _ core.App, record *core.Record) error {
	scheduler := app.GetScheduler()

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

func onWorkflowRecordDelete(_ context.Context, _ core.App, record *core.Record) error {
	scheduler := app.GetScheduler()

	// 从数据库删除时，同时移除定时任务
	scheduler.Remove(buildPbJobKey(record.Id))

	return nil
}
