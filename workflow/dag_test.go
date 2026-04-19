package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func noopA(_ context.Context, x int) (int, error) { return x + 1, nil }
func noopB(_ context.Context) (string, error)     { return "b", nil }
func noopC(_ context.Context, a int, b string) (string, error) {
	return b, nil
}

func TestDAG_Empty_Submit_Errors(t *testing.T) {
	dag := New()
	wf := NewWorkflow(NewMemStore(), NewMemBus(), &captureEnq{})
	err := dag.Submit(context.Background(), wf)
	if err == nil || !strings.Contains(err.Error(), "no steps") {
		t.Errorf("want 'no steps' error, got %v", err)
	}
}

func TestDAG_Duplicate_Step_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("want panic on duplicate step")
		}
	}()
	dag := New()
	dag.Step("a", noopA, 1)
	dag.Step("a", noopA, 1) // duplicate
}

func TestDAG_Cycle_Detected(t *testing.T) {
	dag := New()
	a := dag.Step("a", noopA, 1)
	// Force a cycle by making b reference itself via a's Ref while swapping args.
	// Simpler: use two steps each referencing each other after-the-fact.
	_ = a
	b := dag.Step("b", noopB)
	// Re-add step by manipulating args post-creation to create cycle:
	// Clean option: rebuild DAG with explicit self-reference — requires mutating step args.
	// Use a.args = [b.Ref()] and b.args = [a.Ref()] — simulate by directly poking.
	a.args = []any{b.Ref()}
	b.args = []any{a.Ref()}

	wf := NewWorkflow(NewMemStore(), NewMemBus(), &captureEnq{})
	err := dag.Submit(context.Background(), wf)
	if err == nil || !errors.Is(err, ErrCycle) {
		t.Errorf("want ErrCycle, got %v", err)
	}
}

func TestDAG_UnknownRef_Errors(t *testing.T) {
	dag := New()
	// Step that references "ghost" which doesn't exist.
	phantom := &Step{id: "ghost"}
	dag.Step("a", noopA, phantom.Ref()) // noopA wants int, but we're testing cycle/unknown-ref detection not types

	wf := NewWorkflow(NewMemStore(), NewMemBus(), &captureEnq{})
	err := dag.Submit(context.Background(), wf)
	if err == nil || !strings.Contains(err.Error(), "unknown step") {
		t.Errorf("want unknown-step error, got %v", err)
	}
}

func TestDAG_Submit_PersistsMetaAndSteps(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	dag := New()
	dag.Step("a", noopA, 42)
	dag.Step("b", noopB)

	if err := dag.Submit(context.Background(), wf); err != nil {
		t.Fatal(err)
	}
	meta, _, err := store.GetMeta(context.Background(), dag.ID())
	if err != nil {
		t.Fatal(err)
	}
	if meta.Status != DAGStatusRunning {
		t.Errorf("status=%s", meta.Status)
	}
	steps, _ := store.ListSteps(context.Background(), dag.ID())
	if len(steps) != 2 {
		t.Errorf("got %d steps", len(steps))
	}
}

func TestDAG_Submit_EnqueuesRoots_Only(t *testing.T) {
	enq := &captureEnq{}
	wf := NewWorkflow(NewMemStore(), NewMemBus(), enq)
	dag := New()
	a := dag.Step("a", noopA, 1)
	dag.Step("c", noopC, a.Ref(), "literal")

	if err := dag.Submit(context.Background(), wf); err != nil {
		t.Fatal(err)
	}
	if len(enq.tasks) != 1 {
		t.Fatalf("want 1 root enqueued, got %d", len(enq.tasks))
	}
	if enq.tasks[0].StepID != "a" {
		t.Errorf("enqueued root: %q, want a", enq.tasks[0].StepID)
	}
}

