# CLAUDE.md — guide for AI working on this codebase

This file is the minimum context an AI agent needs to work on ebind productively.

## What ebind is

A Go library that provides:
1. **Task queue** — serializable Go-function-backed tasks over NATS JetStream.
2. **DAG workflow engine** — durable multi-step workflows with dependencies, retries, and dynamic step addition.
3. **Embedded NATS** — in-process NATS JetStream (single-node or 3-node HA cluster) so a production deployment needs no external infrastructure.

**Everything runs on a single NATS dependency.** No Redis, no Postgres.

## The two load-bearing design decisions

### 1. Reflection-based function registry

Functions are not serialized. A registry maps canonical names (derived from `runtime.FuncForPC`) to `reflect.Value`s. At dispatch time, `reflect.Value.Call` invokes the function with JSON-decoded args.

**Why reflection, not generics:** Go lacks variadic generics. A function-first API like `Enqueue(c, MyFunc, a, b, c)` where `a, b, c` match `MyFunc`'s signature cannot be expressed with generics alone.

**Critical invariants:**
- Canonical name = last segment of `runtime.FuncForPC(fn).Name()` (e.g. `github.com/you/app/handlers.Foo` → `handlers.Foo`). Renaming or moving a function breaks in-flight tasks. Use `task.WithName("...")` or `task.Alias("...")` to override.
- Handler signature must be `func(context.Context, args...) (T, error)` or `func(context.Context, args...) error`. Validation happens at `Register` and at `Enqueue` (client-side, before publish).
- `reflect.Value.Call` costs ~100–500 ns — negligible vs. real handler work (I/O, disk).

See `task/registry.go::Register` and `task/registry.go::Describe`.

### 2. State lives in NATS KV; streams carry events and tasks

**KV bucket `ebind-dags`** (workflow only):
- `<dag_id>/meta` — DAG-level metadata (status, default policy)
- `<dag_id>/step/<step_id>` — StepRecord (fn name, unresolved Refs in args, deps, status, retry policy)
- `<dag_id>/result/<step_id>` — raw result bytes

**JetStream streams:**
- `EBIND_TASKS` (WorkQueuePolicy) — task envelopes consumed by workers.
- `EBIND_RESP` (LimitsPolicy, short MaxAge) — per-client response envelopes for Future resolution.
- `EBIND_DLQ` (LimitsPolicy) — failed/dead-lettered tasks.
- `EBIND_DAG_EVENTS` (WorkQueuePolicy) — scheduler events (`DAG.<id>.completed.<step>`, `DAG.<id>.step-added.<step>`).

**All state mutations use CAS** (`KV.Update(key, val, expectedRevision)`). Two writers racing on the same key: one wins, the other retries. This is the foundation that lets the scheduler run in every worker without an explicit "leader" for correctness — KV CAS + JetStream queue semantics + msg-id dedupe give at-most-once effects. The `LeaderElector` is defense-in-depth for failover windows, not a correctness requirement.

## Architecture in one diagram

```
producer ─► TASKS stream ─► worker pool ─► handler fn ─► result
                                    │
                                    ├─► RESP.<client_id>.<task_id> ─► Future.Get/Await
                                    │
                                    └─(if DAG)─► StepHook ─► KV {step=Done, result=bytes}
                                                         │
                                                         └─► DAG events stream ─► Scheduler
                                                                                      │
                                                                                      └─► enqueue next step
```

## Package layout (and what each file does)

```
task/
  task.go                envelope + Response + TaskError types
  registry.go            Register(fn), Describe(fn), Dispatcher.Call
  retry.go               RetryPolicy.NextDelay/ShouldRetry (pure)
  retry_test.go          ~14 boundary cases

client/
  client.go              Client.New, Enqueue, EnqueueOpts, response routing
  future.go              Future.Get + Await[T] generic helper

worker/
  worker.go              Run loop, handle(), policy-aware retry
  middleware.go          Middleware chain builder, Recover, Log
  hook.go                StepHook interface (workflow decoupling seam)

stream/
  setup.go               EnsureStreams (TASKS, RESP, DLQ)

dlq/
  dlq.go                 DLQ.Publish — failed task entries

embed/
  node.go                StartNode (single-node embedded NATS)
  cluster.go             StartCluster (3-node HA, loopback routes, WaitReady)

workflow/
  errors.go              ErrStepFailed, ErrStepSkipped sentinels
  ref.go                 Ref type + ResolveArgs (pure substitution)
  state.go               DAGState + MarkDone/MarkFailed/MarkSkipped (pure)
  store.go               StateStore interface
  store_mem.go           In-memory impl (tests + exported for external use)
  store_nats.go          JetStream KV impl
  events.go              EventBus interface + Event types
  events_mem.go          Channel-based impl for tests
  events_nats.go         JetStream events impl
  enqueuer.go            Enqueuer + LeaderElector interfaces
  enqueuer_nats.go       JetStream task enqueuer
  workflow.go            Workflow coordinator struct
  nats.go                NewFromNATS convenience constructor
  step.go                Step + StepOption
  dag.go                 DAG builder, Submit, cycle detection
  scheduler.go           handleEvent + watchLeadership + sweep
  hook.go                workflow.StepHook (implements worker.StepHook)
  context.go             FromContext — dynamic step addition
  await.go               Await[T] via KV WatchResult + DAGInfo

internal/testutil/
  harness.go             SingleNode helper

cmd/demo/
  main.go                Single-process e2e demo
```

