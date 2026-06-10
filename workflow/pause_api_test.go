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

// TestPause_RunningDAG_NoInFlight verifies that pausing a running DAG with
// no in-flight steps transitions directly to paused and sets PausedAt.
func TestPause_RunningDAG_NoInFlight(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	ctx := context.Background()
	dagID := "test-dag"

	// Store a running DAG with one pending step (no in-flight)
	if err := store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusRunning}, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.PutStep(ctx, dagID, "a", StepRecord{
		DAGID: dagID, StepID: "a", Status: StatusPending, ArgsJSON: json.RawMessage(`[]`),
	}, 0); err != nil {
		t.Fatal(err)
	}

	if err := Pause(ctx, wf, dagID); err != nil {
		t.Fatalf("Pause() error: %v", err)
	}

	meta, _, err := store.GetMeta(ctx, dagID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Status != DAGStatusPaused {
		t.Errorf("status = %s, want %s", meta.Status, DAGStatusPaused)
	}
	if meta.PausedAt.IsZero() {
		t.Error("PausedAt is zero, expected non-zero timestamp")
	}
}

// TestPause_RunningDAG_WithInFlight verifies that pausing a running DAG with
// an in-flight step transitions to pausing (not directly to paused).
func TestPause_RunningDAG_WithInFlight(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	ctx := context.Background()
	dagID := "test-dag"

	// Store a running DAG with one running step (in-flight)
	if err := store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusRunning}, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.PutStep(ctx, dagID, "a", StepRecord{
		DAGID: dagID, StepID: "a", Status: StatusRunning, ArgsJSON: json.RawMessage(`[]`),
	}, 0); err != nil {
		t.Fatal(err)
	}

	if err := Pause(ctx, wf, dagID); err != nil {
		t.Fatalf("Pause() error: %v", err)
	}

	meta, _, err := store.GetMeta(ctx, dagID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Status != DAGStatusPausing {
		t.Errorf("status = %s, want %s", meta.Status, DAGStatusPausing)
	}
}

// TestPause_NotRunning_Error verifies that pausing a DAG in any terminal
// status (done, failed, canceled) returns an error wrapping ErrDAGNotRunning
// with the dagID and current status in the error message.
func TestPause_NotRunning_Error(t *testing.T) {
	tests := []struct {
		name   string
		status DAGStatus
	}{
		{"done", DAGStatusDone},
		{"failed", DAGStatusFailed},
		{"canceled", DAGStatusCanceled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemStore()
			wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
			ctx := context.Background()
			dagID := "test-dag"

			if err := store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: tc.status}, 0); err != nil {
				t.Fatal(err)
			}

			err := Pause(ctx, wf, dagID)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, ErrDAGNotRunning) {
				t.Errorf("error does not wrap ErrDAGNotRunning: %v", err)
			}
			if !strings.Contains(err.Error(), dagID) {
				t.Errorf("error should contain dagID %q: %v", dagID, err)
			}
			if !strings.Contains(err.Error(), string(tc.status)) {
				t.Errorf("error should contain status %q: %v", tc.status, err)
			}
		})
	}
}

// TestPause_AlreadyPausing_Error verifies that pausing a DAG that is already
// pausing returns an error wrapping ErrDAGNotRunning.
func TestPause_AlreadyPausing_Error(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	ctx := context.Background()
	dagID := "test-dag"

	if err := store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusPausing}, 0); err != nil {
		t.Fatal(err)
	}

	err := Pause(ctx, wf, dagID)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrDAGNotRunning) {
		t.Errorf("error does not wrap ErrDAGNotRunning: %v", err)
	}
}

// TestPause_AlreadyPaused_Error verifies that pausing a DAG that is already
// paused returns an error wrapping ErrDAGNotRunning.
func TestPause_AlreadyPaused_Error(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	ctx := context.Background()
	dagID := "test-dag"

	if err := store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusPaused}, 0); err != nil {
		t.Fatal(err)
	}

	err := Pause(ctx, wf, dagID)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrDAGNotRunning) {
		t.Errorf("error does not wrap ErrDAGNotRunning: %v", err)
	}
}

