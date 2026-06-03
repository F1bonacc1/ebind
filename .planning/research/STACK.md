# State of the Stack: Pause/Resume Patterns in DAG Workflow Engines

**Domain:** DAG workflow engine pause/resume implementation patterns
**Researched:** 2026-06-03
**Mode:** Ecosystem research — how existing engines implement pause/resume
**Overall Confidence:** HIGH (official docs verified for Temporal, Airflow, Prefect, Argo, Tekton)

---

## 1. How Engines Represent Pause State

### Pattern A: Status Field on the Workflow/DAG (Prefect, Argo, Airflow, ebind)

This is the dominant pattern. Pause is represented as a **first-class status** on the workflow execution.

| Engine | Pause Status Representation | Notes |
|--------|---------------------------|-------|
| **Prefect** | `PAUSED` state type (not terminal). Also `Suspended` (sub-type of `PAUSED`, process exited). Transitions: `Running → Paused → Running` | Name `Paused`, type `PAUSED`. Also has `Resuming` state (type `SCHEDULED`) for scheduled resume. Full state machine in the Prefect backend. |
| **Argo Workflows** | Workflow `spec.status` becomes `suspended`. Also supports `suspend: {}` template step (a blocking step). | `argo suspend WORKFLOW` sets suspended. `argo resume` clears it. Suspend template step acts as a manual approval gate. |
| **Airflow** | DAG-level `is_paused` boolean flag in the metadata DB. | Coarse-grained: prevents *new* runs, doesn't pause in-flight runs. Setting `is_paused = True` tells the scheduler to skip this DAG. |
| **Tekton** | `PipelineRun` `spec.status: "PipelineRunPending"` — a pending state that prevents task execution. | More of a "not started yet" than pausing in-flight. No resume of paused runs once started. |
| **Temporal** | **No built-in pause status.** Users implement via a Workflow Signal that sets a local variable (`paused = true`), then `workflow.Await` blocks execution. | The pause state is a **local variable in workflow code**, reconstructed via Event History replay. Not a server-side status. |

**Key insight for ebind:** The dominant pattern is a **persisted status field** on the workflow/DAG. Temporal's approach (signal → local variable) is fundamentally different because Temporal workflows are long-lived code executions, not declarative DAGs. ebind's DAG engine is declarative, so the status-field pattern (used by Prefect, Argo, Airflow) is the correct model.

### Pattern B: Two-Phase State for Graceful Draining

Prefect uses a `Cancelling` state type (between `Running` and `Cancelled`). This is analogous to ebind's proposed `pausing` transient state.

**Recommendation: Use `pausing` (transient) → `paused` (stable).** This is the most correct approach because:
- Users see an observable transition. When the CLI shows `paused`, they know no steps are executing.
- The scheduler and worker hook can coordinate through this state: scheduler sees `pausing`, blocks new dispatches, but allows completion events to flow. When in-flight count hits 0, transition to `paused`.
- Prefect has `Cancelling` for the same reason — it's the industry best practice for graceful transitions on distributed systems.

