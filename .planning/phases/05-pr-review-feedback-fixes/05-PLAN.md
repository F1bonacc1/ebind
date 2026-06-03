# Phase 5: PR Review Feedback Fixes

## must_haves

- [ ] Resume from `pausing` state works (CAS-safe, mid-drain)
- [ ] All-terminal paused DAGs finalize via event path, not just sweep
- [ ] Sweep runs periodically while leader (not edge-triggered only)

---

## Wave 1: Resume from Pausing + State Helpers

### Plan 05-01: Resume from Pausing + State Helpers

**Objective:** Allow `Resume()` to transition a DAG from `pausing` to `running`. Add `AllStepsTerminal()` and `DeriveFinalStatus()` helpers to `DAGState`.

**Depends on:** Nothing
**Files modified:** `workflow/state.go`, `workflow/pause.go`, `workflow/state_test.go`
**Autonomous:** yes
**Wave:** 1

#### Task 1: Add AllStepsTerminal and DeriveFinalStatus to DAGState

<read_first>
- workflow/state.go (DAGState methods, step iteration)
- workflow/state_test.go (existing state tests)
</read_first>

<action>
1. Add method `AllStepsTerminal() bool` to `DAGState` in `workflow/state.go`. It iterates all steps in `state.Steps` and returns true only when every step has a terminal status (`StepStatusCompleted`, `StepStatusFailed`, `StepStatusSkipped`). Steps must exist (non-zero length). In-flight or pending steps return false.
2. Add method `DeriveFinalStatus() DAGStatus` to `DAGState` in `workflow/state.go`. Returns `DAGStatusFailed` if any step is `StepStatusFailed`, otherwise `DAGStatusCompleted`. Only meaningful when `AllStepsTerminal()` is true.
3. Add tests in `workflow/state_test.go`:
   - `TestAllStepsTerminal`: empty steps, all terminal mixed statuses, one in-flight step.
   - `TestDeriveFinalStatus`: all completed → completed, any failed → failed.
</action>

<acceptance_criteria>
- `grep -q 'func.*AllStepsTerminal.*bool' workflow/state.go` passes
- `grep -q 'func.*DeriveFinalStatus.*DAGStatus' workflow/state.go` passes
- `go test ./workflow/ -run 'TestAllStepsTerminal|TestDeriveFinalStatus' -v` exits 0 and shows all subtests pass
</acceptance_criteria>

#### Task 2: Allow Resume from Pausing

<read_first>
- workflow/pause.go (Pause() implementation)
- workflow/resume.go (Resume() implementation — may not exist yet; check pause.go for Resume)
- workflow/state.go (DAGStatus constants, CanResume or similar gate)
- workflow/scheduler.go (pausing→paused onCompleted path) to understand the CAS abort pattern
</read_first>

<action>
1. In the `Resume()` function (likely in `workflow/pause.go` or `workflow/resume.go`), modify the status check to accept `DAGStatusPausing` in addition to `DAGStatusPaused` as a valid pre-state for resumption. The existing retry loop already uses CAS on meta, so if a concurrent pausing→paused transition wins, the CAS will fail with `ErrStaleRevision` and the resume will retry — the retry will then see `DAGStatusPaused` (still acceptable) or `DAGStatusPausing` again.
2. Update the event guard in `handleEvent` in `workflow/scheduler.go`: the gate at the top of `handleEvent` that returns nil for `DAGStatusPaused` must NOT block `EventResumed`. Since the existing comment already says "resume event won't reach here because Resume CAS-writes running first", ensure this remains true and add a test for the race.
3. Add tests:
   - Resume from `pausing` succeeds (DAG goes to `running`)
   - Resume from `pausing` while last step completes concurrently (CAS race, either resume or pausing→paused wins, both consistent)
   - Resume from `paused` still works (regression)
   - Resume from `running` still fails (regression)
</action>

<acceptance_criteria>
- `go test ./workflow/ -run 'TestResumeFromPausing' -v` exits 0
- `go test ./workflow/ -run 'TestResumeFromPaused' -v` exits 0 (regression)
- `go test ./workflow/ -count=1 -race` exits 0
</acceptance_criteria>

---

## Wave 2: Event-Path Finalization + Periodic Sweep

### Plan 05-02: Event-Path Finalization + Periodic Sweep

