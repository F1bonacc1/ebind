# Phase 2: Scheduler Pause Awareness — Research

**Researched:** 2026-06-03
**Confidence:** HIGH (validated against running code + existing test patterns)

---

## Current Code State

Phase 1 completed and committed. All foundation types are in place:

- `workflow/state.go` — `DAGStatusPausing` and `DAGStatusPaused` constants, `HasInFlightSteps()`, `CanPause()`, `CanResume()`, `PausedAt field on DAGMeta`, updated `Terminal()` guards for pausing/paused
- `workflow/cancel.go` — Explicit pausing/paused cases in Cancel() switch (fallthrough to cancel logic)
- `workflow/scheduler.go:308` — `maybeFinalize` guards `DAGStatusCanceled || DAGStatusPausing || DAGStatusPaused`
- `workflow/state_test.go` — 13 pure unit tests for all new functions

## Scheduler Code (workflow/scheduler.go) — Exact Line References

| Line | Function | Current Behavior | Phase 2 Change |
|------|----------|-----------------|----------------|
| 117 | `onEvent` | Checks leader, locks mu, calls handleEvent, Ack | **No change** — gate is in handleEvent |
| 146 | `handleEvent` | Loads state, checks Canceled guard, dispatches by kind | Add pausing/paused guard BEFORE Canceled (or alongside) |
| 151 | `handleEvent` guard | `state.Meta.Status == DAGStatusCanceled → return nil` | Add `|| DAGStatusPausing || DAGStatusPaused` |
| 163 | `onCompleted` | MarkDone/MarkFailed, cascade, enqueueReady, maybeFinalize | Add pausing→paused auto-transition after step transitions |
| 198 | `onStepAdded` | Calls enqueueReady | Gate already covered by enqueueReady guard — but handleEvent guard catches pausing/paused before reaching here |
| 204 | `enqueueReady` | Guard: `DAGStatusCanceled` | Add `|| DAGStatusPausing || DAGStatusPaused` |
| 95 | `sweep` | Filter: `dag.Status != DAGStatusRunning → continue` | Add explicit switch: Running → existing, Pausing → transition to paused, Paused → skip |
| 298 | `maybeFinalize` | Guards Canceled, Pausing, Paused (already done in Phase 1) | **No change needed** |

## Gate 1: Event Entry (SG-01)

**Decision D-11:** `handleEvent` returns early when DAG meta status is `pausing` or `paused`.

Implementation at `handleEvent` line 151:
```go
if state.Meta.Status == DAGStatusCanceled || 
   state.Meta.Status == DAGStatusPausing || 
   state.Meta.Status == DAGStatusPaused {
    return nil
}
```

**Key nuance from D-12:** This gate applies to BOTH `EventCompleted` and `EventStepAdded`. No distinction needed.

**Why Ack (not Nak):** The guard is inside `handleEvent`, which is called by `onEvent`. If `handleEvent` returns `nil` (not error), `onEvent` calls `ev.Ack()`. The step outcome was already persisted to KV by the StepHook before the event was published. ACKing just prevents the scheduler from advancing the state machine — no data loss.

## Gate 2: Dispatch (SG-02)

**Decision D-19:** `enqueueReady` adds `pausing` and `paused` alongside existing `DAGStatusCanceled` check.

Implementation at `enqueueReady` line 205:
```go
if len(ready) == 0 || state.Meta.Status == DAGStatusCanceled || 
   state.Meta.Status == DAGStatusPausing || state.Meta.Status == DAGStatusPaused {
    return nil
}
```

**Decision D-20:** `onStepAdded` is covered by this same guard (it calls `enqueueReady` directly).

**Defense-in-depth:** Even if an event somehow passes Gate 1 (e.g., during the pausing→paused transition window), Gate 2 prevents new dispatches.

## pausing→paused Auto-Transition (SG-04)

**Decision D-13:** Auto-transition fires in `onCompleted` after step transitions (MarkDone/MarkFailed/cascade) but before `maybeFinalize`.

