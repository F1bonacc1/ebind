# Architecture: Pause/Resume for ebind DAG Engine

**Research Date:** 2026-06-03
**Mode:** Ecosystem (architecture patterns for pause/resume in event-driven CAS-based DAG engines)
**Target System:** ebind — Go DAG workflow engine over NATS JetStream

---

## Executive Summary

Pause/resume must be layered onto an existing event-driven scheduler with CAS-based state, leader-elected failover, and a pure in-memory state machine. The core insight: **pause is not cancel**. Cancel is terminal and immediate (pending→canceled). Pause is non-terminal and gradual (running steps complete naturally, then the DAG idles). The architectural challenge is the **pausing→paused atomicity** across a distributed scheduler where in-flight step completions arrive asynchronously.

The solution has three architectural layers:
1. **State machine** — two new DAG-level statuses (`pausing`, `paused`) with well-defined transitions
2. **Scheduler gates** — pause checks at three boundaries: event entry, dispatch gate, and sweep
3. **Resume sweep** — re-discovery of ready-to-run steps by re-evaluating `ReadyToRun()` on the stored state

There is no distributed lock or two-phase commit. The architecture relies on the same CAS mechanism already proven for Cancel: meta status CAS + step CAS + event dedupe = at-most-once semantics for pause transitions. The scheduler detects the `pausing→paused` transition naturally on the last completion event.

---

## 1. State Machine (New DAG-Level Statuses)

### Status Constants

```go
const (
    DAGStatusRunning  DAGStatus = "running"
    DAGStatusPausing  DAGStatus = "pausing"   // NEW: transient, in-flight steps draining
    DAGStatusPaused   DAGStatus = "paused"    // NEW: idle, no in-flight steps, no dispatch
    DAGStatusDone     DAGStatus = "done"
    DAGStatusFailed   DAGStatus = "failed"
    DAGStatusCanceled DAGStatus = "canceled"
)
```

### Valid Transitions

```
Submit ──► Running ──► Pausing ──► Paused
             │            │
             │            ├──► Running (Resume)
             │            │
             ├──► Done ───┤
             ├──► Failed ─┤
             └──► Canceled
```

Key rules:
- **Running → Pausing**: User calls `Pause()`. Transient state; in-flight steps are NOT interrupted (they continue executing).
- **Pausing → Paused**: Automatic. When the last in-flight step finishes AND the scheduler processes its completion event. No in-flight + no new dispatches = idle DAG.
- **Paused → Running**: User calls `Resume()`. The scheduler re-discovers ready-to-run steps and enqueues them.
- **Pausing → Running**: User calls `Resume()` (early resume while steps still draining). Transitions directly without waiting for pause.
- **Paused → Canceled**: User calls `Cancel()` on a paused DAG. Pending steps are marked canceled. This is the escape hatch if the pause is no longer wanted but the DAG shouldn't continue.

### What Does NOT Change

- Step-level statuses are **untouched** during pause. `pending` stays `pending`, `running` stays `running`.
- The `Terminal()` function does NOT consider `pausing` or `paused` as terminal — these are non-terminal states.
- Step records, result records, and the step-level state machine are unchanged.
- `ReadyToRun()` is unchanged — it only checks deps, not DAG meta status.

### New Pure Functions on DAGState

```go
// HasInFlightSteps returns true if any step in this DAG is currently running.
// Used by the scheduler to detect the pausing→paused transition.
func (s *DAGState) HasInFlightSteps() bool {
    for _, step := range s.Steps {
        if step.Status == StatusRunning {
            return true
        }
    }
    return false
}

// CanPause returns true if the DAG can accept a pause request.
// Must be running (not already pausing/paused/terminal).
func (s *DAGState) CanPause() bool {
    return s.Meta.Status == DAGStatusRunning
}

// CanResume returns true if the DAG can accept a resume request.
// Must be paused (can also resume pausing as a fast-path).
func (s *DAGState) CanResume() bool {
    return s.Meta.Status == DAGStatusPaused || s.Meta.Status == DAGStatusPausing
}
```

### Terminal() Impact

The existing `Terminal()` function checks if all steps are terminal. Since `pausing` and `paused` are DAG-level statuses (not step-level), `Terminal()` computation is unaffected. However, `maybeFinalize()` should NOT transition a `pausing` or `paused` DAG to `done`/`failed` — if someone pauses a DAG and all remaining steps complete in-flight, the DAG should stay `paused`, not auto-finalize.

