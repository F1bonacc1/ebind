package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/f1bonacc1/ebind/task"
)

// DAGOption configures a DAG at construction time.
type DAGOption func(*DAG)

// WithRetry sets the default retry policy for all steps in this DAG. A step can
// override via WithStepRetry.
func WithRetry(p task.RetryPolicy) DAGOption {
	return func(d *DAG) { pc := p; d.defaultPolicy = &pc }
}

// WithDAGID pins an explicit DAG ID (otherwise a uuid is generated).
func WithDAGID(id string) DAGOption { return func(d *DAG) { d.id = id } }

// DAG is the user-facing workflow builder. Not safe for concurrent Step() calls.
// Submit() is one-shot.
type DAG struct {
	id            string
	steps         []*Step
	stepMap       map[string]*Step
	defaultPolicy *task.RetryPolicy
	mu            sync.Mutex // guards dynamic AddStep post-Submit
	submitted     bool
}

// New constructs an empty DAG. Build it with Step() then Submit().
func New(opts ...DAGOption) *DAG {
	d := &DAG{
		id:      uuid.NewString(),
		stepMap: map[string]*Step{},
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// ID returns the DAG identifier (generated uuid unless WithDAGID was passed).
func (d *DAG) ID() string { return d.id }

// Step adds a node to the DAG. id must be unique within the DAG. fn must satisfy
// the same signature contract as task.Register (ctx first, (T, error) or just error
// return). args match fn's signature; upstream step outputs are referenced via
// Step.Ref() or Step.RefOrDefault(v).
func (d *DAG) Step(id string, fn any, args ...any) *Step {
	return d.addStep(id, fn, args, nil)
}

// StepOpts is the options-flavored variant — same as Step but accepts StepOptions.
func (d *DAG) StepOpts(id string, fn any, opts []StepOption, args ...any) *Step {
	return d.addStep(id, fn, args, opts)
}

func (d *DAG) addStep(id string, fn any, args []any, opts []StepOption) *Step {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, dup := d.stepMap[id]; dup {
		panic(fmt.Sprintf("workflow: duplicate step id %q", id))
	}
	s := &Step{id: id, fn: fn, args: args}
	for _, opt := range opts {
		opt(s)
	}
	if s.policy == nil && d.defaultPolicy != nil {
		pc := *d.defaultPolicy
		s.policy = &pc
	}
	d.steps = append(d.steps, s)
	d.stepMap[id] = s
	return s
}

// stepArgDeps extracts upstream step IDs referenced in a step's args via Refs.
func stepArgDeps(args []any) []string {
	seen := map[string]bool{}
	var deps []string
	for _, a := range args {
		if r, ok := a.(Ref); ok {
			if !seen[r.StepID] {
				seen[r.StepID] = true
				deps = append(deps, r.StepID)
			}
		}
	}
	return deps
}

// requiredDeps returns the step's full required dependency list: Ref-derived
// deps from args + explicit After() deps. Deduped.
func (s *Step) requiredDeps() []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range stepArgDeps(s.args) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, id := range s.afterDeps {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// optionalDeps returns the step's explicit AfterAny() deps, minus any that are
// already required (required takes precedence).
func (s *Step) optionalDeps() []string {
	required := map[string]bool{}
	for _, id := range s.requiredDeps() {
		required[id] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, id := range s.afterAnyDeps {
		if required[id] || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// allDeps returns every dep (required + optional), used for cycle detection.
func (s *Step) allDeps() []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range s.requiredDeps() {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, id := range s.optionalDeps() {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// marshalArgs serializes args to a JSON array, preserving Ref envelopes for
// unresolved references.
func marshalArgs(args []any) (json.RawMessage, error) {
	if args == nil {
		args = []any{}
	}
	raw := make([]json.RawMessage, len(args))
	for i, a := range args {
		var b []byte
		var err error
		switch v := a.(type) {
		case Ref:
			b, err = json.Marshal(v)
		default:
			b, err = marshalAny(v)
		}
		if err != nil {
			return nil, fmt.Errorf("marshal arg %d: %w", i, err)
		}
		raw[i] = b
	}
	return json.Marshal(raw)
}

// marshalAny wraps json.Marshal; exists to keep a single path for any custom
// handling (e.g. future type-aware encoding).
func marshalAny(v any) ([]byte, error) { return json.Marshal(v) }

// detectCycles runs a DFS-based cycle detector over the step graph.
// Walks the full dep closure (Ref-derived + After + AfterAny).
func (d *DAG) detectCycles() error {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var visit func(id string) error
	visit = func(id string) error {
		color[id] = gray
		for _, dep := range d.stepMap[id].allDeps() {
			switch color[dep] {
			case gray:
				return fmt.Errorf("%w: %s ↔ %s", ErrCycle, id, dep)
			case white:
				if _, ok := d.stepMap[dep]; !ok {
					return fmt.Errorf("step %q references unknown step %q", id, dep)
				}
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		color[id] = black
		return nil
	}
	for _, s := range d.steps {
		if color[s.id] == white {
			if err := visit(s.id); err != nil {
				return err
			}
		}
	}
	return nil
}

// Submit validates the DAG, persists meta + step records to the store, and
// enqueues root steps (those with no deps). Idempotent: re-submitting the same
// DAG ID returns ErrStaleRevision for the meta write.
func (d *DAG) Submit(ctx context.Context, wf *Workflow) error {
	d.mu.Lock()
	if d.submitted {
		d.mu.Unlock()
		return fmt.Errorf("workflow: DAG %s already submitted", d.id)
	}
	if len(d.steps) == 0 {
		d.mu.Unlock()
		return fmt.Errorf("workflow: DAG has no steps")
	}
	if err := d.detectCycles(); err != nil {
		d.mu.Unlock()
		return err
	}
	d.submitted = true
	d.mu.Unlock()

	// Validate signatures + resolve fn names.
	stepNames := make(map[string]string, len(d.steps))
	for _, s := range d.steps {
		desc, err := task.Describe(s.fn)
		if err != nil {
			return fmt.Errorf("workflow: step %q: %w", s.id, err)
		}
		stepNames[s.id] = desc.Name
	}

	// Persist meta.
	meta := DAGMeta{
		ID:            d.id,
		Status:        DAGStatusRunning,
		CreatedAt:     time.Now().UTC(),
		DefaultPolicy: d.defaultPolicy,
	}
	if err := wf.Store.PutMeta(ctx, d.id, meta, 0); err != nil {
		return fmt.Errorf("workflow: put meta: %w", err)
	}

	// Persist step records.
	recs := make(map[string]StepRecord, len(d.steps))
	for _, s := range d.steps {
		argsJSON, err := marshalArgs(s.args)
		if err != nil {
			return fmt.Errorf("workflow: step %q: %w", s.id, err)
		}
		rec := StepRecord{
			DAGID:        d.id,
			StepID:       s.id,
			FnName:       stepNames[s.id],
			ArgsJSON:     argsJSON,
			Deps:         s.requiredDeps(),
			OptionalDeps: s.optionalDeps(),
			Status:       StatusPending,
			Optional:     s.optional,
			Policy:       s.policy,
			AddedAt:      time.Now().UTC(),
		}
		if err := wf.Store.PutStep(ctx, d.id, s.id, rec, 0); err != nil {
			return fmt.Errorf("workflow: put step %q: %w", s.id, err)
		}
		recs[s.id] = rec
	}

	// Enqueue roots — steps with no required AND no optional deps. Dependent
	// steps are enqueued by the scheduler on completion events.
	//
	// Before enqueueing, transition each root to Running + StartedAt so that
	// every step in the system has a consistent lifecycle. Dependent steps
	// receive the same transition via Scheduler.enqueueReady/persistStatus.
	for _, s := range d.steps {
		if len(s.allDeps()) != 0 {
			continue
		}
		// Get-then-Put so the rev we CAS against is always current, even if the
		// initial persist step used different revision semantics.
		cur, rev, err := wf.Store.GetStep(ctx, d.id, s.id)
		if err != nil {
			return fmt.Errorf("workflow: get root %q: %w", s.id, err)
		}
		cur.Status = StatusRunning
		cur.StartedAt = time.Now().UTC()
		if err := wf.Store.PutStep(ctx, d.id, s.id, cur, rev); err != nil {
			return fmt.Errorf("workflow: mark root running %q: %w", s.id, err)
		}
		recs[s.id] = cur
		if err := enqueueStep(ctx, wf.Enq, cur, nil, nil); err != nil {
			return fmt.Errorf("workflow: enqueue root %q: %w", s.id, err)
		}
	}
	return nil
}

// enqueueStep resolves Ref args against provided results + statuses (nil for
// root-only calls), then publishes the task envelope via the Enqueuer.
// Returns any ResolveArgs error; a cascade-skip signal is a nil-error return
// with the caller expected to mark the step skipped before calling.
func enqueueStep(
	ctx context.Context,
	enq Enqueuer,
	rec StepRecord,
	results map[string]json.RawMessage,
	statuses map[string]StepStatus,
) error {
	var rawArgs []json.RawMessage
	if err := json.Unmarshal(rec.ArgsJSON, &rawArgs); err != nil {
		return fmt.Errorf("unmarshal args: %w", err)
	}
	resolved, skip, err := ResolveArgs(rawArgs, results, statuses)
	if err != nil {
		return err
	}
	if skip {
		return nil // caller handles cascade-skip via MarkSkipped
	}
	payload, err := json.Marshal(resolved)
	if err != nil {
		return err
	}
	envelope := task.Task{
		ID:          rec.DAGID + ":" + rec.StepID,
		Name:        rec.FnName,
		Payload:     payload,
		EnqueuedAt:  time.Now().UTC(),
		DAGID:       rec.DAGID,
		StepID:      rec.StepID,
		RetryPolicy: rec.Policy,
	}
	return enq.Enqueue(ctx, envelope)
}
