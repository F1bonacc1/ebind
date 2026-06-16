package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Pure state-layer tests (no IO)
// ---------------------------------------------------------------------------

func TestState_ReadyToRun_ExcludesArmedBeforeBP(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a", Status: StatusDone},
		StepRecord{StepID: "b", Deps: []string{"a"}, ArgsJSON: refArgs(Ref{StepID: "a", Mode: RefModeRequired}), BreakBefore: []string{"X"}},
	)
	s.Meta.ActiveBreakpoints = []string{"X"}
	if ready := s.ReadyToRun(); len(ready) != 0 {
		t.Errorf("armed before-BP must block readiness; ready=%v", ready)
	}
}

func TestState_ReadyToRun_InactiveBeforeBP_Runs(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a", Status: StatusDone},
		StepRecord{StepID: "b", Deps: []string{"a"}, ArgsJSON: refArgs(Ref{StepID: "a", Mode: RefModeRequired}), BreakBefore: []string{"X"}},
	)
	// No active labels: BP declared but inactive — the line continues.
	if ready := s.ReadyToRun(); len(ready) != 1 || ready[0] != "b" {
		t.Errorf("inactive before-BP must not block; ready=%v", ready)
	}
}

func TestState_ReadyToRun_ReleasedBeforeBP_Runs(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a", Status: StatusDone},
		StepRecord{StepID: "b", Deps: []string{"a"}, ArgsJSON: refArgs(Ref{StepID: "a", Mode: RefModeRequired}),
			BreakBefore: []string{"X"}, BPBefore: BPStateReleased},
	)
	s.Meta.ActiveBreakpoints = []string{"X"}
	if ready := s.ReadyToRun(); len(ready) != 1 || ready[0] != "b" {
		t.Errorf("released before-BP must not block; ready=%v", ready)
	}
}

func TestState_ReadyToRun_MultiLabel_AnyActiveArms(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "b", ArgsJSON: json.RawMessage(`[]`), BreakBefore: []string{"X", "Y"}},
	)
	s.Meta.ActiveBreakpoints = []string{"Y"}
	if ready := s.ReadyToRun(); len(ready) != 0 {
		t.Errorf("any active label arms the BP; ready=%v", ready)
	}
}

func TestState_ReadyToRun_AdvisoryBlockedMarkNotLoadBearing(t *testing.T) {
	// A blocked mark with no armed label (e.g. stale record) must not block.
	s := makeState(
		StepRecord{StepID: "b", ArgsJSON: json.RawMessage(`[]`), BreakBefore: []string{"X"}, BPBefore: BPStateBlocked},
	)
	if ready := s.ReadyToRun(); len(ready) != 1 {
		t.Errorf("blocked mark without armed label must not gate; ready=%v", ready)
	}
}

func TestState_DepsSatisfied_AfterBPGatesRequiredAndOptionalDeps(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a", Status: StatusDone, BreakAfter: []string{"X"}},
		StepRecord{StepID: "b", Deps: []string{"a"}, ArgsJSON: refArgs(Ref{StepID: "a", Mode: RefModeRequired})},
		StepRecord{StepID: "c", OptionalDeps: []string{"a"}, ArgsJSON: json.RawMessage(`[]`)},
	)
	s.Meta.ActiveBreakpoints = []string{"X"}
	if ready := s.ReadyToRun(); len(ready) != 0 {
		t.Errorf("armed after-BP on a must gate both b (required) and c (optional); ready=%v", ready)
	}
}

func TestState_AfterBP_FanIn_OtherParentLater_StillGated(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a", Status: StatusDone, BreakAfter: []string{"X"}},
		StepRecord{StepID: "b", Status: StatusRunning},
		StepRecord{StepID: "c", Deps: []string{"a", "b"}, ArgsJSON: refArgs(
			Ref{StepID: "a", Mode: RefModeRequired},
			Ref{StepID: "b", Mode: RefModeRequired},
		)},
	)
	s.Meta.ActiveBreakpoints = []string{"X"}
	// b completes later — c must still be gated by a's after-BP.
	if _, err := s.MarkDone("b"); err != nil {
		t.Fatal(err)
	}
	if ready := s.ReadyToRun(); len(ready) != 0 {
		t.Errorf("fan-in dependent must stay gated by a's after-BP; ready=%v", ready)
	}
}

func TestState_AfterBP_Released_DependentReady(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a", Status: StatusDone, BreakAfter: []string{"X"}, BPAfter: BPStateReleased},
		StepRecord{StepID: "b", Deps: []string{"a"}, ArgsJSON: refArgs(Ref{StepID: "a", Mode: RefModeRequired})},
	)
	s.Meta.ActiveBreakpoints = []string{"X"}
	if ready := s.ReadyToRun(); len(ready) != 1 || ready[0] != "b" {
		t.Errorf("released after-BP must not gate; ready=%v", ready)
	}
}

