package dag

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/f1bonacc1/ebind/workflow"
)

// seedBlockedDAG stores a minimal DAG with one step blocked at an armed
// before-breakpoint and one done step holding at an after-breakpoint.
func seedBlockedDAG(t *testing.T, store *workflow.MemStore) workflow.DAGMeta {
	t.Helper()
	meta := workflow.DAGMeta{ID: "d", Status: workflow.DAGStatusRunning, ActiveBreakpoints: []string{"X"}}
	if err := store.PutMeta(context.Background(), "d", meta, 0); err != nil {
		t.Fatal(err)
	}
	putStep(t, store, workflow.StepRecord{
		DAGID: "d", StepID: "a", Status: workflow.StatusDone,
		BreakAfter: []string{"X"}, BPBlockedAt: time.Now().Add(-time.Minute).UTC(),
	})
	putStep(t, store, workflow.StepRecord{
		DAGID: "d", StepID: "b", Status: workflow.StatusPending,
		ArgsJSON: json.RawMessage(`[]`), BreakBefore: []string{"X", "Y"},
	})
	return meta
}

func TestBPTable_RendersStates(t *testing.T) {
	store := workflow.NewMemStore()
	meta := seedBlockedDAG(t, store)
	steps, err := store.ListSteps(context.Background(), "d")
	if err != nil {
		t.Fatal(err)
	}
	infos := workflow.ComputeBreakpoints(meta, steps)
	headers, rows := bpTable(infos)
	if len(headers) != 7 || len(rows) != 2 {
		t.Fatalf("headers=%d rows=%d, want 7/2", len(headers), len(rows))
	}
	// Rows are ordered by step ID: a/after first, then b/before.
	a := rows[0]
	if a[0] != "a" || a[1] != "after" || a[3] != "yes" || a[4] != "blocked" {
		t.Errorf("a row: %v", a)
	}
	if a[6] != "-" {
		// b is held by a's gate? b depends on nothing — a holds no one here.
		t.Errorf("a holding: %q, want -", a[6])
	}
	if a[5] == "-" {
		t.Errorf("a SINCE should render from BPBlockedAt, got %q", a[5])
	}
	b := rows[1]
	if b[0] != "b" || b[1] != "before" || b[2] != "X,Y" || b[4] != "blocked" {
		t.Errorf("b row: %v", b)
	}
}

func TestBPStateSuffix(t *testing.T) {
	if got := bpStateSuffix("", time.Time{}); got != "" {
		t.Errorf("zero state: %q", got)
	}
	if got := bpStateSuffix(workflow.BPStateReleased, time.Time{}); got != "  [released]" {
		t.Errorf("released: %q", got)
	}
	got := bpStateSuffix(workflow.BPStateBlocked, time.Now().Add(-2*time.Minute))
	if !strings.Contains(got, "blocked") || !strings.Contains(got, "ago") {
		t.Errorf("blocked: %q", got)
	}
	if got := bpStateSuffix(workflow.BPStateBlocked, time.Time{}); got != "  [blocked]" {
		t.Errorf("blocked no-time: %q", got)
	}
}

func TestBlockedBPCount(t *testing.T) {
	store := workflow.NewMemStore()
	meta := seedBlockedDAG(t, store)
	steps, _ := store.ListSteps(context.Background(), "d")
	if n := blockedBPCount(meta, steps); n != 2 {
		t.Errorf("blockedBPCount = %d, want 2", n)
	}
	// Inactive labels: nothing blocked.
	meta.ActiveBreakpoints = nil
	if n := blockedBPCount(meta, steps); n != 0 {
		t.Errorf("inactive blockedBPCount = %d, want 0", n)
	}
}
