package workflow

import (
	"encoding/json"
	"time"

	"github.com/f1bonacc1/ebind/task"
)

// DAGStatus is the overall workflow status.
type DAGStatus string

const (
	DAGStatusRunning  DAGStatus = "running"
	DAGStatusDone     DAGStatus = "done"
	DAGStatusFailed   DAGStatus = "failed"
	DAGStatusCanceled DAGStatus = "canceled"
	DAGStatusPausing  DAGStatus = "pausing"
	DAGStatusPaused   DAGStatus = "paused"
)

// BPState is the per-position breakpoint lifecycle persisted on StepRecord.
// "" (zero) = breakpoint not yet hit; blocked = advisory "currently stopped
// here" marker written by the scheduler for observability; released = a
// ResumeBreakpoint passed this breakpoint instance. The gate logic only ever
// reads `released` — the blocked mark is never load-bearing.
type BPState string

const (
	BPStateBlocked  BPState = "blocked"
	BPStateReleased BPState = "released"
)

// BPPosition distinguishes before-execution vs after-execution breakpoints.
type BPPosition string

const (
	BPPositionBefore BPPosition = "before"
	BPPositionAfter  BPPosition = "after"
)

// DAGMeta is the meta record stored in the state store (key: <dag_id>/meta).
type DAGMeta struct {
	ID            string            `json:"id"`
	Status        DAGStatus         `json:"status"`
	CreatedAt     time.Time         `json:"created_at"`
	DefaultPolicy *task.RetryPolicy `json:"default_policy,omitempty"`
	PausedAt      time.Time         `json:"paused_at,omitempty"`
	TerminalSteps []string          `json:"terminal_steps,omitempty"`
	// ActiveBreakpoints is the set of armed breakpoint labels, fixed at Submit
	// (WithActiveBreakpoints). Immutable for the DAG's lifetime.
	ActiveBreakpoints []string `json:"active_breakpoints,omitempty"`
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
	ErrorMessage string            `json:"error_message,omitempty"`
	Optional     bool              `json:"optional,omitempty"`
	Held         bool              `json:"held,omitempty"`         // held by Pause(); prevents enqueue
	HeldAt       time.Time         `json:"held_at,omitempty"`      // when the hold was applied; age-gates orphan repair
	BreakBefore  []string          `json:"break_before,omitempty"` // breakpoint labels gating execution start (immutable)
	BreakAfter   []string          `json:"break_after,omitempty"`  // breakpoint labels gating direct dependents after done (immutable)
	BPBefore     BPState           `json:"bp_before,omitempty"`    // ""→blocked(advisory)→released(monotonic)
	BPAfter      BPState           `json:"bp_after,omitempty"`
	BPBlockedAt  time.Time         `json:"bp_blocked_at,omitempty"` // advisory; when the blocked mark was written
	Policy       *task.RetryPolicy `json:"policy,omitempty"`        // per-step override
	Placement    *PlacementSpec    `json:"placement,omitempty"`
	WorkerID     string            `json:"worker_id,omitempty"`
	AddedAt      time.Time         `json:"added_at"`
	StartedAt    time.Time         `json:"started_at,omitempty"`
	FinishedAt   time.Time         `json:"finished_at,omitempty"`
}

// IsTerminal returns true if this step's status cannot change anymore.
func (s StepRecord) IsTerminal() bool {
	return s.Status == StatusDone || s.Status == StatusFailed || s.Status == StatusSkipped || s.Status == StatusCanceled
}

// DAGState is the in-memory view of a DAG — loaded from the store at scheduler
// evaluation time. All transition methods on DAGState are PURE (return data, no IO).
type DAGState struct {
	Meta  DAGMeta
	Steps map[string]StepRecord // stepID -> record
}

// ReadyToRun returns step IDs whose deps are all `done` (or satisfied-via-default)
// and whose status is `pending` and not held. Used after MarkDone/MarkFailed to
// find next work. Held steps are excluded — they are reserved by Pause().
// Steps blocked at an armed before-breakpoint are excluded until released.
func (s *DAGState) ReadyToRun() []string {
	var out []string
	for id, step := range s.Steps {
		if step.Status != StatusPending || step.Held || s.beforeBPBlocks(step) {
			continue
		}
		if s.depsSatisfied(step) {
			out = append(out, id)
		}
	}
	return out
}

// breakpointArmed reports whether any of the step's breakpoint labels is in
// the DAG's active set.
func breakpointArmed(labels, active []string) bool {
	for _, l := range labels {
		if stringSliceContains(active, l) {
			return true
		}
	}
	return false
}

// beforeBPBlocks: this step must not be enqueued — its before-breakpoint is
// armed and not yet released. Deliberately never consults the advisory
// BPStateBlocked mark; the gate is computed from immutable config (labels ×
// active set) plus the monotonic released flag, so racing schedulers can only
// err toward "still blocked".
func (s *DAGState) beforeBPBlocks(step StepRecord) bool {
	return len(step.BreakBefore) > 0 && step.BPBefore != BPStateReleased &&
		breakpointArmed(step.BreakBefore, s.Meta.ActiveBreakpoints)
}