func TestState_AfterBP_FailedParent_NoGate_CascadesInstead(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a", BreakAfter: []string{"X"}},
		StepRecord{StepID: "b", Deps: []string{"a"}, ArgsJSON: refArgs(Ref{StepID: "a", Mode: RefModeRequired})},
	)
	s.Meta.ActiveBreakpoints = []string{"X"}
	_, skipped, err := s.MarkFailed("a", "boom", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0] != "b" {
		t.Errorf("after-BP fires only on done — failed parent must cascade; skipped=%v", skipped)
	}
	if s.afterBPHolds(s.Steps["a"]) {
		t.Error("afterBPHolds must be false for a failed step")
	}
}

func TestState_BlockedAtBefore_RequiresDepsSatisfied(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a", Status: StatusRunning},
		StepRecord{StepID: "b", Deps: []string{"a"}, ArgsJSON: refArgs(Ref{StepID: "a", Mode: RefModeRequired}), BreakBefore: []string{"X"}},
	)
	s.Meta.ActiveBreakpoints = []string{"X"}
	if blocked := s.BlockedAtBefore(); len(blocked) != 0 {
		t.Errorf("b hasn't arrived at its breakpoint yet (a running); blocked=%v", blocked)
	}
	if _, err := s.MarkDone("a"); err != nil {
		t.Fatal(err)
	}
	if blocked := s.BlockedAtBefore(); len(blocked) != 1 || blocked[0] != "b" {
		t.Errorf("b is now stopped at its breakpoint; blocked=%v", blocked)
	}
}

func TestState_BlockedAtBefore_BehindAfterGatedParent_NotBlocked(t *testing.T) {
	// b has its own before-BP but its parent's after-gate is still holding it —
	// b hasn't arrived at its own breakpoint yet.
	s := makeState(
		StepRecord{StepID: "a", Status: StatusDone, BreakAfter: []string{"X"}},
		StepRecord{StepID: "b", Deps: []string{"a"}, ArgsJSON: refArgs(Ref{StepID: "a", Mode: RefModeRequired}), BreakBefore: []string{"Y"}},
	)
	s.Meta.ActiveBreakpoints = []string{"X", "Y"}
	if blocked := s.BlockedAtBefore(); len(blocked) != 0 {
		t.Errorf("b is held by a's after-gate, not stopped at its own BP; blocked=%v", blocked)
	}
	if holding := s.HoldingAtAfter(); len(holding) != 1 || holding[0] != "a" {
		t.Errorf("a's after-gate should be holding; holding=%v", holding)
	}
}

func TestState_HoldingAtAfter_OnlyDoneSteps(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a", Status: StatusRunning, BreakAfter: []string{"X"}},
		StepRecord{StepID: "b", Status: StatusFailed, BreakAfter: []string{"X"}},
		StepRecord{StepID: "c", Status: StatusDone, BreakAfter: []string{"X"}},
	)
	s.Meta.ActiveBreakpoints = []string{"X"}
	if holding := s.HoldingAtAfter(); len(holding) != 1 || holding[0] != "c" {
		t.Errorf("only done steps hold at after-BP; holding=%v", holding)
	}
}

func TestState_Terminal_StaysRunningWhileBPBlocked(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a", Status: StatusDone},
		StepRecord{StepID: "b", Deps: []string{"a"}, ArgsJSON: refArgs(Ref{StepID: "a", Mode: RefModeRequired}), BreakBefore: []string{"X"}},
	)
	s.Meta.ActiveBreakpoints = []string{"X"}
	status, done := s.Terminal()
	if done || status != DAGStatusRunning {
		t.Errorf("BP-blocked DAG must stay running; status=%s done=%v", status, done)
	}
}

func TestState_CascadeSkip_SkipsThroughBlockedStep(t *testing.T) {
	// Breakpoints stop execution; a cascade-skipped step never executes, so the
	// skip passes straight through a blocked step.
	s := makeState(
		StepRecord{StepID: "a"},
		StepRecord{StepID: "b", Deps: []string{"a"}, ArgsJSON: refArgs(Ref{StepID: "a", Mode: RefModeRequired}), BreakBefore: []string{"X"}},
	)
	s.Meta.ActiveBreakpoints = []string{"X"}
	_, skipped, err := s.MarkFailed("a", "boom", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0] != "b" {
		t.Errorf("blocked step must still cascade-skip; skipped=%v", skipped)
	}
}

