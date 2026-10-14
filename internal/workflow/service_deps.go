package workflow

import (
	"context"

	"github.com/pocketbase/dbx"

	"github.com/certimate-go/certimate/internal/domain"
)

type workflowRepository interface {
	ListEnabledScheduled(ctx context.Context) ([]*domain.Workflow, error)
	GetById(ctx context.Context, id string) (*domain.Workflow, error)
	Save(ctx context.Context, workflow *domain.Workflow) (*domain.Workflow, error)
	MarkClosing(ctx context.Context, id string) error
}

type workflowRunRepository interface {
	GetById(ctx context.Context, id string) (*domain.WorkflowRun, error)
	Save(ctx context.Context, workflowRun *domain.WorkflowRun) (*domain.WorkflowRun, error)
	SaveWithCascading(ctx context.Context, workflowRun *domain.WorkflowRun) (*domain.WorkflowRun, error)
	ListByWorkflowId(ctx context.Context, workflowId string) ([]*domain.WorkflowRun, error)
	CancelActiveByWorkflowId(ctx context.Context, workflowId string) ([]*domain.WorkflowRun, error)
	DeleteWithExprs(ctx context.Context, exprs ...dbx.Expression) (int, error)
}

type certificateRepository interface {
	ListByWorkflowId(ctx context.Context, workflowId string) ([]*domain.Certificate, error)
	Save(ctx context.Context, certificate *domain.Certificate) (*domain.Certificate, error)
}