**Do NOT:**
- Use a boolean flag alongside existing status (Airflow's `is_paused`). This creates multiple sources of truth and requires `running + paused` vs just `paused` logic everywhere. ebind's existing status-based model is correct.
- Skip the transient state (immediate `running → paused`). This is incorrect because in-flight steps exist and need to drain. The system would be in an inconsistent state during that window.

---

## 2. How They Handle In-Flight Tasks During Pause

**Universal pattern across all engines: In-flight tasks/steps complete naturally; new dispatches are blocked.**

| Engine | In-Flight Behavior | Source |
|--------|-------------------|--------|
| **Airflow** | "Any running tasks are allowed to complete and all downstream tasks are put in to a state of 'Scheduled'" | Airflow docs (core concepts / DAGs) |
| **Argo** | "Once suspended, a Workflow will not schedule any new steps until it is resumed" | Argo Workflows walk-through (suspending) |
| **Kubernetes Jobs** (`.spec.suspend`) | "Pods that are already running are allowed to complete" | K8s Job docs |
| **Prefect** | Running → Paused transition waits for current execution to yield | Prefect state docs (state transition diagram) |
| **Temporal** | Activities in-flight continue; workflow code blocks on `workflow.Await` | Temporal SDK docs |
| **Tekton** | Graceful cancel: running tasks complete, then pipeline stops | Tekton PipelineRun docs |

**Key insight for ebind:** The consensus approach is clear. When pause is requested:
1. **Block new dispatches immediately** (scheduler gate checks status before dispatching)
2. **Allow in-flight steps to complete** (worker hook writes results, publishes events)
3. **When all in-flight steps drain, the DAG is fully paused** (transition `pausing → paused`)

**Recommendation:** Use the `pausing` transient state + in-flight step count tracking in the scheduler. When count hits zero and status is `pausing`, CAS-transition to `paused`.

**Do NOT:**
- Cancel or Nak in-flight tasks. This would cause work to be lost or duplicated.
- Immediately NAk completion events for paused DAGs. The events should be consumed (to update step state) but the downstream dispatching should be skipped.
- Implement a hard-abort pause path. Users who want immediate termination use `Cancel()`, which already exists. Hard-abort during pause would create an overlapping code path with different semantics.

### The Counting Problem: Determining "All In-Flight Steps Completed"

ebind's scheduler needs to know when to transition `pausing → paused`. There are two approaches:

**Approach A: Count from State (Recommended)**
- When transitioning to `pausing`, the scheduler counts steps with `StatusRunning` in the DAG state.
- On each completion event, decrement the count.
- When count hits 0, transition `pausing → paused`.
- **Pro:** Simple, no extra storage.
- **Con:** Requires the scheduler to maintain this count per-DAG in memory. On leader failover, the new leader's sweep must re-count.

**Approach B: Derive from `pausing` + Event Stream**
- The `pausing` status alone blocks dispatch. The scheduler simply checks `ReadyToRun()` on each event — if DAG is paused and step is ready, don't dispatch.
- A separate goroutine/sweep periodically checks: "is this DAG `pausing` but no steps are in-flight? If so, transition to `paused`."
- **Pro:** More robust against leader failover.
- **Con:** Periodic check adds latency and complexity.

**Recommendation:** Use **Approach A** (count from state) as the primary mechanism because leader failover during the brief `pausing → paused` window is extremely unlikely, and the sweep on leader acquisition handles that edge case. This is the same pattern ebind uses for other edge-triggered transitions (see existing sweep logic in `scheduler.go`).

---

## 3. Persistence Strategies for Paused State

| Engine | Storage | What's Stored |
|--------|---------|---------------|
| **Airflow** | PostgreSQL metadata DB | `is_paused` boolean in DAG model |
| **Prefect** | PostgreSQL backend DB | Full state object (type, name, timestamp, message) |
| **Argo** | Kubernetes CRD (etcd) | Workflow CRD status field |
| **Temporal** | Temporal Server DB (Cassandra/PostgreSQL/SQLite) | Event History (pause is a Signal event, state reconstructed via replay) |
| **Tekton** | Kubernetes CRD (etcd) | PipelineRun status field, `spec.status: "PipelineRunPending"` |
| **ebind (existing)** | NATS KV bucket `ebind-dags` | DAGMeta (status is a field) |

**Key insight for ebind:** The pause status is just a **new value in an existing field** — no new storage infrastructure needed. The existing KV bucket + CAS pattern handles this perfectly:
- `DAGMeta.Status` takes on new values (`pausing`, `paused`)
- CAS write ensures only one writer transitions the DAG (same pattern as cancel)
- On process restart, the scheduler sweep reads `paused` from KV and skips the DAG

**Do NOT:**
- Introduce a separate KV key for pause state (e.g., `dag-123/pause`). This would require coordinating two keys (meta status + pause info). Store everything in `DAGMeta`.
- Store pause metadata separately from the DAG. The pause reason, timestamp, identity should be fields on `DAGMeta`, not a separate record.
- Use a stream-based approach (e.g., "paused if a `pause` event was the last event"). Event sourcing is overkill for this — a status field is simpler and matches the existing pattern.

### What to store in DAGMeta

```go
type DAGMeta struct {
    // Existing fields...
    Status DAGStatus // now includes StatusPausing, StatusPaused

    // New pause/resume audit fields
    PausedAt    *time.Time `json:"paused_at,omitempty"`
    PausedBy    string     `json:"paused_by,omitempty"`
    PausedReason string    `json:"paused_reason,omitempty"`
    ResumedAt   *time.Time `json:"resumed_at,omitempty"`
    ResumedBy   string     `json:"resumed_by,omitempty"`
    ResumedReason string   `json:"resumed_reason,omitempty"`
}
```

---

## 4. How Resume Re-Evaluates DAG State

**Universal pattern: On resume, the engine reads the DAG state (which hasn't changed during pause), re-evaluates `ReadyToRun()`, and dispatches newly-ready steps.**

| Engine | Resume Behavior |
|--------|----------------|
| **Prefect** | `Resuming` state (type `SCHEDULED`), then transitions to `Running`. Same task state as before pause. |
| **Argo** | `argo resume` → workflow continues from where it paused. Previously-executed steps remain done, pending steps re-evaluated. |
| **Airflow** | Unpause → scheduler picks up DAG again. Existing non-terminal runs resume naturally. |
| **Temporal** | Signal sets `paused = false`, workflow code exits `Await` loop, continues. Activity results from before pause are already in Event History. |
| **ebind (proposed)** | `DagResume()` → CAS transition `paused → running`. Scheduler sees the DAG in next event or sweep. Loads full state. `ReadyToRun()` finds pending steps with satisfied dependencies. Dispatches them. |

**Key insight for ebind:** The resume path is almost **free** — the scheduler's existing `handleEvent()` and `sweep()` logic already does exactly what resume needs:
1. Load DAG state from KV
2. Call `ReadyToRun()` → find steps with satisfied dependencies
3. Dispatch those steps
4. If no ready steps and some pending, do nothing (they'll be dispatched when their dependencies complete)

The only thing `DagResume()` needs to do is:
1. CAS the DAG status from `paused` → `running` (reject if not paused)
2. Optionally trigger an event or just let the next sweep pick it up

**Recommendation:** Have `DagResume()` call an explicit re-evaluation of ready steps inline (or via a dedicated event), rather than waiting for the next sweep. This provides immediate feedback to the user ("4 steps dispatched on resume"). This matches Prefect's `resume_flow_run()` behavior and Argo's `argo resume` behavior.

**Do NOT:**
- Re-submit the DAG from scratch. The DAG state is intact — steps completed before pause remain completed. Only pending steps need consideration.
- Clear results from completed steps. They're still valid.
- Reset retry counts on failed steps. They remain failed.

---

## 5. Transport/API Patterns for Pause/Resume Commands

### CLI Patterns

| Engine | CLI Command | Flags |
|--------|-------------|-------|
| **Temporal** | `temporal workflow pause --workflow-id <id> [--reason "msg"]` | `--workflow-id`, `--run-id`, `--reason` |
| **Airflow** | `airflow dags pause <dag_id>` / `airflow dags unpause <dag_id>` | (positional) |
| **Argo** | `argo suspend WORKFLOW` / `argo resume WORKFLOW` | (positional) |

### API Patterns

| Engine | API |
|--------|-----|
| **Prefect** | `pause_flow_run(flow_run_id)` / `resume_flow_run(flow_run_id)` — Python API |
| **Temporal** | gRPC `SignalWorkflowExecution` with signal name like "pause" |
| **ebind (proposed)** | `Workflow.DagPause(ctx, dagID)` / `Workflow.DagResume(ctx, dagID)` — Go API |

### Key Observations

1. **Every engine uses simple blocking API calls.** The caller expects the call to succeed (or return an error) but does NOT expect to wait for the pause to complete. This is important — `DagPause()` should be fast (a CAS write), not a blocking wait for all in-flight steps to drain.

2. **Optional reason field is universal.** Temporal has `--reason`, Airflow logs the user, ebind should follow suit.

3. **No engine uses message passing for pause.** It's always a direct API call or CLI command. This means ebind's `DagPause()` → CAS write to KV is the correct pattern. The scheduler doesn't need a "pause event" stream — it reads the status from KV on the next event.

4. **Temporal's signal pattern is the outlier.** Temporal uses gRPC Signals because its workflows are long-lived code executions, not declarative DAGs. ebind is declarative — signals would be overkill.

**Recommendation for ebind API surface:**

```go
// On Workflow type:
DagPause(ctx context.Context, dagID string, opts ...PauseOption) error
DagResume(ctx context.Context, dagID string, opts ...ResumeOption) error

// Options types:
type PauseOption func(*PauseOptions)
func PauseWithReason(reason string) PauseOption
func PauseWithCaller(caller string) PauseOption  // defaults to "cli" or "api"

type ResumeOption func(*ResumeOptions)
func ResumeWithReason(reason string) ResumeOption
```

```go
// CLI:
ebctl dag pause <dag-id> [--reason "..."]
ebctl dag resume <dag-id> [--reason "..."]
```

### Command Dispatch Architecture

The pause/resume command flow should be:

```
User: DagPause(ctx, "dag-123")
  ↓ Workflow.DagPause()
  ↓ store.GetMeta("dag-123")
  ↓ validate: status is running|pausing (not done/failed/canceled)
  ↓ set DAGMeta.Status = StatusPausing, set audit fields
  ↓ store.PutMeta("dag-123", meta, expectedRev)  // CAS
  ↓ return nil (fast, don't wait for pausing→paused)

Scheduler (on next event for this DAG):
  ↓ load DAGMeta from KV
  ↓ sees StatusPausing
  ↓ blocks dispatch of ready steps
  ↓ counts in-flight → if 0, CAS transition to StatusPaused

Scheduler (on leader sweep):
  ↓ loads all DAGs
  ↓ for each StatusPausing DAG, count in-flight → if 0, transition to StatusPaused
```

**Do NOT:**
- Publish a `DAG_PAUSE` event to EBIND_DAG_EVENTS. The status-in-KV pattern is cleaner and doesn't require new stream subjects. The scheduler already reads meta on each event — it naturally discovers the pause state.
- Make `DagPause()` blocking until fully paused. Return immediately. Users who want to wait can poll `dag get --watch` or subscribe to DAG events.
- Implement a two-way handshake (API → worker → API). This adds latency and complexity for no benefit.

---

## 6. Go-Specific Libraries and Patterns for Workflow Pause/Resume

### Temporal Go SDK (Signal-Based Pause)

Temporal is the only other Go workflow engine with a pause pattern, and it's **user-implemented**, not built-in:

```go
func MyWorkflow(ctx workflow.Context, ...) error {
    var paused bool
    pausedSignal := workflow.GetSignalChannel(ctx, "pause-signal")
    resumeSignal := workflow.GetSignalChannel(ctx, "resume-signal")

    // ... do work ...

    // Check/wait for pause
    selector := workflow.NewSelector(ctx)
    selector.AddReceive(pausedSignal, func(c workflow.ReceiveChannel, _ bool) {
        c.Receive(ctx, nil)
        paused = true
    })
    selector.Select(ctx)  // blocks until signal or other event

    if paused {
        workflow.Await(ctx, func() bool {
            // Wait until resume signal arrives
            var resume bool
            selector.AddReceive(resumeSignal, func(c workflow.ReceiveChannel, _ bool) {
                c.Receive(ctx, nil)
                resume = true
            })
            // This will block the workflow until resumed
        })
    }
}
```

**Why this doesn't apply to ebind:**
- Temporal workflows are **code-first**: the pause state is a variable in user code, reconstructed deterministically via replay.
- ebind is a **declarative DAG engine**: the DAG structure is defined at submission time, step status is stored in KV. There's no "workflow code" to inject a pause check into.
- The temporal pattern requires the workflow author to build pause support into every workflow. ebind users don't write workflow code — they register functions and define DAGs.

### Go-Specific Libraries

There are **no Go-specific libraries** for workflow pause/resume. The pattern must be implemented at the engine level. This is consistent across the ecosystem — pause/resume is a **control plane feature**, not a library concern.

The closest analog is the **Kubernetes controller pattern** (reconciliation loop with observed state vs desired state), but even that doesn't have a reusable pause library.

### What Go Patterns DO Apply to ebind

| Go Pattern | Where It Applies |
|------------|-----------------|
| `sync.WaitGroup` (conceptual) | Counting in-flight steps: the scheduler maintains a per-DAG count of running steps, decremented on completion events. When count hits 0 for a `pausing` DAG, transition to `paused`. |
| CAS + Retry (existing ebind pattern) | All status transitions use `KV.Update(key, val, expectedRevision)` with retry on CAS failure. Pause/resume follows the same pattern as Cancel. |
| `context.Context` | Passed through `DagPause(ctx, ...)` and `DagResume(ctx, ...)` for cancellation/deadline support. |
| Option functions (functional options) | `PauseWithReason()`, `PauseWithCaller()` etc. follow the same pattern as `StepOption` in the existing codebase. |
| Interface isolation for testability | `MemStore` already implements `StateStore`. Add pause/resume tests using it (no NATS needed). |
| `atomic.Int64` | For the in-flight step count per DAG in the scheduler, an atomic counter avoids mutex contention on the hot path (completion events). |

---

## Summary: What ebind Should Do

### Recommendation: Status-Field Pattern with `pausing` → `paused` Transitions

Based on the research, ebind's approach as defined in PROJECT.md is **correct and aligned with industry practice**. Here's the mapping:

| Aspect | ebind Approach | Industry Precedent | Confidence |
|--------|---------------|-------------------|------------|
| **State representation** | New status values: `pausing`, `paused` (lowercase, consistent) | Prefect (`PAUSED`), Argo (`suspended`), Airflow (`is_paused`) | HIGH |
| **In-flight handling** | Let them complete, block new dispatches | All major engines do the same | HIGH |
| **Persistence** | KV bucket via CAS (existing pattern) | All engines use their existing persistence | HIGH |
| **Resume re-evaluation** | `ReadyToRun()` after status change, same as step completion event | Argo, Prefect do the same | HIGH |
| **Transport/API** | Go API method + CLI command, not signals | Temporal, Airflow, Argo all use direct API/CLI | HIGH |
| **Audit trail** | `paused_at`, `paused_by`, `paused_reason` on DAGMeta | Temporal has `--reason`; Airflow logs user | HIGH |

### What NOT to Do (Based on Ecosystem Evidence)

1. **Do NOT use signals/messages for pause commands.** Every major engine uses direct API calls. Signals add latency, require new stream subjects, and the scheduler already reads meta from KV on every event — it will naturally discover the pause state.

2. **Do NOT hard-abort in-flight tasks.** Every engine lets them complete. This is the industry standard. Users who want immediate termination use Cancel.

3. **Do NOT implement a blocking pause.** `DagPause()` should return immediately after the CAS write. The `pausing → paused` transition happens asynchronously. This matches how Argo's `argo suspend` works — it returns immediately, the workflow transitions to suspended state when it reaches a safe point.

4. **Do NOT re-submit or restart the DAG on resume.** The state is intact. Just flip the status bit and let the scheduler evaluate ready steps.

5. **Do NOT add a new KV bucket or key space.** The `ebind-dags` bucket + DAGMeta is sufficient. Adding `dag-123/pause` as a separate key creates coordination problems.

6. **Do NOT build auto-resume or pause timeout initially.** No major engine supports this natively. Temporal doesn't, Airflow doesn't, Argo doesn't. It's a valid future extension but not MVP.

### Edge Cases (from ecosystem research)

1. **Pause while already paused** → IDEMPOTENT, return success. Matches Argo behavior.

2. **Resume while already running** → IDEMPOTENT, return success. Matches Argo behavior.

3. **Pause a completed/canceled/failed DAG** → ERROR. Airflow and Argo both reject this.

4. **Pause while in `pausing`** → IDEMPOTENT, return success. This is ebind-specific (no other engine has this transient state), but logically correct.

5. **Resume while in `pausing`** → WAIT for `paused` transition, then resume. Or REJECT with "DAG is still transitioning to paused, retry in a moment." The `pausing` window is brief (seconds at most).

6. **Pause with no in-flight steps** → Skip the `pausing` transient state entirely. CAS directly from `running → paused`. This is an optimization that avoids an unnecessary state transition when there's nothing to drain.

### Potential Pitfalls

| Pitfall | Where It Happens | Prevention |
|---------|-----------------|------------|
| Scheduler races on `pausing → paused` transition | Two scheduler instances both see in-flight count hit 0 and try to CAS | CAS handles this — one wins, the other retries and sees already-paused |
| In-flight count is stale after leader failover | New leader's sweep sees `pausing` but doesn't know the count | Sweep must recount from KV state (list steps, count `StatusRunning`). Same as existing sweep-for-stranded-steps logic. |
| `pausing` DAG appears to be "stuck" if in-flight count never hits 0 | A step gets stuck in `StatusRunning` forever (e.g., consumer lost, timeout too long) | The system handles this the same as any other stuck step — the existing timeout/deadline mechanism handles it. The `pausing → paused` transition waits for it. |
| Concurrent pause + resume calls | Two goroutines call pause then resume nearly simultaneously | CAS on DAGMeta handles this. Second call gets a CAS revision mismatch and retries, seeing the updated status. |

---

## Sources

| Source | URL | What It Confirms | Confidence |
|--------|-----|-----------------|------------|
| Prefect States docs | https://docs.prefect.io/latest/concepts/states/ | `PAUSED` state type, `Paused` → `Running` transitions, `Resuming` state | HIGH — official docs |
| Argo Workflows Suspend walk-through | https://argo-workflows.readthedocs.io/en/latest/walk-through/suspending/ | `argo suspend/resume` CLI, `suspend: {}` template, suspend behavior ("will not schedule any new steps") | HIGH — official docs |
| Airflow CLI reference | https://airflow.apache.org/docs/apache-airflow/stable/cli-and-env-variables-ref.html | `airflow dags pause/unpause` commands with positional args | HIGH — official docs |
| Temporal Workflow Execution docs | https://docs.temporal.io/workflow-execution | Workflow statuses (Open: Running, Closed: Cancelled/Completed/etc), Event History replay on resume | HIGH — official docs |
| Temporal Go SDK samples | https://github.com/temporalio/samples-go | Signal-based pause pattern (user-implemented), no built-in pause | HIGH — official repo |
| Tekton PipelineRun docs | https://tekton.dev/docs/pipelines/pipelineruns/#cancelling-a-pipelinerun | Cancel/graceful stop, `PipelineRunPending` status, running tasks complete first | HIGH — official docs |
| Kubernetes Jobs `.spec.suspend` | https://kubernetes.io/docs/concepts/workloads/controllers/job/#suspending-a-job | In-flight pods complete, new pods not created, status preserved across restarts | HIGH — official docs |
| ebind codebase/ARCHITECTURE.md | `.planning/codebase/ARCHITECTURE.md` | Scheduler pattern, CAS-driven state, EventBus + KV, existing Cancel pattern | HIGH — project source |
| ebind PROJECT.md | `.planning/PROJECT.md` | Two-phase pause design constraints, KV CAS requirements, out-of-scope items | HIGH — project source |
| ebind research/FEATURES.md | `.planning/research/FEATURES.md` | Table stakes, differentiators, anti-features, edge cases | HIGH — project source |