Implementation logic in `onCompleted` (after line 181, before `maybeFinalize`):
```go
// After step transitions + cascade skip, before maybeFinalize:

// If the DAG is pausing and has no in-flight steps, transition to paused.
if state.Meta.Status == DAGStatusPausing && !state.HasInFlightSteps() {
    // Reload meta for fresh revision (CAS safety)
    meta, rev, err := s.wf.Store.GetMeta(ctx, state.Meta.ID)
    if err != nil {
        return err  // will cause Nak — retry on next event
    }
    if meta.Status != DAGStatusPaging {
        return nil  // concurrent Resume or Cancel changed it; benign
    }
    meta.Status = DAGStatusPaused
    meta.PausedAt = time.Now().UTC()
    if err := s.wf.Store.PutMeta(ctx, state.Meta.ID, meta, rev); err != nil {
        if errors.Is(err, ErrStaleRevision) {
            return nil  // benign CAS race (Resume won) — sweep will handle
        }
        return err
    }
    // Don't call maybeFinalize — the DAG is now PAUSED, not terminal
    return nil
}
```

**Decision D-14:** If CAS fails (concurrent Resume won the race), return nil — benign.

**Decision D-15:** The event-driven transition is the primary path. Sweep is the reliable fallback.

**Timing nuance:** The auto-transition check happens AFTER the step transition (MarkDone) and cascade skip are applied to the `state` object, but BEFORE `enqueueReady`. This means:
- The completed step is marked done in state ✓
- Cascade-skipped steps are marked skipped in state ✓
- No new dispatches happen (enqueueReady gate catches pausing/paused) ✓
- The DAG transitions to paused if no more in-flight steps ✓

## Gate 3: Sweep Recovery (SG-03)

**Decision D-16:** Sweep transitions `pausing` DAGs to `paused` (recovery for leader crash during pausing).

**Decision D-17:** Sweep auto-finalizes `paused` DAGs if all steps are terminal.

**Decision D-18:** Sweep skips `paused` DAGs with non-terminal steps (zero CPU).

Implementation replacing the simple filter at sweep line 100-103:
```go
for _, dag := range dags {
    switch dag.Status {
    case DAGStatusRunning:
        // existing logic: load state, enqueue ready
        state, err := s.loadState(ctx, dag.ID)
        if err != nil {
            continue
        }
        s.mu.Lock()
        ready := state.ReadyToRun()
        err = s.enqueueReady(ctx, state, ready)
        s.mu.Unlock()
        _ = err

    case DAGStatusPausing:
        // Load state to check in-flight steps
        state, err := s.loadState(ctx, dag.ID)
        if err != nil {
            continue
        }
        if !state.HasInFlightSteps() {
            // All in-flight steps completed — transition to paused
            meta, rev, err := s.wf.Store.GetMeta(ctx, dag.ID)
            if err != nil {
                continue
            }
            if meta.Status != DAGStatusPausing {
                continue  // concurrent writer already changed it
            }
            meta.Status = DAGStatusPaused
            _ = s.wf.Store.PutMeta(ctx, dag.ID, meta, rev) // CAS; benign fail
        }
        // If still has in-flight steps, do nothing

    case DAGStatusPaused:
        // Check if all steps are terminal (auto-finalize)
        state, err := s.loadState(ctx, dag.ID)
        if err != nil {
            continue
        }
        terminalStatus, done := state.Terminal()
        if !done {
            continue  // not all terminal — skip (zero CPU)
        }
        // All steps terminal — CAS transition to terminal status
        meta, rev, err := s.wf.Store.GetMeta(ctx, dag.ID)
        if err != nil {
            continue
        }
        if meta.Status != DAGStatusPaused {
            continue  // concurrent writer changed it
        }
        // Only finalize if we're paused and all steps are terminal
        // This is the override of D-04: paused DAG with all-terminal steps
        // can auto-finalize instead of staying paused forever
        if terminalStatus == DAGStatusDone || terminalStatus == DAGStatusFailed {
            meta.Status = terminalStatus
            _ = s.wf.Store.PutMeta(ctx, dag.ID, meta, rev)
        }

    default:
        continue // done/failed/canceled
    }
}
```

