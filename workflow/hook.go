package workflow

import (
	"context"
	"time"

	"github.com/f1bonacc1/ebind/task"
)

// StepHook implements worker.StepHook by persisting the step outcome to the
// state store and publishing a completion event to the bus. The scheduler
// consumes these events to advance the DAG.
type StepHook struct {
	store StateStore
	bus   EventBus
}

// OnStepDone marks the step as done, writes the result, and publishes a
// `completed` event with Status=done.
func (h *StepHook) OnStepDone(ctx context.Context, t *task.Task, result []byte) error {
	if t.DAGID == "" || t.StepID == "" {
		return nil // ad-hoc task, not part of a DAG
	}
	// CAS-update the step record to Done.
	if err := h.casUpdateStatus(ctx, t.DAGID, t.StepID, StatusDone, ""); err != nil {
		return err
	}
	if err := h.store.PutResult(ctx, t.DAGID, t.StepID, result); err != nil {
		return err
	}
	ev := Event{Kind: EventCompleted, DAGID: t.DAGID, StepID: t.StepID, Status: StatusDone}
	data, _ := MarshalEvent(ev)
	return h.bus.Publish(ctx, EventSubject(ev), data)
}

// OnStepFailed marks the step as failed and publishes a `completed` event with
// Status=failed so the scheduler can cascade skips and re-evaluate readiness.
func (h *StepHook) OnStepFailed(ctx context.Context, t *task.Task, taskErr *task.TaskError) error {
	if t.DAGID == "" || t.StepID == "" {
		return nil
	}
	if err := h.casUpdateStatus(ctx, t.DAGID, t.StepID, StatusFailed, taskErr.Kind); err != nil {
		return err
	}
	ev := Event{Kind: EventCompleted, DAGID: t.DAGID, StepID: t.StepID, Status: StatusFailed, ErrorKind: taskErr.Kind}
	data, _ := MarshalEvent(ev)
	return h.bus.Publish(ctx, EventSubject(ev), data)
}

// casUpdateStatus retries on stale revision (another writer won the race).
func (h *StepHook) casUpdateStatus(ctx context.Context, dagID, stepID string, status StepStatus, errKind string) error {
	for attempt := 0; attempt < 5; attempt++ {
		rec, rev, err := h.store.GetStep(ctx, dagID, stepID)
		if err != nil {
			return err
		}
		if rec.IsTerminal() {
			return nil // already finalized
		}
		rec.Status = status
		rec.ErrorKind = errKind
		rec.FinishedAt = time.Now().UTC()
		err = h.store.PutStep(ctx, dagID, stepID, rec, rev)
		if err == nil {
			return nil
		}
		if err != ErrStaleRevision {
			return err
		}
		// loop and retry
	}
	return ErrStaleRevision
}
