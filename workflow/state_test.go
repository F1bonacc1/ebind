package workflow

import (
	"encoding/json"
	"testing"
)

// makeState builds a DAGState with the given steps for testing.
// Each step uses "args" with a single Ref per dep for realistic cascade behavior.
func makeState(steps ...StepRecord) *DAGState {
	m := map[string]StepRecord{}
	for _, s := range steps {
		if s.Status == "" {
			s.Status = StatusPending
		}
		m[s.StepID] = s
	}
	return &DAGState{Meta: DAGMeta{ID: "test-dag", Status: DAGStatusRunning}, Steps: m}
}

func refArgs(refs ...Ref) json.RawMessage {
	raw := make([]json.RawMessage, len(refs))
	for i, r := range refs {
		b, _ := json.Marshal(r)
		raw[i] = b
	}
	out, _ := json.Marshal(raw)
	return out
}

func TestState_MarkDone_UnlockDependent(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a"},
		StepRecord{StepID: "b", Deps: []string{"a"}, ArgsJSON: refArgs(Ref{StepID: "a", Mode: RefModeRequired})},
	)
	ready, err := s.MarkDone("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0] != "b" {
		t.Errorf("ready: %v, want [b]", ready)
	}
}

func TestState_MarkFailed_CascadesRequired(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a"},
		StepRecord{StepID: "b", Deps: []string{"a"}, ArgsJSON: refArgs(Ref{StepID: "a", Mode: RefModeRequired})},
		StepRecord{StepID: "c", Deps: []string{"b"}, ArgsJSON: refArgs(Ref{StepID: "b", Mode: RefModeRequired})},
	)
	_, skipped, err := s.MarkFailed("a", "boom")
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 2 {
		t.Errorf("want 2 cascaded skips (b, c), got %d: %v", len(skipped), skipped)
	}
	if s.Steps["a"].Status != StatusFailed {
		t.Errorf("a status: %s", s.Steps["a"].Status)
	}
	if s.Steps["b"].Status != StatusSkipped {
		t.Errorf("b should be skipped, got %s", s.Steps["b"].Status)
	}
	if s.Steps["c"].Status != StatusSkipped {
		t.Errorf("c should be skipped (transitive), got %s", s.Steps["c"].Status)
	}
}

func TestState_MarkFailed_RefOrDefaultDoesNotCascade(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a"},
		StepRecord{StepID: "b", Deps: []string{"a"}, ArgsJSON: refArgs(Ref{StepID: "a", Mode: RefModeOrDefault, Default: json.RawMessage(`0`)})},
	)
	ready, skipped, err := s.MarkFailed("a", "boom")
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Errorf("RefOrDefault should not cascade-skip; got skipped: %v", skipped)
	}
	if len(ready) != 1 || ready[0] != "b" {
		t.Errorf("b should be ready now (default substitution); ready=%v", ready)
	}
}

func TestState_MarkFailed_IsIdempotent(t *testing.T) {
	s := makeState(StepRecord{StepID: "a", Status: StatusFailed})
	ready, skipped, err := s.MarkFailed("a", "again")
	if err != nil || ready != nil || skipped != nil {
		t.Errorf("second MarkFailed: ready=%v skipped=%v err=%v", ready, skipped, err)
	}
}

func TestState_AddStep_Dynamic(t *testing.T) {
	s := makeState(StepRecord{StepID: "a", Status: StatusDone})
	err := s.AddStep(StepRecord{StepID: "b", Deps: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	if s.Steps["b"].Status != StatusPending {
		t.Errorf("default status: %s", s.Steps["b"].Status)
	}
	ready := s.ReadyToRun()
	if len(ready) != 1 || ready[0] != "b" {
		t.Errorf("dynamic b should be ready since a is done; ready=%v", ready)
	}
}

func TestState_AddStep_Duplicate(t *testing.T) {
	s := makeState(StepRecord{StepID: "a"})
	if err := s.AddStep(StepRecord{StepID: "a"}); err != ErrDuplicateStep {
		t.Errorf("want ErrDuplicateStep, got %v", err)
	}
}

func TestState_Terminal_AllDone(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a", Status: StatusDone},
		StepRecord{StepID: "b", Status: StatusDone},
	)
	status, done := s.Terminal()
	if !done || status != DAGStatusDone {
		t.Errorf("status=%s done=%v", status, done)
	}
}

func TestState_Terminal_MandatoryFailed(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a", Status: StatusDone},
		StepRecord{StepID: "b", Status: StatusFailed},
	)
	status, done := s.Terminal()
	if !done || status != DAGStatusFailed {
		t.Errorf("status=%s done=%v, want failed/true", status, done)
	}
}

