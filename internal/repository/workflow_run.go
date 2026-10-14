package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/certimate-go/certimate/internal/app"
	"github.com/certimate-go/certimate/internal/domain"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

type WorkflowRunRepository struct{}

func NewWorkflowRunRepository() *WorkflowRunRepository {
	return &WorkflowRunRepository{}
}

func (r *WorkflowRunRepository) GetById(ctx context.Context, id string) (*domain.WorkflowRun, error) {
	record, err := app.GetApp().FindRecordById(domain.CollectionNameWorkflowRun, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrRecordNotFound
		}
		return nil, err
	}

	return r.castRecordToModel(record)
}

func (r *WorkflowRunRepository) Save(ctx context.Context, workflowRun *domain.WorkflowRun) (*domain.WorkflowRun, error) {
	collection, err := app.GetApp().FindCollectionByNameOrId(domain.CollectionNameWorkflowRun)
	if err != nil {
		return workflowRun, err
	}

	var record *core.Record
	if workflowRun.Id == "" {
		record = core.NewRecord(collection)
	} else {
		record, err = app.GetApp().FindRecordById(collection, workflowRun.Id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return workflowRun, err
			}
			record = core.NewRecord(collection)
		}
	}

	record.Set("workflowRef", workflowRun.WorkflowId)
	record.Set("trigger", workflowRun.Trigger.String())
	record.Set("status", workflowRun.Status.String())
	record.Set("startedAt", workflowRun.StartedAt)
	record.Set("endedAt", workflowRun.EndedAt)
	record.Set("graph", workflowRun.Graph)
	record.Set("error", workflowRun.Error)
	err = app.GetApp().Save(record)
	if err != nil {
		return workflowRun, err
	}

	workflowRun.Id = record.Id
	workflowRun.CreatedAt = record.GetDateTime("created").Time()
	workflowRun.UpdatedAt = record.GetDateTime("updated").Time()
	return workflowRun, nil
}