**UPDATE to `maybeFinalize`:** If the DAG status is `pausing` or `paused` and all steps are terminal, the DAG stays `pausing`/`paused`. The caller can resume (to see terminal status) or cancel it explicitly. Alternatively, when all steps are terminal during pause, transition directly to the appropriate terminal status. **Recommendation: stay paused.** Users want to resume and inspect; auto-completing while paused is surprising.

---

## 2. Scheduler Integration: The Three Gates

The scheduler has three distinct boundaries where pause-awareness must be added. Each serves a different purpose:

### Gate 1: Event Processing (`onEvent` in scheduler.go:117)

**What it gates:** Entry to ALL event processing (completion events, step-added events).

**Current behavior:**
```
onEvent:
  if not leader → Nak
  lock mu → handleEvent → unlock mu
  Ack
```

**With pause:** Add a lightweight check AFTER leader check, BEFORE `mu.Lock()`:

```
onEvent:
  if not leader → Nak
  if DAG is pausing|paused → Ack (consume event, don't process)
  lock mu → handleEvent → unlock mu
  Ack
```

**Why ACK not Nak:** If we Nak'd the event during pause, the consumer would redeliver it indefinitely, creating CPU spin. By ACKing it, we acknowledge the event is "consumed" — the step outcome is already recorded in KV by the StepHook, so we're not losing data. We're just choosing not to advance the state machine.

**Important caveat:** The StepHook already writes the step outcome to KV *before* publishing the event. So even if the scheduler ignores the event during pause, the KV state is updated. This means on resume, `loadState()` sees the correct terminal status for each completed step.

### Gate 2: Dispatch (`enqueueReady` in scheduler.go:204)

**What it gates:** Publishing new task envelopes to the TASKS stream.

**Current behavior:**
```go
func (s *Scheduler) enqueueReady(ctx, state, ready) {
    if len(ready) == 0 || state.Meta.Status == DAGStatusCanceled {
        return
    }
    // ... enqueue each ready step
}
```

**With pause:**
```go
func (s *Scheduler) enqueueReady(ctx, state, ready) {
    if len(ready) == 0 || state.Meta.Status == DAGStatusCanceled || 
       state.Meta.Status == DAGStatusPausing || state.Meta.Status == DAGStatusPaused {
        return
    }
    // ... enqueue each ready step
}
```

This is a defense-in-depth check. Even if Gate 1 (event processing) lets an event through, Gate 2 prevents dispatch. This matters during the pausing→paused transition window: an event arrives, enters `handleEvent`, the state machine is advanced (step marked done), but no new dispatches happen.

### Gate 3: Sweep (`sweep` in scheduler.go:95)

**What it gates:** Re-enqueue of stranded ready steps on leader acquisition.

**Current behavior:**
```go
func (s *Scheduler) sweep(ctx) {
    dags, _ := s.wf.Store.ListDAGs(ctx)
    for _, dag := range dags {
        if dag.Status != DAGStatusRunning {
            continue
        }
        // load state, enqueue ready
    }
}
```

**With pause:**
```go
func (s *Scheduler) sweep(ctx) {
    dags, _ := s.wf.Store.ListDAGs(ctx)
    for _, dag := range dags {
        switch dag.Status {
        case DAGStatusRunning:
            // standard re-discover + enqueue
        case DAGStatusPausing:
            // Attempt pausing→paused transition.
            // Load state → if no in-flight steps → CAS meta to paused
            // Do NOT enqueue new steps
        case DAGStatusPaused:
            continue // no CPU needed
        default:
            continue // done/failed/canceled
        }
    }
}
```

The sweep is the **failover recovery path** for pause. If the leader crashed during pausing→paused transition, the sweep completes it. This is the same edge-triggered pattern already used for stranded step re-enqueue.

---

## 3. The Pausing→Paused Transition: Distributed Atomicity

This is the hardest architectural question. There is no single "moment" when a DAG transitions to paused. The transition is distributed across asynchronous completion events.

### Sequence Diagram

