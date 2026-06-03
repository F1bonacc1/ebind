# Architecture

**Analysis Date:** 2026-06-03

## Pattern Overview

**Overall:** Modular library providing three integrated capabilities — a task queue, a DAG workflow engine, and embedded NATS — all backed by a single NATS JetStream dependency.

**Key Characteristics:**
- **Library, not a service** — applications embed ebind in-process; no separate server deployment required.
- **Single external dependency** — NATS JetStream handles persistence, queueing, deduplication, and clustering. No Redis, Postgres, or other infrastructure.
- **Reflection-based function dispatch** — Go functions are registered by canonical name (derived via `runtime.FuncForPC`) and invoked at delivery via `reflect.Value.Call`. This enables a generic `Enqueue(c, MyFunc, a, b, c)` signature impossible with Go generics alone.
- **CAS-driven state management** — All DAG step and metadata mutations use compare-and-swap (via NATS KV revisions). This allows the scheduler to run in every worker without an explicit leader for correctness — KV CAS + JetStream queue semantics + msg-id dedupe give at-most-once effects.
- **Event-driven scheduler** — DAG step completion events flow through a dedicated JetStream stream (`EBIND_DAG_EVENTS`), consumed by a scheduler loop in every worker. The scheduler gates work via a pluggable `LeaderElector`.

## Layers

**Pub/Sub & Stream Infrastructure (`stream/`):**
- Purpose: Defines JetStream stream names, subject conventions, and idempotent stream provisioning.
- Location: `stream/setup.go`
- Contains: `EnsureStreams()` which creates/updates `EBIND_TASKS` (WorkQueuePolicy), `EBIND_RESP` (LimitsPolicy), `EBIND_DLQ` (LimitsPolicy), and subject helper functions (`TaskPublishSubject`, `TargetedTaskSubject`, `TargetToken`).
- Depends on: NATS `jetstream.JetStream` interface
- Used by: `client/`, `worker/`, `workflow/`, `dlq/`

**Task Model & Registry (`task/`):**
- Purpose: Core domain types (`Task`, `Response`, `TaskError`, `RetryPolicy`) and the reflection-based function registry (`Registry`, `Dispatcher`).
- Location: `task/task.go`, `task/registry.go`, `task/retry.go`
- Contains: `Register()` / `MustRegister()` / `Describe()` for handler registration, `Dispatcher.Call()` for reflective dispatch, `CanonicalName()` for deriving stable handler names.
- Depends on: Go `reflect`, `runtime` standard library
- Used by: `client/`, `worker/`, `workflow/`

**Client (Producer) (`client/`):**
- Purpose: Enqueue tasks onto the TASKS stream, await typed responses via `Future`.
- Location: `client/client.go`, `client/future.go`
- Contains: `Client` struct with `Enqueue()` / `EnqueueOpts()` / `EnqueueAsync()`, `Future` with `Get()` / `GetRaw()` / `Await[T]()`.
- Depends on: `task/` (for `Describe`, validation), `stream/` (for subject routing)
- Used by: Application code that produces tasks

**Worker (Consumer) (`worker/`):**
- Purpose: Pull tasks from the TASKS stream, dispatch through a middleware chain to registered handlers, publish responses and DLQ entries.
- Location: `worker/worker.go`, `worker/middleware.go`, `worker/hook.go`, `worker/claims.go`
- Contains: `Worker` with `Run()` (blocking event loop), `Handler` type, `Middleware` chain builder, `Recover()` / `Log()` / `WithMetrics()` middlewares, `StepHook` interface for workflow integration, `ClaimProvider` for targeted delivery.
- Depends on: `task/`, `stream/`, `dlq/`
- Used by: Application code that consumes tasks; `workflow` package via `StepHook`

**Workflow Engine (`workflow/`):**
- Purpose: Define, submit, and schedule DAG workflows with step dependencies, retries, dynamic step addition, cancellation, and Await.
- Location: `workflow/` (28 files)
- Contains: `DAG` builder (`dag.go`), `Workflow` coordinator (`workflow.go`), `Scheduler` (`scheduler.go`), pure DAG state machine (`state.go`), `Ref` argument resolution (`ref.go`), `Step` / `StepOption` (`step.go`), `StateStore` interface + `NatsStore`/`MemStore` impls (`store.go`, `store_nats.go`, `store_mem.go`), `EventBus` interface + `NatsBus`/`MemBus` impls (`events.go`, `events_nats.go`, `events_mem.go`), `Enqueuer` interface + `NatsEnqueuer` (`enqueuer.go`, `enqueuer_nats.go`), `ContextDAG` for dynamic steps (`context.go`), `Await`/`AwaitByID` (`await.go`), `Cancel` (`cancel.go`), `DeleteDAG` (`delete.go`), `Debug`/`DebugPrint` (`debug.go`), `PlacementSpec` (`placement.go`).
- Depends on: `task/`, `worker/` (via `StepHook` interface), `stream/` (via `NatsEnqueuer`)
- Used by: Application code building workflows; `cmd/ebctl` for inspection