// TestResume_PausedDAG verifies that resuming a paused DAG transitions the
// status to running and publishes an EventResumed on the event bus.
func TestResume_PausedDAG(t *testing.T) {
	store := NewMemStore()
	bus := NewMemBus()
	wf := NewWorkflow(store, bus, &captureEnq{})
	ctx := context.Background()
	dagID := "test-dag"

	if err := store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusPaused}, 0); err != nil {
		t.Fatal(err)
	}

	// Subscribe to capture events
	var mu sync.Mutex
	var events []Event
	sub, err := bus.Subscribe(ctx, "DAG.>", func(ev Event) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
		if ev.Ack != nil {
			_ = ev.Ack()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Stop()

	if err := Resume(ctx, wf, dagID); err != nil {
		t.Fatalf("Resume() error: %v", err)
	}

	// Assert status is running
	meta, _, err := store.GetMeta(ctx, dagID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Status != DAGStatusRunning {
		t.Errorf("status = %s, want %s", meta.Status, DAGStatusRunning)
	}

	// Assert EventResumed was published (allow async fan-out to complete)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	found := false
	for _, ev := range events {
		if ev.Kind == EventResumed && ev.DAGID == dagID {
			found = true
			break
		}
	}
	mu.Unlock()
	if !found {
		t.Error("EventResumed was not published on the bus")
	}
}

// TestResume_PausingDAG verifies that resuming a pausing DAG transitions the
// status to running and publishes an EventResumed on the event bus.
func TestResume_PausingDAG(t *testing.T) {
	store := NewMemStore()
	bus := NewMemBus()
	wf := NewWorkflow(store, bus, &captureEnq{})
	ctx := context.Background()
	dagID := "test-dag"

	if err := store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusPausing}, 0); err != nil {
		t.Fatal(err)
	}

	// Subscribe to capture events
	var mu sync.Mutex
	var events []Event
	sub, err := bus.Subscribe(ctx, "DAG.>", func(ev Event) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
		if ev.Ack != nil {
			_ = ev.Ack()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Stop()

	if err := Resume(ctx, wf, dagID); err != nil {
		t.Fatalf("Resume() error: %v", err)
	}

	// Assert status is running
	meta, _, err := store.GetMeta(ctx, dagID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Status != DAGStatusRunning {
		t.Errorf("status = %s, want %s", meta.Status, DAGStatusRunning)
	}

	// Assert EventResumed was published
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	found := false
	for _, ev := range events {
		if ev.Kind == EventResumed && ev.DAGID == dagID {
			found = true
			break
		}
	}
	mu.Unlock()
	if !found {
		t.Error("EventResumed was not published on the bus")
	}
}

// TestResume_NotPaused_Error verifies that resuming a DAG in any non-resumable
// status returns an error wrapping ErrDAGNotPaused with the dagID and current
// status in the error message.
// Note: pausing and running are intentionally absent — both are valid
// pre-states for resume (running for idempotent retry, pausing per review).
func TestResume_NotPaused_Error(t *testing.T) {
	tests := []struct {
		name   string
		status DAGStatus
	}{
		{"done", DAGStatusDone},
		{"failed", DAGStatusFailed},
		{"canceled", DAGStatusCanceled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemStore()
			wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
			ctx := context.Background()
			dagID := "test-dag"

			if err := store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: tc.status}, 0); err != nil {
				t.Fatal(err)
			}

			err := Resume(ctx, wf, dagID)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, ErrDAGNotPaused) {
				t.Errorf("error does not wrap ErrDAGNotPaused: %v", err)
			}
			if !strings.Contains(err.Error(), dagID) {
				t.Errorf("error should contain dagID %q: %v", dagID, err)
			}
			if !strings.Contains(err.Error(), string(tc.status)) {
				t.Errorf("error should contain status %q: %v", tc.status, err)
			}
		})
	}
}

// TestResume_TriggersStepReevaluation is a full integration test: submit a DAG
// with a→b dependency, pause, complete step a (auto-transitions to paused),
// resume, and verify step b gets enqueued.
func TestResume_TriggersStepReevaluation(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	a := dag.Step("a", noopA, 1)
	_ = dag.StepOpts("b", noopA, nil, a.Ref())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startScheduler(t, wf, ctx)
	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatal(err)
	}
	waitEnqueued(t, enq, 1, time.Second) // root a enqueued

	// Pause the DAG by setting meta to pausing (Phase 3 API not used here
	// to isolate scheduler behavior; we directly manipulate store like
	// existing scheduler tests do).
	meta, rev, err := wf.Store.GetMeta(ctx, dag.ID())
	if err != nil {
		t.Fatal(err)
	}
	meta.Status = DAGStatusPausing
	if err := wf.Store.PutMeta(ctx, dag.ID(), meta, rev); err != nil {
		t.Fatal(err)
	}

	// Complete step a — auto-transition should fire (last in-flight step)
	emulateHook(t, wf, dag.ID(), "a", []byte(`2`), nil)

	// Wait for DAG to reach paused state
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m, _, _ := wf.Store.GetMeta(ctx, dag.ID())
		if m.Status == DAGStatusPaused {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	m, _, _ := wf.Store.GetMeta(ctx, dag.ID())
	if m.Status != DAGStatusPaused {
		t.Fatalf("DAG did not transition to paused after step completion; status = %s", m.Status)
	}

	// Step b must NOT have been enqueued during pause
	if enq.count() != 1 {
		t.Fatalf("expected 1 enqueue (a only) during pause, got %d", enq.count())
	}

	// Now Resume the DAG
	if err := Resume(ctx, wf, dag.ID()); err != nil {
		t.Fatalf("Resume() error: %v", err)
	}

	// Verify step b gets enqueued after resume
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if enq.count() >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if enq.count() < 2 {
		t.Errorf("step b was not enqueued after resume; enqueues = %d, want >= 2", enq.count())
	}
	if got := enq.nthStepID(1); got != "b" {
		t.Errorf("second enqueue step ID = %q, want %q", got, "b")
	}
}
