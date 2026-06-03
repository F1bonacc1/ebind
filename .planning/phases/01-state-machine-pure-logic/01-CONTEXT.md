# Phase 1: State Machine & Pure Logic - Context

**Gathered:** 2026-06-03
**Status:** Ready for planning

<domain>
## Phase Boundary

Foundation types and pure state logic for pausing/paused DAG statuses: add DAGStatusPausing/Paused constants, HasInFlightSteps, CanPause, CanResume, update maybeFinalize and Cancel to handle new states, update Terminal(). Zero IO, no scheduler changes, no API or CLI yet.

</domain>

<decisions>
## Implementation Decisions

### Status values + transitions
- **D-01:** New constants: `DAGStatusPausing` ("pausing") and `DAGStatusPaused` ("paused") following existing lowercase naming convention
- **D-02:** `DagPause` skips directly to `paused` (no `pausing` transition) when zero in-flight steps exist at call time
- **D-03:** `HasInFlightSteps` is unexported — internal helper only

### maybeFinalize + Terminal behavior
- **D-04:** `maybeFinalize` does NOT transition pausing or paused DAGs to any final state — paused DAGs stay paused until explicit resume or cancel
- **D-05:** `Terminal()` treats `pausing` as non-terminal (same class as `running` internally); `paused` is also non-terminal — both keep the DAG alive

### Cancel ↔ Pause interaction
- **D-06:** `Cancel()` transitions `pausing` DAGs to `canceled` (mark pending steps as canceled, leave running steps alone)
- **D-07:** `Cancel()` transitions `paused` DAGs to `canceled` (admin escape hatch works through pause)
- **D-08:** `DagPause()` returns a descriptive error when called on DAGs in `done`, `failed`, or `canceled` state (not idempotent)
- **D-09:** `DagResume()` returns a descriptive error when called on DAGs in `running`, `done`, `failed`, or `canceled` state

### DAGMeta fields
- **D-10:** Add `PausedAt time.Time` field to DAGMeta for CLI display and audit

### the agent's Discretion
- Exact error message wording for invalid pause/resume attempts
- Internal helper function signatures beyond the required public API

</decisions>

<canonical_refs>
## Canonical References

### Existing code patterns
- `workflow/state.go` — Existing DAGStatus constants, DAGMeta struct, Terminal(), state machine transitions
- `workflow/cancel.go` — CAS pattern for Cancel, reference for pause/resume API design
- `workflow/state_test.go` — Test patterns for pure state functions (makeState helper, refArgs)
- `workflow/scheduler.go` — maybeFinalize (lines 298-316), handleEvent (lines 146-161), sweep (lines 95-115)
- `workflow/scheduler.go` — enqueueReady guard at line 205 (existing DAGStatusCanceled check)

### Requirements
- `.planning/REQUIREMENTS.md` — ST-01 through ST-06, TST-01
- `.planning/research/ARCHITECTURE.md` — Architecture design for state machine
- `.planning/research/PITFALLS.md` — Known pitfalls for Phase 1 (maybeFinalize overwrite, pausing never transitions)
</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- `makeState()` test helper in `state_test.go` — creates DAGState from step records for pure unit tests
- `refArgs()` test helper — builds JSON ref arguments for dependency chains
- `DAGState` struct — in-memory state machine with pure transition methods (MarkDone, MarkFailed, MarkSkipped, cascadeSkipFrom, ReadyToRun, Terminal)
- `Cancel()` CAS loop pattern in `cancel.go` — reference for CAS retry with ErrStaleRevision

### Established Patterns
- Lowercase string constant names (DAGStatusRunning = "running")
- Pure functions on DAGState return data, do no IO
- CAS retry loops: 5 attempts, break on success, continue on ErrStaleRevision
- Public API methods on Workflow type return error only
- Unexported helpers for internal logic

### Integration Points
- New DAGStatus constants added alongside existing ones in `workflow/state.go`
- `Cancel()` switch statement needs new cases for pausing/paused (line 18-21)
- `Terminal()` needs update — currently checks DAGStatusCanceled but needs to exclude pausing/paused
- `maybeFinalize` needs guard — currently only checks DAGStatusCanceled (line 308)
- `enqueueReady` guard at line 205 checks DAGStatusCanceled — will need pausing/paused too in Phase 2 (out of scope for Phase 1)
- `sweep` filter at line 101 checks DAGStatusRunning — will need update in Phase 2

</code_context>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope.

</deferred>

---

*Phase: 01-state-machine-pure-logic*
*Context gathered: 2026-06-03*
