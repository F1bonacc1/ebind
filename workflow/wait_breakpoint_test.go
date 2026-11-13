package workflow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/f1bonacc1/ebind/task"
)

// WaitForBreakpoint tests (MemStore/MemBus/captureEnq fakes).

type waitResult struct {
	info BreakpointInfo
	err  error
}

func startWait(ctx context.Context, wf *Workflow, dagID string, m BPMatcher) <-chan waitResult {
	out := make(chan waitResult, 1)
	go func() {
		info, err := WaitForBreakpoint(ctx, wf, dagID, m)
		out <- waitResult{info, err}
	}()
	return out
}

func awaitWait(t *testing.T, ch <-chan waitResult) waitResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("WaitForBreakpoint did not return")
	}
	return waitResult{}
}

func assertNotReturned(t *testing.T, ch <-chan waitResult) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("WaitForBreakpoint returned early: %+v, %v", r.info, r.err)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestWaitForBreakpoint_AlreadyStopped_ReturnsImmediately(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	_ = dag.StepOpts("a", noopA, []StepOption{BreakBefore("X")}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	_ = enq
	r := awaitWait(t, startWait(ctx, wf, dag.ID(), BeforeStep("a")))
	if r.err != nil || r.info.StepID != "a" || r.info.Position != BPPositionBefore || r.info.State != BPStateBlocked {
		t.Fatalf("got %+v, %v", r.info, r.err)
	}
}

func TestWaitForBreakpoint_WaiterFirst_ReturnsWhenStepStops(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	_ = dag.StepOpts("up-1", noopA, []StepOption{BreakBefore("X")}, a.Ref())
	_ = dag.StepOpts("b", noopA, []StepOption{BreakAfter("Y")}, a.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X", "Y")); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)

	before := startWait(ctx, wf, dag.ID(), BeforeStep("up-*"))
	desc, err := task.Describe(noopA)
	if err != nil {
		t.Fatal(err)
	}
	// Every step runs noopA; only b has an after breakpoint.
	after := startWait(ctx, wf, dag.ID(), AfterFn(desc.Name))
	// Wrong position for up-1: it never has an after breakpoint.
	wrongPos := startWait(ctx, wf, dag.ID(), AfterStep("up-1"))
	assertNotReturned(t, before)
	assertNotReturned(t, after)

	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	if r := awaitWait(t, before); r.err != nil || r.info.StepID != "up-1" {
		t.Fatalf("before wait = %+v, %v; want up-1", r.info, r.err)
	}
	waitEnqueued(t, enq, 2, time.Second) // b
	emulateHook(t, wf, dag.ID(), "b", []byte(`3`), nil)
	if r := awaitWait(t, after); r.err != nil || r.info.StepID != "b" || r.info.Position != BPPositionAfter {
		t.Fatalf("after wait = %+v, %v; want b/after", r.info, r.err)
	}
	assertNotReturned(t, wrongPos)
}

func TestWaitForBreakpoint_Cancel_ErrDAGTerminal(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	_ = dag.StepOpts("b", noopA, []StepOption{BreakBefore("X")}, a.Ref())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)
	w := startWait(ctx, wf, dag.ID(), BeforeStep("b"))
	assertNotReturned(t, w)
	if err := Cancel(ctx, wf, dag.ID()); err != nil {
		t.Fatal(err)
	}
	if r := awaitWait(t, w); !errors.Is(r.err, ErrDAGTerminal) {
		t.Fatalf("err = %v, want ErrDAGTerminal", r.err)
	}
	// Already terminal: immediate.
	if r := awaitWait(t, startWait(ctx, wf, dag.ID(), BeforeStep("b"))); !errors.Is(r.err, ErrDAGTerminal) {
		t.Fatalf("terminal DAG: err = %v, want ErrDAGTerminal", r.err)
	}
}

func TestWaitForBreakpoint_Timeout(t *testing.T) {
	wf, dag, _ := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	_ = dag.StepOpts("b", noopA, []StepOption{BreakBefore("X")}, a.Ref())
	if err := dag.Submit(context.Background(), wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := WaitForBreakpoint(ctx, wf, dag.ID(), BeforeStep("b")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestWaitForBreakpoint_NotFoundAndDeleted(t *testing.T) {
	wf, dag, _ := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	_ = dag.StepOpts("b", noopA, []StepOption{BreakBefore("X")}, a.Ref())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := WaitForBreakpoint(ctx, wf, "ghost", BeforeStep("b")); !errors.Is(err, ErrDAGNotFound) {
		t.Fatalf("unknown DAG: err = %v, want ErrDAGNotFound", err)
	}

	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	w := startWait(ctx, wf, dag.ID(), BeforeStep("b"))
	assertNotReturned(t, w)
	if err := wf.Store.DeleteMeta(ctx, dag.ID()); err != nil {
		t.Fatal(err)
	}
	if r := awaitWait(t, w); !errors.Is(r.err, ErrDAGNotFound) {
		t.Fatalf("deleted DAG: err = %v, want ErrDAGNotFound", r.err)
	}
}

func TestWaitForBreakpoint_InvalidMatcher(t *testing.T) {
	wf := NewWorkflow(NewMemStore(), NewMemBus(), &captureEnq{})
	if _, err := WaitForBreakpoint(context.Background(), wf, "d", BeforeStep("[")); !errors.Is(err, ErrInvalidMatcher) {
		t.Fatalf("err = %v, want ErrInvalidMatcher", err)
	}
}

// endingWatchStore closes the first WatchDAG channel right after the snapshot,
// the way a NATS watch ends when its node restarts.
type endingWatchStore struct {
	*MemStore
	calls atomic.Int32
}

func (s *endingWatchStore) WatchDAG(ctx context.Context, dagID string) (<-chan DAGChange, error) {
	if s.calls.Add(1) > 1 {
		return s.MemStore.WatchDAG(ctx, dagID)
	}
	inner, err := s.MemStore.WatchDAG(ctx, dagID)
	if err != nil {
		return nil, err
	}
	out := make(chan DAGChange)
	go func() {
		defer close(out)
		for c := range inner {
			out <- c
			if c.Synced {
				return
			}
		}
	}()
	return out, nil
}

func TestWaitForBreakpoint_RewatchesWhenWatchEnds(t *testing.T) {
	store := &endingWatchStore{MemStore: NewMemStore()}
	enq := &captureEnq{}
	wf := NewWorkflow(store, NewMemBus(), enq)
	dag := New()
	a := dag.Step("a", noopA, 1)
	_ = dag.StepOpts("b", noopA, []StepOption{BreakBefore("X")}, a.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf, WithActiveBreakpoints("X")); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)
	w := startWait(ctx, wf, dag.ID(), BeforeStep("b"))
	time.Sleep(50 * time.Millisecond) // first watch has ended by now
	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	if r := awaitWait(t, w); r.err != nil || r.info.StepID != "b" {
		t.Fatalf("got %+v, %v; want b", r.info, r.err)
	}
	if store.calls.Load() < 2 {
		t.Fatalf("WatchDAG calls = %d, want a re-watch", store.calls.Load())
	}
}
