package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/f1bonacc1/ebind/task"
)

// ---------------------------------------------------------------------------
// Pure gate tests: matcher-armed breakpoints (P5)
// ---------------------------------------------------------------------------

func TestState_MatcherArmsUnlabeledStep(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a", Status: StatusDone},
		StepRecord{StepID: "node-1", Deps: []string{"a"}, ArgsJSON: refArgs(Ref{StepID: "a", Mode: RefModeRequired})},
		StepRecord{StepID: "other", Deps: []string{"a"}, ArgsJSON: refArgs(Ref{StepID: "a", Mode: RefModeRequired})},
	)
	s.Meta.ArmedMatchers = []BPMatcher{BeforeStep("node-*")}
	if got := strings.Join(s.ReadyToRun(), ","); got != "other" {
		t.Errorf("ready = %s, want other (node-1 stopped by matcher)", got)
	}
	if got := strings.Join(s.BlockedAtBefore(), ","); got != "node-1" {
		t.Errorf("blocked = %s, want node-1", got)
	}
}

func TestState_GateArming_LabelsMatchersBoth(t *testing.T) {
	step := StepRecord{StepID: "b", FnName: "pkg.B", BreakBefore: []string{"X"}}
	cases := []struct {
		name   string
		legacy []string
		ms     []BPMatcher
		armed  bool
	}{
		{"nothing armed", nil, nil, false},
		{"legacy label", []string{"X"}, nil, true},
		{"label matcher", nil, []BPMatcher{BeforeLabel("X")}, true},
		{"after label matcher does not arm before", nil, []BPMatcher{AfterLabel("X")}, false},
		{"fn matcher", nil, []BPMatcher{BeforeFn("pkg.*")}, true},
		{"step matcher wrong position", nil, []BPMatcher{AfterStep("b")}, false},
		{"both", []string{"X"}, []BPMatcher{BeforeStep("b")}, true},
	}
	for _, c := range cases {
		s := &DAGState{Meta: DAGMeta{ActiveBreakpoints: c.legacy, ArmedMatchers: c.ms}}
		if got := s.beforeBPBlocks(step); got != c.armed {
			t.Errorf("%s: beforeBPBlocks = %v, want %v", c.name, got, c.armed)
		}
		released := step
		released.BPBefore = BPStateReleased
		if s.beforeBPBlocks(released) {
			t.Errorf("%s: released must override arming", c.name)
		}
	}
}

func TestState_AfterMatcher_HoldsOnlyDoneParent(t *testing.T) {
	meta := DAGMeta{ArmedMatchers: []BPMatcher{AfterFn("pkg.Prepare")}}
	done := StepRecord{StepID: "p", FnName: "pkg.Prepare", Status: StatusDone}
	failed := StepRecord{StepID: "p", FnName: "pkg.Prepare", Status: StatusFailed}
	s := &DAGState{Meta: meta}
	if !s.afterBPHolds(done) {
		t.Error("done parent selected by AfterFn must hold its dependents")
	}
	if s.afterBPHolds(failed) {
		t.Error("failed parent must not gate (cascade path unchanged)")
	}
}

// A DAG written by v0.8.0 armed labels through active_breakpoints; it must
// keep stopping after the upgrade.
func TestState_LegacyActiveBreakpointsMetaStillGates(t *testing.T) {
	const v080 = `{"id":"d","status":"running","created_at":"2026-10-01T00:00:00Z","active_breakpoints":["X"]}`
	var meta DAGMeta
	if err := json.Unmarshal([]byte(v080), &meta); err != nil {
		t.Fatal(err)
	}
	s := &DAGState{Meta: meta}
	if !s.beforeBPBlocks(StepRecord{StepID: "b", BreakBefore: []string{"X"}}) {
		t.Error("legacy active_breakpoints must still arm the gate")
	}
	infos := ComputeBreakpoints(meta, []StepRecord{{StepID: "b", Status: StatusPending, BreakBefore: []string{"X"}}})
	if len(infos) != 1 || !infos[0].Armed || infos[0].State != BPStateBlocked {
		t.Errorf("legacy infos = %+v", infos)
	}
}