**Dead-Letter Queue (`dlq/`):**
- Purpose: Publish and re-queue poison tasks that exhausted retries or failed non-retryably.
- Location: `dlq/dlq.go`, `dlq/requeue.go`
- Contains: `Entry` type, `Publish()`, `Fetch()`, `Requeue()`
- Depends on: `task/`, `stream/`
- Used by: `worker/` on terminal failure; `cmd/ebctl` for operator inspection

**Embedded NATS (`embed/`):**
- Purpose: Start and supervise NATS JetStream servers in-process — single-node or 3-node HA cluster.
- Location: `embed/node.go`, `embed/cluster.go`
- Contains: `StartNode()` (single-node), `StartCluster()` (3-node HA with loopback routes), `Node` / `Cluster` types.
- Depends on: `github.com/nats-io/nats-server/v2`
- Used by: Application code wanting zero-infrastructure deployment; tests via `internal/testutil`

## Data Flow

**Task Queue Path (ad-hoc tasks):**

1. `client.Enqueue(c, MyFunc, a, b, c)` — producer calls `task.Describe(MyFunc)` to validate the signature and derive the canonical name, JSON-marshals args, constructs a `Task` envelope with a `ReplyTo` subject (`RESP.<client_id>.<task_id>`), and publishes to `EBIND_TASKS` via `js.Publish` with `Nats-Msg-Id` dedupe.
2. The TASKS stream (WorkQueuePolicy) holds the message until a worker consumes it.
3. `worker.Worker.Run()` — the worker's consumer receives the message; the semaphore gates concurrency; a goroutine runs `w.handle()`.
4. `handle()` — unmarshals the `Task`, checks deadline, retrieves the `Dispatcher` from the `Registry` by `t.Name`, runs the middleware chain (Recover → user middleware → `baseHandler`), which calls `Dispatcher.Call(ctx, payload)`.
5. `Dispatcher.Call()` — unmarshals the payload JSON array into the registered arg types using `reflect.New` + `json.Unmarshal`, calls `reflect.Value.Call`, JSON-marshals the result.
6. On success: publishes a `Response` to `EBIND_RESP` at the `ReplyTo` subject, ACKs the message.
7. On failure: checks `ShouldRetry` — if retryable, `NakWithDelay(delay)`; if terminal, publishes `Response` + DLQ entry, calls `StepHook.OnStepFailed` (if present), `Term()` the message.
8. `client.Client` — has an ephemeral consumer on `EBIND_RESP` filtering `RESP.<client_id>.>`. On receiving the response, it routes it to the waiting `Future.ch` channel via `sync.Map` by task ID.
9. `Future.Get(ctx, &out)` — blocks on the channel, unmarshals the result.

**DAG Workflow Path:**

1. `workflow.New(opts...)` — constructs a `DAG` builder with steps and their dependencies (via `Step().Ref()` / `After()` / `AfterAny()`).
2. `dag.Submit(ctx, wf)` — validates cycles, persists `DAGMeta` and `StepRecord`s to the KV store (`ebind-dags` bucket), transitions root steps to `StatusRunning`, and calls `enqueueStep()` to publish task envelopes to `EBIND_TASKS`.
3. Worker receives the task, dispatches the handler. On completion:
   - `workflow.StepHook.OnStepDone/OnStepFailed` — CAS-updates the step record in KV, writes result bytes, publishes a completion event to `EBIND_DAG_EVENTS` stream.
4. Scheduler (`workflow.Scheduler.Run()`) — subscribes to `EBIND_DAG_EVENTS` via the durable consumer `ebind-scheduler`. On each event:
   - Loads the full DAG state from KV into a `DAGState`.
   - Applies the transition (`MarkDone` / `MarkFailed` / `MarkSkipped`) — pure in-memory operations.
   - Calls `ReadyToRun()` to find newly-ready steps.
   - For each ready step: resolves `Ref` arguments against upstream results (via `ResolveArgs`), persists status as `StatusRunning`, and enqueues the task via `NatsEnqueuer`.
   - Calls `maybeFinalize()` — if all steps are terminal, updates DAG meta status.
