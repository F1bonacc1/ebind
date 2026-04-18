package workflow

import (
	"encoding/json"
	"time"

	"github.com/f1bonacc1/ebind/task"
)

// DAGStatus is the overall workflow status.
type DAGStatus string

const (
	DAGStatusRunning DAGStatus = "running"
	DAGStatusDone    DAGStatus = "done"
	DAGStatusFailed  DAGStatus = "failed"
)

// DAGMeta is the meta record stored in the state store (key: <dag_id>/meta).
type DAGMeta struct {
	ID             string            `json:"id"`
	Status         DAGStatus         `json:"status"`
	CreatedAt      time.Time         `json:"created_at"`
	DefaultPolicy  *task.RetryPolicy `json:"default_policy,omitempty"`
	TerminalSteps  []string          `json:"terminal_steps,omitempty"`
}

// StepRecord is stored per-step (key: <dag_id>/step/<step_id>).
//
// Dependency model:
//   - Deps: required deps. Step waits until each is terminal. If any ends as
//     failed/skipped AND the args don't contain a RefOrDefault on it, this step
//     is cascade-skipped. Combines Ref-derived deps from args + After() options.
//   - OptionalDeps: "wait for completion but don't cascade on failure."
//     Step waits until each is terminal; failure never causes cascade.
//     Populated by AfterAny() options.
type StepRecord struct {
	DAGID        string            `json:"dag_id"`
	StepID       string            `json:"step_id"`
	FnName       string            `json:"fn_name"`
	ArgsJSON     json.RawMessage   `json:"args_json"` // JSON array; may contain Refs
	Deps         []string          `json:"deps,omitempty"`
	OptionalDeps []string          `json:"optional_deps,omitempty"`
	Status       StepStatus        `json:"status"`
	Attempt      int               `json:"attempt"`
	ErrorKind    string            `json:"error_kind,omitempty"`
	Optional     bool              `json:"optional,omitempty"`
	Policy       *task.RetryPolicy `json:"policy,omitempty"` // per-step override
	AddedAt      time.Time         `json:"added_at"`
	StartedAt    time.Time         `json:"started_at,omitempty"`
	FinishedAt   time.Time         `json:"finished_at,omitempty"`
}

// IsTerminal returns true if this step's status cannot change anymore.
func (s StepRecord) IsTerminal() bool {
	return s.Status == StatusDone || s.Status == StatusFailed || s.Status == StatusSkipped
}

// DAGState is the in-memory view of a DAG — loaded from the store at scheduler
// evaluation time. All transition methods on DAGState are PURE (return data, no IO).
type DAGState struct {
	Meta  DAGMeta
	Steps map[string]StepRecord // stepID -> record
}

// ReadyToRun returns step IDs whose deps are all `done` (or satisfied-via-default)
// and whose status is `pending`. Used after MarkDone/MarkFailed to find next work.
func (s *DAGState) ReadyToRun() []string {
	var out []string
	for id, step := range s.Steps {
		if step.Status != StatusPending {
			continue
		}
		if s.depsSatisfied(step) {
			out = append(out, id)
		}
	}
	return out
}

// depsSatisfied: all deps (required + optional) are terminal (done/failed/skipped).
// A failed/skipped required dep is fine for scheduling here — the scheduler's
// ResolveArgs + cascadeSkipFrom decide whether to cascade or substitute default.
func (s *DAGState) depsSatisfied(step StepRecord) bool {
	for _, dep := range step.Deps {
		d, ok := s.Steps[dep]
		if !ok || !d.IsTerminal() {
			return false
		}
	}
	for _, dep := range step.OptionalDeps {
		d, ok := s.Steps[dep]
		if !ok || !d.IsTerminal() {
			return false
		}
	}
	return true
}

// MarkDone transitions a step to `done` status (caller must also PutResult in the store).
// Returns step IDs that became ReadyToRun as a result.
func (s *DAGState) MarkDone(stepID string) ([]string, error) {
	step, ok := s.Steps[stepID]
	if !ok {
		return nil, ErrStepNotFound
	}
	if step.Status == StatusDone {
		return nil, nil // idempotent
	}
	step.Status = StatusDone
	step.FinishedAt = time.Now().UTC()
	s.Steps[stepID] = step
	return s.ReadyToRun(), nil
}

