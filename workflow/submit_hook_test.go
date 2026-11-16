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

// SubmitHook tests (MemStore/MemBus/captureEnq fakes).

func TestSubmitHook_SeesProvisionalMeta_OptionsPersisted(t *testing.T) {
	wf, _, enq := setupDAG(t)
	policy := task.RetryPolicy{MaximumAttempts: 2, NonRetryableErrorKinds: []string{"x"}}
	dag := New(WithLabels("billing", "debug", "billing"), WithRetry(policy))
	_ = dag.Step("charge", noopA, 1)

	var seen DAGMeta
	wf.UseSubmitHook(func(_ context.Context, m DAGMeta) ([]SubmitOption, error) {
		seen = m
		// Mutating the copy must not leak into what Submit persists.
		m.Labels[0] = "mutated"
		m.DefaultPolicy.NonRetryableErrorKinds[0] = "mutated"
		if slices.Contains(m.Labels, "debug") {
			return []SubmitOption{StartPaused(), WithArmedBreakpoints(BeforeStep("charge"))}, nil
		}
		return nil, nil
	})
	if err := dag.Submit(context.Background(), wf); err != nil {
		t.Fatal(err)
	}
	if seen.ID != dag.ID() || seen.CreatedAt.IsZero() || seen.Status != "" || len(seen.ArmedMatchers) != 0 {
		t.Errorf("hook meta = %+v", seen)
	}
	meta, _, _ := wf.Store.GetMeta(context.Background(), dag.ID())
	if strings.Join(meta.Labels, ",") != "billing,debug" {
		t.Errorf("persisted labels = %v", meta.Labels)
	}
	if meta.DefaultPolicy.NonRetryableErrorKinds[0] != "x" {
		t.Errorf("hook mutated the persisted policy: %v", meta.DefaultPolicy)
	}
	if !meta.CreatedAt.Equal(seen.CreatedAt) {
		t.Errorf("CreatedAt %v != hook's %v", meta.CreatedAt, seen.CreatedAt)
	}
	if meta.Status != DAGStatusPaused || len(meta.ArmedMatchers) != 1 || meta.ArmedMatchers[0].Key() != "before:step:charge" {
		t.Errorf("hook options not persisted: status=%s armed=%v", meta.Status, meta.ArmedMatchers)
	}
	rec, _, _ := wf.Store.GetStep(context.Background(), dag.ID(), "charge")
	if !rec.Held {
		t.Error("StartPaused from the hook must create held steps")
	}
	time.Sleep(50 * time.Millisecond)
	if enq.count() != 0 {
		t.Errorf("enqueues = %d, want 0", enq.count())
	}
}

func TestSubmitHook_AddsToCallerOptions(t *testing.T) {
	wf, dag, _ := setupDAG(t)
	_ = dag.Step("a", noopA, 1)
	wf.UseSubmitHook(func(context.Context, DAGMeta) ([]SubmitOption, error) {
		return []SubmitOption{WithArmedBreakpoints(AfterStep("a"))}, nil
	})
	if err := dag.Submit(context.Background(), wf, WithArmedBreakpoints(BeforeStep("a"))); err != nil {
		t.Fatal(err)
	}
	meta, _, _ := wf.Store.GetMeta(context.Background(), dag.ID())
	var keys []string
	for _, m := range meta.ArmedMatchers {
		keys = append(keys, m.Key())
	}
	if got := strings.Join(keys, ","); got != "before:step:a,after:step:a" {
		t.Errorf("armed = %s, want caller's then hook's", got)
	}
}