**Override note (from CONTEXT.md D-17):** D-04 (Phase 1) said "once paused, stays paused until explicit resume or cancel" — this is overridden for the specific case where all steps are terminal while paused, allowing auto-finalization. Paused DAGs with pending steps still require explicit resume.

## Test Plan (TST-02)

### Existing test infrastructure (confirmed working):
- `captureEnq` — captures enqueued tasks for assertion (len, step IDs)
- `emulateHook` — simulates StepHook: writes step status to store + publishes completion event
- `toggleElector` — test elector with runtime leader flip (used in sweep tests)
- `setupDAG` — creates Workflow + DAG + captureEnq with MemStore + MemBus
- `startScheduler` — starts scheduler goroutine with subscription wait
- `waitEnqueued` — polls captureEnq until target count reached

### New test cases needed:

| # | Test Name | What It Validates | SG Req | Pattern |
|---|-----------|-------------------|--------|---------|
| 1 | `TestScheduler_Pause_BlocksDispatch` | Pause a running DAG → step completion event is ACKed but no new dispatches | SG-01, SG-02 | setupDAG with 3-step chain, pause before step 1 complete, verify enqueue stops |
| 2 | `TestScheduler_Pause_TransitionsToPaused` | Last in-flight step completes → meta becomes paused | SG-04 | Setup DAG with 1 running step, pause, complete the step, verify meta == paused |
| 3 | `TestScheduler_Pause_InFlightContinues` | Running step completes normally during pause (result persisted) | SG-01 | Pause DAG with running step, complete it with emulateHook, verify result in store |
| 4 | `TestScheduler_Pause_StepAddedDuringPause` | Dynamic step added during pause stays pending (not enqueued) | SG-02 | Pause DAG, use AddStep manually, publish step-added event, verify 0 enqueues |
| 5 | `TestScheduler_Sweep_HandlesPausingDAG` | Sweep finds pausing DAG with no in-flight → transitions to paused | SG-03 | Manually seed store with status=pausing + done steps, flip elector, verify paused |
| 6 | `TestScheduler_Sweep_SkipsPausedDAG` | Sweep skips paused DAGs entirely (0 enqueues) | SG-03 | Manually seed store with status=paused, flip elector, verify 0 enqueues |
| 7 | `TestScheduler_Sweep_AutoFinalizesPausedDAG` | Sweep auto-finalizes paused DAG when all steps terminal | SG-03 | Seed store with status=paused + all done, flip elector, verify done/failed |

### Test patterns reference:

From `scheduler_test.go`:

**Seeding store manually** (for sweep tests):
```go
store := NewMemStore()
bus := NewMemBus()
enq := &captureEnq{}
elector := &toggleElector{leader: false}
wf := NewWorkflow(store, bus, enq)
wf.Elector = elector
wf.SweepCheckInterval = 50 * time.Millisecond
wf.SweepTimeout = 2 * time.Second

ctx, cancel := context.WithCancel(context.Background())
defer cancel()

_ = store.PutMeta(ctx, dagID, DAGMeta{ID: dagID, Status: DAGStatusPausing}, 0)
_ = store.PutStep(ctx, dagID, "a", StepRecord{...}, 0)
```

**Event-driven tests** (for pause behavior):
```go
wf, dag, enq := setupDAG(t)
// ... configure DAG ...
ctx, cancel := context.WithCancel(context.Background())
defer cancel()
startScheduler(t, wf, ctx)
dag.Submit(ctx, wf)
waitEnqueued(t, enq, 1, time.Second)
// Pause the DAG (need to set meta to pausing manually — no API yet)
meta, rev, _ := wf.Store.GetMeta(ctx, dag.ID())
meta.Status = DAGStatusPausing
wf.Store.PutMeta(ctx, dag.ID(), meta, rev)
// Complete the running step
emulateHook(t, wf, dag.ID(), "a", []byte(`42`), nil)
// Assert: no new enqueues
time.Sleep(200 * time.Millisecond)
if enq.count() != 1 {
    t.Fatal("expected no new enqueues after pause")
}
```

