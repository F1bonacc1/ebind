# Codebase Structure

**Analysis Date:** 2026-06-03

## Directory Layout

```
ebind/
├── task/                       # Core domain types + reflection-based function registry
│   ├── task.go                 # Task, Response, TaskError types + error kind constants
│   ├── registry.go             # Registry, Dispatcher, Register/MustRegister/Describe/CanonicalName
│   ├── registry_test.go        # Tests for registration, dispatch, arg validation
│   ├── retry.go                # RetryPolicy (exponential backoff, attempt caps, non-retryable kinds)
│   └── retry_test.go           # ~14 boundary tests for NextDelay + ShouldRetry
│
├── client/                     # Producer-side: enqueue tasks and await responses
│   ├── client.go               # Client struct, New, Enqueue, EnqueueOpts, EnqueueAsync
│   └── future.go               # Future type with Get, GetRaw, Await[T] generic helper
│
├── worker/                     # Consumer-side: dispatch tasks via registered handlers
│   ├── worker.go               # Worker struct, Run, handle(), retry/deadline logic, publishResponse
│   ├── worker_test.go          # Integration tests with embedded NATS
│   ├── middleware.go           # Handler, Middleware types, Chain, Recover, Log, WithMetrics
│   ├── hook.go                 # StepHook interface (OnStepDone, OnStepFailed — workflow seam)
│   └── claims.go               # ClaimProvider, ClaimsFunc, StaticClaims, ConcreteTarget
│
├── stream/                     # JetStream stream provisioning + subject helpers
│   └── setup.go                # EnsureStreams (EBIND_TASKS, EBIND_RESP, EBIND_DLQ), Config, subject helpers
│
├── dlq/                        # Dead-letter queue — publish, fetch, requeue poison tasks
│   ├── dlq.go                  # Entry type, Publish()
│   ├── requeue.go              # Fetch(), Requeue() — republish + delete DLQ entry
│   ├── requeue_test.go         # Tests for requeue flow
│
├── workflow/                   # DAG workflow engine — builder, scheduler, state, store, events
│   ├── workflow.go             # Workflow struct (bundles Store, Bus, Enq, Elector), NewWorkflow, RunScheduler
│   ├── nats.go                 # NewFromNATS — convenience constructor for NATS-backed Workflow
│   ├── dag.go                  # DAG builder — New, Step, StepOpts, Submit, cycle detection, enqueueStep
│   ├── dag_test.go             # Tests for DAG construction, cycle detection, Submit
│   ├── step.go                 # Step struct, Ref, RefOrDefault, StepOption (Optional, After, AfterAny)
│   ├── state.go                # DAGState, DAGMeta, StepRecord, MarkDone/MarkFailed/MarkSkipped, ReadyToRun, Terminal
│   ├── state_test.go           # Pure unit tests for state transitions
│   ├── ref.go                  # Ref type (required/or_default), ResolveArgs (pure), StepStatus constants
│   ├── ref_test.go             # Tests for Ref resolution logic
│   ├── errors.go               # Sentinel errors: ErrStepFailed, ErrStaleRevision, ErrCycle, etc.
│   ├── store.go                # StateStore interface definition
│   ├── store_mem.go            # MemStore — in-memory CAS implementation for tests
│   ├── store_nats.go           # NatsStore — JetStream KV-backed implementation
│   ├── store_test.go           # Contract tests for StateStore (runs against MemStore and NatsStore)
│   ├── events.go               # Event, EventKind, EventBus interface, EventSubject, MarshalEvent
│   ├── events_mem.go           # MemBus — in-memory fan-out EventBus for tests
│   ├── events_nats.go          # NatsBus — JetStream-backed EventBus (EBIND_DAG_EVENTS stream)
│   ├── enqueuer.go             # Enqueuer interface, LeaderElector interface, alwaysLeader
│   ├── enqueuer_nats.go        # NatsEnqueuer — publishes Task envelopes to TASKS stream
│   ├── scheduler.go            # Scheduler — Run, onEvent, handleEvent, onCompleted, onStepAdded, sweep, enqueueReady
│   ├── scheduler_test.go       # Tests with fakes (MemStore + MemBus + capture enqueuer)
│   ├── hook.go                 # StepHook — implements worker.StepHook, persists outcome to store + publishes event
│   ├── hook_test.go            # Tests for hook behavior
│   ├── context.go              # ContextDAG, FromContext, ContextMiddleware — dynamic step addition from handlers
│   ├── placement.go            # PlacementSpec, OnTarget, ColocateWith, FollowTargetOf, ColocateHere, resolvePlacementTarget
│   ├── await.go                # Await[T], AwaitByID[T], DAGInfo — wait for step results across processes
│   ├── await_test.go           # Tests for Await functionality
│   ├── cancel.go               # Cancel — mark DAG as canceled, transition pending steps
│   ├── delete.go               # DeleteDAG — remove all KV records for a DAG
│   ├── delete_test.go          # Tests for DAG deletion
│   ├── debug.go                # DAGDebug, DebugPrint, StepDebug, StepBlocker — introspection
│   ├── debug_test.go           # Tests for debug output
│   ├── integration_test.go     # Full integration tests with embedded NATS
│   └── errmsg_integration_test.go # Error message truncation integration tests
│
├── embed/                      # In-process NATS JetStream servers
│   ├── node.go                 # NodeConfig, StartNode (single-node embedded NATS)
│   ├── cluster.go              # ClusterConfig, StartCluster (3-node HA, loopback), freePorts
│   └── cluster_test.go         # Cluster integration tests (failover, etc.)
│
├── internal/
│   └── testutil/
│       └── harness.go          # Harness.SingleNode — test helper: embedded NATS + streams + worker + client
│
├── cmd/
│   ├── demo/
│   │   └── main.go             # Single-process end-to-end demo (embedded NATS, register, enqueue, await)
│   └── ebctl/
│       ├── main.go             # Operator CLI entry point (cobra-based)
│       ├── ebctl_integration_test.go # CLI integration tests
│       └── internal/
│           ├── cli/
│           │   └── cli.go      # Shared Context (NATS conn, Workflow, Printer), NewRootCommand, flags
│           ├── format/
│           │   └── format.go   # Printer interface, PrettyPrinter, JSONPrinter, HumanDuration helpers
│           └── commands/
│               ├── dag/
│               │   ├── dag.go      # `ebctl dag` root command (ls, get, tree, cancel, rm, watch, step)
│               │   ├── ls.go       # `ebctl dag ls` — list all DAGs
│               │   ├── get.go      # `ebctl dag get <id>` — show DAG info
│               │   ├── tree.go     # `ebctl dag tree <id>` — hierarchical step tree
│               │   ├── cancel.go   # `ebctl dag cancel <id>` — cancel a DAG
│               │   ├── rm.go       # `ebctl dag rm <id>` — delete DAG records
│               │   ├── watch.go    # `ebctl dag watch <id>` — live DAG event stream
│               │   ├── step.go     # `ebctl dag step get|result <dag-id> <step-id>`
│               │   └── step_test.go # Tests for step commands
│               ├── dlq/
│               │   ├── dlq.go      # `ebctl dlq` root command (ls, show, watch, requeue, purge)
│               │   ├── ls.go       # `ebctl dlq ls` — list DLQ entries
│               │   ├── show.go     # `ebctl dlq show <seq>` — inspect a DLQ entry
│               │   ├── watch.go    # `ebctl dlq watch` — live DLQ stream
│               │   ├── requeue.go  # `ebctl dlq requeue <seq>` — republish task, delete DLQ entry
│               │   └── purge.go    # `ebctl dlq purge` — clear all DLQ entries
│               ├── stream/
│               │   ├── stream.go   # `ebctl stream` and `ebctl consumer` root commands
│               │   ├── ls.go       # `ebctl stream ls` — list ebind streams
│               │   ├── info.go     # `ebctl stream info <name>` — stream details
│               │   ├── peek.go     # `ebctl stream peek <name>` — inspect messages
│               │   ├── purge.go    # `ebctl stream purge <name>` — clear stream
│               │   ├── rmmsg.go    # `ebctl stream rmmsg <name> <seq>` — delete message
│               │   ├── consumer.go # `ebctl consumer ls|info` — consumer operations
│               │   └── version/
│               │       └── version.go # `ebctl version` — show version
│
├── examples/                    # 13 standalone examples demonstrating library features
│   ├── 01-basic/main.go         # Basic enqueue + await
│   ├── 02-retry-policy/main.go  # Retry policy configuration
│   ├── 03-fire-and-forget/main.go
│   ├── 04-cluster-ha/main.go    # 3-node HA cluster
│   ├── 05-middleware/main.go    # Custom middleware
│   ├── 06-workflow-linear/main.go
│   ├── 07-workflow-fanout/main.go
│   ├── 08-workflow-optional/main.go
│   ├── 09-workflow-dynamic/main.go
│   ├── 10-workflow-temporal-deps/main.go
│   ├── 11-workflow-resume/main.go
│   ├── 12-workflow-placement/main.go
│   ├── 13-workflow-cancel/main.go
│   └── README.md
│
├── .golangci.yaml               # Linter config (golangci-lint v2: bodyclose, errcheck, staticcheck, etc.)
├── go.mod                       # Module: github.com/f1bonacc1/ebind, Go 1.26.1
├── go.sum                       # Dependency checksums
├── Makefile                     # Build/test/lint/demo targets
├── CLAUDE.md                    # AI agent guide (design decisions, invariants, package layout)
└── README.md                    # Project overview
```

