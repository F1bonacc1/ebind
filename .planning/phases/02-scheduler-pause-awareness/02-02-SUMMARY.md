---
phase: 02-scheduler-pause-awareness
plan: 02
subsystem: workflow-scheduler
tags: [pause, scheduler, testing, unit-tests, race-detector]
requires: []
provides:
  - "7 new scheduler pause unit tests covering all four gate specifications"
  - "Bug fix: handleEvent guard allows EventCompleted for pausing DAGs (auto-transition was dead code)"
  - "Bug fix: sweep auto-finalize for paused DAGs uses inline all-terminal check (state.Terminal() returns false for paused meta)"
affects: ["03-pause-resume-api", "04-cli-pause-resume"]
tech-stack:
  added: []
  patterns: ["Sweep auto-finalize uses inline all-terminal check for paused DAGs", "EventCompleted passes handleEvent guard for pausing DAGs; EventStepAdded is gated"]
key-files:
  created: []
  modified:
    - workflow/scheduler_test.go
    - workflow/scheduler.go
key-decisions:
  - "EventCompleted must NOT be gated for pausing DAGs (event-driven auto-transition is primary path)"
  - "Sweep auto-finalize for paused DAGs must check step terminality directly, not via state.Terminal()"
requirements-completed: [TST-02]
duration: 12min
completed: 2026-06-03
---

# Phase 02 Plan 02: Scheduler Pause Unit Tests Summary

**7 scheduler pause unit tests with 2 bug fixes in scheduler.go — event entry gate now allows EventCompleted through for pausing DAGs, and sweep auto-finalize uses inline all-terminal check for paused DAGs**

## Performance

- **Duration:** 12 min
- **Started:** 2026-06-03T11:22:00Z
- **Completed:** 2026-06-03T11:25:07Z
- **Tasks:** 1
- **Files modified:** 2 (391 insertions, 6 deletions)

## Accomplishments

- **TestScheduler_Pause_BlocksDispatch** — validates dispatch gate (SG-02): completing step a while pausing does not enqueue step b
- **TestScheduler_Pause_TransitionsToPaused** — validates pausing→paused auto-transition (SG-04): last in-flight completion transitions meta to paused
- **TestScheduler_Pause_InFlightContinues** — validates in-flight steps complete normally during pause (SG-01): result persisted, step status done
- **TestScheduler_Pause_StepAddedDuringPause** — validates step-added event gating (SG-02): dynamic step stays pending during pause
- **TestScheduler_Sweep_HandlesPausingDAG** — validates sweep recovery (SG-03, D-16): sweep transitions pausing→paused on leader acquire
- **TestScheduler_Sweep_SkipsPausedDAG** — validates sweep skip (SG-03, D-18): paused DAG with pending step gets zero CPU
- **TestScheduler_Sweep_AutoFinalizesPausedDAG** — validates sweep auto-finalize (SG-03, D-17): paused DAG with all terminal steps transitions to done

## Task Commits

Each task was committed atomically:

1. **Task 1: Write scheduler pause unit tests** - `d07fedf` (test)

**Plan metadata:** (final metadata commit managed by orchestrator)

## Files Modified

- `workflow/scheduler.go` — 32 lines changed (2 bug fixes: handleEvent guard and sweep auto-finalize)
- `workflow/scheduler_test.go` — 365 lines added (7 test functions)

## Decisions Made

