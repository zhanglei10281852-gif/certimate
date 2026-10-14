package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	pbcron "github.com/pocketbase/pocketbase/tools/cron"

	"github.com/certimate-go/certimate/internal/app"
	"github.com/certimate-go/certimate/internal/domain"
)

type WorkflowRepository struct{}

func NewWorkflowRepository() *WorkflowRepository {
	return &WorkflowRepository{}
}

func (r *WorkflowRepository) ListEnabledScheduled(ctx context.Context) ([]*domain.Workflow, error) {
	records, err := app.GetApp().FindRecordsByFilter(
		domain.CollectionNameWorkflow,
		"enabled={:enabled} && trigger={:trigger}",
		"-created",
		0, 0,
		dbx.Params{"enabled": true, "trigger": domain.WorkflowTriggerTypeScheduled.String()},
	)
	if err != nil {
		return nil, err
	}

	workflows := make([]*domain.Workflow, 0)
	for _, record := range records {
		workflow, err := r.castRecordToModel(record)
		if err != nil {
			return nil, err
		}

		workflows = append(workflows, workflow)
	}

	return workflows, nil
}

func (r *WorkflowRepository) GetById(ctx context.Context, id string) (*domain.Workflow, error) {
	record, err := app.GetApp().FindRecordById(domain.CollectionNameWorkflow, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrRecordNotFound
		}
		return nil, err
	}

	return r.castRecordToModel(record)
}

func (r *WorkflowRepository) Save(ctx context.Context, workflow *domain.Workflow) (*domain.Workflow, error) {
	collection, err := app.GetApp().FindCollectionByNameOrId(domain.CollectionNameWorkflow)
	if err != nil {
		return workflow, err
	}

	var record *core.Record
	if workflow.Id == "" {
		record = core.NewRecord(collection)
	} else {
		record, err = app.GetApp().FindRecordById(collection, workflow.Id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return workflow, domain.ErrRecordNotFound
			}
			return workflow, err
		}
	}

	record.Set("name", workflow.Name)
	record.Set("description", workflow.Description)
	record.Set("trigger", workflow.Trigger.String())
	record.Set("triggerCron", workflow.TriggerCron)
	record.Set("enabled", workflow.Enabled)
	record.Set("graphDraft", workflow.GraphDraft)
	record.Set("graphContent", workflow.GraphContent)
	record.Set("hasDraft", workflow.HasDraft)
	record.Set("hasContent", workflow.HasContent)
	record.Set("draftVersion", workflow.DraftVersion)
	record.Set("contentVersion", workflow.ContentVersion)
	record.Set("lastRunRef", workflow.LastRunId)
	record.Set("lastRunStatus", workflow.LastRunStatus.String())
	record.Set("lastRunTime", workflow.LastRunTime)
	if err := app.GetApp().Save(record); err != nil {
		return workflow, err
	}

	workflow.Id = record.Id
	workflow.CreatedAt = record.GetDateTime("created").Time()
	workflow.UpdatedAt = record.GetDateTime("updated").Time()
	return workflow, nil
}

// SaveDraft 保存草稿，只能推进 baseDraftVersion 所指向的草稿代际。
// 若内容与服务端当前草稿完全一致则不落库、不推进代际；否则在同一事务内推进 draftVersion。
func (r *WorkflowRepository) SaveDraft(ctx context.Context, workflowId string, graph *domain.WorkflowGraph, baseDraftVersion int) (*domain.Workflow, error) {
	if graph == nil {
		return nil, domain.ErrInvalidParams
	}

	collection, err := app.GetApp().FindCollectionByNameOrId(domain.CollectionNameWorkflow)
	if err != nil {
		return nil, err
	}

	var workflow *domain.Workflow
	err = app.GetApp().RunInTransaction(func(txApp core.App) error {
		record, err := r.claimDraftGeneration(txApp, collection, workflowId, baseDraftVersion)
		if err != nil {
			return err
		}

		currentDraft := &domain.WorkflowGraph{}
		if err := record.UnmarshalJSONField("graphDraft", currentDraft); err != nil {
			return fmt.Errorf("field 'graphDraft' is malformed: %w", err)
		}

		// 以已发布内容为基准计算是否仍存在未发布更改；从未发布过时一定存在草稿
		hasDraft := true
		if record.GetBool("hasContent") {
			currentContent := &domain.WorkflowGraph{}
			if err := record.UnmarshalJSONField("graphContent", currentContent); err != nil {
				return fmt.Errorf("field 'graphContent' is malformed: %w", err)
			}
			hasDraft = !reflect.DeepEqual(currentContent, graph)
		}

		// 幂等：草稿内容与 hasDraft 均未变化时直接返回当前版本，不推进代际
		if reflect.DeepEqual(currentDraft, graph) && record.GetBool("hasDraft") == hasDraft {
			workflow, err = r.castRecordToModel(record)
			return err
		}

		record.Set("graphDraft", graph)
		record.Set("hasDraft", hasDraft)
		record.Set("draftVersion", baseDraftVersion+1)
		if err := txApp.Save(record); err != nil {
			return err
		}

		workflow, err = r.castRecordToModel(record)
		return err
	})
	if err != nil {
		return nil, err
	}

	return workflow, nil
}

