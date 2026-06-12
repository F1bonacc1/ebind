package workflow

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/f1bonacc1/ebind/task"
)

// StepHook implements worker.StepHook by persisting the step outcome to the
// state store and publishing a completion event to the bus. The scheduler
// consumes these events to advance the DAG.
type StepHook struct {
	store StateStore
	bus   EventBus
	// maxErrBytes caps the failed-step error message persisted into the step
	// record. 0 ⇒ DefaultMaxStepErrorBytes; negative ⇒ store no message
	// (error kind only). Set via Workflow.MaxStepErrorBytes.
	maxErrBytes int
}

// OnStepDone marks the step as done, writes the result, and publishes a
// `completed` event with Status=done.
func (h *StepHook) OnStepDone(ctx context.Context, t *task.Task, result []byte) error {
	if t.DAGID == "" || t.StepID == "" {
		return nil
	}
	if err := h.casUpdateStatus(ctx, t, StatusDone, "", ""); err != nil {
		return err
	}
	if err := h.store.PutResult(ctx, t.DAGID, t.StepID, result); err != nil {
		return err
	}
	return publishEvent(ctx, h.bus,
		Event{Kind: EventCompleted, DAGID: t.DAGID, StepID: t.StepID, Status: StatusDone})
}

// OnStepFailed marks the step as failed and publishes a `completed` event with
// Status=failed so the scheduler can cascade skips and re-evaluate readiness.
func (h *StepHook) OnStepFailed(ctx context.Context, t *task.Task, taskErr *task.TaskError) error {
	if t.DAGID == "" || t.StepID == "" {
		return nil
	}
	msg := truncateErrorMessage(taskErr.Message, h.maxErrBytes)
	if err := h.casUpdateStatus(ctx, t, StatusFailed, taskErr.Kind, msg); err != nil {
		return err
	}
	return publishEvent(ctx, h.bus, Event{Kind: EventCompleted, DAGID: t.DAGID,
		StepID: t.StepID, Status: StatusFailed, ErrorKind: taskErr.Kind, ErrorMessage: msg})
}

// casUpdateStatus retries on stale revision (another writer won the race).
func (h *StepHook) casUpdateStatus(ctx context.Context, t *task.Task, status StepStatus, errKind, errMsg string) error {
	for attempt := 0; attempt < 5; attempt++ {
		rec, rev, err := h.store.GetStep(ctx, t.DAGID, t.StepID)
		if err != nil {
			return err
		}
		if rec.IsTerminal() {
			return nil
		}
		rec.Status = status
		rec.ErrorKind = errKind
		rec.ErrorMessage = errMsg
		rec.WorkerID = t.WorkerID
		rec.FinishedAt = time.Now().UTC()
		err = h.store.PutStep(ctx, t.DAGID, t.StepID, rec, rev)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrStaleRevision) {
			return err
		}
	}
	return ErrStaleRevision
}

// DefaultMaxStepErrorBytes bounds how many bytes of a failed step's error
// message are persisted into the step record and carried on completion events.
// Handler errors can be large (wrapped chains, panic text), and the step record
// is CAS-updated and re-read by the scheduler on every readiness evaluation, so
// the message is capped. Override via Workflow.MaxStepErrorBytes.
const DefaultMaxStepErrorBytes = 4096

// truncateErrorMessage applies the configured byte cap to a failed step's error
// message. max == 0 selects DefaultMaxStepErrorBytes; max < 0 disables message
// persistence entirely (the step record keeps only the error kind). Truncation
// happens on a UTF-8 rune boundary so the stored value stays valid UTF-8.
func truncateErrorMessage(s string, max int) string {
	if max < 0 {
		return ""
	}
	if max == 0 {
		max = DefaultMaxStepErrorBytes
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