## Directory Purposes

**`task/` — Core Domain Types & Registry:**
- Purpose: Foundation types (`Task`, `Response`, `TaskError`, `RetryPolicy`) used by every other package. The reflection-based function registry lives here.
- Contains: 5 files (3 source, 2 test)
- Key files: `task.go`, `registry.go`, `retry.go`

**`client/` — Producer/Enqueue API:**
- Purpose: Public API for enqueueing tasks and awaiting typed results. The `Client` manages response consumers per NATS connection.
- Contains: 2 files (2 source, 0 test)
- Key files: `client.go` (exported functions: `New`, `Enqueue`, `EnqueueOpts`, `EnqueueAsync`), `future.go` (exported: `Future`, `Await[T]`)

**`worker/` — Consumer/Worker:**
- Purpose: Task consumption, dispatch via middleware chain, response/DLQ publishing, targeted delivery via claims.
- Contains: 5 files (4 source, 1 test)
- Key files: `worker.go` (exported: `Worker`, `Options`, `New`, `Run`, `Use`), `middleware.go` (exported: `Handler`, `Middleware`, `Chain`, `Recover`, `Log`, `WithMetrics`), `hook.go` (exported: `StepHook`), `claims.go` (exported: `ClaimProvider`, `ClaimsFunc`, `StaticClaims`, `ConcreteTarget`)

