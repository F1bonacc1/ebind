# Codebase Concerns

**Analysis Date:** 2026-06-03

## Tech Debt

### 1. KV List operations scan all keys (O(n))

**Issue:** `NatsStore.ListSteps()` (`workflow/store_nats.go:137-158`) and `ListDAGs()` (`workflow/store_nats.go:115-135`) iterate every key in the KV bucket via `kv.ListKeys()`, then filter by prefix. Every key in the bucket is loaded into memory and decoded just to find a subset.

**Files:** `workflow/store_nats.go`

**Impact:** As the number of DAGs or steps grows, these operations become linearly more expensive. The scheduler's `sweep()` calls `ListDAGs()`, then `ListSteps()` per DAG — meaning a sweep of N DAGs with M steps each does N×M key scans. This limits the practical scale of concurrent workflows.

**Fix approach:** Use NATS JetStream KeyValue bucket with keys that map to subject-based queries, or maintain index records (e.g., a single KV entry containing all DAG IDs for a given prefix). Alternatively, delegate list operations to a dedicated JetStream stream with subject-based filtering.

### 2. Dynamic step idempotency relies on FnName match

**Issue:** `ContextDAG.StepOpts()` (`workflow/context.go:115-123`) handles duplicate step creation (from handler retries) by checking if the existing record has the same `FnName`. If the function reference was re-registered under a different name between retries, it silently succeeds — the step exists but points at the wrong handler.

**Files:** `workflow/context.go`

**Impact:** Rare edge case during rolling code deploys where handler registration changes. Could cause a dynamically-added step to execute the wrong function without error.

**Fix approach:** Add `stepID` → `(dagID, stepID)` dedupe that reads the full existing record and validates all fields match, not just FnName. Or publish step-added events with idempotency keys that the scheduler deduplicates.

### 3. Cancel() CAS retry gives up after 5 attempts

**Issue:** `Cancel()` (`workflow/cancel.go:13-30`) retries meta CAS up to 5 times then gives up. If the meta is being concurrently updated by the scheduler's `maybeFinalize()` (`workflow/scheduler.go:298-316`), cancellation may silently fail without error propagation to the caller (the last error is swallowed by the `break` on success, but failure exits the retry loop and returns the last error).

**Files:** `workflow/cancel.go`

**Impact:** Under heavy concurrent finalization + cancellation, a cancel call could return a `ErrStaleRevision` error without having actually canceled the DAG. The caller sees an error but the DAG remains running.

**Fix approach:** Increase retry ceiling, use exponential backoff, or use a write-through pattern that doesn't need retry (e.g., a purpose-built "cancel" record that the scheduler checks).

### 4. Scheduler sweep loads every running DAG into memory

**Issue:** `Scheduler.sweep()` (`workflow/scheduler.go:95-115`) lists ALL running DAGs, loads full state (meta + all step records) for each, and re-evaluates readiness. For deployments with thousands of long-running DAGs, this creates a large, synchronous memory and IO spike on each leadership acquisition.

**Files:** `workflow/scheduler.go`, `workflow/store_nats.go`

**Impact:** Scales poorly with DAG count. A single sweep could take many seconds (or time out at `SweepTimeout` default 60s), during which the leader is busy and event-driven scheduling is also running concurrently.

**Fix approach:** Paginate the DAG list, limit sweep to DAGs with non-terminal steps, or maintain a "dirty DAG" index so sweep only touches DAGs that might need attention.

### 5. Test harness races with time.Sleep

**Issue:** `testutil.SingleNode()` (`internal/testutil/harness.go:68`) sleeps 200ms after starting the worker before returning. This assumes the consumer binds within that window. On a loaded CI machine, the sleep may be insufficient, causing enqueue-before-consume races in tests.

**Files:** `internal/testutil/harness.go`

**Impact:** Occasional flaky tests in CI. The `make test-count` (3× runs) pattern suggests flakiness is a known concern.

**Fix approach:** Replace the sleep with a condition variable or a health-check subscription that confirms the consumer is registered before returning.

### 6. Ephemeral response consumer loses responses on restart

**Issue:** `Client.New()` (`client/client.go:59-65`) creates an ephemeral consumer (non-durable) for response messages. If the client process restarts before consuming all responses, those responses are orphaned — they exist in `EBIND_RESP` stream but no consumer is attached to deliver them.

