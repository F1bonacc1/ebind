# Domain Pitfalls: Pause/Resume in DAG Workflow Engines

**Domain:** Go DAG workflow engine pause/resume implementation
**Researched:** 2026-06-03
**Confidence:** HIGH (validated against Temporal, Airflow, Prefect, Argo, and Kubernetes Job patterns)

---

## Critical Pitfalls

Mistakes that cause data loss, stuck DAGs, or require rewrites.

### P1: Forgetting That Pause Is Not Cancel

**What goes wrong:** Implementers reuse Cancel's code path for pause, which marks pending steps as canceled instead of leaving them pending.

**Why it happens:** Both pause and cancel are "stop the DAG" operations. Cancel already has a well-tested CAS path. It's tempting to generalize it.

**Consequences:**
- Pending steps are irrevocably marked as canceled
- Results from completed steps are orphaned (they exist in KV but the DAG is terminal)
- Resume is impossible — canceled steps can't be re-dispatched
- Users lose work that was queued and ready to execute

**Prevention:**
- Pause is a **non-terminal** operation. Cancel is **terminal**. They must have different CAS paths.
- Pause changes only the DAG meta status. Cancel changes step records.
- The `maybeFinalize` function must NOT treat pause as terminal.
- The distinction is validated by every major engine: Prefect has `Paused` (non-terminal) vs `Cancelled` (terminal). Argo has `suspended` (non-terminal) vs `failed`/`cancelled` (terminal).

**Detection:** Test that after pause and resume, previously-pending steps are dispatched and execute normally.

### P2: NAk-ing Completion Events During Pause Instead of ACKing

**What goes wrong:** When the scheduler sees a completion event for a `pausing` DAG, it Naks the event, causing indefinite redelivery.

**Why it happens:** The scheduler's default response to "this DAG shouldn't advance" is to Nak, which works for non-leader scenarios. It's natural to extend this pattern to pause.