```
User                     Scheduler                    Worker (StepHook)
 │                          │                              │
 ├── Pause(id) ────────────►│                              │
 │                          │  CAS meta: running→pausing   │
 │                          │                              │
 │                          │       (step A is running)    │
 │                          │                              │
 │                          │◄──── OnStepDone(A) ──────────│
 │                          │  StepHook already wrote      │
 │                          │  A→done + result to KV       │
 │                          │                              │
 │   Event: A completed     │                              │
 │   Gate 1: pausing → Ack  │                              │
 │   Gate 2: pausing → skip │                              │
 │   Check HasInFlight?     │                              │
 │   No in-flight →         │                              │
 │   CAS meta: pausing→paused│                             │
 │                          │                              │
 │   DAG is now PAUSED      │                              │
```

### The Actual Implementation in handleEvent

The pausing→paused transition happens inside `handleEvent`, right after the step transition is applied. It's not a separate goroutine or ticker.

```go
func (s *Scheduler) handleEvent(ctx context.Context, ev Event) error {
    state, err := s.loadState(ctx, ev.DAGID)
    if err != nil {
        return err
    }
    
    // Gate 0: Short-circuit for terminal states (existing)
    if state.Meta.Status == DAGStatusCanceled {
        return nil
    }
    
    // Gate 0b: Pause silent-consume
    if state.Meta.Status == DAGStatusPaused {
        return nil // already paused, nothing to do
    }
    
    isPausing := state.Meta.Status == DAGStatusPausing
    
    switch ev.Kind {
    case EventCompleted:
        if err := s.onCompleted(ctx, state, ev); err != nil {
            return err
        }
    case EventStepAdded:
        // Step-added events during pausing: don't process (the new step stays pending)
        if isPausing {
            return nil
        }
        return s.onStepAdded(ctx, state)
    }
    
    // Pausing→paused transition: after applying the completion, check if done.
    if isPausing && !state.HasInFlightSteps() {
        // All in-flight steps finished. Transition to paused.
        meta, rev, err := s.wf.Store.GetMeta(ctx, state.Meta.ID)
        if err != nil {
            return err
        }
        if meta.Status != DAGStatusPausing {
            return nil // concurrent Resume or Cancel already changed it
        }
        // Only transition if actually still pausing
        if state.TerminalAllButDAGStatus() { // all steps terminal
            // Use the natural terminal status (done/failed)
            meta.Status = state.TerminalStatus() // or keep as paused
        } else {
            // There are still pending steps — they just have unfilled deps.
            // The DAG still has work to do when resumed.
            meta.Status = DAGStatusPaused
        }
        return s.wf.Store.PutMeta(ctx, state.Meta.ID, meta, rev)
    }
    
    return nil
}
```

Wait — there's a subtlety here. The `handleEvent` function processes one event at a time. After applying `MarkDone(stepID)`, the state machine might find that other steps are `ReadyToRun()`. But we don't dispatch them because the status is `pausing`. Good.

But what about the pausing→paused check? After `onCompleted`, if the DAG is pausing and `HasInFlightSteps()` returns false, we transition. But what if there are pending steps whose deps are not yet satisfied? Those stay pending. The DAG is paused with pending steps.

Actually, the more natural behavior: **paused means all running steps completed, pending steps remain pending**. This is exactly what the user wants — the DAG is idled mid-execution with some work done and some pending, ready to resume.

If ALL steps are terminal when the last running step finishes, the DAG could logically go to `done`/`failed`. But that's surprising — you paused the DAG, it should stay paused. So we keep the status as `paused` even if all steps are terminal. The user can Cancel or Delete if they want.

### Race Condition Analysis

**Scenario: Resume arrives during pausing→paused CAS**

The `handleEvent` does:
1. Load meta (status=pausing, rev=5)
2. Apply completion transition
3. Detect no in-flight steps → try CAS meta to paused with rev=5
4. But between (1) and (3), Resume() did CAS meta to running with rev=5 → succeeded, now rev=6
5. The pausing→paused CAS fails with ErrStaleRevision
6. handleEvent returns error, Ack is still called (event consumed)
7. Next event or sweep will see status=running and proceed normally

**Outcome:** The event is consumed, the step transition is applied in-memory (but not persisted — oh wait, actually it was persisted by the StepHook before the event!). The pausing→paused transition silently fails, and the DAG is now running again. The next event will pick up. This is correct behavior — the user's Resume intention "wins" over the auto-pausing-transition.

**Scenario: Scheduler crashes during pausing→paused CAS**