func TestDAG_Submit_PersistsPlacement(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	dag := New()
	a := dag.StepOpts("a", noopA, []StepOption{OnTarget("primary")}, 1)
	dag.StepOpts("b", noopB, []StepOption{ColocateWith(a)})
	dag.StepOpts("c", noopB, []StepOption{FollowTargetOf(a)})

	if err := dag.Submit(context.Background(), wf); err != nil {
		t.Fatal(err)
	}

	aRec, _, err := store.GetStep(context.Background(), dag.ID(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if aRec.Placement == nil || aRec.Placement.Mode != PlacementDirect || aRec.Placement.Target != "primary" {
		t.Fatalf("a placement = %+v, want direct primary", aRec.Placement)
	}

	bRec, _, err := store.GetStep(context.Background(), dag.ID(), "b")
	if err != nil {
		t.Fatal(err)
	}
	if bRec.Placement == nil || bRec.Placement.Mode != PlacementColocate || bRec.Placement.StepID != "a" {
		t.Fatalf("b placement = %+v, want colocate_with a", bRec.Placement)
	}

	cRec, _, err := store.GetStep(context.Background(), dag.ID(), "c")
	if err != nil {
		t.Fatal(err)
	}
	if cRec.Placement == nil || cRec.Placement.Mode != PlacementFollow || cRec.Placement.StepID != "a" {
		t.Fatalf("c placement = %+v, want follow_target_of a", cRec.Placement)
	}
}

func TestDAG_Submit_ColocateHereOutsideHandler_Errors(t *testing.T) {
	dag := New()
	dag.StepOpts("a", noopB, []StepOption{ColocateHere()})

	wf := NewWorkflow(NewMemStore(), NewMemBus(), &captureEnq{})
	err := dag.Submit(context.Background(), wf)
	if err == nil || !strings.Contains(err.Error(), "ColocateHere") {
		t.Errorf("want ColocateHere error, got %v", err)
	}
}

func TestDAG_Submit_Idempotent_BlocksResubmit(t *testing.T) {
	wf := NewWorkflow(NewMemStore(), NewMemBus(), &captureEnq{})
	dag := New()
	dag.Step("a", noopA, 1)
	if err := dag.Submit(context.Background(), wf); err != nil {
		t.Fatal(err)
	}
	if err := dag.Submit(context.Background(), wf); err == nil {
		t.Error("second Submit should error")
	}
}

func TestDAG_DefaultPolicy_PropagatesToSteps(t *testing.T) {
	policy := defaultTestPolicy()
	dag := New(WithRetry(policy))
	s := dag.Step("a", noopA, 1)
	if s.policy == nil || s.policy.MaximumAttempts != policy.MaximumAttempts {
		t.Errorf("policy not propagated: %+v", s.policy)
	}
}

func TestDAG_StepPolicy_OverridesDefault(t *testing.T) {
	defaultP := defaultTestPolicy()
	override := defaultP
	override.MaximumAttempts = 99
	dag := New(WithRetry(defaultP))
	s := dag.StepOpts("a", noopA, []StepOption{WithStepRetry(override)}, 1)
	if s.policy.MaximumAttempts != 99 {
		t.Errorf("want override 99, got %d", s.policy.MaximumAttempts)
	}
}

func TestDAG_After_PopulatesRequiredDeps(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	dag := New()
	a := dag.Step("a", noopA, 1)
	dag.StepOpts("b", noopB, []StepOption{After(a)}) // temporal dep, no Ref in args

	if err := dag.Submit(context.Background(), wf); err != nil {
		t.Fatal(err)
	}
	rec, _, err := store.GetStep(context.Background(), dag.ID(), "b")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Deps) != 1 || rec.Deps[0] != "a" {
		t.Errorf("Deps: got %v, want [a]", rec.Deps)
	}
	if len(rec.OptionalDeps) != 0 {
		t.Errorf("OptionalDeps should be empty, got %v", rec.OptionalDeps)
	}
}

func TestDAG_AfterAny_PopulatesOptionalDeps(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	dag := New()
	a := dag.Step("a", noopA, 1)
	dag.StepOpts("b", noopB, []StepOption{AfterAny(a)})

	if err := dag.Submit(context.Background(), wf); err != nil {
		t.Fatal(err)
	}
	rec, _, err := store.GetStep(context.Background(), dag.ID(), "b")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Deps) != 0 {
		t.Errorf("Deps should be empty, got %v", rec.Deps)
	}
	if len(rec.OptionalDeps) != 1 || rec.OptionalDeps[0] != "a" {
		t.Errorf("OptionalDeps: got %v, want [a]", rec.OptionalDeps)
	}
}

func TestDAG_After_NotEnqueuedAsRoot(t *testing.T) {
	// A step with After() is NOT a root — should only enqueue the real root.
	enq := &captureEnq{}
	wf := NewWorkflow(NewMemStore(), NewMemBus(), enq)
	dag := New()
	a := dag.Step("a", noopA, 1)
	dag.StepOpts("b", noopB, []StepOption{After(a)})

	if err := dag.Submit(context.Background(), wf); err != nil {
		t.Fatal(err)
	}
	if enq.count() != 1 || enq.nthStepID(0) != "a" {
		t.Errorf("expected only a enqueued; got count=%d first=%q", enq.count(), enq.nthStepID(0))
	}
}

func TestDAG_Cycle_Detected_Via_After(t *testing.T) {
	dag := New()
	a := dag.Step("a", noopA, 1)
	b := dag.Step("b", noopB)
	// Create After-only cycle: a.After(b), b.After(a).
	a.afterDeps = append(a.afterDeps, b.id)
	b.afterDeps = append(b.afterDeps, a.id)

	wf := NewWorkflow(NewMemStore(), NewMemBus(), &captureEnq{})
	err := dag.Submit(context.Background(), wf)
	if err == nil || !errors.Is(err, ErrCycle) {
		t.Errorf("want ErrCycle for After()-induced cycle, got %v", err)
	}
}

func TestDAG_After_Dedupes_Against_Ref(t *testing.T) {
	// If a step has both a Ref(upstream) in args AND After(upstream), the dep
	// should appear only once in StepRecord.Deps.
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	dag := New()
	a := dag.Step("a", noopA, 1)
	// c has Ref(a) as arg + After(a) redundantly.
	dag.StepOpts("c", noopC, []StepOption{After(a)}, a.Ref(), "literal")

	if err := dag.Submit(context.Background(), wf); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := store.GetStep(context.Background(), dag.ID(), "c")
	if len(rec.Deps) != 1 || rec.Deps[0] != "a" {
		t.Errorf("Deps should dedupe: got %v, want [a]", rec.Deps)
	}
}