5. On leader acquisition (false→true edge of `IsLeader()`), the scheduler runs a full sweep: lists all running DAGs, loads each state, re-enqueues any ready-but-stranded steps (edge-triggered, not level-triggered).

```
producer ─► TASKS stream ─► worker pool ─► handler fn ─► result
                                    │
                                    ├─► RESP.<client_id>.<task_id> ─► Future.Get/Await
                                    │
                                    └─(if DAG)─► StepHook ─► KV {step=Done+result | step=Failed+error_kind/msg}
                                                         │
                                                         └─► DAG events stream ─► Scheduler
                                                                                      │
                                                                                      └─► enqueue next step
```

## Design Patterns

### Reflection-Based Function Registry
File: `task/registry.go`

Functions are not serialized. A `Registry` maps canonical names (derived from `runtime.FuncForPC(fn).Name()` last segment, e.g. `handlers.Foo`) to `reflect.Value`s holding the registered function. At dispatch, `reflect.Value.Call` invokes with JSON-decoded arguments.

**Critical invariants:**
- Canonical name = last segment of `runtime.FuncForPC(fn).Name()` (e.g. `github.com/you/app/handlers.Foo` → `handlers.Foo`). Renaming or moving a function breaks in-flight tasks. Use `task.WithName("...")` or `task.Alias("...")` to override.
- Handler signature must be `func(context.Context, args...) (T, error)` or `func(context.Context, args...) error`. Validation happens at `Register` and at `Enqueue` (client-side, before publish).
- `reflect.Value.Call` costs ~100–500 ns — negligible vs. real handler work (I/O, disk).

### CAS-Driven State (Workflow)
File: `workflow/store.go`, `workflow/store_nats.go`

All DAG state mutations use CAS (`KV.Update(key, val, expectedRevision)`). Two writers racing on the same key: one wins, the other retries. This foundation lets the scheduler run in every worker without an explicit "leader" for correctness — KV CAS + JetStream queue semantics + msg-id dedupe give at-most-once effects.

- KV bucket `ebind-dags`: `<dag_id>/meta`, `<dag_id>/step/<step_id>`, `<dag_id>/result/<step_id>`
- `NatsStore` wraps `jetstream.KeyValue`; `MemStore` emulates revision-based CAS for tests.

### Interface Isolation for Testability
All three IO dependencies of the workflow engine are narrow interfaces:

- `StateStore` — `GetStep`, `PutStep`, `ListSteps`, `GetResult`, `PutResult`, `GetMeta`, `PutMeta`, `ListDAGs`, `DeleteMeta`, `DeleteStep`, `DeleteResult`, `WatchResult`
- `EventBus` — `Publish`, `Subscribe`
- `Enqueuer` — `Enqueue`
- `LeaderElector` — `IsLeader`

Each has a production NATS implementation and an in-memory fake for tests. This enables pure unit-testing of `scheduler.go` and `state.go` without NATS.

### Event-Driven Scheduler
File: `workflow/scheduler.go`

The scheduler is event-driven: completion events from `StepHook` flow through `EBIND_DAG_EVENTS` stream. Each scheduler instance subscribes via a shared durable consumer (cluster-wide at-most-once delivery). The `LeaderElector` gates event processing: non-leaders Nak events; leaders serialize event handling via `s.mu` to avoid CAS races.

- On leader acquisition (false→true edge of `IsLeader()`): a full sweep lists all running DAGs and re-enqueues stranded ready steps (edge-triggered, not level-triggered).
- Defaults: 5s sweep check interval, 60s sweep timeout. Overlap-guarded.

### Middleware Chain (Worker)
File: `worker/middleware.go`

`Handler` = `func(ctx context.Context, t *task.Task) ([]byte, error)`
`Middleware` = `func(Handler) Handler`

`Chain()` composes middlewares left-to-right (first in slice runs outermost). `Recover()` is always the innermost (registered first in the chain construction at `worker.New`). User middlewares wrap around it.

### Placement/Targeted Delivery
File: `worker/claims.go`, `workflow/placement.go`

Workers claim logical targets via `ClaimProvider`. Tasks with a non-empty `Target` field are published to a sub-filtered subject (`TASKS_TARGET.<base64-target>.<name>`). Workers create per-claim consumer durables (`ebind-targets-<token>`) so they only receive messages they own. This eliminates wrong-target NAK loops.

For DAGs, a `PlacementSpec` can be:
- `PlacementDirect` — explicit target claim
- `PlacementColocate` — run on the same concrete worker as another step
- `PlacementFollow` — follow the same logical target as another step
- `PlacementHere` — (dynamic steps only) run on the currently-executing worker