- **handleEvent guard fix:** EventCompleted must reach onCompleted for pausing DAGs so the pausing→paused auto-transition fires. The original guard (Plan 01) also gated EventCompleted, making the auto-transition dead code. The fix allows EventCompleted through while still gating EventStepAdded.
- **Sweep auto-finalize fix:** `state.Terminal()` returns `(running, false)` for paused meta status by design. Sweep auto-finalize must check step terminality directly rather than using `Terminal()`. The derived final status logic mirrors `Terminal()`'s failure-detection but operates independent of meta status.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 1 - Bug] handleEvent guard blocked EventCompleted for pausing DAGs**
- **Found during:** Task 1 (TestScheduler_Pause_TransitionsToPaused)
- **Issue:** The handleEvent guard (line 195) gated ALL events for pausing DAGs, including EventCompleted. This made the pausing→paused auto-transition in onCompleted unreachable (dead code). Tests expecting the auto-transition to fire would time out.
- **Fix:** Changed the guard to only gate EventStepAdded for pausing DAGs. EventCompleted passes through so the state machine advances and the auto-transition can detect when the last in-flight step finishes.
- **Files modified:** workflow/scheduler.go
- **Verification:** TestScheduler_Pause_TransitionsToPaused passes (polls meta for paused status)
- **Committed in:** d07fedf (Task 1)

**2. [Rule 1 - Bug] Sweep auto-finalize used state.Terminal() which returns false for paused meta**
- **Found during:** Task 1 (TestScheduler_Sweep_AutoFinalizesPausedDAG)
- **Issue:** `state.Terminal()` has a check `if s.Meta.Status == DAGStatusPausing || s.Meta.Status == DAGStatusPaused { return DAGStatusRunning, false }`. The sweep code called `Terminal()` to detect auto-finalization, but it always returned false for paused DAGs, preventing auto-finalization.
- **Fix:** Replaced `Terminal()` call with inline all-terminal check + derived final status logic that mirrors Terminal()'s logic but operates independent of meta status.
- **Files modified:** workflow/scheduler.go
- **Verification:** TestScheduler_Sweep_AutoFinalizesPausedDAG passes (polls meta for done status)
- **Committed in:** d07fedf (Task 1)

---

**Total deviations:** 2 auto-fixed (2 Rule 1 - Bug)
**Impact on plan:** Both fixes are essential for correctness. Without them, the auto-transition and sweep auto-finalization paths would be broken. No scope creep.

## Issues Encountered

- The Plan 01 `handleEvent` guard was implemented too broadly — it applied the pausing/paused gate to all event types, including EventCompleted. This was discovered when writing the auto-transition test (Test 2) which rightfully expected the auto-transition to fire from event processing.
- The `state.Terminal()` method is designed to never return terminal for pausing/paused meta status (by design — paused is not a terminal DAG state). The sweep auto-finalize code incorrectly relied on it.

## Threat Surface Scan

No new threat surface introduced. The two bug fixes operate within the existing CAS-mediated StateStore boundary:
- No new network endpoints
- No new auth paths
- No file access patterns added
- No schema changes
- All status mutations use CAS (revision-based optimistic concurrency), consistent with existing patterns

## Known Stubs

None.

## Next Phase Readiness

- All 7 pause scheduler unit tests pass with -race
- Both implementation bugs from Plan 01 are fixed
- Phase 3 (API: DagPause/DagResume) can proceed with confidence that the scheduler gates work correctly
- Phase 4 (CLI: ebctl dag pause/resume) depends on Phase 3 API

## Self-Check: PASSED

- [x] TestScheduler_Pause_BlocksDispatch function exists
- [x] TestScheduler_Pause_TransitionsToPaused function exists
- [x] TestScheduler_Pause_InFlightContinues function exists
- [x] TestScheduler_Pause_StepAddedDuringPause function exists
- [x] TestScheduler_Sweep_HandlesPausingDAG function exists
- [x] TestScheduler_Sweep_SkipsPausedDAG function exists
- [x] TestScheduler_Sweep_AutoFinalizesPausedDAG function exists
- [x] Commit `d07fedf` exists
- [x] `go test ./workflow/... -run TestScheduler -count=1 -race` — all 16 tests pass (9 existing + 7 new)
- [x] `go vet ./workflow/...` passes

---

*Phase: 02-scheduler-pause-awareness*
*Completed: 2026-06-03*