1. StepHook writes step→done to KV and publishes event
2. Scheduler receives event, applies transition in-memory
3. Scheduler crashes before writing meta to paused
4. On restart, new leader's sweep finds DAG with status=pausing
5. Sweep loads state, sees all steps terminal or running=false
6. Sweep performs the paused transition

Wait — but the leader election has the sweep guard. The sweep runs on leader acquisition. If the same node restarts, it re-acquires, sweep runs. If a different node takes over, it was already processing events, and the sweep runs on acquisition.

But here's another issue: the in-memory state machine applied MarkDone to the step in `state`, but the KV step record was already updated by the StepHook. So `loadState` in the sweep will see the step as done. The pausing→paused check will work correctly.

**But what about `enqueueReady` called by `onCompleted` during pausing?** `onCompleted` calls `MarkDone`, then `ReadyToRun`, then `enqueueReady`. But `enqueueReady` checks for `DAGStatusRunning` and skips if not. So pending steps whose deps just became satisfied won't get dispatched. They stay pending until resume. Correct.

---

## 4. Resume: Re-discovering Ready-to-Run Steps

Resume is architecturally simpler than pause because it's an explicit user action, not a distributed automatic transition.

### Resume Flow

```
User                     Scheduler
 │                          │
 ├── Resume(id) ───────────►│
 │                          │  CAS meta: paused→running (or pausing→running)
 │                          │  If meta not pausing/paused → return (no-op)
 │                          │
 │                          │  Load full DAG state
 │                          │  Run ReadyToRun()
 │                          │  For each ready step:
 │                          │    persistStatus(step, running)
 │                          │    resolve args
 │                          │    enqueueStep
 │                          │
 │                          │  (Events naturally flow from here)
```

### Resume Implementation Pattern

The Resume function mirrors `Cancel` in structure (API + CLI + CAS), but instead of canceling steps, it re-dispatches them:

```go
func Resume(ctx context.Context, wf *Workflow, dagID string) error {
    // CAS meta: paused/pausing → running
    for attempt := 0; attempt < 5; attempt++ {
        meta, rev, err := wf.Store.GetMeta(ctx, dagID)
        if err != nil {
            return err
        }
        if meta.Status != DAGStatusPaused && meta.Status != DAGStatusPausing {
            return ErrDAGNotPaused // sentinel for "nothing to resume"
        }
        meta.Status = DAGStatusRunning
        if err := wf.Store.PutMeta(ctx, dagID, meta, rev); err != nil {
            if errors.Is(err, ErrStaleRevision) {
                continue
            }
            return err
        }
        break
    }
    
    // Load state and enqueue ready steps
    state, err := loadState(ctx, wf.Store, dagID)
    if err != nil {
        return err
    }
    
    ready := state.ReadyToRun()
    // Use existing enqueueReady logic (enqueueReady checks StatusRunning, which is now true)
    return enqueueReady(ctx, state, ready) // but enqueueReady needs the scheduler context
}
```

**Design question:** Should Resume be a standalone function (like `Cancel`) or method on `Workflow`? **Recommendation:** standalone function for consistency with Cancel. Add a convenience method on Workflow if desired.

**Design question:** Can Resume share code with `scheduler.enqueueReady`? **Recommendation:** Extract `enqueueReady` into a package-level function that both Resume and the scheduler can call. Currently `enqueueReady` is a method on `*Scheduler`. Extract it to a standalone function that takes `(ctx, store, enq, state, ready)`.

**Additional Resume behavior:** After CAS'ing meta to running, Resume should also publish a synthetic event or trigger the scheduler to re-evaluate. Actually, this isn't needed — the scheduler's next event or sweep will see the running status and process normally. But for low-latency resume, there might not be any pending events. The ready steps won't be dispatched until the next completion event arrives.

**Solution:** Resume directly enqueues ready steps after the meta CAS, rather than waiting for the scheduler. This is a pragmatic choice: Resume is an admin operation that should take effect immediately.

**But there's a subtlety with CAS racing:** Resume's `enqueueReady` runs outside the scheduler's `s.mu`. If a concurrent completion event arrives, the scheduler might also call `enqueueReady`. This is safe because:
- `persistStatus` has CAS retry — only one writer succeeds
- The duplicate `Nats-Msg-Id` (set to `<dag_id>:<step_id>`) prevents double enqueue at the NATS level
- The JetStream dedupe window (default 5 minutes) covers this

This is the same safety model already used for concurrent Cancel + scheduler.