**Files:** `client/client.go`

**Impact:** Fire-and-forget is unaffected. But `Future.Get()` callers that restart before receiving a response will never get it. The response data remains in the stream (up to `ResponseMaxAge`, default 1h) but is inaccessible.

**Fix approach:** Use durable consumers keyed by client ID so reconnecting clients resume receiving responses. However, this requires careful management of consumer lifecycle to avoid leaking durables.

### 7. Worker does not react to NATS disconnection

**Issue:** `Worker.Run()` (`worker/worker.go:128-195`) only exits when `ctx` is canceled. If the NATS connection drops (network partition, server restart), the consume callbacks fail silently and the worker goroutine remains blocked. The worker has no reconnection or health-check logic.

**Files:** `worker/worker.go`

**Impact:** A transient NATS outage causes the worker to become permanently stuck. Operators must restart the process. In embedded NATS mode, this is less likely (same process); in external NATS mode, it's a real operational risk.

**Fix approach:** Add connection event handlers (`nats.Conn.ReconnectHandler`, `nats.Conn.DisconnectedErrHandler`) that trigger worker shutdown or health-check with reconnection.

## Security Considerations

### 1. Reflection-based dispatch accepts arbitrary JSON

**Risk:** `Dispatcher.Call()` (`task/registry.go:129-181`) unmarshals the payload JSON directly into `reflect.New(argTypes[i])` created values. While Go's `json.Unmarshal` is type-safe for concrete types (won't set unexported fields), a crafted payload that triggers resource exhaustion (e.g., deeply nested JSON, large strings, many args) could cause memory issues. Reflection bypasses static analysis that would catch these.

**Files:** `task/registry.go`, `worker/worker.go`

**Current mitigation:** Payload must be a JSON array. Arg count is validated against the registered signature. Each arg is unmarshalled into the registered type — a non-matching type produces a `ErrKindDecode` error.

**Recommendations:** Add maximum payload size enforcement at the worker level (before unmarshalling). Document the security boundary: the TASKS stream should be access-controlled so only trusted producers can publish.

### 2. Error messages may leak sensitive information

**Risk:** Handler error messages (`err.Error()`) are persisted into:
- Step records (`error_message` field, up to `DefaultMaxStepErrorBytes` = 4096 bytes)
- DLQ entries (`EBIND_DLQ` stream, 7d retention)
- Response envelopes (`EBIND_RESP` stream, 1h retention)

If handlers return errors containing sensitive data (PII, tokens, internal paths), this data is durably stored across multiple NATS streams and accessible via `ebctl dlq show` or `ebctl dag step get`.

**Files:** `workflow/hook.go:43-54`, `worker/worker.go:426-441`, `dlq/dlq.go`

**Current mitigation:** `Workflow.MaxStepErrorBytes` can be set to a negative value to persist only the error kind (not the message). Error message truncation at rune boundaries preserves valid UTF-8 but does nothing to filter sensitive content.

**Recommendations:** Document that handlers should not include sensitive data in error messages intended for ebind propagation. Consider adding a `SanitizeError` middleware that redacts patterns before they reach the StepHook.

### 3. Registry is append-only with no eviction

**Risk:** `Registry.Register()` (`task/registry.go:51-101`) has no mechanism to unregister or replace handlers. Long-running processes that dynamically load/unload code modules cannot update the registry.

**Files:** `task/registry.go`

**Current mitigation:** None — the registry only grows.

**Recommendations:** This is an architectural limitation by design. Document it explicitly. If dynamic re-registration becomes a requirement, add `Replace` and `Unregister` methods with appropriate safety checks.

## Performance Bottlenecks

### 1. KV List operations for all DAGs/steps

**Problem:** See Tech Debt #1. `ListDAGs()` and `ListSteps()` are O(n) in total KV keys. Every sweep or status check pays the full scan cost.

**Files:** `workflow/store_nats.go`

**Cause:** NATS KV `ListKeys()` returns all keys in the bucket without server-side filtering. The client must iterate and filter.

**Improvement path:** Add a dedicated JetStream stream that indexes DAG IDs and step IDs as subject-based messages, enabling server-side subject filtering for list operations.

### 2. Scheduler re-loads full state per event

