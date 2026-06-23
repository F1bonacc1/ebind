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

// casStaleRetryBackoff is the base delay between stale-revision CAS retries.
// It grows linearly per attempt. A non-zero delay is essential: GetStep is a
// JetStream KV direct-get that can be served by a replica lagging the quorum
// leader (the window is widest right after a node restart / fresh cluster
// formation). With no delay, all retries re-read the SAME stale revision and
// the CAS can never succeed, so a legitimate completion is dropped. Backing off
// lets the lagging replica catch up so a subsequent GetStep returns the
// committed revision and the CAS commits.
var casStaleRetryBackoff = 50 * time.Millisecond

// casUpdateStatus retries on stale revision (another writer won the race, or a
// direct-get read lagged the quorum). It backs off between attempts so a lagging
// replica can converge rather than spinning on the same stale snapshot.
func (h *StepHook) casUpdateStatus(ctx context.Context, t *task.Task, status StepStatus, errKind, errMsg string) error {
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
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
		_, err = h.store.PutStep(ctx, t.DAGID, t.StepID, rec, rev)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrStaleRevision) {
			return err
		}
		// Stale revision — let the lagging replica converge before re-reading.
		if attempt < maxAttempts-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(casStaleRetryBackoff * time.Duration(attempt+1)):
			}
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