// afterBPHolds: this done step's after-breakpoint is armed and not released,
// so its direct dependents must not start. Gates only StatusDone — failed/
// skipped/canceled parents propagate normally (cascade-skip or RefOrDefault).
func (s *DAGState) afterBPHolds(step StepRecord) bool {
	return step.Status == StatusDone && len(step.BreakAfter) > 0 &&
		step.BPAfter != BPStateReleased &&
		breakpointArmed(step.BreakAfter, s.Meta.ActiveBreakpoints)
}

// BlockedAtBefore returns IDs of pending steps currently stopped AT their
// before-breakpoint: deps satisfied (including upstream after-gates) and the
// gate armed. A step behind an unfinished or after-gated dep is not yet
// "stopped here" — it hasn't arrived at its own breakpoint.
func (s *DAGState) BlockedAtBefore() []string {
	var out []string
	for id, step := range s.Steps {
		if step.Status != StatusPending || !s.beforeBPBlocks(step) {
			continue
		}
		if s.depsSatisfied(step) {
			out = append(out, id)
		}
	}
	return out
}

// HoldingAtAfter returns IDs of done steps whose after-breakpoint gate is
// currently active (holding their direct dependents).
func (s *DAGState) HoldingAtAfter() []string {
	var out []string
	for id, step := range s.Steps {
		if s.afterBPHolds(step) {
			out = append(out, id)
		}
	}
	return out
}

// HasInFlightSteps returns true if any step in the DAG is currently running.
// Used to determine whether pausing can skip directly to paused.
func (s *DAGState) HasInFlightSteps() bool {
	for _, step := range s.Steps {
		if step.Status == StatusRunning {
			return true
		}
	}
	return false
}

// AllStepsTerminal returns true when every step in the DAG is in a terminal
// status (done/failed/skipped/canceled). Returns false if there are no steps.
func (s *DAGState) AllStepsTerminal() bool {
	if len(s.Steps) == 0 {
		return false
	}
	for _, step := range s.Steps {
		if !step.IsTerminal() {
			return false
		}
	}
	return true
}

// DeriveFinalStatus returns the overall DAG final status based on step results.
// Only meaningful when AllStepsTerminal() is true.
// Returns DAGStatusFailed if any non-optional step is failed or skipped,
// otherwise DAGStatusDone.
func (s *DAGState) DeriveFinalStatus() DAGStatus {
	for _, step := range s.Steps {
		if !step.Optional && (step.Status == StatusFailed || step.Status == StatusSkipped) {
			return DAGStatusFailed
		}
	}
	return DAGStatusDone
}

// CanPause returns true if the DAG can accept a pause request.
// Must be running (not already pausing/paused/terminal).
func (s *DAGState) CanPause() bool {
	return s.Meta.Status == DAGStatusRunning
}

// CanResume returns true if the DAG can accept a resume request.
// Must be paused or pausing.
func (s *DAGState) CanResume() bool {
	return s.Meta.Status == DAGStatusPaused || s.Meta.Status == DAGStatusPausing
}

// depsSatisfied: all deps (required + optional) are terminal (done/failed/skipped)
// and none is holding at an armed after-breakpoint. A failed/skipped required dep
// is fine for scheduling here — the scheduler's ResolveArgs + cascadeSkipFrom
// decide whether to cascade or substitute default.
func (s *DAGState) depsSatisfied(step StepRecord) bool {
	for _, dep := range step.Deps {
		d, ok := s.Steps[dep]
		if !ok || !d.IsTerminal() || s.afterBPHolds(d) {
			return false
		}
	}
	for _, dep := range step.OptionalDeps {
		d, ok := s.Steps[dep]
		if !ok || !d.IsTerminal() || s.afterBPHolds(d) {
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
func (s *DAGState) MarkFailed(stepID, errorKind, errorMessage string) (newlyReady, newlySkipped []string, err error) {
	step, ok := s.Steps[stepID]
	if !ok {
		return nil, nil, ErrStepNotFound
	}
	if step.Status != StatusFailed {
		step.Status = StatusFailed
		step.ErrorKind = errorKind
		step.ErrorMessage = errorMessage
		step.FinishedAt = time.Now().UTC()
		s.Steps[stepID] = step
	}
	cascaded := s.cascadeSkipFrom(stepID)
	return s.ReadyToRun(), cascaded, nil
}

// MarkSkipped transitions to `skipped` (idempotent) and cascade-skips downstream.
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
	if s.Meta.Status == DAGStatusCanceled {
		return DAGStatusCanceled, true
	}
	if s.Meta.Status == DAGStatusPausing || s.Meta.Status == DAGStatusPaused {
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