func TestSubmitHook_ErrorPersistsNothing_Resubmittable(t *testing.T) {
	wf, dag, enq := setupDAG(t)
	_ = dag.Step("a", noopA, 1)
	var blocked sync.Mutex
	reject := true
	wf.UseSubmitHook(func(context.Context, DAGMeta) ([]SubmitOption, error) {
		blocked.Lock()
		defer blocked.Unlock()
		if reject {
			return nil, errors.New("maintenance window")
		}
		return nil, nil
	})
	ctx := context.Background()
	err := dag.Submit(ctx, wf)
	if err == nil || !strings.Contains(err.Error(), "submit hook: maintenance window") {
		t.Fatalf("err = %v, want the hook error", err)
	}
	if _, _, err := wf.Store.GetMeta(ctx, dag.ID()); !errors.Is(err, ErrDAGNotFound) {
		t.Fatalf("meta after hook error: %v, want ErrDAGNotFound", err)
	}
	if steps, _ := wf.Store.ListSteps(ctx, dag.ID()); len(steps) != 0 {
		t.Fatalf("steps persisted after hook error: %d", len(steps))
	}

	blocked.Lock()
	reject = false
	blocked.Unlock()
	if err := dag.Submit(ctx, wf); err != nil {
		t.Fatalf("resubmit after fixing the cause: %v", err)
	}
	waitEnqueued(t, enq, 1, time.Second)
}

// An invalid option returned by a hook is caught by the merged validation,
// and also leaves the DAG resubmittable.
func TestSubmitHook_InvalidOption_Resubmittable(t *testing.T) {
	wf, dag, _ := setupDAG(t)
	_ = dag.Step("a", noopA, 1)
	bad := true
	wf.UseSubmitHook(func(context.Context, DAGMeta) ([]SubmitOption, error) {
		if bad {
			return []SubmitOption{WithArmedBreakpoints(BeforeStep("["))}, nil
		}
		return nil, nil
	})
	if err := dag.Submit(context.Background(), wf); !errors.Is(err, ErrInvalidMatcher) {
		t.Fatalf("err = %v, want ErrInvalidMatcher", err)
	}
	bad = false
	if err := dag.Submit(context.Background(), wf); err != nil {
		t.Fatalf("resubmit: %v", err)
	}
}

func TestSubmitHook_RunInRegistrationOrder(t *testing.T) {
	wf, dag, _ := setupDAG(t)
	_ = dag.Step("a", noopA, 1)
	var order []string
	for _, name := range []string{"first", "second", "third"} {
		wf.UseSubmitHook(func(context.Context, DAGMeta) ([]SubmitOption, error) {
			order = append(order, name)
			return nil, nil
		})
	}
	if err := dag.Submit(context.Background(), wf); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "first,second,third" {
		t.Errorf("order = %s", got)
	}
}

// A hook that adds nothing leaves the persisted meta exactly as a Submit
// without hooks writes it.
func TestSubmitHook_NoOptions_MetaUnchanged(t *testing.T) {
	submit := func(withHook bool) []byte {
		wf := NewWorkflow(NewMemStore(), NewMemBus(), &captureEnq{})
		if withHook {
			wf.UseSubmitHook(func(context.Context, DAGMeta) ([]SubmitOption, error) { return nil, nil })
		}
		dag := New(WithDAGID("fixed"), WithLabels("x"))
		_ = dag.Step("a", noopA, 1)
		if err := dag.Submit(context.Background(), wf, WithActiveBreakpoints("L")); err != nil {
			t.Fatal(err)
		}
		meta, _, _ := wf.Store.GetMeta(context.Background(), "fixed")
		meta.CreatedAt = time.Time{}
		b, _ := json.Marshal(meta)
		return b
	}
	if a, b := submit(false), submit(true); string(a) != string(b) {
		t.Errorf("meta differs:\nno hook:   %s\nwith hook: %s", a, b)
	}
}

func TestSubmitHook_ConcurrentSubmitOfOneDAG_OneWins(t *testing.T) {
	wf, dag, _ := setupDAG(t)
	_ = dag.Step("a", noopA, 1)
	release := make(chan struct{})
	var entered sync.WaitGroup
	entered.Add(2)
	wf.UseSubmitHook(func(context.Context, DAGMeta) ([]SubmitOption, error) {
		entered.Done()
		<-release
		return nil, nil
	})
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { errs <- dag.Submit(context.Background(), wf) }()
	}
	entered.Wait() // both Submits are inside the hook, past the first check
	close(release)
	var ok, already int
	for i := 0; i < 2; i++ {
		err := <-errs
		switch {
		case err == nil:
			ok++
		case strings.Contains(err.Error(), "already submitted"):
			already++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 || already != 1 {
		t.Errorf("ok=%d already=%d, want 1/1", ok, already)
	}
}
