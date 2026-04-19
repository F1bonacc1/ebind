package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestAwaitByID_ImmediateResult(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	ctx := context.Background()
	_ = store.PutMeta(ctx, "d", DAGMeta{ID: "d", Status: DAGStatusDone}, 0)
	_ = store.PutStep(ctx, "d", "a", StepRecord{StepID: "a", Status: StatusDone}, 0)
	_ = store.PutResult(ctx, "d", "a", []byte(`"hello"`))

	ctx2, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	got, err := AwaitByID[string](ctx2, wf, "d", "a")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello" {
		t.Errorf("got %q, want hello", got)
	}
}

func TestAwaitByID_AsyncResult(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = store.PutMeta(ctx, "d", DAGMeta{ID: "d", Status: DAGStatusRunning}, 0)
	_ = store.PutStep(ctx, "d", "a", StepRecord{StepID: "a", Status: StatusRunning}, 0)

	go func() {
		time.Sleep(100 * time.Millisecond)
		// Update step to Done then write result to simulate hook ordering.
		rec, rev, _ := store.GetStep(ctx, "d", "a")
		rec.Status = StatusDone
		_ = store.PutStep(ctx, "d", "a", rec, rev)
		_ = store.PutResult(ctx, "d", "a", []byte(`42`))
	}()

	got, err := AwaitByID[int](ctx, wf, "d", "a")
	if err != nil {
		t.Fatal(err)
	}
	if got != 42 {
		t.Errorf("got %d, want 42", got)
	}
}

func TestAwaitByID_Failed(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	ctx := context.Background()
	_ = store.PutMeta(ctx, "d", DAGMeta{ID: "d", Status: DAGStatusFailed}, 0)
	_ = store.PutStep(ctx, "d", "a", StepRecord{StepID: "a", Status: StatusFailed, ErrorKind: "boom"}, 0)

	ctx2, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := AwaitByID[string](ctx2, wf, "d", "a")
	if !errors.Is(err, ErrStepFailed) {
		t.Errorf("want ErrStepFailed, got %v", err)
	}
}

func TestAwaitByID_Skipped(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	ctx := context.Background()
	_ = store.PutMeta(ctx, "d", DAGMeta{ID: "d", Status: DAGStatusFailed}, 0)
	_ = store.PutStep(ctx, "d", "a", StepRecord{StepID: "a", Status: StatusSkipped}, 0)

	ctx2, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := AwaitByID[string](ctx2, wf, "d", "a")
	if !errors.Is(err, ErrStepSkipped) {
		t.Errorf("want ErrStepSkipped, got %v", err)
	}
}

func TestAwaitByID_Canceled(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	ctx := context.Background()
	_ = store.PutMeta(ctx, "d", DAGMeta{ID: "d", Status: DAGStatusCanceled}, 0)
	_ = store.PutStep(ctx, "d", "a", StepRecord{StepID: "a", Status: StatusCanceled}, 0)

	ctx2, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := AwaitByID[string](ctx2, wf, "d", "a")
	if !errors.Is(err, ErrStepCanceled) {
		t.Errorf("want ErrStepCanceled, got %v", err)
	}
}

func TestAwaitByID_ContextCancel(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	ctx := context.Background()
	_ = store.PutMeta(ctx, "d", DAGMeta{ID: "d", Status: DAGStatusRunning}, 0)
	_ = store.PutStep(ctx, "d", "a", StepRecord{StepID: "a", Status: StatusRunning}, 0)

	ctx2, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	_, err := AwaitByID[string](ctx2, wf, "d", "a")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want DeadlineExceeded, got %v", err)
	}
}

// Sanity: Await (the *Step wrapper) still works.
func TestAwait_Wrapper(t *testing.T) {
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	ctx := context.Background()
	_ = store.PutMeta(ctx, "d", DAGMeta{ID: "d", Status: DAGStatusDone}, 0)
	_ = store.PutStep(ctx, "d", "a", StepRecord{StepID: "a", Status: StatusDone}, 0)
	_ = store.PutResult(ctx, "d", "a", []byte(`7`))

	ctx2, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	got, err := Await[int](ctx2, wf, "d", &Step{id: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if got != 7 {
		t.Errorf("got %d, want 7", got)
	}
	_ = json.RawMessage{}
}
