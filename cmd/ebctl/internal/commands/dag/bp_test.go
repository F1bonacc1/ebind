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
	meta := workflow.DAGMeta{ID: "d", Status: workflow.DAGStatusRunning,
		ArmedMatchers: []workflow.BPMatcher{workflow.BeforeLabel("X"), workflow.AfterLabel("X")}}
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
	if len(headers) != 8 || len(rows) != 2 {
		t.Fatalf("headers=%d rows=%d, want 8/2", len(headers), len(rows))
	}
	// Rows are ordered by step ID: a/after first, then b/before.
	a := rows[0]
	if a[0] != "a" || a[1] != "after" || a[3] != "after:label:X" || a[4] != "yes" || a[5] != "blocked" {
		t.Errorf("a row: %v", a)
	}
	if a[7] != "-" {
		// b is held by a's gate? b depends on nothing — a holds no one here.
		t.Errorf("a holding: %q, want -", a[6])
	}
	if a[6] == "-" {
		t.Errorf("a SINCE should render from BPBlockedAt, got %q", a[6])
	}
	b := rows[1]
	if b[0] != "b" || b[1] != "before" || b[2] != "X,Y" || b[5] != "blocked" {
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
	meta.ArmedMatchers = nil
	if n := blockedBPCount(meta, steps); n != 0 {
		t.Errorf("inactive blockedBPCount = %d, want 0", n)
	}
}

func TestParseBPPosition(t *testing.T) {
	for _, ok := range []string{"before", "after"} {
		if p, err := parseBPPosition(ok); err != nil || string(p) != ok {
			t.Errorf("parseBPPosition(%q) = %q, %v", ok, p, err)
		}
	}
	for _, bad := range []string{"", "Before", "during"} {
		if _, err := parseBPPosition(bad); err == nil {
			t.Errorf("parseBPPosition(%q): want error", bad)
		}
	}
}

func TestBPReleaseText(t *testing.T) {
	got := bpReleaseText("d", "upload-eu", workflow.BPPositionBefore, true)
	if !strings.HasPrefix(got, "released upload-eu") || !strings.Contains(got, "before") {
		t.Errorf("released text: %q", got)
	}
	got = bpReleaseText("d", "upload-eu", workflow.BPPositionAfter, false)
	if !strings.HasPrefix(got, "not stopped") || !strings.Contains(got, "after") {
		t.Errorf("not-stopped text: %q", got)
	}
}

func TestBPTable_MatcherOnlyRow(t *testing.T) {
	meta := workflow.DAGMeta{ID: "d", Status: workflow.DAGStatusRunning,
		ArmedMatchers: []workflow.BPMatcher{workflow.BeforeStep("node-*")}}
	steps := []workflow.StepRecord{{DAGID: "d", StepID: "node-1", Status: workflow.StatusPending}}
	_, rows := bpTable(workflow.ComputeBreakpoints(meta, steps))
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	r := rows[0]
	if r[0] != "node-1" || r[2] != "-" || r[3] != "before:step:node-*" || r[4] != "yes" || r[5] != "blocked" {
		t.Errorf("matcher-only row: %v", r)
	}
}

func TestBPLsOptions(t *testing.T) {
	meta := workflow.DAGMeta{ID: "d", Status: workflow.DAGStatusRunning,
		ArmedMatchers: []workflow.BPMatcher{workflow.BeforeLabel("X")}}
	steps := []workflow.StepRecord{
		{DAGID: "d", StepID: "armed", Status: workflow.StatusPending, BreakBefore: []string{"X"}},
		{DAGID: "d", StepID: "unarmed", Status: workflow.StatusPending, BreakBefore: []string{"Y"}},
		{DAGID: "d", StepID: "a", Status: workflow.StatusRunning},
		{DAGID: "d", StepID: "later", Status: workflow.StatusPending, BreakBefore: []string{"X"}, Deps: []string{"a"}},
	}
	infos := workflow.ComputeBreakpoints(meta, steps)
	ids := func(opts []workflow.ListBPOption) string {
		var out []string
		for _, bp := range workflow.FilterBreakpoints(infos, opts...) {
			out = append(out, bp.StepID)
		}
		return strings.Join(out, ",")
	}
	if got := ids(bpLsOptions(false, false)); got != "armed,later" {
		t.Errorf("default = %s, want armed,later", got)
	}
	if got := ids(bpLsOptions(true, false)); got != "armed,later,unarmed" {
		t.Errorf("--all = %s", got)
	}
	if got := ids(bpLsOptions(false, true)); got != "armed" {
		t.Errorf("--blocked = %s, want armed", got)
	}
}
