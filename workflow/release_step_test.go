package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// ReleaseStep tests (MemStore/MemBus/captureEnq fakes).

func TestReleaseStep_Before_ReleasesOnlyThatStep(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	_ = dag.StepOpts("eu", noopA, []StepOption{BreakBefore("X")}, a.Ref())
	_ = dag.StepOpts("us", noopA, []StepOption{BreakBefore("X")}, a.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	tap := tapBus(t, wf)
	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)
	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	waitStepBPState(t, wf, dag.ID(), "eu", BPPositionBefore, BPStateBlocked)
	waitStepBPState(t, wf, dag.ID(), "us", BPPositionBefore, BPStateBlocked)

	ok, err := ReleaseStep(ctx, wf, dag.ID(), "eu", BPPositionBefore)
	if err != nil || !ok {
		t.Fatalf("ReleaseStep(eu) = %v, %v; want true, nil", ok, err)
	}
	waitEnqueued(t, enq, 2, time.Second)
	if got := enq.nthStepID(1); got != "eu" {
		t.Fatalf("second enqueue = %s, want eu", got)
	}
	res := tap.waitForEvent(t, EventBPResumed, "eu", 2*time.Second)
	if res.BPPosition != BPPositionBefore || strings.Join(res.BPLabels, ",") != "X" {
		t.Errorf("bp_resumed(eu): position=%s labels=%v, want before/[X]", res.BPPosition, res.BPLabels)
	}

	time.Sleep(200 * time.Millisecond)
	if got := enq.count(); got != 2 {
		t.Fatalf("us must stay stopped; got %d enqueues", got)
	}
	infos, err := ListBreakpoints(ctx, wf, dag.ID())
	if err != nil {
		t.Fatal(err)
	}
	for _, bp := range infos {
		if bp.StepID == "us" && bp.State != BPStateBlocked {
			t.Errorf("us state = %q, want blocked", bp.State)
		}
	}

	// "Continue" releases what is left: only us.
	if n, err := ResumeBreakpoint(ctx, wf, dag.ID(), "X"); err != nil || n != 1 {
		t.Fatalf("ResumeBreakpoint after ReleaseStep: n=%d err=%v, want 1", n, err)
	}
	waitEnqueued(t, enq, 3, time.Second)
}

func TestReleaseStep_After_ReleasesOnlyThatStepsDependents(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a1 := dag.StepOpts("a1", noopA, []StepOption{BreakAfter("X")}, 1)
	a2 := dag.StepOpts("a2", noopA, []StepOption{BreakAfter("X")}, 2)
	_ = dag.Step("b1", noopA, a1.Ref())
	_ = dag.Step("b2", noopA, a2.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 2, time.Second) // roots a1, a2
	emulateHook(t, wf, dag.ID(), "a1", []byte(`2`), nil)
	emulateHook(t, wf, dag.ID(), "a2", []byte(`3`), nil)
	waitStepBPState(t, wf, dag.ID(), "a1", BPPositionAfter, BPStateBlocked)
	waitStepBPState(t, wf, dag.ID(), "a2", BPPositionAfter, BPStateBlocked)

	// The before position of a1 is not stopped (a1 declares no BreakBefore).
	if ok, err := ReleaseStep(ctx, wf, dag.ID(), "a1", BPPositionBefore); err != nil || ok {
		t.Fatalf("ReleaseStep(a1, before) = %v, %v; want false, nil", ok, err)
	}
	ok, err := ReleaseStep(ctx, wf, dag.ID(), "a1", BPPositionAfter)
	if err != nil || !ok {
		t.Fatalf("ReleaseStep(a1, after) = %v, %v; want true, nil", ok, err)
	}
	waitEnqueued(t, enq, 3, time.Second)
	if got := enq.nthStepID(2); got != "b1" {
		t.Fatalf("third enqueue = %s, want b1", got)
	}
	time.Sleep(200 * time.Millisecond)
	if got := enq.count(); got != 3 {
		t.Fatalf("b2 must stay held by a2; got %d enqueues", got)
	}
}

