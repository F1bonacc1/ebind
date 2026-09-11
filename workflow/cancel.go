package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Cancel marks a DAG as canceled and prevents any new steps from being enqueued.
// Pending steps are transitioned to canceled immediately. Running steps are left
// untouched and may still finish, but their completion does not schedule follow-on work.
//
// Cancel is safe to re-run. On an already-canceled DAG it skips the meta write
// but still sweeps pending steps: ListSteps is a relaxed read that can omit a
// record a lagging replica has not applied yet, and the scheduler ignores
// canceled DAGs, so calling Cancel again is the only repair for a step an
// earlier call missed. A sweep that cannot finish returns an error, never nil.
func Cancel(ctx context.Context, wf *Workflow, dagID string) error {
	finished, err := cancelMeta(ctx, wf, dagID)
	if err != nil || finished {
		return err
	}
	steps, err := wf.Store.ListSteps(ctx, dagID)
	if err != nil {
		return err
	}
	for _, step := range steps {
		if err := cancelStep(ctx, wf, dagID, step.StepID); err != nil {
			return err
		}
	}
	return nil
}

// cancelMeta CAS-writes the DAG meta to canceled. It reports true for a DAG
// that already ended done or failed, which has nothing left to cancel. An
// already-canceled DAG reports false: the caller still runs the step sweep.
func cancelMeta(ctx context.Context, wf *Workflow, dagID string) (bool, error) {
	for attempt := 0; attempt < 5; attempt++ {
		meta, rev, err := wf.Store.GetMeta(ctx, dagID)
		if err != nil {
			return false, err
		}
		switch meta.Status {
		case DAGStatusDone, DAGStatusFailed:
			return true, nil
		case DAGStatusCanceled:
			return false, nil
		}
		// Running, pausing and paused DAGs all transition to canceled.
		meta.Status = DAGStatusCanceled
		if err := wf.Store.PutMeta(ctx, dagID, meta, rev); err != nil {
			if errors.Is(err, ErrStaleRevision) {
				continue
			}
			return false, err
		}
		return false, nil
	}
	return false, fmt.Errorf("workflow: cancel %s: %w", dagID, ErrStaleRevision)
}

// cancelStep CAS-writes one pending step to canceled. Terminal and running
// steps are left as they are.
func cancelStep(ctx context.Context, wf *Workflow, dagID, stepID string) error {
	for attempt := 0; attempt < 5; attempt++ {
		rec, rev, err := wf.Store.GetStep(ctx, dagID, stepID)
		if err != nil {
			return err
		}
		if rec.IsTerminal() || rec.Status == StatusRunning {
			return nil
		}
		rec.Status = StatusCanceled
		rec.FinishedAt = time.Now().UTC()
		if _, err := wf.Store.PutStep(ctx, dagID, stepID, rec, rev); err != nil {
			if errors.Is(err, ErrStaleRevision) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("workflow: cancel step %s: %w", stepID, ErrStaleRevision)
}
