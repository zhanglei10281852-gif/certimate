package dispatcher

import (
	"context"
)

type taskInfo struct {
	WorkflowId string
	RunId      string

	ctx    context.Context
	cancel context.CancelFunc

	// done is closed after the execution goroutine (including all of its
	// completion callbacks) has fully exited. ShutdownWorkflow waits on it to
	// ensure ownership convergence before a workflow deletion is committed.
	done chan struct{}
}
