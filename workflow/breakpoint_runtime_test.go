package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/f1bonacc1/ebind/task"
)

// ---------------------------------------------------------------------------
// Pure gate tests: BPFence tokens (P7)
// ---------------------------------------------------------------------------

func TestState_BeforeFenceToken_BlocksWithoutMetaMatcher(t *testing.T) {
	// The evaluator's meta predates the runtime arm: only the token knows.
	s := makeState(
		StepRecord{StepID: "a", Status: StatusDone},
		StepRecord{StepID: "b", Deps: []string{"a"}, BPFence: []string{"b:before:step:b"},
			ArgsJSON: refArgs(Ref{StepID: "a", Mode: RefModeRequired})},
	)
	if got := s.ReadyToRun(); len(got) != 0 {
		t.Fatalf("ready = %v, want none (b fenced)", got)
	}
	if got := strings.Join(s.BlockedAtBefore(), ","); got != "b" {
		t.Errorf("blocked = %s, want b", got)
	}
	rel := s.Steps["b"]
	rel.BPBefore = BPStateReleased
	s.Steps["b"] = rel
	if got := strings.Join(s.ReadyToRun(), ","); got != "b" {
		t.Errorf("released must override the token: ready = %s", got)
	}
}

func TestState_AfterFenceToken_HoldsOnlyThatDependent(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "x", Status: StatusDone},
		StepRecord{StepID: "held", Deps: []string{"x"}, BPFence: []string{"a:x"}},
		StepRecord{StepID: "free", Deps: []string{"x"}},
	)
	if got := strings.Join(s.ReadyToRun(), ","); got != "free" {
		t.Fatalf("ready = %s, want free", got)
	}
	if got := strings.Join(s.HoldingAtAfter(), ","); got != "x" {
		t.Errorf("holding = %s, want x", got)
	}
	x := s.Steps["x"]
	x.BPAfter = BPStateReleased
	s.Steps["x"] = x
	ready := s.ReadyToRun()
	slices.Sort(ready)
	if got := strings.Join(ready, ","); got != "free,held" {
		t.Errorf("after release ready = %s", got)
	}
	if len(s.HoldingAtAfter()) != 0 {
		t.Error("released x must not hold")
	}
}

func TestState_AfterFenceToken_FailedParentDoesNotGate(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "x", Status: StatusFailed},
		StepRecord{StepID: "d", OptionalDeps: []string{"x"}, BPFence: []string{"a:x"}},
	)
	if got := strings.Join(s.ReadyToRun(), ","); got != "d" {
		t.Errorf("ready = %s, want d (after gates only gate done parents)", got)
	}
}

func TestComputeBreakpoints_FenceTokens(t *testing.T) {
	meta := DAGMeta{Status: DAGStatusRunning} // stale: no matchers
	steps := []StepRecord{
		{StepID: "x", Status: StatusDone},
		{StepID: "y", Status: StatusPending, Deps: []string{"x"}, BPFence: []string{"a:x"}},
		{StepID: "z", Status: StatusPending, BPFence: []string{"b:before:fn:pkg.Z"}},
	}
	infos := ComputeBreakpoints(meta, steps)
	byKey := map[string]BreakpointInfo{}
	for _, bp := range infos {
		byKey[bp.StepID+"@"+string(bp.Position)] = bp
	}
	x := byKey["x@after"]
	if !x.Armed || x.State != BPStateBlocked || strings.Join(x.Holding, ",") != "y" {
		t.Errorf("x@after = %+v", x)
	}
	z := byKey["z@before"]
	if !z.Armed || z.State != BPStateBlocked || strings.Join(z.Matchers, ",") != "before:fn:pkg.Z" {
		t.Errorf("z@before = %+v", z)
	}
	if _, ok := byKey["y@before"]; ok {
		t.Error("an a: token must not create a before entry on the dependent")
	}
}

