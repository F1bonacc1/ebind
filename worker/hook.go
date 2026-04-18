package worker

import (
	"context"

	"github.com/f1bonacc1/ebind/task"
)

// StepHook lets callers observe final task outcomes. Used by the workflow package
// to persist DAG step results/failures without coupling the worker to workflow.
// Called exactly once per terminal outcome (success or final failure, not per retry).
// Never called during intermediate retries.
type StepHook interface {
	OnStepDone(ctx context.Context, t *task.Task, result []byte) error
	OnStepFailed(ctx context.Context, t *task.Task, err *task.TaskError) error
}