---

## 5. CAS Interaction Matrix

All pause/resume state transitions use the same CAS pattern already established by Cancel. This table shows which writers can race and the outcome:

| Writer 1 | Writer 2 | CAS Target | Outcome |
|----------|----------|------------|---------|
| Pause (r→pausing) | handleEvent (completion) | Meta | One wins CAS; the other retries. If Pause wins, handleEvent sees pausing. If handleEvent wins, Pause may need to re-check if in-flight steps remain. |
| Pause (r→pausing) | Cancel (r→cancel) | Meta | Either wins. If Cancel wins, Pause fails with not-running. If Pause wins, Cancel can still happen (pausing→canceled is valid). |
| handleEvent (pausing→paused) | Resume (paused→running) | Meta | Post-pause race. If both detect the transition window, one's CAS fails. |
| Resume (p→running) | handleEvent (completion) | Meta | Resume CASes meta; handleEvent only reads meta. No direct race. handleEvent's enqueueReady is gated by status check. |
| Resume's enqueueReady | Scheduler's enqueueReady | Step record | CAS on step record. Only one persists the Running status. Duplicate enqueue prevented by JetStream dedupe. |

**Key insight:** The CAS pattern is already proven correct for Cancel. Pause/Resume adds two more writers to the same CAS domain, but the retry-until-deadline pattern bounds the contention. The 5-retry limit (already identified as fragile for Cancel in CONCERNS.md) applies equally here.

---

## 6. Step-Hook and Event Model

The StepHook is unchanged. It writes step outcomes to KV and publishes completion events regardless of DAG status. The scheduler decides what to do with the events.

**Why not filter in the StepHook?** The StepHook runs in the worker goroutine, not the scheduler. By the time `OnStepDone` is called, the step has already executed. The StepHook's job is to record the outcome durably — that should always happen. The scheduler is the right place to decide whether to advance the DAG or not.

**Pause-specific events?** Currently there are only `EventCompleted` and `EventStepAdded`. No new event types are needed for pause. The pause is a meta-status check in the scheduler, not a separate event flow.

**One edge case: Step-added events during pause.** If a handler actively running during pause adds a dynamic step (via `workflow.FromContext(ctx).Step(...)`), the step is written to KV and a step-added event is published. The scheduler receives the event but (with pause) ignores it. The step stays pending. On resume, `ReadyToRun()` will find it if its deps are satisfied. **This is correct behavior** — dynamically adding steps to a paused DAG should work, and the step executes when the DAG resumes.

---

## 7. Sweep Changes for Leader Failover

The sweep is the critical recovery path for pause after failover. Here's the detailed behavior:

### Pre-Failover State: DAG is DAGStatusPausing

Before crash:
- Meta status = `pausing`
- Some steps running, some done, some pending

On leader acquisition, sweep processes `pausing` DAGs:

```go
case DAGStatusPausing:
    state, err := s.loadState(ctx, dag.ID)
    if err != nil {
        continue
    }
    if !state.HasInFlightSteps() {
        // All in-flight steps completed. Transition to paused.
        meta, rev, _ := s.wf.Store.GetMeta(ctx, dag.ID)
        if meta.Status == DAGStatusPausing {
            meta.Status = DAGStatusPaused
            _ = s.wf.Store.PutMeta(ctx, dag.ID, meta, rev) // CAS; may fail, who cares
        }
    }
    // If still has in-flight steps, do nothing — they'll complete naturally.
    // Do NOT enqueue new steps.
```

### Pre-Failover State: DAG is DAGStatusPaused

The sweep **skips** paused DAGs entirely — no CPU consumed. This is the "paused DAGs consume no scheduler CPU" requirement from PROJECT.md.

### Pre-Failover State: Resume was called but not yet persisted

This is impossible — Resume is an API call that persists via CAS. If it succeeded, meta is `running`. If it didn't, meta is still `paused`. No partial state.

---

## 8. New Error Sentinels

```go
var ErrDAGNotPaused = errors.New("workflow: DAG is not paused")
var ErrDAGAlreadyPaused = errors.New("workflow: DAG is already paused")
var ErrDAGAlreadyRunning = errors.New("workflow: DAG is already running")
var ErrDAGPaused = errors.New("workflow: DAG is paused")
```

`ErrDAGPaused` is useful for `Await[T]` and other consumers that might need to distinguish "step was canceled because DAG was paused" from other terminal states.