## Test taxonomy

- **Pure unit tests** — no NATS, no goroutines beyond test runner:
  - `task/retry_test.go`
  - `workflow/state_test.go`, `workflow/ref_test.go`, `workflow/dag_test.go`
- **Unit with fakes** — MemStore + MemBus + captureEnq:
  - `workflow/scheduler_test.go`, `workflow/store_test.go`
- **Integration** — real in-process NATS via `embed.StartNode`:
  - `worker/worker_test.go`, `workflow/integration_test.go`
- **Cluster integration** — 3-node in-process cluster:
  - `embed/cluster_test.go`

Run patterns:
```sh
make test              # all tests with -race
make test-short        # unit-only
make test-count        # 3× runs (flake hunt)
make cover             # HTML coverage
```

## Key invariants & gotchas

### Task envelope is immutable across redeliveries
`task.Task.Attempt` is NOT persisted into the envelope. The worker derives the true attempt count from `msg.Metadata().NumDelivered`. Never trust `t.Attempt` as a "set by the producer" value — it's set by the worker on ingestion.

### Consumer MaxDeliver vs task-level MaximumAttempts
The NATS consumer has a hard cap (`worker.Options.MaxDeliver`, default 5). A task's `RetryPolicy.MaximumAttempts` can tighten this but cannot exceed it — the consumer stops redelivering regardless. For DAG workflows that want long retry chains, increase `worker.Options.MaxDeliver` at the worker level.

### Dedupe window
JetStream dedupe uses `Nats-Msg-Id`. For ad-hoc `client.Enqueue`, the ID is the task's uuid. For DAG steps enqueued by the scheduler, the ID is `<dag_id>:<step_id>` — this protects against duplicate-enqueues during scheduler races. The default dedupe window is **5 minutes**; long-polling tests should keep that in mind.

### Scheduler serialization
Within one scheduler instance, `mu` serializes event handling. Across instances, JetStream's durable consumer gives one delivery per event. The `LeaderElector` adds a third guard (non-leaders Nak) but is not required for correctness — KV CAS + dedupe suffice.

### Sweep on leader acquisition
When `IsLeader()` flips false→true, the scheduler sweeps all running DAGs and re-enqueues any stranded ready steps. Edge-triggered, not level-triggered. Defaults: 5s poll, 60s sweep timeout. Overlap-guarded.

### CanonicalName instability
Do not rename or move a registered handler function in production deployments without an alias:
```go
task.MustRegister(reg, Foo, task.Alias("handlers.OldFooName"))
```
Old in-flight messages will resolve through the alias.

## Common tasks for AI agents

### Adding a new middleware
1. Implement the `worker.Middleware` signature — `func(next worker.Handler) worker.Handler`.
2. Add to a worker via `Options.Middleware` or `w.Use(...)`.
3. Write a unit test using `internal/testutil.SingleNode` — register a handler, enqueue via `client.Enqueue`, assert middleware side effects.

### Adding a new DAG feature
1. Start with pure logic in `workflow/state.go` or `workflow/ref.go` — 100% unit-testable, no NATS.
2. Wire into `workflow/scheduler.go` via `handleEvent` or a new sweep.
3. If a new envelope field: add to `task.Task` (and `client.EnqueueOptions` for producer-side plumbing).
4. Integration test in `workflow/integration_test.go` via the existing harness.

### Adding a new StateStore impl
1. Implement the full `StateStore` interface.
2. Write a test function `func TestXxxStore_Contract(t *testing.T) { runStoreContract(t, newXxxStore) }` — the existing contract in `workflow/store_test.go::runStoreContract` covers all required behaviors (CAS, list, watch).

## Changes that require careful review

- **Adding a field to `task.Task`** — wire format change; existing in-flight tasks decode with the new field zero-valued. OK for additive changes.
- **Changing `CanonicalName` derivation** — breaks all in-flight tasks. Don't.
- **Stream subject changes** — must be coordinated with consumer rebuilds.
- **KV key schema changes** — require a migration (stored records won't decode).

## Anti-patterns to avoid

- **Adding a mutex around the registry during dispatch.** Dispatch is read-only; `Registry.Get` is already `RWMutex`-protected.
- **Persisting `task.Task.Attempt`.** Don't — delivery count is authoritative from `msg.Metadata().NumDelivered`.
- **Calling `client.Enqueue` from inside a handler to enqueue the next step.** Use `workflow.FromContext(ctx).Step(...)` — it writes to KV, not directly to TASKS. The scheduler picks it up.
- **Bypassing `EnsureStreams`.** Every ebind deployment must call it once before starting workers/producers.

## When in doubt

1. Run `make test-count` — if something's racy, it'll surface.
2. Read the plan: `/home/eugene/.claude/plans/is-it-possible-in-kind-bird.md` — sections v1, v2, v2.1 explain the design decisions, not just what was built.
3. The `cmd/demo/main.go` is the minimum-viable integration of every component.