**Problem:** Every scheduler event (`onCompleted`, `onStepAdded`) calls `loadState()` (`workflow/scheduler.go:319-333`) which fetches meta + all step records from the store. For a DAG with 1000 steps, this means 1000+ KV `Get` operations per event.

**Files:** `workflow/scheduler.go`, `workflow/store_nats.go`

**Cause:** The scheduler operates on a fresh in-memory snapshot for each event rather than maintaining incremental state.

**Improvement path:** Cache the DAGState in memory and apply transitions locally, only persisting deltas. This would require invalidation on concurrent modifications (e.g., from another scheduler instance after failover).

### 3. Bulk Message Publishing

**Problem:** For DAGs where many steps become ready simultaneously (fan-out patterns), the scheduler enqueues them one at a time via individual `js.Publish()` calls. Each call is a separate network round-trip to NATS.

**Files:** `workflow/enqueuer_nats.go` — no batch publish support.

**Improvement path:** Use `jetstream.PublishAsync()` with a flush at the end of the ready batch to batch publish multiple task envelopes in fewer round-trips.

## Fragile Areas

### 1. CanonicalName instability

**Files:** `task/registry.go:244-254`

**Why fragile:** `CanonicalName` derives the handler name from `runtime.FuncForPC(fn).Pointer()`. This means the name changes if:
- The function is renamed
- The package is renamed or moved
- The file is moved to a different package
- The function becomes a method (or vice versa)

Any of these changes breaks all in-flight tasks that reference the old canonical name. There is no automated detection — operators learn about the breakage when tasks fail with `ErrKindUnknownHandler`.

**Safe modification:** Always use `task.Alias("package.OldName")` when changing function locations. Test by submitting a task before upgrade, deploying, and verifying it still resolves.

**Test coverage:** No test validates canonical name stability across refactors. The `registry_test.go` tests registration round-trips but not name migration.

### 2. Task.Task envelope is a wire format

**Files:** `task/task.go:8-22`