**Design decision:** Should `ErrStepSkipped` or a new sentinel track pause-caused no-op? **Recommendation:** No. Pause doesn't change step statuses — steps stay pending. On resume, they run normally. No new step-level sentinels needed.

---

## 9. CLI Integration

### New Commands

```
ebctl dag pause <dag-id>
ebctl dag resume <dag-id>
```

### Updated Display

`dag ls`:
- Status column shows `pausing` or `paused`
- Steps summary still works (shows pending, running, done, etc.)
- `--status` filter accepts `pausing` and `paused`

`dag get <dag-id>`:
- Shows `pausing` or `paused` as the DAG status

`dag tree`:
- Shows step states (pending/running/done unchanged)
- Paused DAG shown with PAUSED header

### Status Filter Update

In `ls.go:93`:
```go
cmd.Flags().StringVar(&statusFilter, "status", "", "filter by status: running|paused|pausing|done|failed|canceled")
```

---

## 10. Testing Strategy

The test patterns map directly to the existing test infrastructure:

### Pure State Tests (`state_test.go`)

| Test | What it validates |
|------|-------------------|
| `TestState_HasInFlightSteps_NoRunning` | `HasInFlightSteps()` returns false when all steps are terminal/pending |
| `TestState_HasInFlightSteps_HasRunning` | `HasInFlightSteps()` returns true when a step is running |
| `TestState_CanPause` | Only running DAGs can be paused |
| `TestState_CanResume` | Only paused/pausing DAGs can be resumed |
| `TestState_Terminal_Pausing_NotDone` | pausing/paused are NOT terminal in `Terminal()` |
| `TestState_ReadyToRun_UnchangedDuringPause` | `ReadyToRun()` behaves identically regardless of DAG meta status |

### Scheduler Tests (`scheduler_test.go`)

| Test | What it validates | Pattern |
|------|-------------------|---------|
| `TestScheduler_Pause_BlocksDispatch` | Pause a DAG mid-flight → completion events are ACKed but no new steps enqueued | MemStore + captureEnq |
| `TestScheduler_Pause_TransitionsToPaused` | Last in-flight step completes → meta becomes paused | Poll meta status |
| `TestScheduler_Pause_InFlightContinues` | Running step completes normally during pause (result persisted) | emulateHook + check store |
| `TestScheduler_Resume_EnqueuesReady` | Resume a paused DAG → pending steps with satisfied deps are enqueued | captureEnq assertion |
| `TestScheduler_Resume_NoOpForRunning` | Resume on a running DAG → no-op | captureEnq assertion |
| `TestScheduler_PauseThenCancel` | Cancel during pause → pending steps canceled | Cancel test pattern |
| `TestScheduler_Pause_Idempotent` | Pause an already-pausing/paused DAG → no-op | State check |
| `TestScheduler_Sweep_HandlesPausingDAG` | Sweep finds pausing DAG with all steps done → transitions to paused | toggleElector |
| `TestScheduler_Sweep_SkipsPausedDAG` | Sweep skips paused DAGs (no dispatch) | captureEnq = 0 |
| `TestScheduler_Pause_StepAddedDuringPause` | Dynamic step added during pause stays pending until resume | emulateHook + captureEnq |
| `TestScheduler_Pause_ConcurrentResume` | Resume races with pausing→paused → resume wins | CAS race pattern |

### Integration Tests (`integration_test.go`)

| Test | What it validates |
|------|-------------------|
| `TestIntegration_PauseResume` | Full end-to-end: submit → pause → in-flight finishes → resume → rest executes |
| `TestIntegration_PausePersistence` | Pause survives NATS restart (persisted in KV) |
| `TestIntegration_ResumeAfterRestart` | Resume after process restart re-dispatches ready steps |

### Concurrent CAS Stress Test (new file or new test in `store_test.go`)

| Test | What it validates |
|------|-------------------|
| `TestCAS_PauseVsCancel_Race` | Pause and Cancel both racing on meta CAS |
| `TestCAS_PauseVsCompletion_Race` | Pause races with StepHook OnStepDone on step record |
| `TestCAS_ResumeVsScheduler_Race` | Resume CAS on meta races with scheduler's event loop |

---

## 11. Build Order (Dependency Chain)

