package workflow

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/f1bonacc1/ebind/task"
)

// captureEnq records every enqueue for assertion.
type captureEnq struct {
	mu    sync.Mutex
	tasks []task.Task
}

func (c *captureEnq) Enqueue(_ context.Context, t task.Task) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tasks = append(c.tasks, t)
	return nil
}

func (c *captureEnq) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.tasks)
}

func (c *captureEnq) nthStepID(n int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n >= len(c.tasks) {
		return ""
	}
	return c.tasks[n].StepID
}

func defaultTestPolicy() task.RetryPolicy {
	return task.RetryPolicy{
		InitialInterval:    10 * time.Millisecond,
		BackoffCoefficient: 2.0,
		MaximumInterval:    100 * time.Millisecond,
		MaximumAttempts:    3,
	}
}

// setupDAG submits a DAG and returns the Workflow handle and the DAG.
func setupDAG(t *testing.T) (*Workflow, *DAG, *captureEnq) {
	t.Helper()
	enq := &captureEnq{}
	wf := NewWorkflow(NewMemStore(), NewMemBus(), enq)
	dag := New()
	return wf, dag, enq
}

// publishCompletion simulates the hook publishing a completion event.
func publishCompletion(t *testing.T, wf *Workflow, dagID, stepID string, status StepStatus, errKind string) {
	t.Helper()
	ev := Event{Kind: EventCompleted, DAGID: dagID, StepID: stepID, Status: status, ErrorKind: errKind}
	data, _ := MarshalEvent(ev)
	_ = wf.Bus.Publish(context.Background(), EventSubject(ev), data)
}

// emulateHook does what worker.StepHook would: update status in store + publish event.
// Lets us drive the scheduler without a real worker.
func emulateHook(t *testing.T, wf *Workflow, dagID, stepID string, result []byte, te *task.TaskError) {
	t.Helper()
	ctx := context.Background()
	rec, rev, err := wf.Store.GetStep(ctx, dagID, stepID)
	if err != nil {
		t.Fatalf("get step %q: %v", stepID, err)
	}
	if te == nil {
		rec.Status = StatusDone
		if err := wf.Store.PutStep(ctx, dagID, stepID, rec, rev); err != nil {
			t.Fatal(err)
		}
		_ = wf.Store.PutResult(ctx, dagID, stepID, result)
		publishCompletion(t, wf, dagID, stepID, StatusDone, "")
	} else {
		rec.Status = StatusFailed
		rec.ErrorKind = te.Kind
		if err := wf.Store.PutStep(ctx, dagID, stepID, rec, rev); err != nil {
			t.Fatal(err)
		}
		publishCompletion(t, wf, dagID, stepID, StatusFailed, te.Kind)
	}
}

// waitEnqueued polls the capture until the target count is reached (or timeout).
func waitEnqueued(t *testing.T, enq *captureEnq, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if enq.count() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("want %d enqueues, got %d", want, enq.count())
}

func TestScheduler_Linear_AllStepsEnqueued(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	b := dag.StepOpts("b", noopA, nil, a.Ref())
	_ = dag.Step("c", noopC, b.Ref(), "x")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = wf.RunScheduler(ctx) }()
	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second) // root a

	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	waitEnqueued(t, enq, 2, time.Second) // b

	emulateHook(t, wf, dag.ID(), "b", []byte(`3`), nil)
	waitEnqueued(t, enq, 3, time.Second) // c
}

func TestScheduler_FanOutFanIn(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	b := dag.Step("b", noopA, 2)
	_ = dag.Step("d", noopC, a.Ref(), "x")
	_ = dag.Step("e", noopC, b.Ref(), "y")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = wf.RunScheduler(ctx) }()
	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 2, time.Second) // a, b (parallel roots)

	emulateHook(t, wf, dag.ID(), "a", []byte(`10`), nil)
	waitEnqueued(t, enq, 3, time.Second) // d

	emulateHook(t, wf, dag.ID(), "b", []byte(`20`), nil)
	waitEnqueued(t, enq, 4, time.Second) // e
}

func TestScheduler_Mandatory_FailureCascades(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	b := dag.StepOpts("b", noopA, nil, a.Ref())
	_ = dag.Step("c", noopC, b.Ref(), "x")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = wf.RunScheduler(ctx) }()
	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second)

	emulateHook(t, wf, dag.ID(), "a", nil, &task.TaskError{Kind: "boom", Retryable: false})

	// Scheduler should cascade b → skipped → c → skipped.
	// Check eventually.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		bRec, _, _ := wf.Store.GetStep(ctx, dag.ID(), "b")
		cRec, _, _ := wf.Store.GetStep(ctx, dag.ID(), "c")
		if bRec.Status == StatusSkipped && cRec.Status == StatusSkipped {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("expected b and c to be cascade-skipped")
}

func TestScheduler_OptionalFailure_RefOrDefault_Runs(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.StepOpts("a", noopA, []StepOption{Optional()}, 1)
	_ = dag.Step("b", noopA, 7)
	// D depends on A via RefOrDefault(99).
	_ = dag.Step("d", noopA, a.RefOrDefault(99))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = wf.RunScheduler(ctx) }()
	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 2, time.Second) // roots a, b

	// Fail a (optional). d should still run with default 99.
	emulateHook(t, wf, dag.ID(), "a", nil, &task.TaskError{Kind: "flaky", Retryable: false})
	waitEnqueued(t, enq, 3, 2*time.Second) // d

	// Verify d was enqueued with 99 substituted.
	var found bool
	for i := 0; i < enq.count(); i++ {
		if enq.nthStepID(i) == "d" {
			found = true
			enq.mu.Lock()
			payload := enq.tasks[i].Payload
			enq.mu.Unlock()
			var args []json.RawMessage
			_ = json.Unmarshal(payload, &args)
			if len(args) != 1 || string(args[0]) != "99" {
				t.Errorf("d payload: %s, want [99]", payload)
			}
		}
	}
	if !found {
		t.Error("d was not enqueued")
	}
}