**Why fragile:** `task.Task` is serialized to JSON and published to NATS. Adding a field is safe (zero-valued for old messages) but:
- Removing a field breaks old in-flight messages that contain it (JSON decode won't fail, but data is lost)
- Renaming a field changes the wire format
- Any field with `omitempty` may behave differently depending on Go zero values

**Test coverage:** No explicit wire-format backwards compatibility tests in the codebase.

### 3. Scheduler event ordering across leader failover

**Files:** `workflow/scheduler.go:31-42`, `workflow/events_nats.go:48-73`

**Why fragile:** The scheduler subscribes to `DAG.>` events with a shared durable consumer (`ebind-scheduler`). On leader failover:
1. Old leader Nak's events it hasn't processed
2. New leader picks up from the Nak'd event
3. But the old leader may have partially applied state before crashing

The KV CAS layer protects against double-enqueue (duplicate `Nats-Msg-Id`), and step status CAS protects against double-transition. However, the timing window between "event received" and "state persisted" could theoretically cause a step to be enqueued twice if the old leader wrote to KV but crashed before acking the event.

**Safe modification:** This is the known "at-most-once" design described in CLAUDE.md. Adding a write-ahead log or two-phase commit would increase complexity. The current design is a conscious trade-off.

### 4. dlq/requeue.go does not validate against current registry

**Files:** `dlq/requeue.go`

**Why fragile:** Requeuing a DLQ'd task publishes the original envelope back to the TASKS stream. If the handler was renamed or unregistered between the original failure and the requeue, the task will fail with `ErrKindUnknownHandler`. The requeue operation has no validation against the current registry.

**Test coverage:** The `requeue_test.go` tests the requeue mechanics but not name resolution validation.

## Scaling Limits

### Single NATS dependency

**Current capacity:** A single NATS JetStream server (or 3-node cluster) handles all streams, KV buckets, consumers, and routes.

**Limit:** NATS JetStream has per-stream, per-consumer, and per-KV-bucket limits. Key constraints:
- JetStream max streams: default 100 (configurable)
- Max consumers per stream: default 200
- Max KV bucket size: bounded by file storage
- `EBIND_DAG_EVENTS` uses WorkQueue retention (24h max age) — events accumulate until consumed or aged out

**Scaling path:** The architecture requires NATS to scale. For very large deployments, this means scaling the NATS cluster (adding nodes, partitioning streams). The library itself has no built-in partitioning or multi-cluster support.

### DAG step count per DAG

**Current capacity:** All step records for a DAG are loaded into memory by the scheduler on every event for that DAG.

**Limit:** DAGs with tens of thousands of steps become impractical due to memory usage for `loadState()` and O(n) scan of `ReadyToRun()`. Each `ReadyToRun` evaluation iterates all steps (`workflow/state.go:72-83`).

**Scaling path:** Paginate step loading, implement lazy dependency evaluation, or add a "ready set" index.

## Dependencies at Risk

### NATS Server v2.14.1

**Risk:** The embedded NATS dependency (`github.com/nats-io/nats-server/v2 v2.14.1` in `go.mod`) is a specific version. NATS Server v2 has a known fast release cadence with occasional breaking changes in JetStream APIs.

**Files:** `go.mod`

**Impact:** A security vulnerability or bug in this version would affect every ebind deployment that uses embedded NATS. Upgrading requires re-validation of all stream and consumer configuration.

**Migration plan:** No alternative — the architecture is NATS-native. Keep the dependency up to date via Dependabot/Renovate and CI test coverage.

## Missing Critical Features

### No in-memory-only embedded NATS option

**Problem:** `embed.StartNode()` requires a `StoreDir`. There is no "transient" mode for single-node embedded NATS that uses memory-only storage. All tests write to temporary directories.

**Files:** `embed/node.go`

**Blocks:** Lightweight ephemeral deployments (e.g., serverless functions, short-lived batch jobs) that don't need durable storage.

### No per-step timeout override

**Problem:** Workflow steps inherit the DAG-level or worker-level AckWait. There is no per-step option to set a shorter timeout. A step expected to complete in 100ms shares the same timeout as a step that might take 5 minutes.

**Files:** `workflow/step.go` — no `WithStepTimeout` or similar option.

**Blocks:** Fine-grained timeout control for mixed-latency workflows.

### No handler versioning strategy

**Problem:** The canonical name scheme provides no versioning. Deploying `v2` of a handler at the same canonical name as `v1` means in-flight `v1` tasks run `v2` code — which may have incompatible expectations about input data.

**Files:** `task/registry.go`

**Blocks:** Blue-green deployments where both handler versions coexist during the migration window.

## Test Coverage Gaps

### No wire-format backwards compatibility tests

**What's not tested:** Adding new fields to `task.Task` and confirming old-format messages still decode correctly. The zero-value semantics are relied upon but not tested.

**Files:** `task/task.go`

**Risk:** An additive change that accidentally breaks backwards compatibility would only be caught in integration tests against stored NATS data (which are not run as part of CI).

**Priority:** Low — the risk is low for additive changes, but the gap means there's no safety net.

### No CAS conflict stress test

**What's not tested:** Concurrent `Cancel()` + `sweep()` + `StepHook.OnStepDone()` + `Scheduler.onCompleted()` all racing on the same DAG's step records via CAS.

**Files:** `workflow/scheduler.go`, `workflow/cancel.go`, `workflow/hook.go`, `workflow/store_nats.go`

**Risk:** The CAS retry logic (5 attempts) in `persistStatus`, `casUpdateStatus`, and `Cancel` is tested individually but not under sustained concurrent contention. A race that exceeds the retry limit would manifest as an opaque `ErrStaleRevision` error.

**Priority:** Medium — the CAS design is the correctness foundation. Stress testing would validate the retry ceilings.

### No scheduler overload protection tests

**What's not tested:** What happens when events arrive faster than the scheduler can process them. The `mu` lock serializes event handling, but there's no backpressure mechanism — the NATS consumer will continue delivering events to the callback goroutine.

**Files:** `workflow/scheduler.go`, `workflow/events_nats.go:59-67`

**Risk:** Under high event throughput, the scheduler's event handler goroutine blocks on `mu.Lock()`, while the NATS consumer continues to dispatch new goroutines for each event. This creates unbounded goroutine growth.

**Priority:** Low — the WorkQueue policy on the events stream means events are redelivered if not acked. But memory pressure from stacked goroutines is unaddressed.

---

*Concerns audit: 2026-06-03*
