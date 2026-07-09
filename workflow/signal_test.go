package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/f1bonacc1/ebind/task"
)

// ---------------------------------------------------------------------------
// Pure gate tests — no IO
// ---------------------------------------------------------------------------

func TestState_SignalBlocks_AllSemantics(t *testing.T) {
	s := makeState(StepRecord{StepID: "a", WaitSignals: []string{"x", "y"}})

	if got := s.ReadyToRun(); len(got) != 0 {
		t.Fatalf("nil signals map: want blocked, got ready %v", got)
	}
	s.Signals = map[string]SignalRecord{"x": {Name: "x"}}
	if got := s.ReadyToRun(); len(got) != 0 {
		t.Fatalf("1 of 2 delivered: want blocked, got ready %v", got)
	}
	s.Signals["y"] = SignalRecord{Name: "y"}
	if got := s.ReadyToRun(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("all delivered: want [a], got %v", got)
	}
}

func TestState_WaitingOnSignals_RequiresDepsSatisfied(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a"},
		StepRecord{StepID: "b", Deps: []string{"a"}, WaitSignals: []string{"go"},
			ArgsJSON: refArgs(Ref{StepID: "a", Mode: RefModeRequired})},
	)
	if got := s.WaitingOnSignals(); len(got) != 0 {
		t.Fatalf("dep not terminal: want no signal-waiters, got %v", got)
	}
	if _, err := s.MarkDone("a"); err != nil {
		t.Fatal(err)
	}
	if got := s.WaitingOnSignals(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("dep done: want [b], got %v", got)
	}
	// Once delivered, nothing waits.
	s.Signals = map[string]SignalRecord{"go": {Name: "go"}}
	if got := s.WaitingOnSignals(); len(got) != 0 {
		t.Fatalf("delivered: want no waiters, got %v", got)
	}
}

// The three fences — Held, breakpoint, signal — are independent: releasing one
// must not open another.
func TestState_SignalGate_ComposesWithHeldAndBreakpoint(t *testing.T) {
	s := makeState(StepRecord{
		StepID: "a", Held: true,
		BreakBefore: []string{"X"},
		WaitSignals: []string{"go"},
	})
	s.Meta.ActiveBreakpoints = []string{"X"}

	assertBlocked := func(msg string) {
		t.Helper()
		if got := s.ReadyToRun(); len(got) != 0 {
			t.Fatalf("%s: want blocked, got ready %v", msg, got)
		}
	}
	assertBlocked("all three fences")

	rec := s.Steps["a"]
	rec.Held = false
	s.Steps["a"] = rec
	assertBlocked("hold released, bp + signal remain")

	rec.BPBefore = BPStateReleased
	s.Steps["a"] = rec
	assertBlocked("bp released, signal remains")

	s.Signals = map[string]SignalRecord{"go": {Name: "go"}}
	if got := s.ReadyToRun(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("all fences released: want [a], got %v", got)
	}
}

func TestState_Terminal_StaysRunningWhileSignalWaiting(t *testing.T) {
	s := makeState(StepRecord{StepID: "a", WaitSignals: []string{"go"}})
	if status, terminal := s.Terminal(); terminal {
		t.Fatalf("signal-waiting DAG must not be terminal, got %s", status)
	}
}

// A failed required dep cascade-skips a signal-waiting dependent normally, but
// a SigRef arg alone must never trigger a cascade.
func TestState_Cascade_SignalRefDoesNotCascade(t *testing.T) {
	sigArg, _ := json.Marshal(SignalRef("go"))
	args, _ := json.Marshal([]json.RawMessage{sigArg})
	s := makeState(
		StepRecord{StepID: "a"},
		StepRecord{StepID: "b", WaitSignals: []string{"go"}, ArgsJSON: args},
	)
	_, skipped, err := s.MarkFailed("a", "handler", "boom")
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Fatalf("b has no dep on a: want no cascade, got %v", skipped)
	}
	if s.Steps["b"].Status != StatusPending {
		t.Fatalf("b: want pending, got %s", s.Steps["b"].Status)
	}
}

func TestState_Cascade_SkipsSignalWaitingDependentOfFailedStep(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a"},
		StepRecord{StepID: "b", Deps: []string{"a"}, WaitSignals: []string{"go"},
			ArgsJSON: refArgs(Ref{StepID: "a", Mode: RefModeRequired})},
	)
	_, skipped, err := s.MarkFailed("a", "handler", "boom")
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0] != "b" {
		t.Fatalf("want b cascade-skipped despite signal gate, got %v", skipped)
	}
}