func TestStepRecord_BPFenceJSON(t *testing.T) {
	b, err := json.Marshal(StepRecord{StepID: "s", BPFence: []string{"a:x"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"bp_fence":["a:x"]`) {
		t.Errorf("json = %s", b)
	}
	b, _ = json.Marshal(StepRecord{StepID: "s"})
	if strings.Contains(string(b), "bp_fence") {
		t.Errorf("empty fence must be omitted: %s", b)
	}
}

// ---------------------------------------------------------------------------
// ArmBreakpoints / DisarmBreakpoints (fakes)
// ---------------------------------------------------------------------------

func stepRec(t *testing.T, wf *Workflow, dagID, stepID string) StepRecord {
	t.Helper()
	rec, _, err := wf.Store.GetStep(context.Background(), dagID, stepID)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// assertEnqueues waits briefly and checks the enqueue count did not move.
func assertEnqueues(t *testing.T, enq *captureEnq, want int) {
	t.Helper()
	time.Sleep(100 * time.Millisecond)
	if got := enq.count(); got != want {
		t.Fatalf("enqueues = %d, want %d", got, want)
	}
}

func TestArm_RunningDAG_StopsLaterStep_DisarmLetsItRun(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	b := dag.Step("b", noopA, a.Ref())
	_ = dag.Step("c", noopA, b.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)

	rep, err := ArmBreakpoints(ctx, wf, dag.ID(), BeforeStep("b"), BeforeStep("a"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rep.Added, ","); got != "before:step:b,before:step:a" {
		t.Errorf("Added = %s", got)
	}
	if got := strings.Join(rep.Stopped, ","); got != "b@before" {
		t.Errorf("Stopped = %s, want b@before", got)
	}
	if got := strings.Join(rep.Missed, ","); got != "a@before" {
		t.Errorf("Missed = %s, want a@before (already running)", got)
	}
	if got := stepRec(t, wf, dag.ID(), "b").BPFence; strings.Join(got, ",") != "b:before:step:b" {
		t.Errorf("b fence = %v", got)
	}

	// Idempotent repeat.
	rep2, err := ArmBreakpoints(ctx, wf, dag.ID(), BeforeStep("b"))
	if err != nil || len(rep2.Added) != 0 || strings.Join(rep2.Stopped, ",") != "b@before" {
		t.Errorf("repeat = %+v, %v", rep2, err)
	}

	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	waitStepBPState(t, wf, dag.ID(), "b", BPPositionBefore, BPStateBlocked)
	assertEnqueues(t, enq, 1)

	n, err := DisarmBreakpoints(ctx, wf, dag.ID(), BeforeStep("b"))
	if err != nil || n != 1 {
		t.Fatalf("Disarm = %d, %v; want 1", n, err)
	}
	waitEnqueued(t, enq, 2, time.Second)
	if got := enq.nthStepID(1); got != "b" {
		t.Errorf("second enqueue = %s, want b", got)
	}
	rec := stepRec(t, wf, dag.ID(), "b")
	if len(rec.BPFence) != 0 {
		t.Errorf("disarm must drop the token: %v", rec.BPFence)
	}
	if rec.BPBefore == BPStateReleased {
		t.Error("disarm must never write released (F4)")
	}
	meta, _, _ := wf.Store.GetMeta(ctx, dag.ID())
	if len(meta.ArmedMatchers) != 1 || meta.ArmedMatchers[0].Key() != "before:step:a" {
		t.Errorf("ArmedMatchers = %+v", meta.ArmedMatchers)
	}
}

// F4: disarm does not consume the breakpoint the way a release does.
func TestArm_DisarmThenRearm_StopsAgain(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	_ = dag.Step("b", noopA, a.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)

	for i := 0; i < 2; i++ {
		if _, err := ArmBreakpoints(ctx, wf, dag.ID(), BeforeStep("b")); err != nil {
			t.Fatal(err)
		}
		if n, err := DisarmBreakpoints(ctx, wf, dag.ID(), BeforeStep("b")); err != nil || n != 0 {
			t.Fatalf("disarm before b arrived = %d, %v; want 0", n, err)
		}
	}
	if _, err := ArmBreakpoints(ctx, wf, dag.ID(), BeforeStep("b")); err != nil {
		t.Fatal(err)
	}
	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	waitStepBPState(t, wf, dag.ID(), "b", BPPositionBefore, BPStateBlocked)
	assertEnqueues(t, enq, 1)
}

func TestArm_DynamicStepAddedLater_Stops(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	_ = dag.Step("build", noopA, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)
	rep, err := ArmBreakpoints(ctx, wf, dag.ID(), BeforeStep("package-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Stopped)+len(rep.Missed) != 0 {
		t.Errorf("nothing exists to stop yet: %+v", rep)
	}

	cd := &ContextDAG{wf: wf, dagID: dag.ID(), parentStep: "build"}
	if _, err := cd.Step("package-linux", noopA, 3); err != nil {
		t.Fatal(err)
	}
	emulateHook(t, wf, dag.ID(), "build", []byte(`2`), nil)
	waitStepBPState(t, wf, dag.ID(), "package-linux", BPPositionBefore, BPStateBlocked)
	assertEnqueues(t, enq, 1)
}

func TestArm_RunningStep_MissedAndUnaffected(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	_ = dag.Step("b", noopA, a.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)
	rep, err := ArmBreakpoints(ctx, wf, dag.ID(), BeforeStep("a"))
	if err != nil || strings.Join(rep.Missed, ",") != "a@before" || len(rep.Stopped) != 0 {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	waitEnqueued(t, enq, 2, time.Second)
}

func TestArm_AfterStepOnDoneStep_HoldsPendingDependents(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	x := dag.Step("x", noopA, 1)
	_ = dag.StepOpts("b", noopA, []StepOption{After(x)}, a.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 2, time.Second)
	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	waitStepStatus(t, wf, dag.ID(), "a", StatusDone)

	rep, err := ArmBreakpoints(ctx, wf, dag.ID(), AfterStep("a"), AfterStep("x"))
	if err != nil {
		t.Fatal(err)
	}
	// a is done: its pending dependent b is fenced. x is still running:
	// the gate holds b when x completes.
	if got := strings.Join(rep.Stopped, ","); got != "a@after,x@after" {
		t.Errorf("Stopped = %s", got)
	}
	if got := strings.Join(stepRec(t, wf, dag.ID(), "b").BPFence, ","); got != "a:a" {
		t.Errorf("b fence = %s, want a:a", got)
	}
	emulateHook(t, wf, dag.ID(), "x", []byte(`2`), nil)
	waitStepBPState(t, wf, dag.ID(), "x", BPPositionAfter, BPStateBlocked)
	assertEnqueues(t, enq, 2)

	if ok, err := ReleaseStep(ctx, wf, dag.ID(), "a", BPPositionAfter); err != nil || !ok {
		t.Fatalf("ReleaseStep(a, after) = %v, %v", ok, err)
	}
	if got := stepRec(t, wf, dag.ID(), "b").BPFence; len(got) != 0 {
		t.Errorf("release must clear a:a, fence = %v", got)
	}
	assertEnqueues(t, enq, 2) // x still holds b
	if ok, err := ReleaseStep(ctx, wf, dag.ID(), "x", BPPositionAfter); err != nil || !ok {
		t.Fatalf("ReleaseStep(x, after) = %v, %v", ok, err)
	}
	waitEnqueued(t, enq, 3, time.Second)
}

func TestArm_AfterStep_DependentAlreadyStarted_Missed(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	_ = dag.Step("b", noopA, a.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)
	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	waitEnqueued(t, enq, 2, time.Second)
	rep, err := ArmBreakpoints(ctx, wf, dag.ID(), AfterStep("a"))
	if err != nil || strings.Join(rep.Missed, ",") != "a@after" || len(rep.Stopped) != 0 {
		t.Fatalf("report = %+v, %v", rep, err)
	}
}

func TestReleaseStep_Before_ClearsFenceTokens(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	_ = dag.Step("b", noopA, a.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)
	if _, err := ArmBreakpoints(ctx, wf, dag.ID(), BeforeStep("b"), BeforeFn("*")); err != nil {
		t.Fatal(err)
	}
	if got := len(stepRec(t, wf, dag.ID(), "b").BPFence); got != 2 {
		t.Fatalf("b tokens = %d, want 2", got)
	}
	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	waitStepBPState(t, wf, dag.ID(), "b", BPPositionBefore, BPStateBlocked)
	if ok, err := ReleaseStep(ctx, wf, dag.ID(), "b", BPPositionBefore); err != nil || !ok {
		t.Fatalf("ReleaseStep = %v, %v", ok, err)
	}
	waitEnqueued(t, enq, 2, time.Second)
	if got := stepRec(t, wf, dag.ID(), "b").BPFence; len(got) != 0 {
		t.Errorf("fence after release = %v", got)
	}
}

// Arm on a paused DAG composes with Held: Resume releases the hold, the
// breakpoint still stops the step; arming never touches Held.
func TestArm_PausedDAG_ComposesWithHeld(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	_ = dag.Step("b", noopA, a.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, StartPaused()); err != nil {
		t.Fatal(err)
	}
	rep, err := ArmBreakpoints(ctx, wf, dag.ID(), BeforeStep("a"))
	if err != nil || strings.Join(rep.Stopped, ",") != "a@before" {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	if rec := stepRec(t, wf, dag.ID(), "a"); !rec.Held {
		t.Fatal("arm must not touch Held")
	}
	if err := Resume(ctx, wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	waitStepBPState(t, wf, dag.ID(), "a", BPPositionBefore, BPStateBlocked)
	assertEnqueues(t, enq, 0)
	if n, err := DisarmBreakpoints(ctx, wf, dag.ID(), BeforeStep("a")); err != nil || n != 1 {
		t.Fatalf("Disarm = %d, %v", n, err)
	}
	waitEnqueued(t, enq, 1, time.Second)
}

func TestArmDisarm_Errors(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	ctx := context.Background()
	_ = store.PutMeta(ctx, "done", DAGMeta{ID: "done", Status: DAGStatusDone}, 0)

	if _, err := ArmBreakpoints(ctx, wf, "ghost", BeforeStep("x")); !errors.Is(err, ErrDAGNotFound) {
		t.Errorf("arm unknown: %v", err)
	}
	if _, err := DisarmBreakpoints(ctx, wf, "ghost", BeforeStep("x")); !errors.Is(err, ErrDAGNotFound) {
		t.Errorf("disarm unknown: %v", err)
	}
	if rep, err := ArmBreakpoints(ctx, wf, "done", BeforeStep("x")); err != nil || len(rep.Added) != 0 {
		t.Errorf("arm terminal: %+v, %v", rep, err)
	}
	if meta, _, _ := store.GetMeta(ctx, "done"); len(meta.ArmedMatchers) != 0 {
		t.Error("arm on a terminal DAG must not write")
	}
	if n, err := DisarmBreakpoints(ctx, wf, "done", BeforeStep("x")); err != nil || n != 0 {
		t.Errorf("disarm terminal: %d, %v", n, err)
	}
	for _, ms := range [][]BPMatcher{nil, {BeforeStep("[")}, {BeforeStep("ok"), {Kind: "tag"}}} {
		if _, err := ArmBreakpoints(ctx, wf, "done", ms...); !errors.Is(err, ErrInvalidMatcher) {
			t.Errorf("arm %v: %v, want ErrInvalidMatcher", ms, err)
		}
		if _, err := DisarmBreakpoints(ctx, wf, "done", ms...); !errors.Is(err, ErrInvalidMatcher) {
			t.Errorf("disarm %v: %v, want ErrInvalidMatcher", ms, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Stale meta: the scheduler's direct-get meta lags behind the leader
// ---------------------------------------------------------------------------

// staleMetaStore answers GetMeta from a frozen snapshot (a lagging replica);
// GetMetaLatest still reads the real record (the stream leader).
type staleMetaStore struct {
	StateStore
	mu     sync.Mutex
	frozen map[string]DAGMeta
}

func (s *staleMetaStore) freeze(ctx context.Context, dagID string) {
	meta, _, err := s.StateStore.GetMeta(ctx, dagID)
	if err != nil {
		panic(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.frozen == nil {
		s.frozen = map[string]DAGMeta{}
	}
	s.frozen[dagID] = meta
}

func (s *staleMetaStore) GetMeta(ctx context.Context, dagID string) (DAGMeta, uint64, error) {
	meta, rev, err := s.StateStore.GetMeta(ctx, dagID)
	if err != nil {
		return meta, rev, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.frozen[dagID]; ok {
		// Keep the live status so pause/finalize paths behave; only the
		// armed set lags, which is what this store exercises.
		old.Status = meta.Status
		return old, rev, nil
	}
	return meta, rev, nil
}

func TestDisarm_StaleMetaScheduler_DispatchesOnResumedEvent(t *testing.T) {
	store := &staleMetaStore{StateStore: NewMemStore()}
	enq := &captureEnq{}
	wf := NewWorkflow(store, NewMemBus(), enq)
	dag := New()
	a := dag.Step("a", noopA, 1)
	_ = dag.Step("b", noopA, a.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, WithArmedBreakpoints(BeforeStep("b"))); err != nil {
		t.Fatal(err)
	}
	store.freeze(ctx, dag.ID()) // the scheduler keeps seeing b armed
	waitEnqueued(t, enq, 1, time.Second)
	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	waitStepBPState(t, wf, dag.ID(), "b", BPPositionBefore, BPStateBlocked)

	if n, err := DisarmBreakpoints(ctx, wf, dag.ID(), BeforeStep("b")); err != nil || n != 1 {
		t.Fatalf("Disarm = %d, %v", n, err)
	}
	// No sweep runs here: only the EventResumed the disarm published, and
	// onResumed's leader read, can dispatch b.
	waitEnqueued(t, enq, 2, time.Second)
}

func TestArm_StaleMetaScheduler_AfterStepStillHolds(t *testing.T) {
	store := &staleMetaStore{StateStore: NewMemStore()}
	enq := &captureEnq{}
	wf := NewWorkflow(store, NewMemBus(), enq)
	dag := New()
	a := dag.Step("a", noopA, 1)
	_ = dag.Step("b", noopA, a.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	store.freeze(ctx, dag.ID()) // the scheduler never sees the arm in GetMeta
	waitEnqueued(t, enq, 1, time.Second)
	rep, err := ArmBreakpoints(ctx, wf, dag.ID(), AfterStep("a"))
	if err != nil || strings.Join(rep.Stopped, ",") != "a@after" {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	// a was running at arm time: no token anywhere. Only enqueueReady's
	// leader read stops b.
	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	waitStepBPState(t, wf, dag.ID(), "a", BPPositionAfter, BPStateBlocked)
	assertEnqueues(t, enq, 1)
}

// ---------------------------------------------------------------------------
// Sweep repair
// ---------------------------------------------------------------------------

func TestSweep_RepairsOrphanedFenceTokens(t *testing.T) {
	store := NewMemStore()
	enq := &captureEnq{}
	wf := NewWorkflow(store, NewMemBus(), enq)
	ctx := context.Background()
	const id = "orphan"
	args := json.RawMessage(`[1]`)
	if err := store.PutMeta(ctx, id, DAGMeta{ID: id, Status: DAGStatusRunning,
		ArmedMatchers: []BPMatcher{BeforeStep("kept")}}, 0); err != nil {
		t.Fatal(err)
	}
	fn := mustFnName(t)
	put := func(rec StepRecord) {
		rec.DAGID, rec.FnName, rec.ArgsJSON = id, fn, args
		if _, err := store.PutStep(ctx, id, rec.StepID, rec, 0); err != nil {
			t.Fatal(err)
		}
	}
	put(StepRecord{StepID: "up", Status: StatusDone, BPAfter: BPStateReleased})
	put(StepRecord{StepID: "disarmed", Status: StatusPending, BPFence: []string{"b:before:step:disarmed"}})
	put(StepRecord{StepID: "released-up", Status: StatusPending, Deps: []string{"up"}, BPFence: []string{"a:up"}})
	put(StepRecord{StepID: "kept", Status: StatusPending, BPFence: []string{"b:before:step:kept"}})
	_ = store.PutResult(ctx, id, "up", []byte(`1`))

	s := &Scheduler{wf: wf}
	if err := s.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := stepRec(t, wf, id, "disarmed").BPFence; len(got) != 0 {
		t.Errorf("token of a disarmed matcher must be pruned: %v", got)
	}
	if got := stepRec(t, wf, id, "released-up").BPFence; len(got) != 0 {
		t.Errorf("token of a released upstream must be pruned: %v", got)
	}
	if got := stepRec(t, wf, id, "kept").BPFence; strings.Join(got, ",") != "b:before:step:kept" {
		t.Errorf("token of an armed matcher must stay: %v", got)
	}
	var dispatched []string
	for i := 0; i < enq.count(); i++ {
		dispatched = append(dispatched, enq.nthStepID(i))
	}
	slices.Sort(dispatched)
	if got := strings.Join(dispatched, ","); got != "disarmed,released-up" {
		t.Errorf("dispatched = %s, want disarmed,released-up", got)
	}
}

// A persistStatus caller with a stale view must not dispatch a fenced step.
func TestPersistStatus_RefusesLiveFence_DropsVoidTokens(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	ctx := context.Background()
	const id = "guard"
	_ = store.PutMeta(ctx, id, DAGMeta{ID: id, Status: DAGStatusRunning}, 0)
	_, _ = store.PutStep(ctx, id, "up", StepRecord{StepID: "up", Status: StatusDone}, 0)
	_, _ = store.PutStep(ctx, id, "s", StepRecord{StepID: "s", Status: StatusPending, Deps: []string{"up"},
		BPFence: []string{"a:up"}}, 0)
	s := &Scheduler{wf: wf}
	if err := s.persistStatus(ctx, id, "s", StatusRunning); !errors.Is(err, errStepNotEnqueueable) {
		t.Fatalf("live a: token: err = %v, want errStepNotEnqueueable", err)
	}
	up, rev, _ := store.GetStep(ctx, id, "up")
	up.BPAfter = BPStateReleased
	_, _ = store.PutStep(ctx, id, "up", up, rev)
	if err := s.persistStatus(ctx, id, "s", StatusRunning); err != nil {
		t.Fatalf("void token: %v", err)
	}
	if rec := stepRec(t, wf, id, "s"); rec.Status != StatusRunning || len(rec.BPFence) != 0 {
		t.Errorf("rec = %+v", rec)
	}
}

func mustFnName(t *testing.T) string {
	t.Helper()
	desc, err := task.Describe(noopA)
	if err != nil {
		t.Fatal(err)
	}
	return desc.Name
}

func waitStepStatus(t *testing.T, wf *Workflow, dagID, stepID string, want StepStatus) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec, _, err := wf.Store.GetStep(context.Background(), dagID, stepID); err == nil && rec.Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("step %s never reached %s", stepID, want)
}
