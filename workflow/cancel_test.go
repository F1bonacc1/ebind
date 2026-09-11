package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// lossyListStore models NatsStore.ListSteps under a lagging direct-get
// replica: the listed key's Get misses and the record is skipped, so it is
// absent from the result while the call still returns nil.
type lossyListStore struct {
	*MemStore
	hide string
}

func (s *lossyListStore) ListSteps(ctx context.Context, dagID string) ([]StepRecord, error) {
	all, err := s.MemStore.ListSteps(ctx, dagID)
	if err != nil {
		return nil, err
	}
	var out []StepRecord
	for _, r := range all {
		if r.StepID != s.hide {
			out = append(out, r)
		}
	}
	return out, nil
}

// staleStore fails every CAS update (expectedRev != 0) of the chosen record
// kind with ErrStaleRevision: a concurrent writer that always wins, or a
// replica serving a stale revision on every read. Creates pass through.
type staleStore struct {
	*MemStore
	meta, steps bool
}

func (s *staleStore) PutMeta(ctx context.Context, dagID string, meta DAGMeta, expectedRev uint64) error {
	if s.meta && expectedRev != 0 {
		return ErrStaleRevision
	}
	return s.MemStore.PutMeta(ctx, dagID, meta, expectedRev)
}

func (s *staleStore) PutStep(ctx context.Context, dagID, stepID string, rec StepRecord, expectedRev uint64) (uint64, error) {
	if s.steps && expectedRev != 0 {
		return 0, ErrStaleRevision
	}
	return s.MemStore.PutStep(ctx, dagID, stepID, rec, expectedRev)
}

// seedCancelDAG writes a running DAG with a running root and a pending
// dependent: the shape of e2e 06_FailCancelDelete when Cancel is called.
func seedCancelDAG(t *testing.T, store StateStore, dagID string) {
	t.Helper()
	ctx := context.Background()
	if err := store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusRunning}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutStep(ctx, dagID, "root", StepRecord{
		DAGID: dagID, StepID: "root", FnName: "noopA", Status: StatusRunning,
		ArgsJSON: json.RawMessage(`[1]`),
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutStep(ctx, dagID, "dep", StepRecord{
		DAGID: dagID, StepID: "dep", FnName: "noopA", Status: StatusPending,
		Deps: []string{"root"}, ArgsJSON: json.RawMessage(`[1]`),
	}, 0); err != nil {
		t.Fatal(err)
	}
}

func stepStatusOf(t *testing.T, store StateStore, dagID, stepID string) StepStatus {
	t.Helper()
	rec, _, err := store.GetStep(context.Background(), dagID, stepID)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Status
}

func TestCancel_Rerun_CancelsStepMissedByLossyList(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	seedCancelDAG(t, mem, "d")
	store := &lossyListStore{MemStore: mem, hide: "dep"}
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})

	// During the lag window the list omits dep, so Cancel cannot see it.
	if err := Cancel(ctx, wf, "d"); err != nil {
		t.Fatalf("Cancel #1: %v", err)
	}
	if got := stepStatusOf(t, mem, "d", "dep"); got != StatusPending {
		t.Fatalf("precondition: dep = %s after lossy Cancel, want pending", got)
	}

	// Replica caught up: re-running Cancel on the now-canceled DAG repairs dep.
	store.hide = ""
	if err := Cancel(ctx, wf, "d"); err != nil {
		t.Fatalf("Cancel #2: %v", err)
	}
	if got := stepStatusOf(t, mem, "d", "dep"); got != StatusCanceled {
		t.Errorf("dep = %s after re-run Cancel, want canceled", got)
	}
	if got := stepStatusOf(t, mem, "d", "root"); got != StatusRunning {
		t.Errorf("root = %s, want running (Cancel leaves running steps alone)", got)
	}

	actx, acancel := context.WithTimeout(ctx, 2*time.Second)
	defer acancel()
	if _, err := AwaitByID[int](actx, wf, "d", "dep"); !errors.Is(err, ErrStepCanceled) {
		t.Errorf("Await(dep) = %v, want ErrStepCanceled", err)
	}
}

func TestCancel_StepCASExhaustion_ReturnsError(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	seedCancelDAG(t, mem, "d")
	wf := NewWorkflow(&staleStore{MemStore: mem, steps: true}, NewMemBus(), &captureEnq{})

	if err := Cancel(ctx, wf, "d"); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("Cancel = %v, want ErrStaleRevision (a step it could not cancel)", err)
	}
	if got := stepStatusOf(t, mem, "d", "dep"); got != StatusPending {
		t.Errorf("dep = %s, want pending", got)
	}
}

func TestCancel_MetaCASExhaustion_ReturnsErrorWithoutTouchingSteps(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	seedCancelDAG(t, mem, "d")
	wf := NewWorkflow(&staleStore{MemStore: mem, meta: true}, NewMemBus(), &captureEnq{})

	if err := Cancel(ctx, wf, "d"); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("Cancel = %v, want ErrStaleRevision", err)
	}
	meta, _, err := mem.GetMeta(ctx, "d")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Status != DAGStatusRunning {
		t.Errorf("meta = %s, want running", meta.Status)
	}
	// Canceling steps under a DAG whose meta never became canceled would leave
	// the scheduler treating it as live with canceled steps in it.
	if got := stepStatusOf(t, mem, "d", "dep"); got != StatusPending {
		t.Errorf("dep = %s, want pending (no sweep without a canceled meta)", got)
	}
}