func TestComputeBreakpoints_States(t *testing.T) {
	meta := DAGMeta{ID: "d", Status: DAGStatusRunning, ActiveBreakpoints: []string{"X"}}
	steps := []StepRecord{
		{StepID: "a", Status: StatusDone, BreakAfter: []string{"X"}, BPBlockedAt: time.Now().UTC()},
		{StepID: "b", Status: StatusPending, Deps: []string{"a"}, ArgsJSON: json.RawMessage(`[]`), BreakBefore: []string{"X"}},
		{StepID: "c", Status: StatusPending, ArgsJSON: json.RawMessage(`[]`), BreakBefore: []string{"Y"}},
		{StepID: "d", Status: StatusDone, BreakBefore: []string{"X"}, BPBefore: BPStateReleased},
	}
	infos := ComputeBreakpoints(meta, steps)
	if len(infos) != 4 {
		t.Fatalf("want 4 breakpoint infos, got %d: %+v", len(infos), infos)
	}
	byKey := map[string]BreakpointInfo{}
	for _, bp := range infos {
		byKey[bp.StepID+"/"+string(bp.Position)] = bp
	}
	a := byKey["a/after"]
	if !a.Armed || a.State != BPStateBlocked || len(a.Holding) != 1 || a.Holding[0] != "b" {
		t.Errorf("a/after: %+v, want armed+blocked holding [b]", a)
	}
	if a.BlockedSince.IsZero() {
		t.Errorf("a/after: BlockedSince should come from the advisory mark")
	}
	// b's before-BP is armed but b is held by a's after-gate — declared, not blocked.
	b := byKey["b/before"]
	if !b.Armed || b.State != "" {
		t.Errorf("b/before: %+v, want armed, state \"\" (not yet at its BP)", b)
	}
	c := byKey["c/before"]
	if c.Armed || c.State != "" {
		t.Errorf("c/before: %+v, want inactive", c)
	}
	d := byKey["d/before"]
	if d.State != BPStateReleased {
		t.Errorf("d/before: %+v, want released", d)
	}
}

func TestComputeBreakpoints_TerminalDAG_NothingBlocked(t *testing.T) {
	// A leaf's armed after-BP holds no dependents, so the DAG finalizes despite
	// it — the projection must not report it as blocked forever (it would be
	// unresumable: ResumeBreakpoint is a no-op on terminal DAGs).
	steps := []StepRecord{{StepID: "leaf", Status: StatusDone, BreakAfter: []string{"X"}}}
	for _, status := range []DAGStatus{DAGStatusDone, DAGStatusFailed, DAGStatusCanceled} {
		meta := DAGMeta{ID: "d", Status: status, ActiveBreakpoints: []string{"X"}}
		infos := ComputeBreakpoints(meta, steps)
		if len(infos) != 1 {
			t.Fatalf("%s: want 1 info, got %+v", status, infos)
		}
		if infos[0].State == BPStateBlocked {
			t.Errorf("%s: terminal DAG must not report blocked breakpoints: %+v", status, infos[0])
		}
		if n := CountBlocked(infos); n != 0 {
			t.Errorf("%s: CountBlocked = %d, want 0", status, n)
		}
	}
	// Released state still renders on a terminal DAG.
	meta := DAGMeta{ID: "d", Status: DAGStatusDone, ActiveBreakpoints: []string{"X"}}
	rel := []StepRecord{{StepID: "a", Status: StatusDone, BreakBefore: []string{"X"}, BPBefore: BPStateReleased}}
	if infos := ComputeBreakpoints(meta, rel); len(infos) != 1 || infos[0].State != BPStateReleased {
		t.Errorf("released state must survive terminal projection: %+v", infos)
	}
}

func TestDebug_Blockers_GatedByAfterBP(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	ctx := context.Background()
	_ = store.PutMeta(ctx, "d", DAGMeta{ID: "d", Status: DAGStatusRunning, ActiveBreakpoints: []string{"X"}}, 0)
	_, _ = store.PutStep(ctx, "d", "a", StepRecord{DAGID: "d", StepID: "a", Status: StatusDone, BreakAfter: []string{"X"}}, 0)
	_, _ = store.PutStep(ctx, "d", "b", StepRecord{DAGID: "d", StepID: "b", Status: StatusPending,
		Deps: []string{"a"}, ArgsJSON: json.RawMessage(`[]`)}, 0)

	dbg, err := Debug(ctx, wf, "d")
	if err != nil {
		t.Fatal(err)
	}
	if len(dbg.Blockers) != 1 {
		t.Fatalf("blockers: %+v, want exactly one", dbg.Blockers)
	}
	bl := dbg.Blockers[0]
	// WaitingOn stays pure step IDs; the after-gate is reported structurally.
	if bl.StepID != "b" || len(bl.WaitingOn) != 0 || len(bl.GatedBy) != 1 || bl.GatedBy[0] != "a" {
		t.Errorf("blocker = %+v, want {b, WaitingOn:[], GatedBy:[a]}", bl)
	}
}

// ---------------------------------------------------------------------------
// DAG builder / Submit tests
// ---------------------------------------------------------------------------

