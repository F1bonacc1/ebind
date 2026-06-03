# Phase 3: Pause API + Resume API - Context

**Gathered:** 2026-06-03
**Status:** Ready for planning

<domain>
## Phase Boundary

Go API functions Pause(ctx, wf, dagID) and Resume(ctx, wf, dagID) following the same pattern as Cancel(). Pause transitions running→pausing (or→paused if zero in-flight), Resume transitions paused→running and triggers step re-evaluation via event bus.

</domain>

<decisions>
## Implementation Decisions

### API signature
- **D-21:** Standalone functions `Pause(ctx, wf, dagID)` and `Resume(ctx, wf, dagID)` — consistent with existing `Cancel(ctx, wf, dagID)` pattern
- **D-22:** Return `error` only — same as Cancel, keep it simple
- **D-23:** No options structs for now (no PauseReason, no force flags) — can add later via functional options pattern

### Resume dispatch
- **D-24:** Resume publishes a synthetic event (via event bus) rather than directly enqueuing — goes through scheduler's normal serialized path
- **D-25:** Resume transitions meta→running, then publishes `Event{Kind: "resumed"}` on the bus for the scheduler to process

### Sentinel errors
- **D-26:** New sentinel errors: `ErrDAGNotRunning` and `ErrDAGNotPaused`
- **D-27:** Wrapped errors include dagID and current status: `fmt.Errorf("ebind: cannot pause DAG %s: status is %s: %w", dagID, meta.Status, ErrDAGNotRunning)`

### Pause behavior
- **D-28:** `Pause` checks `CanPause()` — returns `ErrDAGNotRunning` if not running (covers done, failed, canceled)
- **D-29:** If `HasInFlightSteps()` is false, CAS-transition directly running→paused (skip pausing)
- **D-30:** If `HasInFlightSteps()` is true, CAS-transition running→pausing (let scheduler auto-transition to paused when last step finishes)

### Resume behavior
- **D-31:** `Resume` checks `CanResume()` — returns `ErrDAGNotPaused` if not paused
- **D-32:** CAS-transition paused→running, then publish resume event to bus

### the agent's Discretion
- Exact resume event kind string
- Where to publish new sentinel errors (workflow/errors.go)
- Helper function for the common CAS pattern (GetMeta → check → PutMeta)
- test file for API functions (workflow/pause_api_test.go or similar)

</decisions>

<canonical_refs>
## Canonical References

### API pattern to follow
- `workflow/cancel.go` — Cancel function signature, CAS pattern, step iteration
- `workflow/errors.go` — Existing sentinel errors (ErrStepNotFound, ErrStaleRevision, etc.)
- `workflow/workflow.go` — Workflow struct with Store, Bus, Enq fields

### State machine primitives
- `workflow/state.go` — DAGStatusRunning/Pausing/Paused, DAGState.CanPause(), CanResume(), HasInFlightSteps()
- `workflow/scheduler.go` — Event types, EventBus publish pattern

### Event types
- `workflow/events.go` — EventKind constants, Event struct, EventSubject, MarshalEvent
- `.planning/phases/01-state-machine-pure-logic/01-CONTEXT.md` — Phase 1 decisions (CanPause, CanResume, HasInFlightSteps)
- `.planning/phases/02-scheduler-pause-awareness/02-CONTEXT.md` — Phase 2 decisions (scheduler already handles events)

### Requirements
- `.planning/REQUIREMENTS.md` — API-01 through API-04

</canonical_refs>

<code_context>
## Existing Code Insights

### Cancel pattern (reference implementation in cancel.go)
```go
func Cancel(ctx context.Context, wf *Workflow, dagID string) error {
    for attempt := 0; attempt < 5; attempt++ {
        meta, rev, err := wf.Store.GetMeta(ctx, dagID)
        if err != nil { return err }
        switch meta.Status {
        case DAGStatusDone, DAGStatusFailed, DAGStatusCanceled:
            return nil
        case DAGStatusPausing, DAGStatusPaused:
            // explicit fallthrough
        }
        meta.Status = DAGStatusCanceled
        if err := wf.Store.PutMeta(ctx, dagID, meta, rev); err != nil {
            if errors.Is(err, ErrStaleRevision) { continue }
            return err
        }
        break
    }
    // then iterate steps...
}
```

### Pause API shape
```go
func Pause(ctx context.Context, wf *Workflow, dagID string) error {
    // 5-retry CAS:
    // 1. GetMeta
    // 2. Check CanPause() — if not, return ErrDAGNotRunning with context
    // 3. If HasInFlightSteps() → meta.Status = DAGStatusPausing
    //    Else → meta.Status = DAGStatusPaused (skip pausing)
    // 4. PutMeta with CAS
    // 5. On ErrStaleRevision, retry
}
```

### Resume API shape
```go
func Resume(ctx context.Context, wf *Workflow, dagID string) error {
    // 5-retry CAS:
    // 1. GetMeta
    // 2. Check CanResume() — if not, return ErrDAGNotPaused with context
    // 3. meta.Status = DAGStatusRunning
    // 4. PutMeta with CAS
    // 5. On ErrStaleRevision, retry

    // After CAS success:
    // ev := Event{Kind: EventResumed, DAGID: dagID}
    // data, _ := MarshalEvent(ev)
    // wf.Bus.Publish(ctx, EventSubject(ev), data)
}
```

### Integration points
- New file `workflow/pause.go` for Pause and Resume functions
- New sentinel errors in `workflow/errors.go`
- Event kind `EventResumed` in `workflow/events.go`
- The resume event needs scheduler handling (modify handleEvent in scheduler.go to accept EventResumed — but that's Phase 2's scheduler code, already done via event dispatch)

</code_context>

<deferred>
## Deferred Ideas

- PauseReason/ResumeReason fields — can add as optional parameter later
- Force pause flag (cancel in-flight steps) — out of scope per PROJECT.md
- Async pause (fire-and-forget with notification) — v2

</deferred>

---

*Phase: 03-pause-api-resume-api*
*Context gathered: 2026-06-03*
