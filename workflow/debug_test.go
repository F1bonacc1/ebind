package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// baseTime gives tests a fixed reference point so durations are deterministic.
var baseTime = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func seedDAG(t *testing.T, steps ...StepRecord) (*Workflow, string) {
	t.Helper()
	store := NewMemStore()
	wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
	dagID := "dag-debug"
	ctx := context.Background()
	if err := store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusRunning, CreatedAt: baseTime}, 0); err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		s.DAGID = dagID
		if err := store.PutStep(ctx, dagID, s.StepID, s, 0); err != nil {
			t.Fatal(err)
		}
	}
	return wf, dagID
}

func TestDebug_Counts(t *testing.T) {
	wf, id := seedDAG(t,
		StepRecord{StepID: "a", Status: StatusDone, AddedAt: baseTime, StartedAt: baseTime.Add(10 * time.Millisecond), FinishedAt: baseTime.Add(110 * time.Millisecond)},
		StepRecord{StepID: "b", Status: StatusFailed, AddedAt: baseTime, StartedAt: baseTime.Add(5 * time.Millisecond), FinishedAt: baseTime.Add(25 * time.Millisecond), ErrorKind: "boom", Attempt: 3},
		StepRecord{StepID: "c", Status: StatusSkipped, AddedAt: baseTime, FinishedAt: baseTime.Add(30 * time.Millisecond)},
		StepRecord{StepID: "d", Status: StatusPending, AddedAt: baseTime},
		StepRecord{StepID: "e", Status: StatusRunning, AddedAt: baseTime, StartedAt: baseTime.Add(40 * time.Millisecond)},
	)

	dbg, err := Debug(context.Background(), wf, id)
	if err != nil {
		t.Fatal(err)
	}

	want := map[StepStatus]int{
		StatusDone: 1, StatusFailed: 1, StatusSkipped: 1, StatusPending: 1, StatusRunning: 1,
	}
	for k, v := range want {
		if dbg.Counts[k] != v {
			t.Errorf("Counts[%s] = %d, want %d", k, dbg.Counts[k], v)
		}
	}
	if len(dbg.Steps) != 5 {
		t.Errorf("want 5 steps, got %d", len(dbg.Steps))
	}
}

func TestDebug_Durations(t *testing.T) {
	wf, id := seedDAG(t,
		StepRecord{StepID: "a", Status: StatusDone,
			AddedAt:    baseTime,
			StartedAt:  baseTime.Add(5 * time.Millisecond),
			FinishedAt: baseTime.Add(105 * time.Millisecond),
		},
	)

	dbg, _ := Debug(context.Background(), wf, id)
	got := dbg.Steps[0]
	if got.QueueDuration != 5*time.Millisecond {
		t.Errorf("QueueDuration = %v, want 5ms", got.QueueDuration)
	}
	if got.ExecDuration != 100*time.Millisecond {
		t.Errorf("ExecDuration = %v, want 100ms", got.ExecDuration)
	}
}

func TestDebug_TotalDuration_ZeroWhileRunning(t *testing.T) {
	wf, id := seedDAG(t,
		StepRecord{StepID: "a", Status: StatusDone, AddedAt: baseTime, StartedAt: baseTime.Add(1 * time.Millisecond), FinishedAt: baseTime.Add(100 * time.Millisecond)},
		StepRecord{StepID: "b", Status: StatusRunning, AddedAt: baseTime, StartedAt: baseTime.Add(50 * time.Millisecond)},
	)
	dbg, _ := Debug(context.Background(), wf, id)
	if dbg.TotalDuration != 0 {
		t.Errorf("TotalDuration should be 0 while running, got %v", dbg.TotalDuration)
	}
}

func TestDebug_TotalDuration_AllTerminal(t *testing.T) {
	wf, id := seedDAG(t,
		StepRecord{StepID: "a", Status: StatusDone, AddedAt: baseTime, StartedAt: baseTime, FinishedAt: baseTime.Add(50 * time.Millisecond)},
		StepRecord{StepID: "b", Status: StatusDone, AddedAt: baseTime, StartedAt: baseTime, FinishedAt: baseTime.Add(200 * time.Millisecond)},
	)
	dbg, _ := Debug(context.Background(), wf, id)
	if dbg.TotalDuration != 200*time.Millisecond {
		t.Errorf("TotalDuration = %v, want 200ms", dbg.TotalDuration)
	}
}