// A step that has not arrived at its breakpoint is not pre-released: the
// breakpoint still stops it when it gets there.
func TestReleaseStep_NotStoppedYet_FalseAndStillStops(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	_ = dag.StepOpts("b", noopA, []StepOption{BreakBefore("X")}, a.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)

	// b is still waiting on a.
	if ok, err := ReleaseStep(ctx, wf, dag.ID(), "b", BPPositionBefore); err != nil || ok {
		t.Fatalf("ReleaseStep(b) before arrival = %v, %v; want false, nil", ok, err)
	}
	rec, _, _ := wf.Store.GetStep(ctx, dag.ID(), "b")
	if rec.BPBefore == BPStateReleased {
		t.Fatal("ReleaseStep must not pre-release a step that is not stopped")
	}

	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	waitStepBPState(t, wf, dag.ID(), "b", BPPositionBefore, BPStateBlocked)
	time.Sleep(100 * time.Millisecond)
	if got := enq.count(); got != 1 {
		t.Fatalf("b must stop at its breakpoint; got %d enqueues", got)
	}

	// Now it is stopped: the release works, and a repeat is a no-op.
	if ok, err := ReleaseStep(ctx, wf, dag.ID(), "b", BPPositionBefore); err != nil || !ok {
		t.Fatalf("ReleaseStep(b) = %v, %v; want true, nil", ok, err)
	}
	waitEnqueued(t, enq, 2, time.Second)
	if ok, err := ReleaseStep(ctx, wf, dag.ID(), "b", BPPositionBefore); err != nil || ok {
		t.Fatalf("repeat ReleaseStep(b) = %v, %v; want false, nil", ok, err)
	}
}

func TestReleaseStep_UnarmedLabel_False(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	_ = dag.StepOpts("a", noopA, []StepOption{BreakBefore("X")}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := dag.Submit(ctx, wf); err != nil { // X not armed: a runs
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)
	if ok, err := ReleaseStep(ctx, wf, dag.ID(), "a", BPPositionBefore); err != nil || ok {
		t.Fatalf("unarmed: %v, %v; want false, nil", ok, err)
	}
}

func TestReleaseStep_PausedDAG_TakesEffectOnResume(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	_ = dag.StepOpts("b", noopA, []StepOption{BreakBefore("X")}, a.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)
	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	waitStepBPState(t, wf, dag.ID(), "b", BPPositionBefore, BPStateBlocked)

	if err := Pause(ctx, wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	if ok, err := ReleaseStep(ctx, wf, dag.ID(), "b", BPPositionBefore); err != nil || !ok {
		t.Fatalf("ReleaseStep while paused = %v, %v; want true, nil", ok, err)
	}
	time.Sleep(300 * time.Millisecond)
	rec, _, _ := wf.Store.GetStep(ctx, dag.ID(), "b")
	if !rec.Held || rec.BPBefore != BPStateReleased {
		t.Fatalf("b: held=%v bp_before=%q, want held and released", rec.Held, rec.BPBefore)
	}
	if got := enq.count(); got != 1 {
		t.Fatalf("paused DAG must not enqueue; got %d", got)
	}

	if err := Resume(ctx, wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 2, time.Second)
}

func TestReleaseStep_Errors(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	ctx := context.Background()
	_ = store.PutMeta(ctx, "run", DAGMeta{ID: "run", Status: DAGStatusRunning}, 0)
	_ = store.PutMeta(ctx, "done", DAGMeta{ID: "done", Status: DAGStatusDone}, 0)

	if _, err := ReleaseStep(ctx, wf, "ghost", "a", BPPositionBefore); !errors.Is(err, ErrDAGNotFound) {
		t.Errorf("unknown DAG: err = %v, want ErrDAGNotFound", err)
	}
	if _, err := ReleaseStep(ctx, wf, "run", "ghost", BPPositionBefore); !errors.Is(err, ErrStepNotFound) {
		t.Errorf("unknown step: err = %v, want ErrStepNotFound", err)
	}
	if ok, err := ReleaseStep(ctx, wf, "done", "a", BPPositionBefore); err != nil || ok {
		t.Errorf("terminal DAG: %v, %v; want false, nil", ok, err)
	}
	if _, err := ReleaseStep(ctx, wf, "run", "a", "during"); err == nil {
		t.Error("unknown position: want error")
	}
}

func TestReleaseStep_CrashBetweenCASAndPublish_RetryConverges(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	dag.StepOpts("a", noopA, []StepOption{BreakBefore("X")}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	// A ReleaseStep that crashed after its CAS write, before the publish.
	rec, rev, err := wf.Store.GetStep(ctx, dag.ID(), "a")
	if err != nil {
		t.Fatal(err)
	}
	rec.BPBefore = BPStateReleased
	if _, err := wf.Store.PutStep(ctx, dag.ID(), "a", rec, rev); err != nil {
		t.Fatal(err)
	}

	startScheduler(t, wf, ctx)
	if ok, err := ReleaseStep(ctx, wf, dag.ID(), "a", BPPositionBefore); err != nil || ok {
		t.Fatalf("retry: %v, %v; want false, nil", ok, err)
	}
	waitEnqueued(t, enq, 1, time.Second)
}