**Objective:** Move all-terminal finalization of paused DAGs from sweep-only to the event path. Make sweep periodic while leader.

**Depends on:** Plan 05-01 (needs `AllStepsTerminal()` and `DeriveFinalStatus()`)
**Files modified:** `workflow/scheduler.go`, `workflow/scheduler_test.go`
**Autonomous:** yes
**Wave:** 2

#### Task 1: All-Terminal Finalization in Event Path

<read_first>
- workflow/scheduler.go (onCompleted method, sweep Paused case)
- workflow/state.go (AllStepsTerminal, DeriveFinalStatus from Plan 05-01)
</read_first>

<action>
1. In `onCompleted` in `workflow/scheduler.go`, after the existing pausing→paused transition block (which fires when `DAGStatusPausing && !HasInFlightSteps`), add a check: if the DAG is now `DAGStatusPaused` (either just transitioned or already was) AND `state.AllStepsTerminal()` is true, derive the final status via `state.DeriveFinalStatus()` and CAS-write meta to that final status (`DAGStatusCompleted` or `DAGStatusFailed`).
2. On the resume path (when `EventResumed` is handled), also call a helper `maybeFinalize` that checks if the resumed DAG is `DAGStatusRunning` and all steps are already terminal — if so, finalize immediately (this handles the edge case where a paused DAG with all terminal steps gets resumed and should skip straight to completion).
3. The sweep's paused→finalized block (currently the only path) should remain as crash recovery/repair only — but with the periodic sweep (Task 2), it will actually run if needed.
4. Add tests:
   - Paused DAG with all steps terminal auto-finalizes via `onCompleted` event
   - Resumed DAG with all steps terminal finalizes immediately
   - Paused DAG with in-flight steps does NOT finalize
</action>

<acceptance_criteria>
- `go test ./workflow/ -run 'TestPausedAutoFinalize|TestResumeTerminalFinalize' -v` exits 0
- `grep -q 'AllStepsTerminal\|maybeFinalize' workflow/scheduler.go` passes
- `go test ./workflow/ -count=1 -race` exits 0
</acceptance_criteria>

#### Task 2: Periodic Sweep While Leader

<read_first>
- workflow/scheduler.go (sweep method, watchLeadership, sweepTicker)
- workflow/workflow.go (Scheduler creation, Options)
</read_first>

<action>
1. Modify the `watchLeadership` method (or the leadership goroutine) in `workflow/scheduler.go` to run sweep periodically while leader, not just once on the false→true edge. Use the existing `sweepInterval` ticker (or add one if absent).
2. Preserve the edge-triggered full sweep on leadership acquisition (the existing behavior) — add a periodic sweep on top of it.
3. The periodic sweep should:
   - Run every `s.sweepInterval` (default 5s) while leader
   - Not overlap with itself (use existing overlap guard or add one)
   - Not block the event loop (run in a goroutine or use the existing sweep called from the leadership loop)
4. The pausing→paused recovery case in sweep remains as repair-only; with periodic sweeps it will now actually catch stuck pausing DAGs.
5. The paused→finalized case in sweep remains as repair-only; with periodic sweeps it will catch any that the event path missed (e.g., leader crash recovery).
6. Add tests to verify sweep runs periodically while leader and does not run while non-leader.
</action>

<acceptance_criteria>
- `grep -q 'sweepInterval\|sweepTicker\|time\.NewTicker' workflow/scheduler.go` passes
- `go test ./workflow/ -run 'TestSweepPeriodic' -v -count=1` exits 0
- `go test ./workflow/ -count=1 -race` exits 0
</acceptance_criteria>

#### Task 3: Cover Remaining Off-by-One in Sweep

<read_first>
- workflow/scheduler.go (sweep method, loadState usage)
</read_first>

<action>
1. In the `sweep` method, verify that the `DAGStatusPaused` case re-reads meta with a fresh `GetMeta` call before finalizing (not using the stale `dag.Status` from the listing). If it already does, verify this is correct. If not, add the fresh read pattern consistent with the `DAGStatusPausing` case.
</action>

<acceptance_criteria>
- `grep -n 'GetMeta' workflow/scheduler.go | grep -i paused` shows the paused case reads fresh meta
- `go test ./workflow/ -count=1 -race` exits 0
</acceptance_criteria>
