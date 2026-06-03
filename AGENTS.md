<!-- GSD:project-start source:PROJECT.md -->
## Project

**ebind**

ebind is a Go library that provides a task queue and DAG workflow engine over NATS JetStream. It enables durable, multi-step workflows with step dependencies, retries, and dynamic step addition — with no external dependencies beyond NATS itself. Applications embed ebind in-process for production-grade orchestration without separate infrastructure.

**Core Value:** Durable, reliable workflow execution on a single dependency. A paused DAG should stay paused across process restarts, with no CPU consumed, and resume exactly where it left off.

### Constraints

- **Compatibility**: New DAG statuses must follow existing lowercase naming convention (`pausing`, `paused`)
- **Compatibility**: Pause must use KV CAS for state transitions, consistent with existing Cancel() pattern
- **Compatibility**: PAUSED state must be backward-compatible with existing stores — old DAGs without pause support continue working
- **Behavior**: Paused DAGs must consume no scheduler CPU resources beyond the status check in the event loop
<!-- GSD:project-end -->

<!-- GSD:stack-start source:codebase/STACK.md -->
## Technology Stack

## Language
- Go 1.26.1 (`go 1.26.1` in `go.mod`)
- Toolchain: `go1.26.3`
- Module path: `github.com/f1bonacc1/ebind`
- All source files across 12 packages and 2 commands
## Runtime
- Compiled binary — no external runtime dependencies (single static binary)
- No interpreter or VM required
- Go modules (`go.mod` + `go.sum`)
- Lockfile: `go.sum` present (3223 bytes)
## Build & Dev Tooling
- Standard `go build` (multi-package via `./...`)
- Makefile at `/home/pawel/repo/ebind/Makefile` — targets: `build`, `test`, `test-short`, `test-count`, `cover`, `lint`, `lint-fix`, `fmt`, `vet`, `tidy`, `demo`, `ebctl`, `ci`, `clean`
- `go build -o bin/ebctl ./cmd/ebctl` for the operator CLI
- GitHub Actions in `.github/workflows/ci.yml`:
- Release pipeline: `.github/workflows/release.yml`
- CodeQL analysis: `.github/workflows/codeql.yml`
- Dependabot: `.github/dependabot.yml`
- `golangci-lint` v2 config at `.golangci.yaml`
- Enabled linters: `bodyclose`, `copyloopvar`, `errcheck`, `errorlint`, `gocritic`, `govet`, `ineffassign`, `misspell`, `nilerr`, `noctx`, `revive`, `staticcheck`, `unconvert`, `unparam`, `unused`
- Formatters: `gofmt`, `goimports`
- `make fmt` runs `go fmt` + `goimports -w`
## Dependencies
### Direct Dependencies
| Package | Version | Purpose | Used In |
|---------|---------|---------|---------|
| `github.com/nats-io/nats.go` | v1.52.0 | NATS Go client — all NATS communication | `client/`, `worker/`, `stream/`, `dlq/`, `workflow/`, `embed/`, `cmd/` |
| `github.com/nats-io/nats-server/v2` | v2.14.1 | Embedded NATS JetStream server | `embed/node.go`, `embed/cluster.go` |
| `github.com/spf13/cobra` | v1.10.2 | CLI framework | `cmd/ebctl/` |
| `github.com/google/uuid` | v1.6.0 | UUID generation for task/client/DAG IDs | `client/`, `workflow/`, `dlq/` |
### Indirect Dependencies
| Package | Version | Purpose |
|---------|---------|---------|
| `github.com/klauspost/compress` | v1.18.6 | NATS wire compression |
| `github.com/nats-io/nkeys` | v0.4.15 | NATS ed25519 keys |
| `github.com/nats-io/jwt/v2` | v2.8.1 | NATS JWT auth |
| `github.com/nats-io/nuid` | v1.0.1 | NATS unique ID generation |
| `github.com/minio/highwayhash` | v1.0.4 | NATS hash function |
| `github.com/google/go-tpm` | v0.9.8 | TPM support (NATS server) |
| `golang.org/x/crypto` | v0.51.0 | Cryptographic primitives |
| `golang.org/x/sys` | v0.44.0 | OS syscall interface |
| `golang.org/x/time` | v0.15.0 | Rate/time utilities |
| `github.com/inconshreveable/mousetrap` | v1.1.0 | Cobra Windows support |
| `github.com/spf13/pflag` | v1.0.9 | Cobra flag parsing |
| `github.com/antithesishq/antithesis-sdk-go` | v0.7.0-default-no-op | Fault injection testing SDK (indirect no-op) |
## Key Stdlib Packages Used
| Package | Where Used | Purpose |
|---------|-----------|---------|
| `reflect` | `task/registry.go` | Core — function signature inspection, dynamic dispatch via `reflect.Value.Call` |
| `runtime` | `task/registry.go` | `runtime.FuncForPC` to derive canonical handler names |
| `encoding/json` | All packages | Universal serialization: args, results, task envelopes, events, step records, DLQ entries |
| `sync` | `task/registry.go`, `worker/worker.go`, `workflow/scheduler.go`, `workflow/state.go`, `client/client.go`, `workflow/store_mem.go` | RWMutex (registry), Mutex (scheduler serialization), atomic (worker stopping), sync.Map (client waiters), sync.Mutex (MemStore, MemBus) |
| `context` | All packages | `context.Context` — first param of every handler, cancellation, deadlines |
| `sync/atomic` | `worker/worker.go` | `atomic.Bool` for worker stopping state; `atomic.Pointer` for claim cache |
| `log/slog` | `worker/middleware.go` | Structured logging middleware (`Log` middleware) |
| `testing` | `*_test.go` files | Standard Go testing framework |
| `net` | `embed/cluster.go` | TCP listener for free port allocation in clusters |
| `net/url` | `embed/cluster.go` | Route URL construction |
| `path/filepath` | `embed/cluster.go` | Cluster per-node store dir paths |
| `text/tabwriter` | `cmd/ebctl/internal/format/` | Pretty CLI table rendering |
| `unicode/utf8` | `workflow/hook.go` | UTF-8 rune boundary truncation for error messages |
| `slices` | `task/retry.go` | `slices.Contains` for non-retryable error kinds |
| `math` | `task/retry.go` | `math.Pow` for exponential backoff |
| `time` | All packages | Timestamps, delays, backoff, deadlines, tickers |
| `errors` | `task/registry.go`, `workflow/` | `errors.As` for `TaskError` unwrapping; sentinel errors |
| `io` | `cmd/ebctl/internal/format/` | Output writer abstraction |
| `os` | `embed/`, `cmd/ebctl/`, `cmd/demo/` | Temp dirs, file I/O, stdin stat |
## Configuration
- NATS connection URL via `EBIND_NATS_URL` env var (defaults to `nats://localhost:4222`) — `cmd/ebctl/internal/cli/cli.go`
- No other env vars required
- `go.mod` — Go version, module path, dependency versions
- `.golangci.yaml` — lint rules
- `worker.Options` — concurrency, backoff, middleware, claims, retry policy
- `stream.Config` — replicas, max ages, duplicate window
- `workflow.Workflow` — store, bus, enqueuer, elector, sweep timing
- `embed.NodeConfig` / `embed.ClusterConfig` — server name, host, port, store dir, ready wait
## Platform Requirements
- Go 1.26.1+
- Access to NATS (embedded or external)
- `golangci-lint` v2 for linting
- Single binary deployment
- Embedded NATS requires writable store directory (`StoreDir`)
- No external database or service dependencies beyond NATS itself
- 3-node cluster mode requires 3 loopback or routable addresses
<!-- GSD:stack-end -->

