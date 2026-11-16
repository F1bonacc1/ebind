package workflow

import (
	"context"
	"errors"
	"testing"
	"time"
)

// StartPaused tests (MemStore/MemBus/captureEnq fakes).

func TestStartPaused_SubmitWritesPausedAndHeld_NothingRuns(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	_ = dag.Step("b", noopA, a.Ref())
	// An armed before-BP root would normally get an advisory mark and a
	// bp_hit at Submit; a start-paused Submit must write neither.
	_ = dag.StepOpts("c", noopA, []StepOption{BreakBefore("X")}, 2)

	ctx := context.Background()
	tap := tapBus(t, wf)
	before := time.Now().UTC()
	if err := dag.Submit(ctx, wf, StartPaused(), WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}

	meta, _, err := wf.Store.GetMeta(ctx, dag.ID())
	if err != nil {
		t.Fatal(err)
	}
	if meta.Status != DAGStatusPaused {
		t.Errorf("meta status = %s, want paused", meta.Status)
	}
	if meta.PausedAt.Before(before) {
		t.Errorf("PausedAt = %v, want set at submit", meta.PausedAt)
	}
	steps, err := wf.Store.ListSteps(ctx, dag.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 {
		t.Fatalf("steps = %d, want 3", len(steps))
	}
	for _, s := range steps {
		if s.Status != StatusPending || !s.Held || s.HeldAt.IsZero() {
			t.Errorf("step %s: status=%s held=%v held_at=%v, want pending+held", s.StepID, s.Status, s.Held, s.HeldAt)
		}
		if s.BPBefore != "" || !s.BPBlockedAt.IsZero() {
			t.Errorf("step %s: advisory bp mark written at start-paused submit", s.StepID)
		}
	}

	time.Sleep(100 * time.Millisecond)
	if got := enq.count(); got != 0 {
		t.Errorf("enqueues = %d, want 0", got)
	}
	tap.mu.Lock()
	n := len(tap.events)
	tap.mu.Unlock()
	if n != 0 {
		t.Errorf("events published = %d, want 0", n)
	}
}

func TestStartPaused_ResumeDispatchesExactlyTheRoots(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	_ = dag.Step("b", noopA, a.Ref())
	_ = dag.Step("c", noopA, 2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, StartPaused()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := enq.count(); got != 0 {
		t.Fatalf("enqueues before Resume = %d, want 0", got)
	}

	if err := Resume(ctx, wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 2, time.Second)
	time.Sleep(100 * time.Millisecond)
	if got := enq.count(); got != 2 {
		t.Fatalf("enqueues after Resume = %d, want 2 (roots a, c)", got)
	}
	got := map[string]bool{enq.nthStepID(0): true, enq.nthStepID(1): true}
	if !got["a"] || !got["c"] {
		t.Fatalf("enqueued %v, want roots a and c", got)
	}
	steps, _ := wf.Store.ListSteps(ctx, dag.ID())
	for _, s := range steps {
		if s.Held {
			t.Errorf("step %s still held after Resume", s.StepID)
		}
	}

	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	waitEnqueued(t, enq, 3, time.Second)
	if got := enq.nthStepID(2); got != "b" {
		t.Errorf("third enqueue = %s, want b", got)
	}
}

// Breakpoint and signal gates still apply to roots released by Resume, and
// the advisory breakpoint mark is written then.
func TestStartPaused_ResumeHonorsBreakpointAndSignalGates(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	_ = dag.StepOpts("bp", noopA, []StepOption{BreakBefore("X")}, 1)
	_ = dag.StepOpts("sig", noopA, []StepOption{WaitForSignal("go")}, 2)
	_ = dag.Step("free", noopA, 3)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, StartPaused(), WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	if err := Resume(ctx, wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)
	if got := enq.nthStepID(0); got != "free" {
		t.Fatalf("first enqueue = %s, want free", got)
	}
	waitStepBPState(t, wf, dag.ID(), "bp", BPPositionBefore, BPStateBlocked)

	if _, err := Signal(ctx, wf, dag.ID(), "go", nil); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 2, time.Second)
	if got := enq.nthStepID(1); got != "sig" {
		t.Fatalf("second enqueue = %s, want sig", got)
	}
	time.Sleep(100 * time.Millisecond)
	if got := enq.count(); got != 2 {
		t.Fatalf("bp must stay stopped; enqueues = %d", got)
	}
}

// A signal delivered while start-paused is durable and satisfies the gate
// after Resume.
func TestStartPaused_SignalWhilePaused_RunsAfterResume(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	_ = dag.StepOpts("sig", noopA, []StepOption{WaitForSignal("go")}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, StartPaused()); err != nil {
		t.Fatal(err)
	}
	if ok, err := Signal(ctx, wf, dag.ID(), "go", "payload"); err != nil || !ok {
		t.Fatalf("Signal while paused = %v, %v", ok, err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := enq.count(); got != 0 {
		t.Fatalf("paused DAG must not enqueue on signal; got %d", got)
	}
	if err := Resume(ctx, wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)
}

func TestStartPaused_CancelCancelsEveryStep(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	_ = dag.Step("b", noopA, a.Ref())

	ctx := context.Background()
	if err := dag.Submit(ctx, wf, StartPaused()); err != nil {
		t.Fatal(err)
	}
	if err := Cancel(ctx, wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	meta, _, _ := wf.Store.GetMeta(ctx, dag.ID())
	if meta.Status != DAGStatusCanceled {
		t.Errorf("meta status = %s, want canceled", meta.Status)
	}
	steps, _ := wf.Store.ListSteps(ctx, dag.ID())
	for _, s := range steps {
		if s.Status != StatusCanceled {
			t.Errorf("step %s status = %s, want canceled", s.StepID, s.Status)
		}
	}
	if got := enq.count(); got != 0 {
		t.Errorf("enqueues = %d, want 0", got)
	}
}

// The leader sweep must treat start-paused holds as a deliberate pause, not
// orphans: meta stays paused, holds stay, nothing is enqueued.
func TestStartPaused_SweepLeavesItAlone(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	_ = dag.Step("b", noopA, a.Ref())

	ctx := context.Background()
	if err := dag.Submit(ctx, wf, StartPaused()); err != nil {
		t.Fatal(err)
	}
	// Age the holds past the orphan threshold, as if paused for a long time.
	steps, _ := wf.Store.ListSteps(ctx, dag.ID())
	for _, s := range steps {
		rec, rev, _ := wf.Store.GetStep(ctx, dag.ID(), s.StepID)
		rec.HeldAt = time.Now().Add(-24 * time.Hour).UTC()
		if _, err := wf.Store.PutStep(ctx, dag.ID(), s.StepID, rec, rev); err != nil {
			t.Fatal(err)
		}
	}

	s := &Scheduler{wf: wf}
	if err := s.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	meta, _, _ := wf.Store.GetMeta(ctx, dag.ID())
	if meta.Status != DAGStatusPaused {
		t.Errorf("meta status after sweep = %s, want paused", meta.Status)
	}
	steps, _ = wf.Store.ListSteps(ctx, dag.ID())
	for _, st := range steps {
		if !st.Held || st.Status != StatusPending {
			t.Errorf("step %s after sweep: held=%v status=%s", st.StepID, st.Held, st.Status)
		}
	}
	if got := enq.count(); got != 0 {
		t.Errorf("sweep enqueued %d, want 0", got)
	}
}

func TestStartPaused_PauseReturnsNotRunning(t *testing.T) {
	wf, dag, _ := setupDAG(t)
	_ = dag.Step("a", noopA, 1)
	ctx := context.Background()
	if err := dag.Submit(ctx, wf, StartPaused()); err != nil {
		t.Fatal(err)
	}
	if err := Pause(ctx, wf, dag.ID()); !errors.Is(err, ErrDAGNotRunning) {
		t.Errorf("Pause on start-paused DAG: err = %v, want ErrDAGNotRunning", err)
	}
}

func TestStartPaused_DebugShowsPausedAllPending(t *testing.T) {
	wf, dag, _ := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	_ = dag.Step("b", noopA, a.Ref())
	ctx := context.Background()
	if err := dag.Submit(ctx, wf, StartPaused()); err != nil {
		t.Fatal(err)
	}
	dbg, err := Debug(ctx, wf, dag.ID())
	if err != nil {
		t.Fatal(err)
	}
	if dbg.Meta.Status != DAGStatusPaused {
		t.Errorf("debug meta status = %s, want paused", dbg.Meta.Status)
	}
	if dbg.Counts[StatusPending] != 2 || len(dbg.Steps) != 2 {
		t.Errorf("debug counts = %v steps = %d, want 2 pending", dbg.Counts, len(dbg.Steps))
	}
}
