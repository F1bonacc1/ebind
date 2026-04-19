package workflow

import (
	"context"
	"time"
)

// Cancel marks a DAG as canceled and prevents any new steps from being enqueued.
// Pending steps are transitioned to canceled immediately. Running steps are left
// untouched and may still finish, but their completion does not schedule follow-on work.
func Cancel(ctx context.Context, wf *Workflow, dagID string) error {
	for attempt := 0; attempt < 5; attempt++ {
		meta, rev, err := wf.Store.GetMeta(ctx, dagID)
		if err != nil {
			return err
		}
		switch meta.Status {
		case DAGStatusDone, DAGStatusFailed, DAGStatusCanceled:
			return nil
		}
		meta.Status = DAGStatusCanceled
		if err := wf.Store.PutMeta(ctx, dagID, meta, rev); err != nil {
			if err == ErrStaleRevision {
				continue
			}
			return err
		}
		break
	}

	steps, err := wf.Store.ListSteps(ctx, dagID)
	if err != nil {
		return err
	}
	for _, step := range steps {
		for attempt := 0; attempt < 5; attempt++ {
			rec, rev, err := wf.Store.GetStep(ctx, dagID, step.StepID)
			if err != nil {
				return err
			}
			if rec.IsTerminal() || rec.Status == StatusRunning {
				break
			}
			rec.Status = StatusCanceled
			rec.FinishedAt = time.Now().UTC()
			if err := wf.Store.PutStep(ctx, dagID, rec.StepID, rec, rev); err != nil {
				if err == ErrStaleRevision {
					continue
				}
				return err
			}
			break
		}
	}
	return nil
}