**Consequences:**
- CPU spin: the event is redelivered repeatedly (up to MaxDeliver, then DLQ'd)
- The DLQ fills with harmless completion events
- On resume, the last event may have been DLQ'd, causing the DAG to miss the final step completion
- Even if not DLQ'd, the delay between redeliveries (NakWithDelay) creates a lag between resume and step dispatch

**Prevention:**
- **ACK events during pause.** The StepHook wrote the step outcome to KV before publishing the event. Consuming the event (ACKing it) doesn't lose data — it just prevents the scheduler from advancing the state machine.
- On resume, `loadState()` reads the already-persisted step outcomes from KV. No events are missed.

**Detection:** Test that a completion event during pause is ACKed (consumer progress advanced) and the step outcome is readable from KV after resume.

### P3: Race Between `pausing → paused` Auto-Transition and Resume

**What goes wrong:** The scheduler detects "all in-flight steps done" and CAS-transitions meta from `pausing → paused`. Simultaneously, the user calls `Resume()` which CAS-transitions meta from `pausing → running`. Both read the same CAS revision. One wins, the other fails.

**Why it happens:** Two legitimate concurrent operations target the same KV key. The scheduler's auto-transition and the user's Resume are both valid.

**Consequences (if not handled):**
- If Resume loses the CAS: the DAG goes to `paused` despite the user asking for resume. The user gets a CAS error (or error sentinel), tries again. Eventually succeeds.
- If the auto-transition loses: the DAG goes to `running` despite possibly having no steps left to dispatch (if all steps completed). The scheduler detects this and either sits idle or finalizes.
- Neither outcome is catastrophic, but both are confusing UX.

**Prevention:**
- Make Resume idempotent for `pausing` state: if CAS fails because the auto-transition won, load meta, see `paused`, and try again (CAS `paused → running`). This is a 2-attempt strategy.
- Document that Resume on a `pausing` DAG may take 2 CAS attempts if the auto-transition races.
- Alternative: make the auto-transition a best-effort inline operation. If the CAS fails, log it and let the sweep pick it up. The sweep is the reliable recovery path.

**Recommended approach:** The auto-transition in `handleEvent` should be best-effort: if CAS fails, log a debug message and return. The sweep handles the transition reliably. This makes the user's Resume "win" naturally — the first CAS attempt establishes the `running` status.

### P4: Sweep Doesn't Handle `pausing → paused` Transition

**What goes wrong:** On leader failover during the `pausing → paused` window, the new leader's sweep skips `pausing` DAGs because it only processes `running` DAGs (existing behavior).

**Why it happens:** The existing sweep was designed for "re-enqueue stranded ready steps." It only looks at `running` DAGs. Adding `pausing` requires an explicit case.

**Consequences:**
- A DAG stuck in `pausing` after failover stays `pausing` forever
- No steps are dispatched (correct) but the DAG never reaches `paused` (incorrectly stuck)
- User can't explicitly detect that the DAG is fully drained (is it still draining, or stuck?)
- Resume still works (CAS `pausing → running` is valid), but the DAG was never fully paused

**Prevention:**
- Add explicit `DAGStatusPausing` case to the sweep's status switch
- In that case: load state, check `HasInFlightSteps()`, if no in-flight steps and meta still `pausing`, CAS to `paused`
- This is the same edge-triggered recovery pattern the sweep already uses for stranded steps

**Detection:** Write a test where a leader fails during `pausing` and the new leader's sweep correctly transitions to `paused`.

---

## Moderate Pitfalls

### P5: Auto-Finalizing a Paused DAG When All Steps Become Terminal

**What goes wrong:** The last in-flight step completes while the DAG is paused. All steps are now terminal (done/failed). `maybeFinalize` transitions the DAG to `done` or `failed` automatically.

**Why it happens:** `maybeFinalize` is called after every step completion event in `handleEvent`. It checks "are all steps terminal?" If yes, it finalizes. It doesn't currently check the DAG meta status.

**Consequences:**
- User pauses a DAG mid-execution
- All running steps complete (during the drain)
- The DAG auto-finalizes to `done`/`failed`
- User expected to find the DAG paused, but it's terminal
- Resume becomes impossible (can't resume a terminal DAG)
- User loses the ability to inspect the DAG in its mid-execution state

**Prevention:**
- Update `maybeFinalize` (or the caller in `handleEvent`) to skip finalization if DAG status is `pausing` or `paused`
- The DAG should only be finalized by explicit user action (Cancel) or natural progression when in `running` status
- Alternative approach: always finalize, even during pause. **Argument against:** Pause is explicitly a user action that should preserve state. Auto-finalizing subverts user intent.

**Detection:** Test: pause a DAG where the last running step completes → DAG stays paused, not done/failed.

### P6: Resume Enqueueing Steps That Already Have Results in KV

**What goes wrong:** On resume, `ReadyToRun()` finds steps whose deps are satisfied. The system re-enqueues them, but they already completed before the pause.

**Why it happens:** `ReadyToRun()` checks step dependencies. If a step's deps are satisfied and the step isn't running/terminal, it's "ready." But what about steps that completed during the pause drain window? They're already `done` in KV — their status is `StatusDone`, so `ReadyToRun()` correctly excludes them.

**Wait — actually this can't happen.** The StepHook writes the step as `StatusDone` to KV before publishing the completion event. On resume, `loadState()` reads the step as `done`. `ReadyToRun()` checks `step.Status == StatusPending` — done steps are excluded. So this isn't actually a pitfall.

**But here's the real issue:** What if a step was enqueued by the scheduler (marked `StatusRunning` in KV) but the worker never picked it up before pause? On resume, `loadState()` sees the step as `StatusRunning` but it never actually executed. `ReadyToRun()` won't include it (it's running, not pending). The step is stuck in `running` limbo.

**Prevention:** This is a pre-existing issue with any step that gets stuck in `StatusRunning`. The existing timeout/deadline mechanism handles this — when the step's deadline expires, it gets retried. On resume, if a step is `running` but was never actually dispatched (pause blocked the publish), the deadline will eventually fire and the step gets re-attempted.

**Alternative:** On resume, re-enqueue steps that are `StatusRunning` but whose delay-before-pause means they were never dispatched. But this is hard to detect — how do you know if a `running` step was dispatched or not? The JetStream dedupe window (5 min) is the safety net: if the step was already enqueued, the dedupe prevents duplicate execution.

**Recommendation:** Accept the 5-minute dedupe window. If the step was enqueued, it executes and the result is written. If it wasn't enqueued (pause blocked the publish), the re-enqueue on resume publishes it. The dedupe ID is `<dag_id>:<step_id>` — since the scheduler's first enqueue attempt was blocked by pause, there's no duplicate. This is correct.

### P7: Dynamic Step Addition During Pause

**What goes wrong:** A handler that's still executing during the `pausing` drain window adds a dynamic step (via `Step(...)` on the workflow context). The step is written to KV, a step-added event is published. The scheduler ignores it (because `pausing`). The step stays pending forever.

**Why it happens:** The scheduler's `handleEvent` discards step-added events during pause. The step exists in KV but is never dispatched.

**Consequences:**
- The new step's deps are satisfied (or not — it's pending)
- On resume, `ReadyToRun()` finds it (if deps satisfied) and dispatches it
- **This is actually correct behavior.** The step isn't lost — it's just pending until resume.
- The only surprise is timing: the step was added while the DAG was "stopping," but it runs when the DAG resumes.

**Prevention:** Document this behavior. Users who add dynamic steps should expect them to execute on resume if the DAG is paused.

### P8: Pause Reason Stored But Never Displayed

**What goes wrong:** The pause reason, timestamp, and caller identity are stored in DAGMeta but not surfaced in the CLI output.

**Why it happens:** The CLI `dag get` and `dag ls` commands need explicit formatting changes to display the new fields. This is easy to overlook.

**Consequences:**
- The pause audit trail exists (persisted in KV) but is invisible to operators
- Operators can't answer "who paused this DAG and why?"
- The feature technically works but fails the UX requirement

**Prevention:** Add pause audit fields to `dag get` output as a required part of the CLI changes in Phase 2 (not deferred). Example output:

```
DAG: my-dag (PAUSED)
Paused at:  2026-06-03 14:22:01 UTC
Paused by:  operator@example.com
Reason:     Waiting for database migration to complete
Steps:      4 done, 2 pending, 0 running, 0 failed
```

---

## Minor Pitfalls

### P9: `DAGStatusRunning` Used in String Comparisons UI-Wide

**What goes wrong:** Code that compares DAG status to `"running"` exactly (not via the constant) misses the new statuses.

**Why it happens:** String literals like `if status == "running"` are scattered across the codebase. Adding `"pausing"` and `"paused"` as valid "not terminal" statuses requires finding all these comparisons.

**Prevention:**
- Use the `DAGStatus` constants consistently
- Add a `IsPaused()`, `IsPausing()` helper, and update `IsRunning()` if it exists
- Audit all string comparisons to `status` in the codebase during Phase 1
- The affected areas: scheduler event gate, sweep filter, CLI display, status filter, maybeFinalize

### P10: Pause Status Filter Breaks in `ebctl dag ls --status`

**What goes wrong:** The `--status` flag filters by exact match. If the user types `Paused` (uppercase P) instead of `paused` (lowercase), they get no results.

**Why it happens:** ebind uses lowercase status strings. Users may naturally type `PAUSED` or `Paused`, especially if they're familiar with Prefect or UIs that display uppercase.

**Prevention:** Make the status filter case-insensitive for display purposes, or normalize the input. This is a UX polish, not a correctness issue.

### P11: Pausing a DAG with Zero In-Flight Steps Goes Through Two States Unnecessarily

**What goes wrong:** User pauses a DAG where no steps are currently running (all steps are pending or hanging on unsatisfied deps). The DAG goes `running → pausing → paused` unnecessarily.

**Why it happens:** The pause API unconditionally transitions to `pausing`. The scheduler then detects zero in-flight and transitions to `paused` on the next event or sweep.

**Consequences:**
- The user briefly sees `pausing` in the CLI output before it changes to `paused`
- This can confuse users who expect pause to be instantaneous
- An extra unnecessary CAS write

**Prevention:** In the `Pause()` function, after loading state and before CAS, check if `HasInFlightSteps()` is false. If no steps are running, CAS directly from `running → paused`, skipping `pausing`. This is an optimization that avoids the transient state when there's nothing to drain.

```go
func Pause(ctx, wf, dagID, reason) error {
    meta, rev, err := wf.Store.GetMeta(ctx, dagID)
    state, err := loadState(ctx, wf.Store, dagID)

    targetStatus := DAGStatusPausing
    if !state.HasInFlightSteps() {
        targetStatus = DAGStatusPaused  // skip pausing, nothing to drain
    }

    meta.Status = targetStatus
    meta.PausedAt = time.Now()
    meta.PausedBy = caller
    meta.PausedReason = reason
    return wf.Store.PutMeta(ctx, dagID, meta, rev) // CAS
}
```

### P12: Resume With No Ready Steps Returns Success But No Steps Are Dispatched

**What goes wrong:** User resumes a paused DAG where all steps are terminal (completed during the drain window, or never had pending steps). The DAG transitions to `running` but no steps are dispatched. The user is confused.

**Why it happens:** `ReadyToRun()` returns an empty list. Resume completes successfully but nothing happens.

**Consequences:**
- User sees "DAG resumed successfully" but no steps execute
- The DAG may immediately be `done`/`failed` if all steps are terminal (if `maybeFinalize` fires)
- User may think resume failed

**Prevention:**
- After resume, if `ReadyToRun()` is empty, log a debug message and potentially return a different success message: "DAG resumed but no steps are ready to run"
- If all steps are terminal after resume, consider auto-finalizing (or acknowledging this in the return)
- Document this behavior: "Resume makes the DAG runnable. If no steps are pending, the DAG may remain in `running` with no active work until new steps are added or existing steps' dependencies change."

### P13: Pause/Pending/Resume Loop with Auto-Triggered DAGs

**What goes wrong:** If a DAG is designed to auto-submit new instances (cron-like, or via external triggers), pausing one instance doesn't prevent new instances from being submitted.

**Why it happens:** This isn't really a bug — pausing a DAG execution instance is different from pausing a DAG definition. Airflow distinguishes: pausing a DAG (definition) prevents new runs. Pausing a DAG run (execution) stops that specific run.

**For ebind:** ebind doesn't have "DAG definitions" that auto-submit. Each DAG is independently submitted. Pausing a specific DAG execution only affects that execution.

**Prevention:** Ensure the API documentation clarifies: "Pause pauses the current DAG execution. It does not prevent new DAGs from being submitted. Future DAGs with the same name (if any) will start in `running` status."

---

## Phase-Specific Warnings

| Phase Topic | Likely Pitfall | Mitigation |
|-------------|---------------|------------|
| Phase 1: State machine constants + pure logic | Status string mismatch (using `"PAUSED"` instead of `"paused"`) | Use typed `DAGStatus` constants, not string literals |
| Phase 2: Pause API + CLI | CAS failure during `DagPause` is confusing to user | Ensure error message says "DAG was modified by another process; retry" not just "stale revision" |
| Phase 2: Pause API + CLI | Pause reason is accepted but stored as empty string | Default to "No reason provided" if reason is empty |
| Phase 2: CLI display | Status column wraps or truncates "pausing"/"paused" | Verify column widths in the table formatter |
| Phase 3: Scheduler pause gates | Completion events for paused DAGs are Nak'd instead of Ack'd | **Critical**: Pin this in code review. Comment: "Intentional ACK — step outcome already persisted by StepHook" |
| Phase 3: pausing→paused transition | CAS fails on concurrent Resume → event handler returns error → event is not ACKed | Return nil (not error) when CAS fails — the event is already consumed by StepHook; the sweep handles the transition |
| Phase 3: Sweep pausing→paused | CAS on meta conflicts with concurrent `handleEvent` | Sweep uses GetMeta+PutMeta CAS. If handleEvent already transitioned, sweep's CAS fails harmlessly. |
| Phase 4: Resume direct enqueue | Steps enqueued outside scheduler's `s.mu` race with scheduler events | CAS on step record + JetStream dedupe provides safety. Document this pattern. |
| Phase 4: Resume with no ready steps | User thinks resume failed because nothing happens | Return success with a note: "DAG resumed, but no steps are ready to run" |
| All phases | Adding `pausing`/`paused` to status comparisons throughout codebase | Audit: grep for all `Status == DAGStatusRunning` comparisons and add pause-aware conditions |

---

## Sources

- **Prefect States docs** — https://docs.prefect.io/latest/concepts/states/ — PAUSED state type behavior, state transitions
- **Argo Workflows suspend walk-through** — https://argo-workflows.readthedocs.io/en/latest/walk-through/suspending/ — suspend/resume CLI, in-flight behavior
- **Airflow DAG pausing** — https://airflow.apache.org/docs/apache-airflow/stable/core-concepts/dags.html — is_paused flag, in-flight completion
- **Temporal Workflow Execution docs** — https://docs.temporal.io/workflow-execution — replay-based recovery, signal pattern
- **Kubernetes Jobs suspend** — https://kubernetes.io/docs/concepts/workloads/controllers/job/#suspending-a-job — in-flight pod completion
- **ebind CLAUDE.md** — Existing invariants: CAS pattern, scheduler design, StepHook behavior
- **ebind codebase/ARCHITECTURE.md** — Scheduler, sweep, state machine concurrency model
- **ebind PROJECT.md** — Scope constraints: no auto-resume, no hard-abort, persist across restarts
- **ebind research/ARCHITECTURE.md** — Full architecture analysis, CAS interaction matrix
- **ebind research/FEATURES.md** — Feature edge cases, completion criteria
- **ebind research/STACK.md** — Cross-engine patterns for pause state representation, persistence, APIs