func TestDebug_Blockers(t *testing.T) {
	// c is pending; its required dep a is pending; optional dep b is pending.
	// Both show up in WaitingOn.
	wf, id := seedDAG(t,
		StepRecord{StepID: "a", Status: StatusPending, AddedAt: baseTime},
		StepRecord{StepID: "b", Status: StatusPending, AddedAt: baseTime},
		StepRecord{StepID: "c", Status: StatusPending, AddedAt: baseTime, Deps: []string{"a"}, OptionalDeps: []string{"b"}},
	)

	dbg, _ := Debug(context.Background(), wf, id)

	// a and b have no deps — they're pending but NOT blocked (ready to run).
	// c is blocked.
	byID := map[string]StepBlocker{}
	for _, b := range dbg.Blockers {
		byID[b.StepID] = b
	}
	if _, ok := byID["c"]; !ok {
		t.Fatalf("c should be a blocker; got %+v", dbg.Blockers)
	}
	cB := byID["c"]
	if len(cB.WaitingOn) != 2 {
		t.Errorf("c waiting on %v, want [a b]", cB.WaitingOn)
	}
	// Pending-but-ready should NOT appear.
	if _, ok := byID["a"]; ok {
		t.Error("a should not be a blocker (ready to run)")
	}
}

func TestDebug_Blockers_ExcludesTerminalDeps(t *testing.T) {
	wf, id := seedDAG(t,
		StepRecord{StepID: "a", Status: StatusDone, AddedAt: baseTime, StartedAt: baseTime, FinishedAt: baseTime.Add(time.Millisecond)},
		StepRecord{StepID: "b", Status: StatusPending, AddedAt: baseTime, Deps: []string{"a"}},
	)
	dbg, _ := Debug(context.Background(), wf, id)
	for _, blk := range dbg.Blockers {
		if blk.StepID == "b" {
			t.Errorf("b should be ready (a is done), not a blocker: %+v", blk)
		}
	}
}

func TestDebug_NotFound(t *testing.T) {
	wf := NewWorkflow(NewMemStore(), NewMemBus(), &captureEnq{})
	_, err := Debug(context.Background(), wf, "nonexistent")
	if err != ErrDAGNotFound {
		t.Errorf("want ErrDAGNotFound, got %v", err)
	}
}

func TestDebug_Steps_OrderedByAddedAt(t *testing.T) {
	// Steps added in reverse time order; output should be sorted ascending.
	wf, id := seedDAG(t,
		StepRecord{StepID: "late", Status: StatusDone, AddedAt: baseTime.Add(100 * time.Millisecond)},
		StepRecord{StepID: "early", Status: StatusDone, AddedAt: baseTime},
		StepRecord{StepID: "mid", Status: StatusDone, AddedAt: baseTime.Add(50 * time.Millisecond)},
	)
	dbg, _ := Debug(context.Background(), wf, id)
	ids := []string{dbg.Steps[0].StepID, dbg.Steps[1].StepID, dbg.Steps[2].StepID}
	want := []string{"early", "mid", "late"}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("steps[%d]=%s, want %s", i, ids[i], want[i])
		}
	}
}

func TestDebugPrint_Output(t *testing.T) {
	wf, id := seedDAG(t,
		StepRecord{StepID: "fetch", Status: StatusDone,
			AddedAt: baseTime, StartedAt: baseTime.Add(2 * time.Millisecond), FinishedAt: baseTime.Add(152 * time.Millisecond),
		},
		StepRecord{StepID: "enrich", Status: StatusFailed, ErrorKind: "validation", Attempt: 1,
			AddedAt: baseTime, StartedAt: baseTime.Add(5 * time.Millisecond), FinishedAt: baseTime.Add(25 * time.Millisecond),
		},
		StepRecord{StepID: "combine", Status: StatusSkipped, AddedAt: baseTime,
			Deps: []string{"enrich"}, ArgsJSON: mustRefArgs(Ref{StepID: "enrich", Mode: RefModeRequired}),
		},
	)
	var buf bytes.Buffer
	if err := DebugPrint(context.Background(), wf, id, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// Basic structural assertions — avoid byte-exact so minor formatting tweaks
	// don't break the tests.
	for _, want := range []string{
		"DAG " + id,
		"fetch",
		"enrich",
		"combine",
		"done",
		"failed",
		"skipped",
		"kind=validation",
		"(cascade)",
		"blockers: none",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("DebugPrint output missing %q; output:\n%s", want, out)
		}
	}
}

func TestDebugPrint_Blockers_Section(t *testing.T) {
	wf, id := seedDAG(t,
		StepRecord{StepID: "a", Status: StatusRunning, AddedAt: baseTime, StartedAt: baseTime.Add(time.Millisecond)},
		StepRecord{StepID: "b", Status: StatusPending, AddedAt: baseTime, Deps: []string{"a"}},
	)
	var buf bytes.Buffer
	_ = DebugPrint(context.Background(), wf, id, &buf)
	out := buf.String()
	if !strings.Contains(out, "blockers:") || strings.Contains(out, "blockers: none") {
		t.Errorf("expected blockers section listing b; got:\n%s", out)
	}
	if !strings.Contains(out, "b waiting on:") {
		t.Errorf("expected 'b waiting on:'; got:\n%s", out)
	}
}

func mustRefArgs(refs ...Ref) json.RawMessage {
	raw := make([]json.RawMessage, len(refs))
	for i, r := range refs {
		b, _ := json.Marshal(r)
		raw[i] = b
	}
	out, _ := json.Marshal(raw)
	return out
}