// ---------------------------------------------------------------------------
// ResolveArgs with SigRefs
// ---------------------------------------------------------------------------

func TestResolveArgs_SignalRef_Substitutes(t *testing.T) {
	sigArg, _ := json.Marshal(SignalRef("approve"))
	lit, _ := json.Marshal(42)
	args := []json.RawMessage{sigArg, lit}

	signals := map[string]SignalRecord{
		"approve": {Name: "approve", Payload: json.RawMessage(`{"by":"eugene"}`)},
	}
	out, skip, err := ResolveArgs(args, nil, nil, signals)
	if err != nil || skip {
		t.Fatalf("err=%v skip=%v", err, skip)
	}
	if string(out[0]) != `{"by":"eugene"}` {
		t.Errorf("arg0: got %s", out[0])
	}
	if string(out[1]) != "42" {
		t.Errorf("arg1 literal: got %s", out[1])
	}
}

func TestResolveArgs_SignalRef_EmptyPayloadIsNull(t *testing.T) {
	sigArg, _ := json.Marshal(SignalRef("go"))
	out, _, err := ResolveArgs([]json.RawMessage{sigArg}, nil, nil,
		map[string]SignalRecord{"go": {Name: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(out[0]) != "null" {
		t.Errorf("want null, got %s", out[0])
	}
}

func TestResolveArgs_SignalRef_MissingIsError(t *testing.T) {
	sigArg, _ := json.Marshal(SignalRef("go"))
	_, _, err := ResolveArgs([]json.RawMessage{sigArg}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `signal "go" not delivered`) {
		t.Fatalf("want missing-signal error, got %v", err)
	}
}

func TestResolveArgs_MixedRefAndSignalRef(t *testing.T) {
	refArg, _ := json.Marshal(Ref{StepID: "a", Mode: RefModeRequired})
	sigArg, _ := json.Marshal(SignalRef("go"))
	args := []json.RawMessage{refArg, sigArg}

	out, skip, err := ResolveArgs(args,
		map[string]json.RawMessage{"a": json.RawMessage(`"res"`)},
		map[string]StepStatus{"a": StatusDone},
		map[string]SignalRecord{"go": {Name: "go", Payload: json.RawMessage(`1`)}})
	if err != nil || skip {
		t.Fatalf("err=%v skip=%v", err, skip)
	}
	if string(out[0]) != `"res"` || string(out[1]) != "1" {
		t.Errorf("got %s / %s", out[0], out[1])
	}
}

// ---------------------------------------------------------------------------
// Builder / Submit
// ---------------------------------------------------------------------------

func TestDAG_SigRef_IsNotAStepDep(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	// "deploy" references a signal named like a step — must not become a dep
	// nor trip cycle detection's unknown-step check.
	a := dag.Step("a", noopA, 1)
	dag.StepOpts("deploy", noopWithSignal, []StepOption{WaitForSignal("extra")},
		SignalRef("not-a-step"), a.Ref())

	if err := dag.Submit(context.Background(), wf); err != nil {
		t.Fatalf("submit: %v", err)
	}
	rec, _, err := wf.Store.GetStep(context.Background(), dag.ID(), "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rec.Deps, []string{"a"}) {
		t.Errorf("deps: want [a], got %v", rec.Deps)
	}
	// Union of option + SigRef names, deduped, order-preserved.
	if !reflect.DeepEqual(rec.WaitSignals, []string{"extra", "not-a-step"}) {
		t.Errorf("wait_signals: want [extra not-a-step], got %v", rec.WaitSignals)
	}
	_ = enq
}

func noopWithSignal(_ context.Context, payload any, x int) (int, error) { return x, nil }

func TestDAG_Submit_ValidatesSignals(t *testing.T) {
	cases := []struct {
		name string
		opts []StepOption
		args []any
	}{
		{"zero-name WaitForSignal", []StepOption{WaitForSignal()}, nil},
		{"empty WaitForSignal name", []StepOption{WaitForSignal("")}, nil},
		{"empty SignalRef name", nil, []any{SignalRef("")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf, dag, _ := setupDAG(t)
			dag.StepOpts("s", noopB, tc.opts, tc.args...)
			if err := dag.Submit(context.Background(), wf); err == nil {
				t.Fatal("want validation error, got nil")
			}
		})
	}
}

func TestDAG_Submit_SignalGatedRootLeftPending(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	dag.StepOpts("gate", noopB, []StepOption{WaitForSignal("go")})
	dag.Step("free", noopB)

	if err := dag.Submit(context.Background(), wf); err != nil {
		t.Fatal(err)
	}
	if enq.count() != 1 || enq.nthStepID(0) != "free" {
		t.Fatalf("want only free enqueued, got %d (%q)", enq.count(), enq.nthStepID(0))
	}
	rec, _, _ := wf.Store.GetStep(context.Background(), dag.ID(), "gate")
	if rec.Status != StatusPending {
		t.Fatalf("gate: want pending, got %s", rec.Status)
	}
}

func TestDAG_Submit_PreDeliveredSignalRootEnqueued(t *testing.T) {
	wf, dag2, enq := setupDAG(t)
	// Simulate the WithDAGID re-use / concurrent-Signal case: the record
	// exists before Submit's root loop runs.
	dagID := "presig-dag"
	err := wf.Store.PutSignal(context.Background(), dagID,
		SignalRecord{DAGID: dagID, Name: "go", Payload: json.RawMessage(`"now"`), DeliveredAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}

	dag := New(WithDAGID(dagID))
	dag.StepOpts("gate", noopWithSignal, nil, SignalRef("go"), 7)
	if err := dag.Submit(context.Background(), wf); err != nil {
		t.Fatal(err)
	}
	if enq.count() != 1 || enq.nthStepID(0) != "gate" {
		t.Fatalf("want gate enqueued at Submit, got %d", enq.count())
	}
	// The buffered payload must already be resolved into the envelope.
	var payload []json.RawMessage
	if err := json.Unmarshal(enqTask(enq, 0).Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if string(payload[0]) != `"now"` {
		t.Errorf("payload arg0: want \"now\", got %s", payload[0])
	}
	_ = dag2
}

func enqTask(c *captureEnq, n int) task.Task {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tasks[n]
}

// ---------------------------------------------------------------------------
// Signal() API semantics
// ---------------------------------------------------------------------------

func TestSignal_BeforeSubmit_ErrDAGNotFound(t *testing.T) {
	wf, _, _ := setupDAG(t)
	_, err := Signal(context.Background(), wf, "nope", "go", nil)
	if !errors.Is(err, ErrDAGNotFound) {
		t.Fatalf("want ErrDAGNotFound, got %v", err)
	}
}

func TestSignal_EmptyName_Error(t *testing.T) {
	wf, _, _ := setupDAG(t)
	if _, err := Signal(context.Background(), wf, "d", "", nil); err == nil {
		t.Fatal("want error for empty name")
	}
}

func TestSignal_Duplicate_FirstWins(t *testing.T) {
	wf, dag, _ := setupDAG(t)
	dag.StepOpts("gate", noopB, []StepOption{WaitForSignal("go")})
	if err := dag.Submit(context.Background(), wf); err != nil {
		t.Fatal(err)
	}
	tap := tapBus(t, wf)

	delivered, err := Signal(context.Background(), wf, dag.ID(), "go", map[string]string{"v": "1"})
	if err != nil || !delivered {
		t.Fatalf("first: delivered=%v err=%v", delivered, err)
	}
	delivered, err = Signal(context.Background(), wf, dag.ID(), "go", map[string]string{"v": "2"})
	if err != nil || delivered {
		t.Fatalf("second: want (false, nil), got (%v, %v)", delivered, err)
	}

	rec, err := wf.Store.GetSignal(context.Background(), dag.ID(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if string(rec.Payload) != `{"v":"1"}` {
		t.Errorf("first payload must win, got %s", rec.Payload)
	}
	// Both deliveries publish the wake-up event (crash-convergence contract).
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if countEvents(tap, EventSignal) >= 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("want 2 EventSignal publishes, got %d", countEvents(tap, EventSignal))
}

func countEvents(tp *eventTap, kind EventKind) int {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	n := 0
	for _, ev := range tp.events {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

func TestSignal_TerminalDAG_NoOpNoWrite(t *testing.T) {
	wf, dag, _ := setupDAG(t)
	dag.Step("a", noopB)
	if err := dag.Submit(context.Background(), wf); err != nil {
		t.Fatal(err)
	}
	if err := Cancel(context.Background(), wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	delivered, err := Signal(context.Background(), wf, dag.ID(), "go", nil)
	if err != nil || delivered {
		t.Fatalf("terminal DAG: want (false, nil), got (%v, %v)", delivered, err)
	}
	if _, err := wf.Store.GetSignal(context.Background(), dag.ID(), "go"); !errors.Is(err, ErrSignalNotFound) {
		t.Fatalf("terminal DAG must not persist the record, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Scheduler behavior (fakes: MemStore + MemBus + captureEnq)
// ---------------------------------------------------------------------------

func TestScheduler_SignalWakesWaitingStep_ExactlyOnce(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	dag.StepOpts("b", noopWithSignal, []StepOption{After(a)}, SignalRef("approve"), 2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)

	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, 2*time.Second) // root a
	emulateHook(t, wf, dag.ID(), "a", json.RawMessage(`1`), nil)

	// Dep satisfied, signal missing: b must stay pending.
	time.Sleep(200 * time.Millisecond)
	if enq.count() != 1 {
		t.Fatalf("pre-signal: want 1 enqueue, got %d", enq.count())
	}

	delivered, err := Signal(ctx, wf, dag.ID(), "approve", map[string]string{"by": "eugene"})
	if err != nil || !delivered {
		t.Fatalf("signal: delivered=%v err=%v", delivered, err)
	}
	waitEnqueued(t, enq, 2, 2*time.Second)
	if enq.nthStepID(1) != "b" {
		t.Fatalf("want b enqueued, got %q", enq.nthStepID(1))
	}
	// Payload resolved into the envelope.
	var args []json.RawMessage
	if err := json.Unmarshal(enqTask(enq, 1).Payload, &args); err != nil {
		t.Fatal(err)
	}
	if string(args[0]) != `{"by":"eugene"}` {
		t.Errorf("payload: got %s", args[0])
	}
	// Exactly once.
	time.Sleep(200 * time.Millisecond)
	if enq.count() != 2 {
		t.Fatalf("want exactly 2 enqueues, got %d", enq.count())
	}
}

func TestScheduler_TwoSignals_AllRequired(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	dag.StepOpts("gate", noopB, []StepOption{WaitForSignal("x", "y")})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)

	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	if _, err := Signal(ctx, wf, dag.ID(), "x", nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if enq.count() != 0 {
		t.Fatalf("1 of 2 signals: want 0 enqueues, got %d", enq.count())
	}
	if _, err := Signal(ctx, wf, dag.ID(), "y", nil); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, 2*time.Second)
}

func TestScheduler_DynamicStep_ConsumesBufferedSignal(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	dag.Step("a", noopA, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)

	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, 2*time.Second)

	// Signal first — buffered. The step arrives later (dynamic add path:
	// CAS-create record + EventStepAdded, same as ContextDAG.StepOpts).
	if _, err := Signal(ctx, wf, dag.ID(), "go", nil); err != nil {
		t.Fatal(err)
	}
	rec := StepRecord{
		DAGID: dag.ID(), StepID: "dyn", FnName: "noopB",
		ArgsJSON: json.RawMessage(`[]`), Status: StatusPending,
		WaitSignals: []string{"go"}, AddedAt: time.Now().UTC(),
	}
	if _, err := wf.Store.PutStep(ctx, dag.ID(), "dyn", rec, 0); err != nil {
		t.Fatal(err)
	}
	ev := Event{Kind: EventStepAdded, DAGID: dag.ID(), StepID: "dyn"}
	data, _ := MarshalEvent(ev)
	_ = wf.Bus.Publish(ctx, EventSubject(ev), data)

	waitEnqueued(t, enq, 2, 2*time.Second)
	if enq.nthStepID(1) != "dyn" {
		t.Fatalf("want dyn enqueued immediately (buffered signal), got %q", enq.nthStepID(1))
	}
}

func TestScheduler_SignalDuringPause_TakesEffectOnResume(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	dag.StepOpts("gate", noopB, []StepOption{WaitForSignal("go")})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)

	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	if err := Pause(ctx, wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	delivered, err := Signal(ctx, wf, dag.ID(), "go", nil)
	if err != nil || !delivered {
		t.Fatalf("signal during pause: delivered=%v err=%v", delivered, err)
	}
	time.Sleep(200 * time.Millisecond)
	if enq.count() != 0 {
		t.Fatalf("paused: want 0 enqueues, got %d", enq.count())
	}
	if err := Resume(ctx, wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, 2*time.Second)
	if enq.nthStepID(0) != "gate" {
		t.Fatalf("want gate after resume, got %q", enq.nthStepID(0))
	}
}

func TestScheduler_SignalDuringPausing_DroppedThenConverges(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	dag.Step("a", noopA, 1) // root: in-flight while pausing
	dag.StepOpts("gate", noopB, []StepOption{WaitForSignal("go")})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)

	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, 2*time.Second)
	if err := Pause(ctx, wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	meta, _, _ := wf.Store.GetMeta(ctx, dag.ID())
	if meta.Status != DAGStatusPausing {
		t.Fatalf("want pausing (a in-flight), got %s", meta.Status)
	}
	// EventSignal is dropped during the drain; the record is durable.
	if _, err := Signal(ctx, wf, dag.ID(), "go", nil); err != nil {
		t.Fatal(err)
	}
	emulateHook(t, wf, dag.ID(), "a", json.RawMessage(`1`), nil) // drain → paused
	waitDAGStatus(t, wf, dag.ID(), DAGStatusPaused)
	if enq.count() != 1 {
		t.Fatalf("paused: want no new enqueues, got %d", enq.count())
	}
	if err := Resume(ctx, wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 2, 2*time.Second)
	if enq.nthStepID(1) != "gate" {
		t.Fatalf("want gate after resume, got %q", enq.nthStepID(1))
	}
}

func waitDAGStatus(t *testing.T, wf *Workflow, dagID string, want DAGStatus) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		meta, _, err := wf.Store.GetMeta(context.Background(), dagID)
		if err == nil && meta.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	meta, _, _ := wf.Store.GetMeta(context.Background(), dagID)
	t.Fatalf("DAG status: want %s, got %s", want, meta.Status)
}

// Crash between the durable write and the event publish: the sweep must
// recover the waiting step from the record alone.
func TestScheduler_Sweep_RecoversUnpublishedSignal(t *testing.T) {
	store := NewMemStore()
	enq := &captureEnq{}
	elector := &toggleElector{leader: false}
	wf := NewWorkflow(store, NewMemBus(), enq)
	wf.Elector = elector
	wf.SweepCheckInterval = 50 * time.Millisecond
	wf.SweepTimeout = 2 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dagID := "sig-crash-dag"
	_ = store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusRunning}, 0)
	_, _ = store.PutStep(ctx, dagID, "gate", StepRecord{
		DAGID: dagID, StepID: "gate", FnName: "noopB", Status: StatusPending,
		WaitSignals: []string{"go"}, ArgsJSON: json.RawMessage(`[]`),
	}, 0)
	// The "crash": record written, no EventSignal ever published.
	_ = store.PutSignal(ctx, dagID, SignalRecord{DAGID: dagID, Name: "go", DeliveredAt: time.Now().UTC()})

	startScheduler(t, wf, ctx)
	time.Sleep(150 * time.Millisecond)
	if enq.count() != 0 {
		t.Fatalf("non-leader: want 0, got %d", enq.count())
	}
	elector.set(true)
	waitEnqueued(t, enq, 1, 3*time.Second)
	if enq.nthStepID(0) != "gate" {
		t.Fatalf("sweep should enqueue gate, got %q", enq.nthStepID(0))
	}
}

func TestScheduler_SignalAndBreakpoint_BothMustRelease(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	dag.StepOpts("gate", noopB, []StepOption{BreakBefore("X"), WaitForSignal("go")})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)

	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	if _, err := Signal(ctx, wf, dag.ID(), "go", nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if enq.count() != 0 {
		t.Fatalf("bp still armed: want 0 enqueues, got %d", enq.count())
	}
	if _, err := ResumeBreakpoint(ctx, wf, dag.ID(), "X"); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, 2*time.Second)
	time.Sleep(200 * time.Millisecond)
	if enq.count() != 1 {
		t.Fatalf("want exactly 1 enqueue, got %d", enq.count())
	}
}

func TestScheduler_Cancel_CancelsSignalWaiter_LateSignalNoOp(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	dag.StepOpts("gate", noopB, []StepOption{WaitForSignal("go")})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)

	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	if err := Cancel(ctx, wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := wf.Store.GetStep(ctx, dag.ID(), "gate")
	if rec.Status != StatusCanceled {
		t.Fatalf("gate: want canceled, got %s", rec.Status)
	}
	delivered, err := Signal(ctx, wf, dag.ID(), "go", nil)
	if err != nil || delivered {
		t.Fatalf("late signal: want (false, nil), got (%v, %v)", delivered, err)
	}
	time.Sleep(150 * time.Millisecond)
	if enq.count() != 0 {
		t.Fatalf("canceled DAG: want 0 enqueues, got %d", enq.count())
	}
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// Regression guard for the MarshalEvent hand-written literal: every JSON field
// on Event must survive a marshal→unmarshal round trip.
func TestMarshalEvent_RoundTrip_AllFields(t *testing.T) {
	in := Event{
		Kind: EventSignal, DAGID: "d", StepID: "s", Status: StatusFailed,
		ErrorKind: "handler", ErrorMessage: "boom",
		BPPosition: BPPositionBefore, BPLabels: []string{"X", "Y"},
		SignalName: "approve",
	}
	data, err := MarshalEvent(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := UnmarshalEvent(data)
	if err != nil {
		t.Fatal(err)
	}
	out.Ack, out.Nak = nil, nil
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip mismatch:\n in: %+v\nout: %+v", in, out)
	}
}

func TestEventSubject_SignalNameEncoded(t *testing.T) {
	ev := Event{Kind: EventSignal, DAGID: "dag-1", StepID: "dag-1", SignalName: "a.b *>tricky name"}
	subj := EventSubject(ev)
	tokens := strings.Split(subj, ".")
	if len(tokens) != 4 {
		t.Fatalf("want 4 tokens (watch filter DAG.<id>.*.* must match), got %d: %s", len(tokens), subj)
	}
	last := tokens[3]
	if strings.ContainsAny(last, " *>") || last == "" {
		t.Fatalf("token not subject-safe: %q", last)
	}
}

// ---------------------------------------------------------------------------
// ComputeSignals projection
// ---------------------------------------------------------------------------

func TestComputeSignals_States(t *testing.T) {
	meta := DAGMeta{ID: "d", Status: DAGStatusRunning}
	steps := []StepRecord{
		{StepID: "b", Status: StatusPending, WaitSignals: []string{"go", "other"}},
		{StepID: "c", Status: StatusDone, WaitSignals: []string{"go"}}, // done: never a waiter
	}
	sigs := []SignalRecord{
		{Name: "other", DeliveredAt: time.Now()},
		{Name: "extra", DeliveredAt: time.Now()}, // buffered, unreferenced
	}
	infos := ComputeSignals(meta, steps, sigs)
	if len(infos) != 3 {
		t.Fatalf("want 3 infos (extra, go, other), got %d: %+v", len(infos), infos)
	}
	byName := map[string]SignalInfo{}
	for _, si := range infos {
		byName[si.Name] = si
	}
	if !byName["extra"].Delivered || len(byName["extra"].Waiting) != 0 {
		t.Errorf("extra: %+v", byName["extra"])
	}
	if byName["go"].Delivered || !reflect.DeepEqual(byName["go"].Waiting, []string{"b"}) {
		t.Errorf("go: %+v", byName["go"])
	}
	if !byName["other"].Delivered || len(byName["other"].Waiting) != 0 {
		t.Errorf("other: %+v", byName["other"])
	}
}

func TestCountWaitingSteps_DistinctAcrossSignals(t *testing.T) {
	infos := []SignalInfo{
		{Name: "x", Waiting: []string{"a", "b"}},
		{Name: "y", Waiting: []string{"b", "c"}}, // b waits on both — counted once
		{Name: "z", Delivered: true},
	}
	if got := CountWaitingSteps(infos); got != 3 {
		t.Fatalf("want 3 distinct waiting steps, got %d", got)
	}
	if got := CountWaitingSteps(nil); got != 0 {
		t.Fatalf("nil infos: want 0, got %d", got)
	}
}

func TestComputeSignals_TerminalDAG_NoWaiters(t *testing.T) {
	meta := DAGMeta{ID: "d", Status: DAGStatusFailed}
	steps := []StepRecord{{StepID: "b", Status: StatusPending, WaitSignals: []string{"go"}}}
	infos := ComputeSignals(meta, steps, nil)
	if len(infos) != 1 || len(infos[0].Waiting) != 0 {
		t.Fatalf("terminal DAG must report no waiters: %+v", infos)
	}
}