// PublishDraft 原子发布：要求当前草稿代际仍等于 baseDraftVersion，
// 校验通过后把图、触发方式、定时表达式作为同一个已发布代提交，同时推进 draftVersion 与 contentVersion。
// 校验失败时事务回滚，服务端既有草稿与已发布内容均保持不变。
func (r *WorkflowRepository) PublishDraft(ctx context.Context, workflowId string, baseDraftVersion int) (*domain.Workflow, error) {
	collection, err := app.GetApp().FindCollectionByNameOrId(domain.CollectionNameWorkflow)
	if err != nil {
		return nil, err
	}

	var workflow *domain.Workflow
	err = app.GetApp().RunInTransaction(func(txApp core.App) error {
		record, err := r.claimDraftGeneration(txApp, collection, workflowId, baseDraftVersion)
		if err != nil {
			return err
		}

		draft := &domain.WorkflowGraph{}
		if err := record.UnmarshalJSONField("graphDraft", draft); err != nil {
			return fmt.Errorf("field 'graphDraft' is malformed: %w", err)
		}
		if len(draft.Nodes) == 0 {
			return domain.NewError(400, "workflow graph draft is empty")
		}
		if err := draft.Verify(); err != nil {
			return domain.NewError(400, fmt.Sprintf("workflow graph draft is invalid: %s", err.Error()))
		}

		trigger, triggerCron, err := draft.TriggerSettings()
		if err != nil {
			return domain.NewError(400, err.Error())
		}
		if trigger == domain.WorkflowTriggerTypeScheduled {
			if triggerCron == "" {
				return domain.NewError(400, "trigger cron expression is required for scheduled workflow")
			}
			if _, err := pbcron.NewSchedule(triggerCron); err != nil {
				return domain.NewError(400, fmt.Sprintf("invalid trigger cron expression: %s", err.Error()))
			}
		}

		// 图、触发方式、定时表达式在同一事务中一起成为新的已发布代
		record.Set("graphContent", record.GetRaw("graphDraft"))
		record.Set("trigger", trigger.String())
		record.Set("triggerCron", triggerCron)
		record.Set("hasContent", true)
		record.Set("hasDraft", false)
		record.Set("draftVersion", baseDraftVersion+1)
		record.Set("contentVersion", record.GetInt("contentVersion")+1)
		if err := txApp.Save(record); err != nil {
			return err
		}

		workflow, err = r.castRecordToModel(record)
		return err
	})
	if err != nil {
		return nil, err
	}

	return workflow, nil
}