func TestDAG_Submit_PersistsBreakpointsAndActiveSet(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	dag := New()
	a := dag.StepOpts("a", noopA, []StepOption{BreakAfter("A1", "A2")}, 1)
	dag.StepOpts("b", noopA, []StepOption{BreakBefore("X")}, a.Ref())

	if err := dag.Submit(context.Background(), wf, WithActiveBreakpoints("X", "X", "Z")); err != nil {
		t.Fatal(err)
	}
	meta, _, err := store.GetMeta(context.Background(), dag.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.ActiveBreakpoints) != 2 || meta.ActiveBreakpoints[0] != "X" || meta.ActiveBreakpoints[1] != "Z" {
		t.Errorf("ActiveBreakpoints = %v, want deduped [X Z]", meta.ActiveBreakpoints)
	}
	aRec, _, _ := store.GetStep(context.Background(), dag.ID(), "a")
	if len(aRec.BreakAfter) != 2 || aRec.BreakAfter[0] != "A1" {
		t.Errorf("a.BreakAfter = %v", aRec.BreakAfter)
	}
	bRec, _, _ := store.GetStep(context.Background(), dag.ID(), "b")
	if len(bRec.BreakBefore) != 1 || bRec.BreakBefore[0] != "X" {
		t.Errorf("b.BreakBefore = %v", bRec.BreakBefore)
	}
}

func TestDAG_Submit_ZeroLabelBreakpoint_Errors(t *testing.T) {
	for _, opt := range []StepOption{BreakBefore(), BreakAfter()} {
		dag := New()
		dag.StepOpts("a", noopA, []StepOption{opt}, 1)
		wf := NewWorkflow(NewMemStore(), NewMemBus(), &captureEnq{})
		err := dag.Submit(context.Background(), wf)
		if err == nil || !strings.Contains(err.Error(), "at least one label") {
			t.Errorf("want zero-label error, got %v", err)
		}
	}
}

func TestDAG_Submit_EmptyLabel_Errors(t *testing.T) {
	dag := New()
	dag.StepOpts("a", noopA, []StepOption{BreakBefore("X", "")}, 1)
	wf := NewWorkflow(NewMemStore(), NewMemBus(), &captureEnq{})
	err := dag.Submit(context.Background(), wf)
	if err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Errorf("want empty-label error, got %v", err)
	}

	dag2 := New()
	dag2.Step("a", noopA, 1)
	err = dag2.Submit(context.Background(), wf, WithActiveBreakpoints(""))
	if err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Errorf("want empty active-label error, got %v", err)
	}
}

func TestDAG_Submit_ActiveLabelMatchingNoStep_OK(t *testing.T) {
	dag := New()
	dag.Step("a", noopA, 1)
	wf := NewWorkflow(NewMemStore(), NewMemBus(), &captureEnq{})
	if err := dag.Submit(context.Background(), wf, WithActiveBreakpoints("FutureDynamicLabel")); err != nil {
		t.Errorf("active label matching no step must be allowed: %v", err)
	}
}

func TestDAG_Submit_RootWithActiveBeforeBP_NotEnqueued(t *testing.T) {
	enq := &captureEnq{}
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), enq)
	dag := New()
	dag.StepOpts("a", noopA, []StepOption{BreakBefore("X")}, 1)
	dag.Step("b", noopB) // unblocked parallel root

	if err := dag.Submit(context.Background(), wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	if enq.count() != 1 || enq.nthStepID(0) != "b" {
		t.Fatalf("only unblocked root b should be enqueued; got %d (first=%s)", enq.count(), enq.nthStepID(0))
	}
	aRec, _, _ := store.GetStep(context.Background(), dag.ID(), "a")
	if aRec.Status != StatusPending {
		t.Errorf("blocked root a must stay pending, got %s", aRec.Status)
	}
}

func TestDAG_Submit_RootWithInactiveBeforeBP_Enqueued(t *testing.T) {
	enq := &captureEnq{}
	wf := NewWorkflow(NewMemStore(), NewMemBus(), enq)
	dag := New() // no active labels
	dag.StepOpts("a", noopA, []StepOption{BreakBefore("X")}, 1)

	if err := dag.Submit(context.Background(), wf); err != nil {
		t.Fatal(err)
	}
	if enq.count() != 1 {
		t.Fatalf("inactive BP root must be enqueued; got %d", enq.count())
	}
}

// ---------------------------------------------------------------------------
// Scheduler + ResumeBreakpoint tests (MemStore/MemBus/captureEnq fakes)
// ---------------------------------------------------------------------------

// waitStepBPState polls until the step's persisted breakpoint state for the
// given position matches want (or times out).
func waitStepBPState(t *testing.T, wf *Workflow, dagID, stepID string, pos BPPosition, want BPState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec, _, err := wf.Store.GetStep(context.Background(), dagID, stepID)
		if err == nil {
			got := rec.BPBefore
			if pos == BPPositionAfter {
				got = rec.BPAfter
			}
			if got == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	rec, _, _ := wf.Store.GetStep(context.Background(), dagID, stepID)
	t.Fatalf("step %s never reached %s=%q (bp_before=%q bp_after=%q)", stepID, pos, want, rec.BPBefore, rec.BPAfter)
}

func TestScheduler_BeforeBP_BlocksEnqueue_AndMarksAdvisory(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	b := dag.StepOpts("b", noopA, []StepOption{BreakBefore("X")}, a.Ref())
	_ = dag.Step("c", noopC, b.Ref(), "x")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second) // root a

	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)

	// The scheduler should write the advisory blocked mark — and never enqueue b.
	waitStepBPState(t, wf, dag.ID(), "b", BPPositionBefore, BPStateBlocked)
	if got := enq.count(); got != 1 {
		t.Errorf("expected 1 enqueue (a only), got %d", got)
	}
	bRec, _, _ := wf.Store.GetStep(ctx, dag.ID(), "b")
	if bRec.Status != StatusPending {
		t.Errorf("b status = %s, want pending", bRec.Status)
	}
	if bRec.BPBlockedAt.IsZero() {
		t.Error("BPBlockedAt should be set with the advisory mark")
	}
}