**Important:** Since Phase 3 (API) hasn't been built yet, tests for pause behavior must manually set the meta status to `DAGStatusPausing` or `DAGStatusPaused` via `store.PutMeta`.

## Race Conditions

### Primary race: pausing→paused auto-transition vs Resume

In `onCompleted`, the scheduler:
1. Reads meta (status=pausing, rev=5) via `loadState()` at function entry
2. Applies step transition to in-memory state
3. Checks `HasInFlightSteps()` on the in-memory state
4. If no in-flight: does GetMeta + CAS PutMeta

Between step 1 and step 4, Resume could CAS meta to `running` with rev=5 → succeeds, rev=6. The pausing→paused CAS fails with `ErrStaleRevision`.

**Per D-14:** Return nil. The event is ACKed (step outcome persisted by StepHook). The DAG is now running. Next event will pick up.

### Sweep CAS race with concurrent handleEvent

Sweep does GetMeta (rev=N), checks status is still pausing, then CAS PutMeta with rev=N. If `handleEvent` already transitioned to paused (via auto-transition), the sweep's CAS fails with `ErrStaleRevision`. **Benign** — transition was already completed.

### Sweep CAS race with concurrent Resume

Sweep loads meta (status=pausing), loads state, finds no in-flight, does GetMeta again (still pausing), does CAS. If Resume changed to `running` between GetMeta and PutMeta, CAS fails. **Benign** — Resume won.

## Edge Cases

### Pausing with zero in-flight steps at pause time

When a user runs `running → pausing` and there are zero in-flight steps, the DAG is in `pausing` with nothing to drain. The auto-transition fires on the next event or sweep. If no events arrive, the sweep handles it on leader acquisition (or next sweep tick).

**Per D-02 (Phase 1):** The `DagPause` API (Phase 3) will skip directly to `paused` when zero in-flight exist at call time, avoiding the transient `pausing` state. But for now, `pausing → paused` via event or sweep handles it correctly.

### Cancel during pausing

The `Cancel()` function (Phase 1) has explicit fallthrough for `DAGStatusPausing` and `DAGStatusPaused`. It CAS-transitions meta to `canceled` and marks pending steps as canceled. Running steps are left to complete but their completion won't schedule follow-on work (because the DAG is now `canceled`).

### Event flood during pause

If a DAG has many running steps that all complete while `pausing`, each one sends a completion event. All of them enter `handleEvent`, the guard catches them as `pausing`/`paused` (after the first one transitions to `paused`), and all are ACKed. No CPU is wasted on state transitions for subsequent events.

### Stale snapshot after pause

In `onEvent`, `handleEvent` is called within `s.mu` lock. The guard at line 151 reads `state.Meta.Status` from a freshly loaded state. There's no stale snapshot risk because we load fresh state at every `handleEvent` call.

The stale snapshot concern applies to `enqueueReady` when called by `Resume` (Phase 3) — but that's out of scope for Phase 2.

## Pitfalls to Avoid

| Pitfall | Risk | Mitigation |
|---------|------|------------|
| NAK'ing events during pause (P2) | CPU spin, DLQ fill | Guard in handleEvent returns nil → event is ACKed by onEvent |
| Sweep doesn't handle pausing (P4) | DAG stuck in pausing forever | Add explicit DAGStatusPausing case to sweep |
| Auto-finalizing paused DAG (P5) | User can't resume | maybeFinalize already guards pausing/paused (Phase 1) |
| Forgetting pausing guard for EventStepAdded | Dynamic steps dispatched during pause | Gate applies to ALL events (D-12) |

## Summary

Phase 2 touches exactly 1 file: `workflow/scheduler.go`. All changes are in 4 locations:

1. **handleEvent line 151** — Add pausing/paused guard (SG-01)
2. **onCompleted (around line 195)** — Add pausing→paused auto-transition (SG-04)
3. **enqueueReady line 205** — Add pausing/paused guard (SG-02)
4. **sweep (lines 100-113)** — Replace simple running-only filter with switch (SG-03)

Plus new tests in `workflow/scheduler_test.go`.