<!-- GSD:conventions-start source:CONVENTIONS.md -->
## Conventions

## Naming Patterns
- Use `snake_case.go` for package-file naming (e.g., `store_mem.go`, `events_nats.go`, `enqueuer_nats.go`)
- Test files: `*_test.go` (e.g., `retry_test.go`, `state_test.go`, `scheduler_test.go`)
- One file per major type/role within a package, except when complementary types share a file
- Exported functions use `PascalCase` (e.g., `Register`, `CanonicalName`, `NewRegistry`, `Chain`, `Recover`, `Enqueue`, `Await`)
- Unexported functions and methods use `camelCase` (e.g., `setDefaults`, `baseHandler`, `handleEvent`, `cascadeSkipFrom`, `snapshotUpstream`, `publishResponse`, `enqueueStep`)
- Factory functions named `New*` (e.g., `NewRegistry()`, `NewWorkflow(...)`, `New(...)` for DAG, `NewMemStore()`, `NewMemBus()`)
- Prefix `Default*` for default configuration constructors (e.g., `DefaultRetryPolicy()`, `NoRetryPolicy()`)
- Prefix `Must*` for panicking variants (e.g., `MustRegister`)
- Exported package-level constants use PascalCase (e.g., `ErrKindHandler`, `DAGStatusRunning`, `EventCompleted`)
- Unexported package-level vars use camelCase with no prefix (e.g., `ctxType`, `errType`)
- Sentinel errors use `Err*` prefixed PascalCase (e.g., `ErrStepFailed`, `ErrCycle`, `ErrStaleRevision`)
- Boolean fields omit `Is`/`Has` prefix unless disambiguation needed (e.g., `Optional` not `IsOptional`, `hasResult` not `resultExists`)
- Exported types use `PascalCase` (e.g., `Task`, `Response`, `TaskError`, `DAGState`, `StepRecord`, `DAGMeta`, `Ref`, `Event`, `Workflow`)
- Interface types use `*er` suffix or descriptive noun (e.g., `Middleware`, `StepHook`, `StateStore`, `EventBus`, `Enqueuer`, `ClaimProvider`, `LeaderElector`, `Subscription`)
- Type aliases for string enums: `type DAGStatus string`, `type EventKind string`, `type RefMode string`, `type StepStatus string`
- Option/configuration structs: `Options` or `Config` (e.g., `worker.Options`, `stream.Config`, `client.Options`, `embed.NodeConfig`, `embed.ClusterConfig`)
## Code Style
- `gofmt` enforced via golangci-lint formatter section (`.golangci.yaml` uses `gofmt` formatter)
- `goimports` enforced for import ordering — `go tool goimports -w .` in `make fmt`
- No `fieldalignment` or `shadow` checks (explicitly disabled in `govet`)
- `golangci-lint` configured in `.golangci.yaml` with `version: "2"` format
- Enabled linters:
- Revive rules: `var-naming`, `error-return`, `error-strings`, `error-naming`, `if-return`, `range`, `receiver-naming`, `indent-error-flow`, `superfluous-else`
- Linting exclusions for test files: `bodyclose`, `errcheck`, `unparam`
- Linting exclusions for `cmd/demo/`: `errcheck`
- Example code excluded from linting (path: `^examples/`)
- Preset exclusions: `comments`, `common-false-positives`, `legacy`, `std-error-handling`
## Import Organization
- None used. All imports use the module path `github.com/f1bonacc1/ebind/...`
- External packages: `nats.go`, `jetstream` sub-package, `uuid`, `cobra`
## Error Handling
- Sentinel errors with `errors.New()` in `workflow/errors.go`:
- Wrapped errors with `fmt.Errorf("...: %w", err)` consistently:
- Custom error type `TaskError` with structured fields (`Kind`, `Message`, `Retryable`):
- `TaskError` implements the `error` interface: `func (e *TaskError) Error() string { return e.Kind + ": " + e.Message }`
- Error kind constants as string constants in `task/task.go` (e.g., `ErrKindHandler`, `ErrKindDecode`, `ErrKindPanic`)
- Producer-side validation returns `fmt.Errorf("ebind: ...")` prefixed errors
- `errors.As()` used to unwrap `TaskError` from return values in worker dispatch
- `errors.Is()` used for sentinel comparison (e.g., `errors.Is(err, ErrStaleRevision)`)
- Non-retryable vs retryable distinguished via `Retryable bool` field on `TaskError`
- CAS retry loops: up to 5 attempts with stale-revision detection:
## Logging
- Structured logging with key-value pairs
- Context-aware via `InfoContext`/`ErrorContext`
- Duration recorded in milliseconds
- Logging is optional middleware, not mandatory
- In the worker's `handle()`, errors are not logged directly — left to middleware
- In the scheduler, errors are silently swallowed with explanatory comment: `// production impl should log; swallowing here keeps durable delivery alive`
## Comments
- Package-level doc comments on every exported package (e.g., `// Package worker consumes tasks from the TASKS stream...`)
- Exported types and functions always have doc comments
- Uncommented internal functions for simple getters/setters
- Code comments explain _why_ not _what_ (e.g., shutdown ordering rationale in `worker.go`)
- Race-condition and ordering-critical sections have detailed comments with the full reasoning
- Not applicable (Go). Standard Go doc comments with `//` style.
- No `godox` linter exclusion needed — comments are used substantively.
## Function Design
- `ctx context.Context` is always the first parameter in handler functions registered with the registry
- Options passed via functional options pattern (e.g., `RegisterOpt`, `StepOption`, `DAGOption`, `StepOption`)
- Configuration via struct (e.g., `Options`, `Config`, `EnqueueOptions`)
- Handler signatures: `func(context.Context, args...) (T, error)` or `func(context.Context, args...) error`
- No bare returns — always explicit
- Named returns only for defer-based panics (`recover` middleware): `func(ctx context.Context, t *task.Task) (result []byte, err error)`
## Module Design
- Mix of exported and unexported as needed
- Core API types/constructors are exported; internal implementation details are unexported
- Interface types exported for callers to implement (e.g., `StateStore`, `EventBus`, `StepHook`, `ClaimProvider`)
- `sync.Map`, `atomic.Pointer`, `atomic.Bool`, `atomic.Int32` used for concurrent fields
## Common Patterns
- `StateStore` interface (`workflow/store.go`) — in-memory (`store_mem.go`) and NATS KV (`store_nats.go`) implementations
- `EventBus` interface (`workflow/events.go`) — channel-based (`events_mem.go`) and JetStream (`events_nats.go`)
- `Enqueuer` interface (`workflow/enqueuer.go`) — captured by `captureEnq` in tests
- `StepHook` interface (`worker/hook.go`) — implemented by `workflow.StepHook`
- `ClaimProvider` interface (`worker/claims.go`) — implemented via `ClaimsFunc` adapter or `mutableClaims`
- `LeaderElector` interface — tests use `toggleElector`
- All store mutations use optimistic concurrency via `expectedRev` (uint64)
- `ErrStaleRevision` signals CAS failure; writers retry on stale
- Patterns for CAS loops in `workflow/hook.go::casUpdateStatus` and `workflow/scheduler.go::persistStatus`
- Value receivers for pure, stateless methods (e.g., `RetryPolicy.NextDelay()`, `RetryPolicy.ShouldRetry()`)
- Pointer receivers for state-mutating methods (e.g., `DAGState.MarkDone()`, `Registry.Register()`)
- Pointer receivers for worker/client with mutable state (e.g., `Worker.Run()`, `Worker.Use()`, `Client.Close()`)
- `t.Helper()` called at the top of all test helper functions
- `t.Cleanup()` for resource teardown instead of manual `defer` in tests
- `t.TempDir()` for temporary directories
<!-- GSD:conventions-end -->