**`workflow/` — DAG Workflow Engine:**
- Purpose: Define DAGs with steps and dependencies, submit to a store-backed coordinator, schedule step execution via event-driven loop.
- Contains: 28 files (22 source, 6 test)
- Key files: `workflow.go` (exported: `Workflow`, `NewWorkflow`), `dag.go` (exported: `DAG`, `New`, `Submit`), `scheduler.go` (exported: `Scheduler`, `Run`), `state.go` (exported: `DAGState`, `DAGMeta`, `StepRecord`, `MarkDone`, `MarkFailed`, `ReadyToRun`), `store.go`/`store_nats.go`/`store_mem.go` (exported: `StateStore`, `NatsStore`, `MemStore`), `events.go`/`events_nats.go`/`events_mem.go` (exported: `EventBus`, `NatsBus`, `MemBus`)

**`stream/` — Stream Infrastructure:**
- Purpose: JetStream stream provisioning and subject routing conventions.
- Contains: 1 file
- Key files: `setup.go` (exported: `Config`, `EnsureStreams`, `TaskPublishSubject`, `TargetToken`)

**`dlq/` — Dead-Letter Queue:**
- Purpose: Publish, fetch, and requeue poison tasks.
- Contains: 3 files (2 source, 1 test)
- Key files: `dlq.go` (exported: `Entry`, `Publish`), `requeue.go` (exported: `Fetch`, `Requeue`)

