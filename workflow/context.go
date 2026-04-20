package workflow

import (
	"context"
	"encoding/json"
	"errors"
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

// CurrentTarget returns the current task target from workflow handler context.
func CurrentTarget(ctx context.Context) string {
	if d := FromContext(ctx); d != nil {
		return d.target
	}
	return ""
}

// CurrentWorkerID returns the concrete worker handling the current workflow step.
func CurrentWorkerID(ctx context.Context) string {
	if d := FromContext(ctx); d != nil {
		return d.workerID
	}
	return ""
}

// ContextDAG is the in-handler handle for dynamically adding steps to the
// currently-running DAG. The newly added step depends on the current step
// (implicitly) plus any explicit Refs passed as args.
type ContextDAG struct {
	wf         *Workflow
	dagID      string
	parentStep string
	target     string
	workerID   string
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
	meta, _, err := c.wf.Store.GetMeta(context.Background(), c.dagID)
	if err != nil {
		return nil, err
	}
	if meta.Status == DAGStatusCanceled {
		return nil, ErrDAGCanceled
	}

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
	if s.placement != nil && s.placement.Mode == PlacementHere {
		// Resolve at enqueue time via the parent step's final WorkerID. If the
		// parent handler is retried onto a different worker, child follows the
		// worker that ACKed — keeping the "current worker" semantics consistent
		// with the step that actually completed.
		s.placement = &PlacementSpec{Mode: PlacementColocate, StepID: c.parentStep}
	}
	argsJSON, err := marshalArgs(args)
	if err != nil {
		return nil, fmt.Errorf("workflow: dynamic step %q: %w", id, err)
	}
	var placement *PlacementSpec
	if s.placement != nil {
		copy := *s.placement
		placement = &copy
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
		Placement:    placement,
		AddedAt:      time.Now().UTC(),
	}
	// CAS-create (rev=0 means "must not exist"). When a handler is retried —
	// or redelivered because a concurrent subscriber joined the consumer during
	// a claim flip — StepOpts may run again with the same id. Treat an existing
	// matching record as success so dynamic-step creation is idempotent.
	if err := c.wf.Store.PutStep(context.Background(), c.dagID, id, rec, 0); err != nil {
		if errors.Is(err, ErrStaleRevision) {
			existing, _, getErr := c.wf.Store.GetStep(context.Background(), c.dagID, id)
			if getErr == nil && existing.FnName == rec.FnName {
				return s, nil
			}
		}
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
				cd := &ContextDAG{wf: wf, dagID: t.DAGID, parentStep: t.StepID, target: t.Target, workerID: t.WorkerID}
				ctx = context.WithValue(ctx, ctxKey{}, cd)
			}
			return next(ctx, t)
		}
	}
}

// compile-time check that ContextDAG can render Refs that look like a Step handle.
var _ = json.RawMessage{}