func TestDAGMeta_ArmedMatchersJSON(t *testing.T) {
	b, err := json.Marshal(DAGMeta{ID: "d", ArmedMatchers: []BPMatcher{BeforeStep("n-*")}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"armed_matchers":[{"position":"before","kind":"step","pattern":"n-*"}]`) {
		t.Errorf("json = %s", b)
	}
	if strings.Contains(string(b), "active_breakpoints") {
		t.Errorf("empty legacy field must be omitted: %s", b)
	}
}

func TestComputeBreakpoints_MatcherEntries(t *testing.T) {
	meta := DAGMeta{Status: DAGStatusRunning, ArmedMatchers: []BPMatcher{
		BeforeStep("node-*"), BeforeFn("pkg.Up"), AfterLabel("Y"),
	}}
	steps := []StepRecord{
		{StepID: "node-1", FnName: "pkg.Up", Status: StatusPending},
		{StepID: "plain", FnName: "pkg.Other", Status: StatusPending},
		{StepID: "decl", Status: StatusPending, BreakBefore: []string{"Z"}},
	}
	infos := ComputeBreakpoints(meta, steps)
	if len(infos) != 2 {
		t.Fatalf("infos = %+v, want node-1 and decl", infos)
	}
	byStep := map[string]BreakpointInfo{}
	for _, bp := range infos {
		byStep[bp.StepID] = bp
	}
	n := byStep["node-1"]
	if !n.Armed || n.State != BPStateBlocked || strings.Join(n.Matchers, ",") != "before:step:node-*,before:fn:pkg.Up" || len(n.Labels) != 0 {
		t.Errorf("node-1 = %+v", n)
	}
	d := byStep["decl"]
	if d.Armed || d.State != "" || len(d.Matchers) != 0 {
		t.Errorf("declared unarmed = %+v", d)
	}
	if got := FilterBreakpoints(infos, OnlyArmed()); len(got) != 1 || got[0].StepID != "node-1" {
		t.Errorf("OnlyArmed = %+v", got)
	}
	if got := FilterBreakpoints(infos, OnlyBlocked()); len(got) != 1 || got[0].StepID != "node-1" {
		t.Errorf("OnlyBlocked = %+v", got)
	}
}

func TestDebug_MatcherOnlyNoteShowsKey(t *testing.T) {
	notes := breakpointNotes([]BreakpointInfo{{StepID: "n", Position: BPPositionBefore,
		Matchers: []string{"before:step:n*"}, Armed: true, State: BPStateBlocked}})
	if notes["n"] != "bp:blocked[before:step:n*]" {
		t.Errorf("note = %q", notes["n"])
	}
}

// ---------------------------------------------------------------------------
// Submit + scheduler tests (fakes)
// ---------------------------------------------------------------------------

func TestSubmit_ArmedBreakpoints_ValidatesAndDedupes(t *testing.T) {
	wf := NewWorkflow(NewMemStore(), NewMemBus(), &captureEnq{})
	bad := New()
	bad.Step("a", noopA, 1)
	if err := bad.Submit(context.Background(), wf, WithArmedBreakpoints(BeforeStep("["))); !errors.Is(err, ErrInvalidMatcher) {
		t.Fatalf("invalid matcher: err = %v, want ErrInvalidMatcher", err)
	}

	dag := New()
	dag.Step("a", noopA, 1)
	if err := dag.Submit(context.Background(), wf,
		WithActiveBreakpoints("X"),
		WithArmedBreakpoints(BeforeLabel("X"), BeforeStep("a"), BeforeStep("a"))); err != nil {
		t.Fatal(err)
	}
	meta, _, _ := wf.Store.GetMeta(context.Background(), dag.ID())
	var keys []string
	for _, m := range meta.ArmedMatchers {
		keys = append(keys, m.Key())
	}
	if got := strings.Join(keys, ","); got != "before:label:X,after:label:X,before:step:a" {
		t.Errorf("ArmedMatchers = %s", got)
	}
}

func TestScheduler_BeforeStepMatcher_StopsUnlabeledRootAndDependent(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("node-0", noopA, 1)
	_ = dag.Step("node-1", noopA, a.Ref())
	_ = dag.Step("verify", noopA, 5)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, WithArmedBreakpoints(BeforeStep("node-*"))); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)
	if got := enq.nthStepID(0); got != "verify" {
		t.Fatalf("first enqueue = %s, want verify (node-0 stopped)", got)
	}
	waitStepBPState(t, wf, dag.ID(), "node-0", BPPositionBefore, BPStateBlocked)

	if ok, err := ReleaseStep(ctx, wf, dag.ID(), "node-0", BPPositionBefore); err != nil || !ok {
		t.Fatalf("ReleaseStep(node-0) = %v, %v", ok, err)
	}
	waitEnqueued(t, enq, 2, time.Second)
	emulateHook(t, wf, dag.ID(), "node-0", []byte(`2`), nil)
	// The matcher stays armed: node-1 stops when it arrives.
	waitStepBPState(t, wf, dag.ID(), "node-1", BPPositionBefore, BPStateBlocked)
	time.Sleep(100 * time.Millisecond)
	if got := enq.count(); got != 2 {
		t.Fatalf("node-1 must stop; enqueues = %d", got)
	}
}

func TestScheduler_AfterFnMatcher_HoldsDependents(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	p := dag.Step("prepare", noopA, 1)
	_ = dag.Step("upgrade", noopA, p.Ref())
	desc, err := task.Describe(noopA)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	// Every step runs noopA; prepare is the only one with a dependent.
	if err := dag.Submit(ctx, wf, WithArmedBreakpoints(AfterFn(desc.Name))); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)
	emulateHook(t, wf, dag.ID(), "prepare", []byte(`2`), nil)
	waitStepBPState(t, wf, dag.ID(), "prepare", BPPositionAfter, BPStateBlocked)
	time.Sleep(100 * time.Millisecond)
	if got := enq.count(); got != 1 {
		t.Fatalf("upgrade must be held; enqueues = %d", got)
	}
	if n, err := ReleaseMatching(ctx, wf, dag.ID(), AfterStep("prepare")); err != nil || n != 1 {
		t.Fatalf("ReleaseMatching = %d, %v; want 1", n, err)
	}
	waitEnqueued(t, enq, 2, time.Second)
}

func TestScheduler_DynamicUnlabeledStep_StoppedByMatcher(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	_ = dag.Step("build", noopA, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, WithArmedBreakpoints(BeforeStep("package-*"))); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)

	// build's handler adds an unlabeled dynamic step, then completes.
	cd := &ContextDAG{wf: wf, dagID: dag.ID(), parentStep: "build"}
	if _, err := cd.Step("package-linux", noopA, 3); err != nil {
		t.Fatal(err)
	}
	emulateHook(t, wf, dag.ID(), "build", []byte(`2`), nil)
	waitStepBPState(t, wf, dag.ID(), "package-linux", BPPositionBefore, BPStateBlocked)
	time.Sleep(100 * time.Millisecond)
	if got := enq.count(); got != 1 {
		t.Fatalf("dynamic step must stop; enqueues = %d", got)
	}
}

// ---------------------------------------------------------------------------
// ReleaseMatching (fakes)
// ---------------------------------------------------------------------------

func TestReleaseMatching_ReleasesSelectedOnly_StaysArmed(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	_ = dag.Step("node-1", noopA, a.Ref())
	_ = dag.Step("node-2", noopA, a.Ref())
	_ = dag.Step("db", noopA, a.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	tap := tapBus(t, wf)
	if err := dag.Submit(ctx, wf, WithArmedBreakpoints(BeforeStep("node-*"), BeforeStep("db"))); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)
	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	for _, id := range []string{"node-1", "node-2", "db"} {
		waitStepBPState(t, wf, dag.ID(), id, BPPositionBefore, BPStateBlocked)
	}

	// Position filter: nothing is stopped after.
	if n, err := ReleaseMatching(ctx, wf, dag.ID(), AfterStep("node-*")); err != nil || n != 0 {
		t.Fatalf("after position: n=%d err=%v, want 0", n, err)
	}
	n, err := ReleaseMatching(ctx, wf, dag.ID(), BeforeStep("node-*"))
	if err != nil || n != 2 {
		t.Fatalf("ReleaseMatching(node-*) = %d, %v; want 2", n, err)
	}
	waitEnqueued(t, enq, 3, time.Second)
	res := tap.waitForEvent(t, EventBPResumed, "node-1", 2*time.Second)
	if res.BPPosition != BPPositionBefore {
		t.Errorf("bp_resumed position = %s", res.BPPosition)
	}
	time.Sleep(100 * time.Millisecond)
	if got := enq.count(); got != 3 {
		t.Fatalf("db must stay stopped; enqueues = %d", got)
	}
	if n, err := ReleaseMatching(ctx, wf, dag.ID(), BeforeStep("node-*")); err != nil || n != 0 {
		t.Fatalf("repeat: n=%d err=%v, want 0", n, err)
	}

	// Still armed: a dynamic node-3 added later stops again.
	cd := &ContextDAG{wf: wf, dagID: dag.ID(), parentStep: "node-1"}
	if _, err := cd.Step("node-3", noopA, 4); err != nil {
		t.Fatal(err)
	}
	emulateHook(t, wf, dag.ID(), "node-1", []byte(`3`), nil)
	waitStepBPState(t, wf, dag.ID(), "node-3", BPPositionBefore, BPStateBlocked)
}

func TestReleaseMatching_Errors(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	ctx := context.Background()
	_ = store.PutMeta(ctx, "done", DAGMeta{ID: "done", Status: DAGStatusDone}, 0)
	if _, err := ReleaseMatching(ctx, wf, "ghost", BeforeStep("x")); !errors.Is(err, ErrDAGNotFound) {
		t.Errorf("unknown DAG: %v", err)
	}
	if n, err := ReleaseMatching(ctx, wf, "done", BeforeStep("x")); err != nil || n != 0 {
		t.Errorf("terminal: n=%d err=%v", n, err)
	}
	if _, err := ReleaseMatching(ctx, wf, "done", BeforeStep("[")); !errors.Is(err, ErrInvalidMatcher) {
		t.Errorf("invalid: %v", err)
	}
}