func TestState_Terminal_OptionalFailed_DAGIsDone(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a", Status: StatusDone},
		StepRecord{StepID: "b", Status: StatusFailed, Optional: true},
	)
	status, done := s.Terminal()
	if !done || status != DAGStatusDone {
		t.Errorf("optional failure should not fail DAG; status=%s done=%v", status, done)
	}
}

func TestState_Terminal_StillRunning(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a", Status: StatusDone},
		StepRecord{StepID: "b", Status: StatusRunning},
	)
	status, done := s.Terminal()
	if done {
		t.Error("should not be done")
	}
	if status != DAGStatusRunning {
		t.Errorf("status=%s", status)
	}
}

func TestState_ReadyToRun_SkipsWithUnfinishedDep(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a", Status: StatusRunning},
		StepRecord{StepID: "b", Deps: []string{"a"}, ArgsJSON: refArgs(Ref{StepID: "a", Mode: RefModeRequired})},
	)
	ready := s.ReadyToRun()
	if len(ready) != 0 {
		t.Errorf("want no ready steps (a is running), got %v", ready)
	}
}

func TestState_After_CascadesOnFail(t *testing.T) {
	// b depends on a via After() — no Ref in args.
	s := makeState(
		StepRecord{StepID: "a"},
		StepRecord{StepID: "b", Deps: []string{"a"}, ArgsJSON: json.RawMessage(`[]`)},
	)
	_, skipped, err := s.MarkFailed("a", "boom")
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0] != "b" {
		t.Errorf("want b cascade-skipped, got %v", skipped)
	}
	if s.Steps["b"].Status != StatusSkipped {
		t.Errorf("b status: %s", s.Steps["b"].Status)
	}
}

func TestState_AfterAny_DoesNotCascadeOnFail(t *testing.T) {
	// b has OptionalDeps on a. a fails, b should still become ready.
	s := makeState(
		StepRecord{StepID: "a"},
		StepRecord{StepID: "b", OptionalDeps: []string{"a"}, ArgsJSON: json.RawMessage(`[]`)},
	)
	ready, skipped, err := s.MarkFailed("a", "boom")
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Errorf("AfterAny must not cascade; got skipped=%v", skipped)
	}
	if len(ready) != 1 || ready[0] != "b" {
		t.Errorf("b should be ready after a's terminal transition; ready=%v", ready)
	}
}

func TestState_AfterAny_WaitsForTerminal(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a", Status: StatusRunning},
		StepRecord{StepID: "b", OptionalDeps: []string{"a"}, ArgsJSON: json.RawMessage(`[]`)},
	)
	ready := s.ReadyToRun()
	if len(ready) != 0 {
		t.Errorf("b should wait for a to be terminal; ready=%v", ready)
	}
}

func TestState_After_TerminalParent_EnablesRun(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a", Status: StatusDone},
		StepRecord{StepID: "b", Deps: []string{"a"}, ArgsJSON: json.RawMessage(`[]`)},
	)
	ready := s.ReadyToRun()
	if len(ready) != 1 || ready[0] != "b" {
		t.Errorf("b should be ready once a done; ready=%v", ready)
	}
}

func TestState_Mixed_After_And_RefOrDefault(t *testing.T) {
	// b has: RefOrDefault(a) in args (so Deps contains a); After(c) adds c.
	// a fails → b uses default, does NOT cascade. c fails separately → b cascade.
	s := makeState(
		StepRecord{StepID: "a"},
		StepRecord{StepID: "c"},
		StepRecord{StepID: "b", Deps: []string{"a", "c"}, ArgsJSON: refArgs(
			Ref{StepID: "a", Mode: RefModeOrDefault, Default: json.RawMessage(`0`)},
		)},
	)
	// Fail a: b should NOT cascade (RefOrDefault).
	_, skipped, _ := s.MarkFailed("a", "boom")
	if len(skipped) != 0 {
		t.Errorf("RefOrDefault should prevent cascade; got %v", skipped)
	}
	// Now fail c: b has no RefOrDefault for c → cascade expected.
	_, skipped2, _ := s.MarkFailed("c", "boom")
	if len(skipped2) != 1 || skipped2[0] != "b" {
		t.Errorf("After-style dep without matching RefOrDefault should cascade; got %v", skipped2)
	}
}

func TestState_ReadyToRun_FanIn(t *testing.T) {
	s := makeState(
		StepRecord{StepID: "a", Status: StatusDone},
		StepRecord{StepID: "b", Status: StatusDone},
		StepRecord{StepID: "c", Deps: []string{"a", "b"}, ArgsJSON: refArgs(
			Ref{StepID: "a", Mode: RefModeRequired},
			Ref{StepID: "b", Mode: RefModeRequired},
		)},
	)
	ready := s.ReadyToRun()
	if len(ready) != 1 || ready[0] != "c" {
		t.Errorf("fan-in c should be ready once both parents done; ready=%v", ready)
	}
}
