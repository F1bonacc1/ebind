package dag

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/f1bonacc1/ebind/workflow"
)

func TestSignalTable_RendersStates(t *testing.T) {
	store := workflow.NewMemStore()
	meta := workflow.DAGMeta{ID: "d", Status: workflow.DAGStatusRunning}
	if err := store.PutMeta(context.Background(), "d", meta, 0); err != nil {
		t.Fatal(err)
	}
	putStep(t, store, workflow.StepRecord{
		DAGID: "d", StepID: "deploy", Status: workflow.StatusPending,
		ArgsJSON: json.RawMessage(`[]`), WaitSignals: []string{"approval"},
	})
	if err := store.PutSignal(context.Background(), "d", workflow.SignalRecord{
		DAGID: "d", Name: "budget-ok",
		Payload:     json.RawMessage(`{"limit":1000000000000000000000000000000000000000}`),
		DeliveredAt: time.Now().Add(-time.Minute).UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	steps, err := store.ListSteps(context.Background(), "d")
	if err != nil {
		t.Fatal(err)
	}
	sigs, err := store.ListSignals(context.Background(), "d")
	if err != nil {
		t.Fatal(err)
	}
	infos := workflow.ComputeSignals(meta, steps, sigs)
	headers, rows := signalTable(infos)
	if len(headers) != 5 || len(rows) != 2 {
		t.Fatalf("headers=%d rows=%d, want 5/2", len(headers), len(rows))
	}
	// Sorted by name: approval (awaited), budget-ok (delivered).
	a := rows[0]
	if a[0] != "approval" || a[1] != "no" || a[2] != "-" || a[3] != "deploy" || a[4] != "-" {
		t.Errorf("approval row: %v", a)
	}
	b := rows[1]
	if b[0] != "budget-ok" || b[1] != "yes" || b[2] == "-" || b[3] != "-" {
		t.Errorf("budget-ok row: %v", b)
	}
	if utf8.RuneCountInString(b[4]) > 40 || !strings.HasSuffix(b[4], "…") {
		t.Errorf("payload should be truncated to ≤40 runes with ellipsis: %q", b[4])
	}
}

func TestTruncatePayload(t *testing.T) {
	if got := truncatePayload(nil, 10); got != "-" {
		t.Errorf("empty: %q", got)
	}
	if got := truncatePayload([]byte(`{"a":1}`), 10); got != `{"a":1}` {
		t.Errorf("short: %q", got)
	}
	if got := truncatePayload([]byte("0123456789abcdef"), 10); got != "012345678…" {
		t.Errorf("long: %q", got)
	}
}