// toggleElector is a test elector whose IsLeader() state can be flipped at runtime.
type toggleElector struct {
	mu     sync.Mutex
	leader bool
}

func (t *toggleElector) IsLeader() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.leader
}

func (t *toggleElector) set(v bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.leader = v
}

func TestScheduler_Sweep_OnLeaderAcquire_EnqueuesStranded(t *testing.T) {
	// Manually construct DAG state with step A done + B stranded ready.
	store := NewMemStore()
	bus := NewMemBus()
	enq := &captureEnq{}
	elector := &toggleElector{leader: false}
	wf := NewWorkflow(store, bus, enq)
	wf.Elector = elector
	wf.SweepCheckInterval = 50 * time.Millisecond
	wf.SweepTimeout = 2 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Seed state: a done, b pending ready.
	dagID := "stranded-dag"
	_ = store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusRunning}, 0)
	_ = store.PutStep(ctx, dagID, "a", StepRecord{DAGID: dagID, StepID: "a", FnName: "noopA", Status: StatusDone, ArgsJSON: json.RawMessage(`[]`)}, 0)
	_ = store.PutResult(ctx, dagID, "a", json.RawMessage(`42`))
	refA, _ := json.Marshal(Ref{StepID: "a", Mode: RefModeRequired})
	bArgs, _ := json.Marshal([]json.RawMessage{refA})
	_ = store.PutStep(ctx, dagID, "b", StepRecord{DAGID: dagID, StepID: "b", FnName: "noopA", Status: StatusPending, Deps: []string{"a"}, ArgsJSON: bArgs}, 0)

	go func() { _ = wf.RunScheduler(ctx) }()

	// While non-leader: no sweep runs, no enqueues.
	time.Sleep(300 * time.Millisecond)
	if enq.count() != 0 {
		t.Fatalf("non-leader: expected 0 enqueues, got %d", enq.count())
	}

	// Flip to leader: within 2 ticks, sweep runs and enqueues b.
	elector.set(true)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if enq.count() >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if enq.count() != 1 {
		t.Fatalf("after leader acquire: want 1 enqueue (b), got %d", enq.count())
	}
	if enq.nthStepID(0) != "b" {
		t.Errorf("expected b, got %s", enq.nthStepID(0))
	}
}

func TestScheduler_Sweep_EdgeTriggered_NoDuplicateRuns(t *testing.T) {
	// While leader stays true, sweep runs only ONCE (on the initial edge).
	store := NewMemStore()
	enq := &captureEnq{}
	elector := &toggleElector{leader: false}
	wf := NewWorkflow(store, NewMemBus(), enq)
	wf.Elector = elector
	wf.SweepCheckInterval = 30 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dagID := "d"
	_ = store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusRunning}, 0)
	_ = store.PutStep(ctx, dagID, "a", StepRecord{DAGID: dagID, StepID: "a", FnName: "noopA", Status: StatusPending, ArgsJSON: json.RawMessage(`[]`)}, 0)

	go func() { _ = wf.RunScheduler(ctx) }()
	elector.set(true)
	time.Sleep(300 * time.Millisecond) // many ticks pass, all see leader=true

	// Sweep should have enqueued a exactly once despite the many ticks.
	if enq.count() != 1 {
		t.Errorf("want 1 enqueue (one sweep), got %d", enq.count())
	}
}

func TestScheduler_Sweep_TriggersAgainAfterReAcquire(t *testing.T) {
	// After losing leadership and regaining it, a new sweep runs.
	store := NewMemStore()
	enq := &captureEnq{}
	elector := &toggleElector{leader: false}
	wf := NewWorkflow(store, NewMemBus(), enq)
	wf.Elector = elector
	wf.SweepCheckInterval = 30 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dagID := "d"
	_ = store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusRunning}, 0)
	_ = store.PutStep(ctx, dagID, "a", StepRecord{DAGID: dagID, StepID: "a", FnName: "noopA", Status: StatusPending, ArgsJSON: json.RawMessage(`[]`)}, 0)

	go func() { _ = wf.RunScheduler(ctx) }()

	elector.set(true)
	time.Sleep(150 * time.Millisecond)
	first := enq.count()
	if first == 0 {
		t.Fatal("first acquire: no enqueue")
	}

	// Add another stranded step while non-leader.
	elector.set(false)
	time.Sleep(100 * time.Millisecond)
	_ = store.PutStep(ctx, dagID, "b", StepRecord{DAGID: dagID, StepID: "b", FnName: "noopA", Status: StatusPending, ArgsJSON: json.RawMessage(`[]`)}, 0)

	// Re-acquire: second sweep should pick up both a (if it failed to mark Running) and b.
	elector.set(true)
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if enq.count() > first {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if enq.count() <= first {
		t.Errorf("re-acquire: no new enqueue (first=%d, now=%d)", first, enq.count())
	}
}

func TestScheduler_Finalizes_DAG_Status(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	dag.Step("a", noopA, 1)
	dag.Step("b", noopA, 2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = wf.RunScheduler(ctx) }()
	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 2, time.Second)

	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)
	emulateHook(t, wf, dag.ID(), "b", []byte(`3`), nil)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m, _, _ := wf.Store.GetMeta(ctx, dag.ID())
		if m.Status == DAGStatusDone {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("DAG never finalized to done")
}