func (r *WorkflowRunRepository) SaveWithCascading(ctx context.Context, workflowRun *domain.WorkflowRun) (*domain.WorkflowRun, error) {
	collection, err := app.GetApp().FindCollectionByNameOrId(domain.CollectionNameWorkflowRun)
	if err != nil {
		return workflowRun, err
	}

	var record *core.Record
	if workflowRun.Id == "" {
		record = core.NewRecord(collection)
	} else {
		record, err = app.GetApp().FindRecordById(collection, workflowRun.Id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return workflowRun, err
			}
			record = core.NewRecord(collection)
		}
	}

	err = app.GetApp().RunInTransaction(func(txApp core.App) error {
		record.Set("workflowRef", workflowRun.WorkflowId)
		record.Set("trigger", workflowRun.Trigger.String())
		record.Set("status", workflowRun.Status.String())
		record.Set("startedAt", workflowRun.StartedAt)
		record.Set("endedAt", workflowRun.EndedAt)
		record.Set("graph", workflowRun.Graph)
		record.Set("error", workflowRun.Error)
		err = txApp.Save(record)
		if err != nil {
			return err
		}

		workflowRun.Id = record.Id
		workflowRun.CreatedAt = record.GetDateTime("created").Time()
		workflowRun.UpdatedAt = record.GetDateTime("updated").Time()

		// 事务级联更新所属工作流的最后运行记录
		workflowRecord, err := txApp.FindRecordById(domain.CollectionNameWorkflow, workflowRun.WorkflowId)
		if err != nil {
			return err
		} else if workflowRun.Id == workflowRecord.GetString("lastRunRef") {
			workflowRecord.IgnoreUnchangedFields(true)
			workflowRecord.Set("lastRunStatus", record.GetString("status"))
			err = txApp.Save(workflowRecord)
			if err != nil {
				return err
			}
		} else if workflowRecord.GetDateTime("lastRunTime").Time().IsZero() || workflowRun.StartedAt.After(workflowRecord.GetDateTime("lastRunTime").Time()) {
			workflowRecord.IgnoreUnchangedFields(true)
			workflowRecord.Set("lastRunRef", record.Id)
			workflowRecord.Set("lastRunStatus", record.GetString("status"))
			workflowRecord.Set("lastRunTime", record.GetString("startedAt"))
			err = txApp.Save(workflowRecord)
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return workflowRun, err
	}

	return workflowRun, nil
}

func (r *WorkflowRunRepository) DeleteWithExprs(ctx context.Context, exprs ...dbx.Expression) (int, error) {
	records, err := app.GetApp().FindAllRecords(domain.CollectionNameWorkflowRun, exprs...)
	if err != nil {
		return 0, nil
	}

	var ret int
	var errs []error
	for _, record := range records {
		if err := app.GetApp().Delete(record); err != nil {
			errs = append(errs, err)
		} else {
			ret++
		}
	}

	if len(errs) > 0 {
		return ret, errors.Join(errs...)
	}

	return ret, nil
}

// ListByWorkflowId returns all runs of the given workflow, newest first.
func (r *WorkflowRunRepository) ListByWorkflowId(ctx context.Context, workflowId string) ([]*domain.WorkflowRun, error) {
	records, err := app.GetApp().FindRecordsByFilter(
		domain.CollectionNameWorkflowRun,
		"workflowRef={:workflowId}",
		"-created",
		0, 0,
		dbx.Params{"workflowId": workflowId},
	)
	if err != nil {
		return nil, err
	}

	workflowRuns := make([]*domain.WorkflowRun, 0, len(records))
	for _, record := range records {
		workflowRun, err := r.castRecordToModel(record)
		if err != nil {
			return nil, err
		}

		workflowRuns = append(workflowRuns, workflowRun)
	}

	return workflowRuns, nil
}

// CancelActiveByWorkflowId marks every still pending/processing run of the
// workflow as canceled. It returns the affected run models so callers can
// produce diagnostic messages and wait on the exact generations that were
// active. This is part of the workflow ownership shutdown: it is the
// database-side fence that stops queued generations from being dispatched
// after an in-memory queue entry slipped past the close gate.
func (r *WorkflowRunRepository) CancelActiveByWorkflowId(ctx context.Context, workflowId string) ([]*domain.WorkflowRun, error) {
	records, err := app.GetApp().FindRecordsByFilter(
		domain.CollectionNameWorkflowRun,
		"workflowRef={:workflowId} && (status={:pending} || status={:processing})",
		"-created",
		0, 0,
		dbx.Params{
			"workflowId": workflowId,
			"pending":    domain.WorkflowRunStatusTypePending.String(),
			"processing": domain.WorkflowRunStatusTypeProcessing.String(),
		},
	)
	if err != nil {
		return nil, err
	}

	workflowRuns := make([]*domain.WorkflowRun, 0, len(records))
	for _, record := range records {
		record.IgnoreUnchangedFields(true)
		record.Set("status", domain.WorkflowRunStatusTypeCanceled.String())
		record.Set("endedAt", time.Now())
		if err := app.GetApp().Save(record); err != nil {
			return nil, err
		}

		workflowRun, err := r.castRecordToModel(record)
		if err != nil {
			return nil, err
		}
		workflowRuns = append(workflowRuns, workflowRun)
	}

	return workflowRuns, nil
}

func (r *WorkflowRunRepository) ResetStatusIfHanging(ctx context.Context) error {
	return app.GetApp().RunInTransaction(func(txApp core.App) error {
		var err error

		// 重置工作流上最后一次运行的悬挂状态
		_, err = txApp.DB().
			NewQuery(fmt.Sprintf("UPDATE %s SET lastRunStatus = '%s' WHERE lastRunStatus = '%s' OR lastRunStatus = '%s'",
				domain.CollectionNameWorkflow,
				domain.WorkflowRunStatusTypeCanceled.String(),
				domain.WorkflowRunStatusTypePending.String(),
				domain.WorkflowRunStatusTypeProcessing.String(),
			)).
			Execute()
		if err != nil {
			return err
		}

		// 重置运行记录的悬挂状态，避免进程异常退出后旧代际永远停留在排队/执行中
		_, err = txApp.DB().
			NewQuery(fmt.Sprintf("UPDATE %s SET status = '%s', endedAt = CURRENT_TIMESTAMP WHERE status = '%s' OR status = '%s'",
				domain.CollectionNameWorkflowRun,
				domain.WorkflowRunStatusTypeCanceled.String(),
				domain.WorkflowRunStatusTypePending.String(),
				domain.WorkflowRunStatusTypeProcessing.String(),
			)).
			Execute()
		if err != nil {
			return err
		}

		return nil
	})
}

func (r *WorkflowRunRepository) castRecordToModel(record *core.Record) (*domain.WorkflowRun, error) {
	if record == nil {
		return nil, fmt.Errorf("the record is nil")
	}

	graph := &domain.WorkflowGraph{}
	if err := record.UnmarshalJSONField("graph", &graph); err != nil {
		return nil, fmt.Errorf("field 'graph' is malformed")
	}

	workflowRun := &domain.WorkflowRun{
		Meta: domain.Meta{
			Id:        record.Id,
			CreatedAt: record.GetDateTime("created").Time(),
			UpdatedAt: record.GetDateTime("updated").Time(),
		},
		WorkflowId: record.GetString("workflowRef"),
		Status:     domain.WorkflowRunStatusType(record.GetString("status")),
		Trigger:    domain.WorkflowTriggerType(record.GetString("trigger")),
		StartedAt:  record.GetDateTime("startedAt").Time(),
		EndedAt:    record.GetDateTime("endedAt").Time(),
		Graph:      graph,
		Error:      record.GetString("error"),
	}
	return workflowRun, nil
}