```
Phase 1: State Machine Constants + Pure Logic
  ├── DAGStatusPausing, DAGStatusPaused constants
  ├── HasInFlightSteps(), CanPause(), CanResume()
  ├── Update maybeFinalize to skip paused/pausing
  ├── Pure unit tests only
  │
  ├──► Phase 2: Pause API + CLI
  │     ├── Pause() function (CAS meta → pausing)
  │     ├── ErrDAGNotPaused, ErrDAGAlreadyPaused sentinels
  │     ├── ebctl dag pause <id> command
  │     ├── CLI status display for pausing/paused
  │     └── Scheduler tests with captureEnq
  │
  ├──► Phase 3: Scheduler Pause Awareness
  │     ├── Gate 1: onEvent pause check
  │     ├── Gate 2: enqueueReady pause check
  │     ├── Gate 3: sweep handles pausing→paused
  │     ├── handleEvent pausing→paused transition
  │     └── Scheduler + integration tests
  │
  └──► Phase 4: Resume API + CLI
        ├── Resume() function (CAS meta → running)
        ├── Re-discover/enqueue ready steps
        ├── ebctl dag resume <id> command
        ├── ERR_DAGAlreadyRunning sentinel
        └── Full integration tests
```

**Phase ordering rationale:**
- Phase 1 has zero external dependencies (pure logic). Can be built and tested first.
- Phase 2 depends on Phase 1 constants and can be tested standalone with captureEnq.
- Phase 3 depends on Phase 2 (needs the pausing status to test scheduler behavior).
- Phase 4 depends on Phase 1+2+3 (needs a paused DAG to resume).
- CLIdisplay updates in Phase 2 (show pausing/paused) and Phase 4 (show resumed status).

**Research flags:**
- Phase 3: The pausing→paused transition race condition with Resume needs careful design review. The "last completion event wins" model has a theoretical window where two completion events arrive concurrently for different steps, both enter handleEvent (serialized by mu, so okay), but the first one transitions to paused and the second sees paused and returns. Is there a window where both see pausing and both try to CAS to paused? Yes — but one CAS fails with stale revision, the error is returned but the event is ACKed. On the next event (or sweep), the DAG is already paused. This is safe but the error path in handleEvent should log rather than silently swalling.
- Phase 4: Resume's direct enqueue of ready steps bypasses the scheduler's `s.mu`. While CAS + dedupe provides safety, this is a different concurrent access pattern than the scheduler normally sees. Worth a specific integration test.

---

## 12. Sources

### Codebase Sources (HIGH confidence)
- `/home/pawel/repo/ebind/workflow/scheduler.go` — scheduler event loop, sweep, event handling
- `/home/pawel/repo/ebind/workflow/state.go` — pure DAG state machine, status constants
- `/home/pawel/repo/ebind/workflow/cancel.go` — Cancel pattern (pause reference)
- `/home/pawel/repo/ebind/workflow/store.go` — StateStore interface, CAS semantics
- `/home/pawel/repo/ebind/workflow/store_mem.go` — MemStore implementation (test pattern reference)
- `/home/pawel/repo/ebind/workflow/scheduler_test.go` — Test patterns, captureEnq, toggleElector
- `/home/pawel/repo/ebind/workflow/state_test.go` — Pure state test patterns
- `/home/pawel/repo/ebind/workflow/hook.go` — StepHook (event producer)
- `/home/pawel/repo/ebind/workflow/events.go` — Event model
- `/home/pawel/repo/ebind/workflow/enqueuer.go` — Enqueuer + LeaderElector interfaces
- `/home/pawel/repo/ebind/.planning/PROJECT.md` — Requirements, constraints, scope
- `/home/pawel/repo/ebind/.planning/codebase/ARCHITECTURE.md` — Existing architecture docs
- `/home/pawel/repo/ebind/.planning/codebase/CONCERNS.md` — Known concerns (5-retry ceiling, sweep scaling)
- `/home/pawel/repo/ebind/CLAUDE.md` — Design decisions, invariants

### External Sources (MEDIUM confidence)
- Temporal.io Workflow Pause/Resume docs — Replay-based approach (fundamentally different architecture from ebind's event-driven CAS model, but validates the "let in-flight complete" design choice)
- AWS Step Functions docs — No native pause/resume; uses callback pattern for human-in-the-loop (validates that pause/resume is a complex distributed state problem)

---

*Architecture analysis: 2026-06-03*