// RollbackDraft 用当前已发布内容重置草稿，要求草稿代际仍等于 baseDraftVersion，
// 以避免用陈旧页面丢弃他人新保存的草稿或新发布的内容。
func (r *WorkflowRepository) RollbackDraft(ctx context.Context, workflowId string, baseDraftVersion int) (*domain.Workflow, error) {
	collection, err := app.GetApp().FindCollectionByNameOrId(domain.CollectionNameWorkflow)
	if err != nil {
		return nil, err
	}

	var workflow *domain.Workflow
	err = app.GetApp().RunInTransaction(func(txApp core.App) error {
		record, err := r.claimDraftGeneration(txApp, collection, workflowId, baseDraftVersion)
		if err != nil {
			return err
		}

		if !record.GetBool("hasContent") {
			return domain.ErrWorkflowNotPublished
		}

		content := &domain.WorkflowGraph{}
		if err := record.UnmarshalJSONField("graphContent", content); err != nil {
			return fmt.Errorf("field 'graphContent' is malformed: %w", err)
		}
		if len(content.Nodes) == 0 {
			return domain.NewError(400, "workflow graph content is empty")
		}

		trigger, triggerCron, err := content.TriggerSettings()
		if err != nil {
			return domain.NewError(400, err.Error())
		}
		if trigger == domain.WorkflowTriggerTypeScheduled {
			if _, err := pbcron.NewSchedule(triggerCron); err != nil {
				return domain.NewError(400, fmt.Sprintf("invalid trigger cron expression: %s", err.Error()))
			}
		}

		record.Set("graphDraft", record.GetRaw("graphContent"))
		record.Set("trigger", trigger.String())
		record.Set("triggerCron", triggerCron)
		record.Set("hasDraft", false)
		record.Set("draftVersion", baseDraftVersion+1)
		if err := txApp.Save(record); err != nil {
			return err
		}

		workflow, err = r.castRecordToModel(record)
		return err
	})
	if err != nil {
		return nil, err
	}

	return workflow, nil
}

// claimDraftGeneration 在事务内以条件 UPDATE 占用并校验草稿代际。
// 它必须是事务中的首个写操作：WHERE draftVersion = baseDraftVersion 使得版本校验与写锁获取原子完成，
// 并发提交时落后的一方会得到 0 行受影响，从而返回 ErrWorkflowVersionConflict 而不会覆盖新内容。
func (r *WorkflowRepository) claimDraftGeneration(txApp core.App, collection *core.Collection, workflowId string, baseDraftVersion int) (*core.Record, error) {
	result, err := txApp.DB().
		NewQuery(fmt.Sprintf(
			"UPDATE %s SET draftVersion = draftVersion WHERE id = {:id} AND draftVersion = {:baseDraftVersion}",
			domain.CollectionNameWorkflow,
		)).
		Bind(dbx.Params{"id": workflowId, "baseDraftVersion": baseDraftVersion}).
		Execute()
	if err != nil {
		return nil, err
	}

	record, findErr := txApp.FindRecordById(collection, workflowId)
	if findErr != nil {
		if errors.Is(findErr, sql.ErrNoRows) {
			return nil, domain.ErrRecordNotFound
		}
		return nil, findErr
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, domain.ErrWorkflowVersionConflict
	}

	return record, nil
}

func (r *WorkflowRepository) castRecordToModel(record *core.Record) (*domain.Workflow, error) {
	if record == nil {
		return nil, fmt.Errorf("the record is nil")
	}

	graphDraft := &domain.WorkflowGraph{}
	if err := record.UnmarshalJSONField("graphDraft", graphDraft); err != nil {
		return nil, fmt.Errorf("field 'graphDraft' is malformed")
	}

	graphContent := &domain.WorkflowGraph{}
	if err := record.UnmarshalJSONField("graphContent", graphContent); err != nil {
		return nil, fmt.Errorf("field 'graphContent' is malformed")
	}

	workflow := &domain.Workflow{
		Meta: domain.Meta{
			Id:        record.Id,
			CreatedAt: record.GetDateTime("created").Time(),
			UpdatedAt: record.GetDateTime("updated").Time(),
		},
		Name:           record.GetString("name"),
		Description:    record.GetString("description"),
		Trigger:        domain.WorkflowTriggerType(record.GetString("trigger")),
		TriggerCron:    record.GetString("triggerCron"),
		Enabled:        record.GetBool("enabled"),
		GraphDraft:     graphDraft,
		GraphContent:   graphContent,
		HasDraft:       record.GetBool("hasDraft"),
		HasContent:     record.GetBool("hasContent"),
		DraftVersion:   record.GetInt("draftVersion"),
		ContentVersion: record.GetInt("contentVersion"),
		LastRunId:      record.GetString("lastRunRef"),
		LastRunStatus:  domain.WorkflowRunStatusType(record.GetString("lastRunStatus")),
		LastRunTime:    record.GetDateTime("lastRunTime").Time(),
	}
	return workflow, nil
}
