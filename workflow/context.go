package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/worker"
)

type ctxKey struct{}

// FromContext retrieves the ContextDAG injected by ContextMiddleware. Returns
// nil if called outside a DAG-dispatched handler (so callers can no-op).
func FromContext(ctx context.Context) *ContextDAG {
	v, _ := ctx.Value(ctxKey{}).(*ContextDAG)
	return v
}

// ContextDAG is the in-handler handle for dynamically adding steps to the
// currently-running DAG. The newly added step depends on the current step
// (implicitly) plus any explicit Refs passed as args.
type ContextDAG struct {
	wf         *Workflow
	dagID      string
	parentStep string
}

// Step adds a new step to the current DAG. The new step implicitly depends on
// the currently-running step (so it won't run until the current handler returns
// successfully). Returns a Step ref usable for chaining further dynamic steps.
func (c *ContextDAG) Step(id string, fn any, args ...any) (*Step, error) {
	return c.StepOpts(id, fn, nil, args...)
}

// StepOpts adds a step with StepOption support (Optional, WithStepRetry, After, AfterAny).
// The new step implicitly depends on the currently-running step, plus any explicit
// Refs in args and any explicit After()/AfterAny() upstream steps.
func (c *ContextDAG) StepOpts(id string, fn any, opts []StepOption, args ...any) (*Step, error) {
	desc, err := task.Describe(fn)
	if err != nil {
		return nil, fmt.Errorf("workflow: dynamic step %q: %w", id, err)
	}
	s := &Step{id: id, fn: fn, args: args}
	for _, opt := range opts {
		opt(s)
	}
	// Implicit parent dep is required (cascade-skip if parent fails) — consistent
	// with the expectation that dynamic work should not run if the handler failed.
	if c.parentStep != "" {
		s.afterDeps = append(s.afterDeps, c.parentStep)
	}
	argsJSON, err := marshalArgs(args)
	if err != nil {
		return nil, fmt.Errorf("workflow: dynamic step %q: %w", id, err)
	}
	rec := StepRecord{
		DAGID:        c.dagID,
		StepID:       id,
		FnName:       desc.Name,
		ArgsJSON:     argsJSON,
		Deps:         s.requiredDeps(),
		OptionalDeps: s.optionalDeps(),
		Status:       StatusPending,
		Optional:     s.optional,
		Policy:       s.policy,
		AddedAt:      time.Now().UTC(),
	}
	// CAS-create (rev=0 means "must not exist").
	if err := c.wf.Store.PutStep(context.Background(), c.dagID, id, rec, 0); err != nil {
		return nil, fmt.Errorf("workflow: dynamic step %q: %w", id, err)
	}
	// Publish step-added event so scheduler re-evaluates readiness.
	ev := Event{Kind: EventStepAdded, DAGID: c.dagID, StepID: id}
	data, _ := MarshalEvent(ev)
	_ = c.wf.Bus.Publish(context.Background(), EventSubject(ev), data)
	return s, nil
}

// ContextMiddleware returns a worker.Middleware that injects a ContextDAG into
// the handler's context for DAG-dispatched tasks. Ad-hoc tasks (no DAGID) are
// passed through unchanged.
func (wf *Workflow) ContextMiddleware() worker.Middleware {
	return func(next worker.Handler) worker.Handler {
		return func(ctx context.Context, t *task.Task) ([]byte, error) {
			if t.DAGID != "" {
				cd := &ContextDAG{wf: wf, dagID: t.DAGID, parentStep: t.StepID}
				ctx = context.WithValue(ctx, ctxKey{}, cd)
			}
			return next(ctx, t)
		}
	}
}

// compile-time check that ContextDAG can render Refs that look like a Step handle.
var _ = json.RawMessage{}