<!-- GSD:architecture-start source:ARCHITECTURE.md -->
## Architecture

## Pattern Overview
- **Library, not a service** — applications embed ebind in-process; no separate server deployment required.
- **Single external dependency** — NATS JetStream handles persistence, queueing, deduplication, and clustering. No Redis, Postgres, or other infrastructure.
- **Reflection-based function dispatch** — Go functions are registered by canonical name (derived via `runtime.FuncForPC`) and invoked at delivery via `reflect.Value.Call`. This enables a generic `Enqueue(c, MyFunc, a, b, c)` signature impossible with Go generics alone.
- **CAS-driven state management** — All DAG step and metadata mutations use compare-and-swap (via NATS KV revisions). This allows the scheduler to run in every worker without an explicit leader for correctness — KV CAS + JetStream queue semantics + msg-id dedupe give at-most-once effects.
- **Event-driven scheduler** — DAG step completion events flow through a dedicated JetStream stream (`EBIND_DAG_EVENTS`), consumed by a scheduler loop in every worker. The scheduler gates work via a pluggable `LeaderElector`.
## Layers
- Purpose: Defines JetStream stream names, subject conventions, and idempotent stream provisioning.
- Location: `stream/setup.go`
- Contains: `EnsureStreams()` which creates/updates `EBIND_TASKS` (WorkQueuePolicy), `EBIND_RESP` (LimitsPolicy), `EBIND_DLQ` (LimitsPolicy), and subject helper functions (`TaskPublishSubject`, `TargetedTaskSubject`, `TargetToken`).
- Depends on: NATS `jetstream.JetStream` interface
- Used by: `client/`, `worker/`, `workflow/`, `dlq/`
- Purpose: Core domain types (`Task`, `Response`, `TaskError`, `RetryPolicy`) and the reflection-based function registry (`Registry`, `Dispatcher`).
- Location: `task/task.go`, `task/registry.go`, `task/retry.go`
- Contains: `Register()` / `MustRegister()` / `Describe()` for handler registration, `Dispatcher.Call()` for reflective dispatch, `CanonicalName()` for deriving stable handler names.
- Depends on: Go `reflect`, `runtime` standard library
- Used by: `client/`, `worker/`, `workflow/`
- Purpose: Enqueue tasks onto the TASKS stream, await typed responses via `Future`.
- Location: `client/client.go`, `client/future.go`
- Contains: `Client` struct with `Enqueue()` / `EnqueueOpts()` / `EnqueueAsync()`, `Future` with `Get()` / `GetRaw()` / `Await[T]()`.
- Depends on: `task/` (for `Describe`, validation), `stream/` (for subject routing)
- Used by: Application code that produces tasks
- Purpose: Pull tasks from the TASKS stream, dispatch through a middleware chain to registered handlers, publish responses and DLQ entries.
- Location: `worker/worker.go`, `worker/middleware.go`, `worker/hook.go`, `worker/claims.go`
- Contains: `Worker` with `Run()` (blocking event loop), `Handler` type, `Middleware` chain builder, `Recover()` / `Log()` / `WithMetrics()` middlewares, `StepHook` interface for workflow integration, `ClaimProvider` for targeted delivery.
- Depends on: `task/`, `stream/`, `dlq/`
- Used by: Application code that consumes tasks; `workflow` package via `StepHook`
- Purpose: Define, submit, and schedule DAG workflows with step dependencies, retries, dynamic step addition, cancellation, and Await.
- Location: `workflow/` (28 files)
- Contains: `DAG` builder (`dag.go`), `Workflow` coordinator (`workflow.go`), `Scheduler` (`scheduler.go`), pure DAG state machine (`state.go`), `Ref` argument resolution (`ref.go`), `Step` / `StepOption` (`step.go`), `StateStore` interface + `NatsStore`/`MemStore` impls (`store.go`, `store_nats.go`, `store_mem.go`), `EventBus` interface + `NatsBus`/`MemBus` impls (`events.go`, `events_nats.go`, `events_mem.go`), `Enqueuer` interface + `NatsEnqueuer` (`enqueuer.go`, `enqueuer_nats.go`), `ContextDAG` for dynamic steps (`context.go`), `Await`/`AwaitByID` (`await.go`), `Cancel` (`cancel.go`), `DeleteDAG` (`delete.go`), `Debug`/`DebugPrint` (`debug.go`), `PlacementSpec` (`placement.go`).
- Depends on: `task/`, `worker/` (via `StepHook` interface), `stream/` (via `NatsEnqueuer`)
- Used by: Application code building workflows; `cmd/ebctl` for inspection
- Purpose: Publish and re-queue poison tasks that exhausted retries or failed non-retryably.
- Location: `dlq/dlq.go`, `dlq/requeue.go`
- Contains: `Entry` type, `Publish()`, `Fetch()`, `Requeue()`
- Depends on: `task/`, `stream/`
- Used by: `worker/` on terminal failure; `cmd/ebctl` for operator inspection
- Purpose: Start and supervise NATS JetStream servers in-process — single-node or 3-node HA cluster.
- Location: `embed/node.go`, `embed/cluster.go`
- Contains: `StartNode()` (single-node), `StartCluster()` (3-node HA with loopback routes), `Node` / `Cluster` types.
- Depends on: `github.com/nats-io/nats-server/v2`
- Used by: Application code wanting zero-infrastructure deployment; tests via `internal/testutil`
## Data Flow
```
```
## Design Patterns
### Reflection-Based Function Registry
- Canonical name = last segment of `runtime.FuncForPC(fn).Name()` (e.g. `github.com/you/app/handlers.Foo` → `handlers.Foo`). Renaming or moving a function breaks in-flight tasks. Use `task.WithName("...")` or `task.Alias("...")` to override.
- Handler signature must be `func(context.Context, args...) (T, error)` or `func(context.Context, args...) error`. Validation happens at `Register` and at `Enqueue` (client-side, before publish).
- `reflect.Value.Call` costs ~100–500 ns — negligible vs. real handler work (I/O, disk).
### CAS-Driven State (Workflow)
- KV bucket `ebind-dags`: `<dag_id>/meta`, `<dag_id>/step/<step_id>`, `<dag_id>/result/<step_id>`
- `NatsStore` wraps `jetstream.KeyValue`; `MemStore` emulates revision-based CAS for tests.
### Interface Isolation for Testability
- `StateStore` — `GetStep`, `PutStep`, `ListSteps`, `GetResult`, `PutResult`, `GetMeta`, `PutMeta`, `ListDAGs`, `DeleteMeta`, `DeleteStep`, `DeleteResult`, `WatchResult`
- `EventBus` — `Publish`, `Subscribe`
- `Enqueuer` — `Enqueue`
- `LeaderElector` — `IsLeader`
### Event-Driven Scheduler
- On leader acquisition (false→true edge of `IsLeader()`): a full sweep lists all running DAGs and re-enqueues stranded ready steps (edge-triggered, not level-triggered).
- Defaults: 5s sweep check interval, 60s sweep timeout. Overlap-guarded.
### Middleware Chain (Worker)
### Placement/Targeted Delivery
- `PlacementDirect` — explicit target claim
- `PlacementColocate` — run on the same concrete worker as another step
- `PlacementFollow` — follow the same logical target as another step
- `PlacementHere` — (dynamic steps only) run on the currently-executing worker
## How DAG Scheduler and Worker Interact
## Concurrency Model
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
- `sem chan struct{}` — semaphore in `worker.Run` bounds concurrency (`opts.Concurrency`).
- `fut.ch chan *task.Response` — `Future` channel, buffered 1, for response delivery from NATS callback to `Future.Get()` caller.
- `s.wf.Bus` — event delivery channel (internal to NATS consumer in `NatsBus`, channel-based fan-out in `MemBus`).
- `done chan struct{}` — in `worker.Run` for shutdown drain coordination.
- `closed chan struct{}` — in `client.Client` for close notification.
- `task.Registry.mu` — `sync.RWMutex` protects the handler map. `Get()` uses RLock; `Register()`/`MustRegister()` uses Lock.
- `workflow.Scheduler.mu` — `sync.Mutex` serializes intra-process event handling so CAS on step records doesn't race between concurrent event deliveries.
- `workflow.Scheduler.leaderMu` — `sync.Mutex` guards `wasLeader`/`sweepRunning` state.
- `worker.claimSubsMu` — `sync.Mutex` guards `activeClaimSubs` map.
- `client.Client.waiters` — `sync.Map` (lock-free) for waiter channel registration/lookup.
- `workflow.MemStore.mu` — `sync.Mutex` for in-memory state access.
- `workflow.MemBus.mu` — `sync.Mutex` for subscriber list access.
- `worker.stopping` — `atomic.Bool` for shutdown fast-path in consume callbacks.
- `worker.claimCache` — `atomic.Pointer[[]string]` for lock-free claim set reads.
- Worker consume callbacks use a semaphore (`sem`) + `WaitGroup` pattern: callback acquires sem, does `wg.Add(1)`, spawns goroutine. Shutdown sequence: set `stopping=true`, stop consumers, drain `<-Closed()`, then `wg.Wait()` with timeout.
- Scheduler event handling is serialized within one process via `s.mu`. Cross-process serialization is the user's `LeaderElector` responsibility. The combination of KV CAS + JetStream queue semantics + msg-id dedupe provides correctness even without leadership.
- Claim subscriptions intentionally NOT stopped on claim loss: in-flight handlers for a dropped claim could get redelivered before Ack, breaking at-least-once semantics. Stray deliveries are NAKed via `ownsTarget` check.
<!-- GSD:architecture-end -->

<!-- GSD:skills-start source:skills/ -->
## Project Skills

No project skills found. Add skills to any of: `.claude/skills/`, `.agents/skills/`, `.cursor/skills/`, or `.github/skills/` with a `SKILL.md` index file.
<!-- GSD:skills-end -->

<!-- GSD:workflow-start source:GSD defaults -->
## GSD Workflow Enforcement

Before using Edit, Write, or other file-changing tools, start work through a GSD command so planning artifacts and execution context stay in sync.

Use these entry points:
- `/gsd-quick` for small fixes, doc updates, and ad-hoc tasks
- `/gsd-debug` for investigation and bug fixing
- `/gsd-execute-phase` for planned phase work

Do not make direct repo edits outside a GSD workflow unless the user explicitly asks to bypass it.
<!-- GSD:workflow-end -->



<!-- GSD:profile-start -->
## Developer Profile

> Profile not yet configured. Run `/gsd-profile-user` to generate your developer profile.
> This section is managed by `generate-claude-profile` -- do not edit manually.
<!-- GSD:profile-end -->