**`embed/` — Embedded NATS:**
- Purpose: In-process NATS JetStream server management.
- Contains: 3 files (2 source, 1 test)
- Key files: `node.go` (exported: `NodeConfig`, `Node`, `StartNode`), `cluster.go` (exported: `ClusterConfig`, `Cluster`, `StartCluster`)

## Key File Locations

**Entry Points:**
- `cmd/demo/main.go`: Single-process end-to-end demo. Starts embedded NATS, registers handlers, enqueues tasks, awaits responses.
- `cmd/ebctl/main.go`: Operator CLI. Cobra-based subcommands for inspecting DAGs, streams, consumers, and DLQ.

**Configuration:**
- `.golangci.yaml`: Linter rules (golangci-lint v2 format, 18 linters enabled)
- `go.mod`: Go module definition, Go 1.26.1
- `Makefile`: Build, test, lint, and demo targets

**Public Library API Surface (exported entry points for application code):**

| Package | Exported Name | Description |
|---------|--------------|-------------|
| `task` | `Registry` | Handler registry with `Register`, `MustRegister`, `Get`, `Names` |
| `task` | `Describe` | Introspect a function signature (no registration required) |
| `task` | `CanonicalName` | Derive stable name from `runtime.FuncForPC` |
| `task` | `Dispatcher` | Reflective call dispatch: `Call`, `ValidateArgs`, `ArgTypes`, `HasResult` |
| `task` | `Task` | Task envelope (ID, Name, Payload, ReplyTo, Deadline, RetryPolicy, DAGID, etc.) |
| `task` | `Response` | Task result envelope (Result, Error, Attempts) |
| `task` | `TaskError` | Typed error with Kind, Message, Retryable |
| `task` | `RetryPolicy` | Backoff/attempt policy with `NextDelay`, `ShouldRetry` |
| `client` | `Client` | Producer connection with `Enqueue`, `EnqueueOpts`, `EnqueueAsync`, `Close` |
| `client` | `Future` | Async result handle with `Get`, `GetRaw` |
| `client` | `Await[T]` | Generic typed future resolution |
| `worker` | `Worker` | Consumer with `New`, `Run`, `Use` |
| `worker` | `Handler` | `func(ctx, *Task) ([]byte, error)` |
| `worker` | `Middleware` | `func(Handler) Handler` |
| `worker` | `Chain` | Compose middleware stack |
| `worker` | `Recover`, `Log`, `WithMetrics` | Built-in middleware |
| `worker` | `StepHook` | Interface for workflow integration |
| `worker` | `ClaimProvider`, `StaticClaims`, `ConcreteTarget` | Targeted delivery |
| `stream` | `EnsureStreams` | Idempotent stream provisioning |
| `stream` | `TaskPublishSubject`, `TargetToken` | Subject routing helpers |
| `dlq` | `Publish`, `Fetch`, `Requeue` | DLQ management |
| `embed` | `StartNode` | Single-node embedded NATS |
| `embed` | `StartCluster` | 3-node HA cluster |
| `workflow` | `DAG`, `New`, `Step`, `Submit` | DAG builder |
| `workflow` | `Workflow`, `NewWorkflow`, `NewFromNATS`, `RunScheduler` | Workflow coordinator |
| `workflow` | `Ref`, `Step.Ref()`, `Step.RefOrDefault()` | Step result references |
| `workflow` | `Optional`, `After`, `AfterAny`, `WithStepRetry` | Step options |
| `workflow` | `OnTarget`, `ColocateWith`, `FollowTargetOf`, `ColocateHere` | Placement directives |
| `workflow` | `Cancel`, `DeleteDAG` | DAG lifecycle |
| `workflow` | `Await[T]`, `AwaitByID[T]`, `DAGInfo` | Cross-process result waiting |
| `workflow` | `Debug`, `DebugPrint` | DAG state introspection |
| `workflow` | `FromContext`, `ContextDAG.Step` | Dynamic step addition |
| `workflow` | `ContextMiddleware` | Middleware to inject `ContextDAG` |
| `workflow` | `NatsStore`, `MemStore` | State store implementations |
| `workflow` | `NatsBus`, `MemBus` | Event bus implementations |

