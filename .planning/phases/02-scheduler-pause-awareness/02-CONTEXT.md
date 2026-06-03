# Phase 2: Scheduler Pause Awareness - Context

**Gathered:** 2026-06-03
**Status:** Ready for planning

<domain>
## Phase Boundary

Three scheduler gates for pausing/paused DAGs: event entry gate (Ack events without processing), dispatch gate (skip enqueueReady for non-running DAGs), sweep recovery (transition pausing→paused, auto-finalize paused if terminal, skip otherwise). Includes pausing→paused auto-transition in onCompleted when last in-flight step finishes.

</domain>

<decisions>
## Implementation Decisions

### Event entry gate (SG-01)
- **D-11:** `handleEvent` returns early (same pattern as existing `DAGStatusCanceled` guard at line 151) when DAG meta status is `pausing` or `paused` — event is Acked by `onEvent` after `handleEvent` returns normally
- **D-12:** Gate applies to BOTH `EventCompleted` and `EventStepAdded` — no difference, gate all events for non-running DAGs

### pausing→paused auto-transition (SG-04)
- **D-13:** Auto-transition fires in `onCompleted` after MarkDone/MarkFailed — when DAG is `pausing` and `HasInFlightSteps()` returns false, CAS-transition meta to `DAGStatusPaused`
- **D-14:** If CAS fails (concurrent `Resume` won the race), return nil — the Resume is valid, the DAG is now running again, benign CAS loss
- **D-15:** The event-driven transition is the primary path; sweep is the reliable fallback on leader failover

### Sweep recovery (SG-03)
- **D-16:** Sweep transitions `pausing` DAGs to `paused` (recovery for leader crash during pausing)
- **D-17:** Sweep auto-finalizes `paused` DAGs if all steps are terminal (calls `Terminal()` + `maybeFinalize`)
- **D-18:** Sweep skips `paused` DAGs with non-terminal steps (not ready to finalize) — zero CPU
- **Override note:** D-04 (Phase 1) said "once paused, stays paused until explicit resume or cancel" — this is overridden for the specific case where all steps are terminal while paused, allowing auto-finalization. Paused DAGs with pending steps still require explicit resume.

### Dispatch gate (SG-02)
- **D-19:** `enqueueReady` guard at line 205 adds `pausing` and `paused` alongside existing `DAGStatusCanceled` check — skip dispatch for non-running DAGs
- **D-20:** `onStepAdded` is covered by the same guard — no separate logic needed

### the agent's Discretion
- Exact placement of the pausing→paused CAS call in `onCompleted`
- Whether to add a separate `tryCompletePausing` helper function
- Error handling details (logging, error wrapping) for the CAS race in auto-transition

</decisions>

<canonical_refs>
## Canonical References

### Scheduler code
- `workflow/scheduler.go` — Full scheduler: handleEvent (line 146), onCompleted (line 163), onStepAdded (line 198), enqueueReady (line 204), sweep (line 95), maybeFinalize (line 298)
- `workflow/scheduler.go:151` — Existing Canceled guard in handleEvent (model for pausing/paused gate)
- `workflow/scheduler.go:205` — Existing Canceled guard in enqueueReady (model for dispatch gate)
- `workflow/scheduler.go:101` — Existing sweep filter: only DAGStatusRunning

### Phase 1 outputs
- `workflow/state.go` — DAGStatusPausing/Paused constants, HasInFlightSteps(), Phase 1 decisions (D-01 through D-10)
- `.planning/phases/01-state-machine-pure-logic/01-CONTEXT.md` — Phase 1 decisions (D-04 overridden for auto-finalize case)

### Research
- `.planning/research/ARCHITECTURE.md` — Three-gate architecture model
- `.planning/research/PITFALLS.md` — Known pitfalls for scheduler gates

### Requirements
- `.planning/REQUIREMENTS.md` — SG-01 through SG-04, TST-02

</canonical_refs>

<code_context>
## Existing Code Insights

### Event entry gate integration
- `handleEvent` at line 151 checks `state.Meta.Status == DAGStatusCanceled` and returns nil
- Add `|| state.Meta.Status == DAGStatusPausing || state.Meta.Status == DAGStatusPaused`
- The event is still Acked by `onEvent` (line 131-133) because `handleEvent` returns nil (not error)

### Dispatch gate integration
- `enqueueReady` at line 205 checks `state.Meta.Status == DAGStatusCanceled`
- Add `|| state.Meta.Status == DAGStatusPausing || state.Meta.Status == DAGStatusPaused`
- This also gates `onStepAdded` since it calls `enqueueReady`

### Sweep recovery integration
- `sweep` at line 101 filters `dag.Status != DAGStatusRunning`
- Add handling for `DAGStatusPausing`: load state, CAS-transition meta to `DAGStatusPaused`
- Add handling for `DAGStatusPaused`: load state, call `state.Terminal()` — if terminal, CAS-transition to done/failed; if not terminal, skip
- Both operations are wrapped in CAS retry to handle concurrent writers

### pausing→paused auto-transition
- In `onCompleted` (line 163), after step transition + enqueueReady + maybeFinalize flow
- Or more precisely, after step transition but before enqueueReady — if DAG is pausing and no more in-flight steps, transition to paused
- CAS on meta for pausing→paused; if CAS fails (Resume won), return nil

### Test patterns
- `workflow/scheduler_test.go` — Uses `captureEnq` enqueuer + MemStore + toggleElector
- New tests: event gate (completion for paused DAG not dispatched), dispatch gate (enqueueReady skips paused), sweep (pausing→paused transition), auto-transition (onCompleted last step → paused)

</code_context>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope.

</deferred>

---

*Phase: 02-scheduler-pause-awareness*
*Context gathered: 2026-06-03*