func TestScheduler_AfterBP_HoldsDirectDependents_ParallelBranchRuns(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.StepOpts("a", noopA, []StepOption{BreakAfter("X")}, 1)
	_ = dag.Step("b", noopA, a.Ref())
	x := dag.Step("x", noopA, 5) // independent parallel branch
	_ = dag.Step("y", noopA, x.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 2, time.Second) // roots a, x

	// a completes — its after-gate holds b.
	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	waitStepBPState(t, wf, dag.ID(), "a", BPPositionAfter, BPStateBlocked)

	aRec, _, _ := wf.Store.GetStep(ctx, dag.ID(), "a")
	if aRec.Status != StatusDone {
		t.Fatalf("a must complete normally, got %s", aRec.Status)
	}
	if res, err := wf.Store.GetResult(ctx, dag.ID(), "a"); err != nil || string(res) != `2` {
		t.Errorf("a's result must be persisted: %s err=%v", res, err)
	}

	// Independent branch keeps flowing: x completes → y enqueued. b stays held.
	emulateHook(t, wf, dag.ID(), "x", []byte(`6`), nil)
	waitEnqueued(t, enq, 3, time.Second) // y
	if enq.nthStepID(2) != "y" {
		t.Errorf("third enqueue should be y, got %s", enq.nthStepID(2))
	}
	bRec, _, _ := wf.Store.GetStep(ctx, dag.ID(), "b")
	if bRec.Status != StatusPending {
		t.Errorf("b must be held by a's after-gate, got %s", bRec.Status)
	}
}

func TestResumeBreakpoint_ReleasesAllWithLabel_AndEnqueues(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	// Two parallel lines blocked on the same label.
	_ = dag.StepOpts("b1", noopA, []StepOption{BreakBefore("X")}, a.Ref())
	_ = dag.StepOpts("b2", noopA, []StepOption{BreakBefore("X", "Y")}, a.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)
	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	waitStepBPState(t, wf, dag.ID(), "b1", BPPositionBefore, BPStateBlocked)
	waitStepBPState(t, wf, dag.ID(), "b2", BPPositionBefore, BPStateBlocked)

	n, err := ResumeBreakpoint(ctx, wf, dag.ID(), "X")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("released = %d, want 2", n)
	}
	waitEnqueued(t, enq, 3, time.Second) // b1 + b2
}