## Naming Conventions

**Files:**
- `snake_case.go` for all Go source files (`worker.go`, `store_nats.go`, `events_mem.go`)
- Test files: `*_test.go` co-located with source (`worker_test.go`, `state_test.go`)
- Example directories: numbered prefix + kebab-case (`01-basic/`, `06-workflow-linear/`)

**Directories:**
- All lowercase, single word where possible (`task/`, `client/`, `worker/`, `stream/`, `dlq/`, `embed/`, `workflow/`)

**Packages:**
- Short lowercase names matching directory name

## JetStream Streams & KV Structure

**Streams (provisioned by `stream.EnsureStreams`):**

| Stream | Type | Subjects | Purpose | MaxAge | Replicas |
|--------|------|----------|---------|--------|----------|
| `EBIND_TASKS` | WorkQueue | `TASKS.>`, `TASKS_TARGET.>` | Task envelopes consumed by workers | 24h | Configurable |
| `EBIND_RESP` | Limits | `RESP.>` | Per-task responses for `Future` resolution | 1h | Configurable |
| `EBIND_DLQ` | Limits | `DLQ.>` | Dead-lettered (failed terminal) tasks | 7d | Configurable |

**Additional stream (created by `workflow.NewNatsBus`):**

| Stream | Type | Subjects | Purpose | MaxAge | Replicas |
|--------|------|----------|---------|--------|----------|
| `EBIND_DAG_EVENTS` | WorkQueue | `DAG.>` | Scheduler completion/step-added events | 24h | Configurable |

**KV Bucket (created by `workflow.NewNatsStore`):**

| Bucket | Purpose |
|--------|---------|
| `ebind-dags` | DAG metadata + step records + step results (CAS via revisions) |

**KV Key Schema:**
- `<dag_id>/meta` → `DAGMeta` (status, default policy, created_at)
- `<dag_id>/step/<step_id>` → `StepRecord` (fn name, args, deps, status, retry policy, error info)
- `<dag_id>/result/<step_id>` → raw result bytes

## Where to Add New Code

**New Feature (library addition):**
- If it extends the task model: `task/` (e.g., new envelope fields in `Task`)
- If it's a new production-side capability: add to existing file or create new file in `client/`
- If it's a new consumer-side capability: `worker/` (new middleware, new hook interface)
- If it's a new workflow feature: `workflow/` — start with pure logic in `state.go` or `ref.go`, wire into `scheduler.go`, integration test in `integration_test.go`
- If it's a new stream/infrastructure: `stream/`
- Tests: co-located with source (`*_test.go`)

**New Middleware:**
- Implement `worker.Middleware` signature: `func(next worker.Handler) worker.Handler`
- Add to `worker/middleware.go` or a new file in `worker/`
- Wire via `worker.Options.Middleware` or `w.Use(...)`
- Test with `internal/testutil.SingleNode` harness

**New Protobuf or Wire Format Change:**
- Adding a field to `task.Task`: additive change (zero-valued for in-flight tasks)
- Changing `CanonicalName` derivation: breaks all in-flight tasks — do not do
- Stream subject changes: must coordinate with consumer rebuilds

**New Command (ebctl):**
- Create file/folder in `cmd/ebctl/internal/commands/<name>/`
- Register in `cmd/ebctl/main.go` via `root.AddCommand(...)`

**Tests:**
- Pure unit: `workflow/state_test.go`, `workflow/ref_test.go`, `task/retry_test.go`
- Fakes-based: `workflow/scheduler_test.go`, `workflow/store_test.go`
- Integration (embedded NATS): `worker/worker_test.go`, `workflow/integration_test.go`
- Cluster integration: `embed/cluster_test.go`

---

*Structure analysis: 2026-06-03*
