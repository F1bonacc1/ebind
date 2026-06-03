---
phase: 03-pause-api-resume-api
plan: 01
subsystem: api
tags: [go, workflow, pause, resume, dag, state-machine]

requires:
  - phase: 01-state-machine-pure-logic
    provides: DAGStatusPausing/Paused states, CanPause/CanResume/HasInFlightSteps state helpers, PausedAt meta field
  - phase: 02-scheduler-pause-awareness
    provides: Scheduler pausing→paused auto-transition, paused event gating, sweep pause-awareness

provides:
  - Pause(ctx, wf, dagID) standalone function
  - Resume(ctx, wf, dagID) standalone function
  - ErrDAGNotRunning sentinel error
  - ErrDAGNotPaused sentinel error
  - EventResumed event kind constant
  - Scheduler EventResumed handler (routes to onStepAdded)

affects:
  - 04-cli-pause-resume

tech-stack:
  added: []
  patterns:
    - "Standalone Pause/Resume functions matching Cancel() CAS pattern"
    - "Pause publishes synthetic event on bus for scheduler serialized processing"

key-files:
  created:
    - workflow/pause.go
  modified:
    - workflow/errors.go
    - workflow/events.go
    - workflow/scheduler.go

key-decisions:
  - "Pause() without options struct for now (no PauseReason, no force flag)"
  - "Resume() publishes EventResumed on bus instead of direct enqueue — goes through scheduler's serialized event path"
  - "EventResumed StepID set to dagID to ensure valid NATS subject (empty StepID would produce invalid trailing-dot subject)"

patterns-established:
  - "New sentinel errors follow existing workflow/errors.go pattern with workflow: prefix"
  - "New EventKind constants use lowercase string convention consistent with existing events"
  - "Scheduler EventResumed handler delegates to onStepAdded for ready-step re-evaluation"

requirements-completed: [API-01, API-02, API-03, API-04]

duration: 8min
completed: 2026-06-03
---

# Phase 03 Plan 01: Pause/Resume API Functions Summary

**Standalone Pause() and Resume() Go API functions with sentinel errors, EventResumed constant, and scheduler handler — matching the Cancel() CAS pattern**

## Performance

- **Duration:** 8 min
- **Started:** 2026-06-03
- **Completed:** 2026-06-03
- **Tasks:** 2
- **Files modified:** 4

## Accomplishments

- `Pause(ctx, wf, dagID)` — transitions running→pausing (with in-flight steps) or running→paused (no in-flight), using 5-attempt CAS retry loop
- `Resume(ctx, wf, dagID)` — transitions paused→running, publishes `EventResumed` on the event bus for scheduler serialized step re-evaluation
- `ErrDAGNotRunning` and `ErrDAGNotPaused` sentinel errors with descriptive wrapped error messages
- `EventResumed EventKind = "resumed"` constant added to events.go
- Scheduler `handleEvent` now routes `EventResumed` to `onStepAdded` for ready-step re-enueue
- All error messages include dagID and current status for operational debugging

## Task Commits

Each task was committed atomically:

1. **Task 1: Add sentinel errors, EventResumed constant, and scheduler handler** - `c5bbddc` (feat)
2. **Task 2: Implement Pause() and Resume() in workflow/pause.go** - `cfe1401` (feat)

## Files Created/Modified

- `workflow/pause.go` — New file with `Pause()` and `Resume()` standalone functions (89 lines, created)
- `workflow/errors.go` — Added `ErrDAGNotRunning` and `ErrDAGNotPaused` sentinels (modified)
- `workflow/events.go` — Added `EventResumed EventKind = "resumed"` constant, updated doc comment (modified)
- `workflow/scheduler.go` — Added `case EventResumed` routing to `onStepAdded` in `handleEvent` (modified)

## Decisions Made

- **Publish-vs-direct enqueue for Resume:** Resume publishes a synthetic EventResumed on the event bus (rather than directly re-enqueuing steps), so step dispatch goes through the scheduler's normal serialized `onStepAdded` path. This ensures CAS correctness through the existing `s.mu` lock.
- **EventResumed StepID:** Set to `dagID` (the DAG identifier) to produce a valid NATS subject. An empty StepID would produce a subject with a trailing dot and empty token (`DAG.<id>.resumed.`) which is invalid in NATS subjects.
- **No options structs:** Both functions take only `(ctx, wf, dagID)` — no `PauseReason`, force flag, or options. Consistent with the existing `Cancel()` and deferrable to future needs.
- **Wrapped errors:** Both functions return `fmt.Errorf("ebind: ...: %w", ErrDAGNot*)` including dagID and current status for operational debugging, consistent with AGENTS.md conventions.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] Set EventResumed StepID to dagID to avoid invalid NATS subject**
- **Found during:** Task 2 (Implement Pause/Resume)
- **Issue:** The plan's Resume code creates `Event{Kind: EventResumed, DAGID: dagID}` without setting StepID. When passed to `EventSubject()`, this produces `DAG.<dagID>.resumed.` — a subject with an empty trailing token, which is invalid in NATS subjects.
- **Fix:** Set `StepID: dagID` in the EventResumed struct. The handler ignores StepID for EventResumed, and the subject becomes `DAG.<dagID>.resumed.<dagID>` which is valid.
- **Files modified:** `workflow/pause.go`
- **Verification:** `go vet ./workflow/...` and `go build ./...` pass; all existing tests pass.
- **Committed in:** `cfe1401` (Task 2 commit)

---

**Total deviations:** 1 auto-fixed (1 bug)
**Impact on plan:** Minor fix — preserves the plan's intent while ensuring NATS subject validity. No scope creep.

## Issues Encountered

None — execution was straightforward. All changes followed the Cancel() pattern as specified in the plan.

## Known Stubs

None — all code is fully wired. Pause() and Resume() are complete standalone functions.

## Threat Flags

None — threat model from plan covers all introduced surface. CAS mitigations implemented, DoS risk accepted per plan, error messages contain only dagID/status (no secrets).

## Next Phase Readiness

- Pause/Resume API functions are ready for CLI integration (Phase 04)
- Functions are exported from the workflow package and follow the same pattern as Cancel()
- All sentinel errors are available for CLI error-handling patterns
- Scheduler already processes EventResumed (wired to onStepAdded in this plan)

## Self-Check: PASSED

All created files verified, all commits confirmed, all exported symbols present, vet and build pass.

- [x] `workflow/pause.go` exists with `Pause()` and `Resume()` functions
- [x] `workflow/errors.go` contains `ErrDAGNotRunning` and `ErrDAGNotPaused`
- [x] `workflow/events.go` contains `EventResumed EventKind = "resumed"`
- [x] `workflow/scheduler.go` has `case EventResumed:` in handleEvent switch
- [x] `go vet ./...` passes
- [x] `go build ./...` passes
- [x] All existing tests still pass: `go test ./workflow/... -count=1 -race -short` passes

---
*Phase: 03-pause-api-resume-api*
*Completed: 2026-06-03*