## How DAG Scheduler and Worker Interact

The worker and scheduler are decoupled through two narrow interfaces:

1. **`worker.StepHook`** — The worker calls `OnStepDone(ctx, t, result)` or `OnStepFailed(ctx, t, err)` exactly once per terminal outcome (not on intermediate retries). The `workflow.StepHook` implementation:
   - CAS-updates the step status in KV (`store.PutStep`)
   - Writes result bytes (`store.PutResult`)
   - Publishes a completion event to `EBIND_DAG_EVENTS`

2. **`EventBus`** — Carries completion events (`DAG.<dag_id>.completed.<step_id>`) and step-added events (`DAG.<dag_id>.step-added.<step_id>`) from the hook to the scheduler.

3. The **scheduler** listens on `EBIND_DAG_EVENTS` via a shared durable consumer. On receiving an event, it loads DAG state, transitions the state machine, enqueues newly-ready steps (via `Enqueuer`), and finalizes the DAG if all steps are terminal.

The `StepHook` is wired into `worker.Options.StepHook`. The `Enqueuer` publishes to the same `EBIND_TASKS` stream the worker consumes from — so DAG steps are just regular tasks with `DAGID`/`StepID` fields set.

## Concurrency Model

**Goroutines:**

| Component | Goroutines |
|-----------|-----------|
| `embed.StartNode` | 1 for NATS server (`go srv.Start()`) |
| `embed.StartCluster` | 1 per node (Phase 2: `go srv.Start()`) |
| `worker.Run` | 1 per dispatched task (goroutine spawned per message from `cons.Consume` callback), bounded by semaphore (`opts.Concurrency`, default 16) |
| `worker.runClaimRefresher` | 1 background goroutine for claim cache refresh |
| `client.Client` | 1 goroutine for the response consumer's dispatch loop (internal to NATS) |
| `workflow.Scheduler.Run` | 1 main event subscription goroutine + 1 for `watchLeadership` |
| `workflow.MemBus.Publish` | 1 per subscriber (fan-out via `go s.handler(ev)`) |
| `workflow.NatsStore.WatchResult` | 1 per watch for KV update forwarding |

**Channels:**

- `sem chan struct{}` — semaphore in `worker.Run` bounds concurrency (`opts.Concurrency`).
- `fut.ch chan *task.Response` — `Future` channel, buffered 1, for response delivery from NATS callback to `Future.Get()` caller.
- `s.wf.Bus` — event delivery channel (internal to NATS consumer in `NatsBus`, channel-based fan-out in `MemBus`).
- `done chan struct{}` — in `worker.Run` for shutdown drain coordination.
- `closed chan struct{}` — in `client.Client` for close notification.

**Mutexes:**

- `task.Registry.mu` — `sync.RWMutex` protects the handler map. `Get()` uses RLock; `Register()`/`MustRegister()` uses Lock.
- `workflow.Scheduler.mu` — `sync.Mutex` serializes intra-process event handling so CAS on step records doesn't race between concurrent event deliveries.
- `workflow.Scheduler.leaderMu` — `sync.Mutex` guards `wasLeader`/`sweepRunning` state.
- `worker.claimSubsMu` — `sync.Mutex` guards `activeClaimSubs` map.
- `client.Client.waiters` — `sync.Map` (lock-free) for waiter channel registration/lookup.
- `workflow.MemStore.mu` — `sync.Mutex` for in-memory state access.
- `workflow.MemBus.mu` — `sync.Mutex` for subscriber list access.

**Atomic operations:**
- `worker.stopping` — `atomic.Bool` for shutdown fast-path in consume callbacks.
- `worker.claimCache` — `atomic.Pointer[[]string]` for lock-free claim set reads.

**Key synchronization invariants:**
- Worker consume callbacks use a semaphore (`sem`) + `WaitGroup` pattern: callback acquires sem, does `wg.Add(1)`, spawns goroutine. Shutdown sequence: set `stopping=true`, stop consumers, drain `<-Closed()`, then `wg.Wait()` with timeout.
- Scheduler event handling is serialized within one process via `s.mu`. Cross-process serialization is the user's `LeaderElector` responsibility. The combination of KV CAS + JetStream queue semantics + msg-id dedupe provides correctness even without leadership.
- Claim subscriptions intentionally NOT stopped on claim loss: in-flight handlers for a dropped claim could get redelivered before Ack, breaking at-least-once semantics. Stray deliveries are NAKed via `ownsTarget` check.

---

*Architecture analysis: 2026-06-03*