// MarkFailed transitions to `failed` (idempotent) and cascade-skips all downstream
// steps whose required deps are unsatisfied by the failure. Returns (newlyReady, newlySkipped).
//
// The cascade runs unconditionally even when the step is already Failed — the
// hook may have written the Failed status to the store before this event was
// delivered, so the transition looks idempotent but the cascade still needs to
// happen to propagate the skip downstream.
func (s *DAGState) MarkFailed(stepID, errorKind string) (newlyReady, newlySkipped []string, err error) {
	step, ok := s.Steps[stepID]
	if !ok {
		return nil, nil, ErrStepNotFound
	}
	if step.Status != StatusFailed {
		step.Status = StatusFailed
		step.ErrorKind = errorKind
		step.FinishedAt = time.Now().UTC()
		s.Steps[stepID] = step
	}
	cascaded := s.cascadeSkipFrom(stepID)
	return s.ReadyToRun(), cascaded, nil
}

// MarkSkipped transitions to `skipped` (idempotent) and cascade-skips downstream.
// Cascade runs unconditionally — see MarkFailed for rationale.
func (s *DAGState) MarkSkipped(stepID string) ([]string, error) {
	step, ok := s.Steps[stepID]
	if !ok {
		return nil, ErrStepNotFound
	}
	if step.Status != StatusSkipped {
		step.Status = StatusSkipped
		step.FinishedAt = time.Now().UTC()
		s.Steps[stepID] = step
	}
	cascaded := s.cascadeSkipFrom(stepID)
	return cascaded, nil
}

// cascadeSkipFrom walks down from a failed/skipped step and cascade-skips every
// dependent that was waiting on it in a required way. Transitive.
//
// A dependent D is cascade-skipped when `target` failed/skipped if:
//   - target ∈ D.Deps (required), AND
//   - D's args DO NOT contain a RefOrDefault on target (if they do, D will run
//     with the default substituted and should not be skipped).
//
// Steps that declared target only via AfterAny (in OptionalDeps) never cascade.
func (s *DAGState) cascadeSkipFrom(rootID string) []string {
	var skipped []string
	queue := []string{rootID}
	seen := map[string]bool{rootID: true}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for otherID, other := range s.Steps {
			if other.Status != StatusPending && other.Status != StatusRunning {
				continue
			}
			if !stringSliceContains(other.Deps, cur) {
				continue
			}
			if hasRefMode(other.ArgsJSON, cur, RefModeOrDefault) {
				continue // RefOrDefault handles the failure gracefully
			}
			other.Status = StatusSkipped
			other.FinishedAt = time.Now().UTC()
			s.Steps[otherID] = other
			skipped = append(skipped, otherID)
			if !seen[otherID] {
				seen[otherID] = true
				queue = append(queue, otherID)
			}
		}
	}
	return skipped
}

// hasRefMode returns true if argsJSON contains a Ref with the given mode pointing at targetStepID.
func hasRefMode(argsJSON json.RawMessage, targetStepID string, mode RefMode) bool {
	var rawArgs []json.RawMessage
	if err := json.Unmarshal(argsJSON, &rawArgs); err != nil {
		return false
	}
	for _, raw := range rawArgs {
		if ref, ok := DecodeRef(raw); ok {
			if ref.StepID == targetStepID && ref.Mode == mode {
				return true
			}
		}
	}
	return false
}

// AddStep inserts a new step record into the state (used for dynamic DAGs).
// Returns error on duplicate ID.
func (s *DAGState) AddStep(rec StepRecord) error {
	if _, exists := s.Steps[rec.StepID]; exists {
		return ErrDuplicateStep
	}
	if rec.Status == "" {
		rec.Status = StatusPending
	}
	if rec.AddedAt.IsZero() {
		rec.AddedAt = time.Now().UTC()
	}
	s.Steps[rec.StepID] = rec
	return nil
}

// Terminal returns (status, done). done is true if all steps are in terminal states.
// status is:
//   - DAGStatusDone: all mandatory steps reached done (optional may have failed/skipped)
//   - DAGStatusFailed: at least one mandatory step ended in failed or skipped
//   - DAGStatusRunning: at least one step is non-terminal
func (s *DAGState) Terminal() (DAGStatus, bool) {
	allTerminal := true
	hasFailure := false
	for _, step := range s.Steps {
		if !step.IsTerminal() {
			allTerminal = false
		}
		if !step.Optional && (step.Status == StatusFailed || step.Status == StatusSkipped) {
			hasFailure = true
		}
	}
	if !allTerminal {
		return DAGStatusRunning, false
	}
	if hasFailure {
		return DAGStatusFailed, true
	}
	return DAGStatusDone, true
}

func stringSliceContains(ss []string, x string) bool {
	for _, s := range ss {
		if s == x {
			return true
		}
	}
	return false
}