func TestResumeBreakpoint_AnyLabelOfMultiLabelBP(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	// Armed via X...
	dag.StepOpts("a", noopA, []StepOption{BreakBefore("X", "Y")}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	waitStepBPState(t, wf, dag.ID(), "a", BPPositionBefore, BPStateBlocked)

	// ...but resumable via its other label Y.
	n, err := ResumeBreakpoint(ctx, wf, dag.ID(), "Y")
	if err != nil || n != 1 {
		t.Fatalf("resume via second label: n=%d err=%v", n, err)
	}
	waitEnqueued(t, enq, 1, time.Second)
}

func TestResumeBreakpoint_AfterGate_ReleasesDependents(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.StepOpts("a", noopA, []StepOption{BreakAfter("X")}, 1)
	_ = dag.Step("b", noopA, a.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)
	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	waitStepBPState(t, wf, dag.ID(), "a", BPPositionAfter, BPStateBlocked)

	n, err := ResumeBreakpoint(ctx, wf, dag.ID(), "X")
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	waitEnqueued(t, enq, 2, time.Second) // b released
	if enq.nthStepID(1) != "b" {
		t.Errorf("second enqueue should be b, got %s", enq.nthStepID(1))
	}
}

func TestResumeBreakpoint_LabelStaysArmed_DynamicStepBlocksAgain(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	dag.StepOpts("a", noopA, []StepOption{BreakBefore("X")}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	waitStepBPState(t, wf, dag.ID(), "a", BPPositionBefore, BPStateBlocked)
	if n, err := ResumeBreakpoint(ctx, wf, dag.ID(), "X"); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	waitEnqueued(t, enq, 1, time.Second)

	// Simulate a dynamically added step carrying the same (still armed) label.
	rec := StepRecord{
		DAGID: dag.ID(), StepID: "dyn", FnName: "noopA", Status: StatusPending,
		ArgsJSON: json.RawMessage(`[7]`), BreakBefore: []string{"X"}, AddedAt: time.Now().UTC(),
	}
	if _, err := wf.Store.PutStep(ctx, dag.ID(), "dyn", rec, 0); err != nil {
		t.Fatal(err)
	}
	ev := Event{Kind: EventStepAdded, DAGID: dag.ID(), StepID: "dyn"}
	data, _ := MarshalEvent(ev)
	_ = wf.Bus.Publish(ctx, EventSubject(ev), data)

	// Continue semantics: dyn blocks again on the same label.
	waitStepBPState(t, wf, dag.ID(), "dyn", BPPositionBefore, BPStateBlocked)
	if got := enq.count(); got != 1 {
		t.Errorf("dyn must not be enqueued (label stays armed); got %d enqueues", got)
	}
}

func TestResumeBreakpoint_Idempotent(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	dag.StepOpts("a", noopA, []StepOption{BreakBefore("X")}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	waitStepBPState(t, wf, dag.ID(), "a", BPPositionBefore, BPStateBlocked)

	if n, err := ResumeBreakpoint(ctx, wf, dag.ID(), "X"); err != nil || n != 1 {
		t.Fatalf("first resume: n=%d err=%v", n, err)
	}
	waitEnqueued(t, enq, 1, time.Second)

	if n, err := ResumeBreakpoint(ctx, wf, dag.ID(), "X"); err != nil || n != 0 {
		t.Fatalf("second resume: n=%d err=%v, want 0/nil", n, err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := enq.count(); got != 1 {
		t.Errorf("idempotent resume must not re-enqueue; got %d", got)
	}
}

func TestResumeBreakpoint_NoMatchingLabel_Zero(t *testing.T) {
	wf, dag, _ := setupDAG(t)
	dag.StepOpts("a", noopA, []StepOption{BreakBefore("X")}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	waitStepBPState(t, wf, dag.ID(), "a", BPPositionBefore, BPStateBlocked)

	if n, err := ResumeBreakpoint(ctx, wf, dag.ID(), "Other"); err != nil || n != 0 {
		t.Errorf("non-matching label: n=%d err=%v, want 0/nil", n, err)
	}
	rec, _, _ := wf.Store.GetStep(ctx, dag.ID(), "a")
	if rec.BPBefore == BPStateReleased {
		t.Error("non-matching label must not release")
	}
}

func TestResumeBreakpoint_TerminalDAG_Zero(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	ctx := context.Background()
	_ = store.PutMeta(ctx, "d", DAGMeta{ID: "d", Status: DAGStatusDone}, 0)
	if n, err := ResumeBreakpoint(ctx, wf, "d", "X"); err != nil || n != 0 {
		t.Errorf("terminal DAG: n=%d err=%v, want 0/nil", n, err)
	}
}

func TestResumeBreakpoint_UnknownDAG_ErrDAGNotFound(t *testing.T) {
	wf := NewWorkflow(NewMemStore(), NewMemBus(), &captureEnq{})
	if _, err := ResumeBreakpoint(context.Background(), wf, "ghost", "X"); !errors.Is(err, ErrDAGNotFound) {
		t.Errorf("want ErrDAGNotFound, got %v", err)
	}
}

func TestResumeBreakpoint_CrashBetweenCASAndPublish_RetryConverges(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	dag.StepOpts("a", noopA, []StepOption{BreakBefore("X")}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}

	// Simulate a ResumeBreakpoint that crashed after the CAS write but before
	// the event publish: release the flag manually, no event.
	rec, rev, err := wf.Store.GetStep(ctx, dag.ID(), "a")
	if err != nil {
		t.Fatal(err)
	}
	rec.BPBefore = BPStateReleased
	if _, err := wf.Store.PutStep(ctx, dag.ID(), "a", rec, rev); err != nil {
		t.Fatal(err)
	}

	startScheduler(t, wf, ctx)
	// The retry finds nothing blocked (0) but still publishes the resume event,
	// which enqueues the already-released step.
	if n, err := ResumeBreakpoint(ctx, wf, dag.ID(), "X"); err != nil || n != 0 {
		t.Fatalf("retry: n=%d err=%v, want 0/nil", n, err)
	}
	waitEnqueued(t, enq, 1, time.Second)
}

func TestResumeBreakpoint_FinalizesAfterLastResume(t *testing.T) {
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

	// While blocked: DAG must not finalize.
	meta, _, _ := wf.Store.GetMeta(ctx, dag.ID())
	if meta.Status != DAGStatusRunning {
		t.Fatalf("DAG must stay running while blocked, got %s", meta.Status)
	}

	if _, err := ResumeBreakpoint(ctx, wf, dag.ID(), "X"); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 2, time.Second)
	emulateHook(t, wf, dag.ID(), "b", []byte(`3`), nil)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m, _, _ := wf.Store.GetMeta(ctx, dag.ID())
		if m.Status == DAGStatusDone {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	m, _, _ := wf.Store.GetMeta(ctx, dag.ID())
	t.Fatalf("DAG never finalized after resume; status=%s", m.Status)
}

func TestResumeBreakpoint_BothPositions_TwoStops(t *testing.T) {
	// A step with BreakBefore+BreakAfter on the same label = two distinct
	// breakpoints: continue past the first, run, stop at the second.
	wf, dag, enq := setupDAG(t)
	a := dag.StepOpts("a", noopA, []StepOption{BreakBefore("X"), BreakAfter("X")}, 1)
	_ = dag.Step("b", noopA, a.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	waitStepBPState(t, wf, dag.ID(), "a", BPPositionBefore, BPStateBlocked)

	// Continue #1: only the before-stop is currently hit.
	if n, err := ResumeBreakpoint(ctx, wf, dag.ID(), "X"); err != nil || n != 1 {
		t.Fatalf("first continue: n=%d err=%v, want 1", n, err)
	}
	waitEnqueued(t, enq, 1, time.Second) // a runs
	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	waitStepBPState(t, wf, dag.ID(), "a", BPPositionAfter, BPStateBlocked)
	if got := enq.count(); got != 1 {
		t.Fatalf("b must be held by a's after-gate; got %d enqueues", got)
	}

	// Continue #2 releases the after-gate.
	if n, err := ResumeBreakpoint(ctx, wf, dag.ID(), "X"); err != nil || n != 1 {
		t.Fatalf("second continue: n=%d err=%v, want 1", n, err)
	}
	waitEnqueued(t, enq, 2, time.Second) // b
}

// ---------------------------------------------------------------------------
// Informational bp_hit / bp_resumed events
// ---------------------------------------------------------------------------

// eventTap collects every event published on the bus, alongside whatever other
// subscribers (the scheduler) receive.
type eventTap struct {
	mu     sync.Mutex
	events []Event
}

func tapBus(t *testing.T, wf *Workflow) *eventTap {
	t.Helper()
	tap := &eventTap{}
	_, err := wf.Bus.Subscribe(context.Background(), "DAG.>", func(ev Event) {
		if ev.Ack != nil {
			_ = ev.Ack()
		}
		tap.mu.Lock()
		tap.events = append(tap.events, ev)
		tap.mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	return tap
}

func (tp *eventTap) find(kind EventKind, stepID string) (Event, bool) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	for _, ev := range tp.events {
		if ev.Kind == kind && ev.StepID == stepID {
			return ev, true
		}
	}
	return Event{}, false
}

func (tp *eventTap) waitForEvent(t *testing.T, kind EventKind, stepID string, timeout time.Duration) Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ev, ok := tp.find(kind, stepID); ok {
			return ev
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("event %s for step %q never observed", kind, stepID)
	return Event{}
}

func TestBPEvents_HitAndResumed_FullCycle(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.StepOpts("a", noopA, []StepOption{BreakAfter("X")}, 1)
	_ = dag.StepOpts("b", noopA, []StepOption{BreakBefore("X")}, a.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	tap := tapBus(t, wf)
	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)
	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)

	// a completes and its after-gate engages — the scheduler announces the hit
	// with the position's full label config.
	hit := tap.waitForEvent(t, EventBPHit, "a", 2*time.Second)
	if hit.BPPosition != BPPositionAfter || strings.Join(hit.BPLabels, ",") != "X" {
		t.Errorf("bp_hit(a): position=%s labels=%v, want after/[X]", hit.BPPosition, hit.BPLabels)
	}

	if n, err := ResumeBreakpoint(ctx, wf, dag.ID(), "X"); err != nil || n != 1 {
		t.Fatalf("first resume: n=%d err=%v", n, err)
	}
	res := tap.waitForEvent(t, EventBPResumed, "a", 2*time.Second)
	if res.BPPosition != BPPositionAfter || strings.Join(res.BPLabels, ",") != "X" {
		t.Errorf("bp_resumed(a): position=%s labels=%v, want after/[X]", res.BPPosition, res.BPLabels)
	}

	// b then arrives at its own before-breakpoint: a second hit.
	hitB := tap.waitForEvent(t, EventBPHit, "b", 2*time.Second)
	if hitB.BPPosition != BPPositionBefore {
		t.Errorf("bp_hit(b): position=%s, want before", hitB.BPPosition)
	}
	if n, err := ResumeBreakpoint(ctx, wf, dag.ID(), "X"); err != nil || n != 1 {
		t.Fatalf("second resume: n=%d err=%v", n, err)
	}
	resB := tap.waitForEvent(t, EventBPResumed, "b", 2*time.Second)
	if resB.BPPosition != BPPositionBefore {
		t.Errorf("bp_resumed(b): position=%s, want before", resB.BPPosition)
	}
	waitEnqueued(t, enq, 2, time.Second) // b dispatched after the release
}

func TestDAG_Submit_BlockedRoot_PublishesBPHit(t *testing.T) {
	enq := &captureEnq{}
	wf := NewWorkflow(NewMemStore(), NewMemBus(), enq)
	tap := tapBus(t, wf)
	dag := New()
	dag.StepOpts("a", noopA, []StepOption{BreakBefore("X", "Y")}, 1)
	// No scheduler running: Submit itself must announce the blocked root.
	if err := dag.Submit(context.Background(), wf, WithActiveBreakpoints("Y")); err != nil {
		t.Fatal(err)
	}
	hit := tap.waitForEvent(t, EventBPHit, "a", time.Second)
	if hit.BPPosition != BPPositionBefore || strings.Join(hit.BPLabels, ",") != "X,Y" {
		t.Errorf("bp_hit: position=%s labels=%v, want before with full config [X Y]", hit.BPPosition, hit.BPLabels)
	}
	if enq.count() != 0 {
		t.Errorf("blocked root must not be enqueued; got %d", enq.count())
	}
}

// ---------------------------------------------------------------------------
// Pause/orphan-sweep isolation
// ---------------------------------------------------------------------------

func TestDAGResume_DoesNotReleaseBPBlocks(t *testing.T) {
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
	if err := Resume(ctx, wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // let the resume event process

	rec, _, _ := wf.Store.GetStep(ctx, dag.ID(), "b")
	if rec.Held {
		t.Error("DAG resume must release the pause hold")
	}
	if rec.BPBefore == BPStateReleased {
		t.Error("DAG resume must NOT release the breakpoint")
	}
	if got := enq.count(); got != 1 {
		t.Errorf("b must stay blocked after DAG resume; got %d enqueues", got)
	}

	// The breakpoint resume still works afterwards.
	if n, err := ResumeBreakpoint(ctx, wf, dag.ID(), "X"); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	waitEnqueued(t, enq, 2, time.Second)
}

func TestResumeBreakpoint_DoesNotReleasePauseHolds(t *testing.T) {
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
	// Release the breakpoint while paused: persisted, but the pause fence and
	// the gated resume event keep b from being enqueued.
	if n, err := ResumeBreakpoint(ctx, wf, dag.ID(), "X"); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	time.Sleep(300 * time.Millisecond)

	rec, _, _ := wf.Store.GetStep(ctx, dag.ID(), "b")
	if !rec.Held {
		t.Error("breakpoint resume must NOT release the pause hold")
	}
	if rec.BPBefore != BPStateReleased {
		t.Error("breakpoint release must persist while paused")
	}
	meta, _, _ := wf.Store.GetMeta(ctx, dag.ID())
	if meta.Status != DAGStatusPaused {
		t.Errorf("meta must stay paused, got %s", meta.Status)
	}
	if got := enq.count(); got != 1 {
		t.Errorf("paused DAG must not enqueue; got %d", got)
	}

	// DAG-level resume now releases the hold and the already-released
	// breakpoint no longer gates: b is enqueued.
	if err := Resume(ctx, wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 2, time.Second)
}

func TestSweep_DoesNotEnqueueOrReleaseBPBlocked_EvenWhenOld(t *testing.T) {
	store := NewMemStore()
	enq := &captureEnq{}
	elector := &toggleElector{leader: false}
	wf := NewWorkflow(store, NewMemBus(), enq)
	wf.Elector = elector
	wf.SweepCheckInterval = 30 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Seed: a blocked step with an ancient advisory mark — far older than
	// heldOrphanAge. The orphan-hold repair must not touch it.
	dagID := "bp-sweep"
	_ = store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusRunning, ActiveBreakpoints: []string{"X"}}, 0)
	_, _ = store.PutStep(ctx, dagID, "a", StepRecord{
		DAGID: dagID, StepID: "a", FnName: "noopA", Status: StatusPending,
		ArgsJSON: json.RawMessage(`[1]`), BreakBefore: []string{"X"},
		BPBefore: BPStateBlocked, BPBlockedAt: time.Now().Add(-24 * time.Hour).UTC(),
	}, 0)

	startScheduler(t, wf, ctx)
	elector.set(true)
	time.Sleep(400 * time.Millisecond) // several sweep ticks

	if got := enq.count(); got != 0 {
		t.Errorf("sweep must not enqueue a BP-blocked step; got %d", got)
	}
	rec, _, _ := store.GetStep(ctx, dagID, "a")
	if rec.BPBefore != BPStateBlocked {
		t.Errorf("sweep must not release BP state; got %q", rec.BPBefore)
	}
}

func TestScheduler_MarkBlocked_StaleSnapshot_DoesNotClobberRelease(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	ctx := context.Background()

	dagID := "stale-mark"
	_ = store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusRunning, ActiveBreakpoints: []string{"X"}}, 0)
	_, _ = store.PutStep(ctx, dagID, "a", StepRecord{
		DAGID: dagID, StepID: "a", FnName: "noopA", Status: StatusPending,
		ArgsJSON: json.RawMessage(`[1]`), BreakBefore: []string{"X"},
	}, 0)

	// Snapshot the state BEFORE the release lands.
	sched := &Scheduler{wf: wf}
	stale, err := sched.loadState(ctx, dagID)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := ResumeBreakpoint(ctx, wf, dagID, "X"); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}

	// A racing scheduler with the stale snapshot tries to mark a as blocked.
	sched.markBlockedBreakpoints(ctx, stale)

	rec, _, _ := store.GetStep(ctx, dagID, "a")
	if rec.BPBefore != BPStateReleased {
		t.Errorf("stale marker must not clobber the release; got %q", rec.BPBefore)
	}
}
