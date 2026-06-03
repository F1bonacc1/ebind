# Coding Conventions

**Analysis Date:** 2026-06-03

## Naming Patterns

**Files:**
- Use `snake_case.go` for package-file naming (e.g., `store_mem.go`, `events_nats.go`, `enqueuer_nats.go`)
- Test files: `*_test.go` (e.g., `retry_test.go`, `state_test.go`, `scheduler_test.go`)
- One file per major type/role within a package, except when complementary types share a file

**Functions:**
- Exported functions use `PascalCase` (e.g., `Register`, `CanonicalName`, `NewRegistry`, `Chain`, `Recover`, `Enqueue`, `Await`)
- Unexported functions and methods use `camelCase` (e.g., `setDefaults`, `baseHandler`, `handleEvent`, `cascadeSkipFrom`, `snapshotUpstream`, `publishResponse`, `enqueueStep`)
- Factory functions named `New*` (e.g., `NewRegistry()`, `NewWorkflow(...)`, `New(...)` for DAG, `NewMemStore()`, `NewMemBus()`)
- Prefix `Default*` for default configuration constructors (e.g., `DefaultRetryPolicy()`, `NoRetryPolicy()`)
- Prefix `Must*` for panicking variants (e.g., `MustRegister`)

**Variables:**
- Exported package-level constants use PascalCase (e.g., `ErrKindHandler`, `DAGStatusRunning`, `EventCompleted`)
- Unexported package-level vars use camelCase with no prefix (e.g., `ctxType`, `errType`)
- Sentinel errors use `Err*` prefixed PascalCase (e.g., `ErrStepFailed`, `ErrCycle`, `ErrStaleRevision`)
- Boolean fields omit `Is`/`Has` prefix unless disambiguation needed (e.g., `Optional` not `IsOptional`, `hasResult` not `resultExists`)

**Types:**
- Exported types use `PascalCase` (e.g., `Task`, `Response`, `TaskError`, `DAGState`, `StepRecord`, `DAGMeta`, `Ref`, `Event`, `Workflow`)
- Interface types use `*er` suffix or descriptive noun (e.g., `Middleware`, `StepHook`, `StateStore`, `EventBus`, `Enqueuer`, `ClaimProvider`, `LeaderElector`, `Subscription`)
- Type aliases for string enums: `type DAGStatus string`, `type EventKind string`, `type RefMode string`, `type StepStatus string`
- Option/configuration structs: `Options` or `Config` (e.g., `worker.Options`, `stream.Config`, `client.Options`, `embed.NodeConfig`, `embed.ClusterConfig`)

## Code Style

**Formatting:**
- `gofmt` enforced via golangci-lint formatter section (`.golangci.yaml` uses `gofmt` formatter)
- `goimports` enforced for import ordering — `go tool goimports -w .` in `make fmt`
- No `fieldalignment` or `shadow` checks (explicitly disabled in `govet`)

**Linting:**
- `golangci-lint` configured in `.golangci.yaml` with `version: "2"` format
- Enabled linters:
  - `bodyclose`, `copyloopvar`, `errcheck`, `errorlint`, `gocritic`, `govet`, `ineffassign`, `misspell`, `nilerr`, `noctx`, `revive`, `staticcheck`, `unconvert`, `unparam`, `unused`
- Revive rules: `var-naming`, `error-return`, `error-strings`, `error-naming`, `if-return`, `range`, `receiver-naming`, `indent-error-flow`, `superfluous-else`
- Linting exclusions for test files: `bodyclose`, `errcheck`, `unparam`
- Linting exclusions for `cmd/demo/`: `errcheck`
- Example code excluded from linting (path: `^examples/`)
- Preset exclusions: `comments`, `common-false-positives`, `legacy`, `std-error-handling`

**Run command:**
```bash
make lint          # golangci-lint run ./...
make lint-fix      # golangci-lint run --fix ./...
make fmt           # go fmt + go tool goimports -w .
make vet           # go vet ./...
make ci            # vet + lint + test (full pipeline)
```

## Import Organization

**Order:**
1. Standard library packages
2. Third-party packages (separated by blank line from stdlib)
3. Internal/self imports (`github.com/f1bonacc1/ebind/...`)

**Example** (from `worker/worker.go`):
```go
import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "sort"
    "sync"
    "sync/atomic"
    "time"

    "github.com/google/uuid"
    "github.com/nats-io/nats.go"
    "github.com/nats-io/nats.go/jetstream"

    "github.com/f1bonacc1/ebind/dlq"
    "github.com/f1bonacc1/ebind/stream"
    "github.com/f1bonacc1/ebind/task"
)
```

**Path Aliases:**
- None used. All imports use the module path `github.com/f1bonacc1/ebind/...`
- External packages: `nats.go`, `jetstream` sub-package, `uuid`, `cobra`

## Error Handling

**Patterns:**
- Sentinel errors with `errors.New()` in `workflow/errors.go`:
  ```go
  var ErrStepFailed = errors.New("workflow: step failed")
  ```
- Wrapped errors with `fmt.Errorf("...: %w", err)` consistently:
  ```go
  return fmt.Errorf("worker: jetstream: %w", err)
  return fmt.Errorf("ebind: Register: not a function: %T", fn)
  ```
- Custom error type `TaskError` with structured fields (`Kind`, `Message`, `Retryable`):
  ```go
  type TaskError struct {
      Kind      string `json:"kind"`
      Message   string `json:"message"`
      Retryable bool   `json:"retryable"`
  }
  ```
- `TaskError` implements the `error` interface: `func (e *TaskError) Error() string { return e.Kind + ": " + e.Message }`
- Error kind constants as string constants in `task/task.go` (e.g., `ErrKindHandler`, `ErrKindDecode`, `ErrKindPanic`)
- Producer-side validation returns `fmt.Errorf("ebind: ...")` prefixed errors
- `errors.As()` used to unwrap `TaskError` from return values in worker dispatch
- `errors.Is()` used for sentinel comparison (e.g., `errors.Is(err, ErrStaleRevision)`)
- Non-retryable vs retryable distinguished via `Retryable bool` field on `TaskError`
- CAS retry loops: up to 5 attempts with stale-revision detection:
  ```go
  for attempt := 0; attempt < 5; attempt++ {
      // get, modify, CAS
      err = store.PutStep(ctx, dagID, stepID, rec, rev)
      if err == nil { return nil }
      if !errors.Is(err, ErrStaleRevision) { return err }
  }
  return ErrStaleRevision
  ```

**Must-pattern for registration:**
```go
func MustRegister(r *Registry, fn any, opts ...RegisterOpt) {
    if err := Register(r, fn, opts...); err != nil {
        panic(err)
    }
}
```

## Logging

**Framework:** `log/slog` from standard library, used in `worker/middleware.go`:
```go
func Log(log *slog.Logger) Middleware {
    // emits task.start, task.error, task.done structured logs
    log.InfoContext(ctx, "task.start", "task_id", t.ID, "name", t.Name, ...)
    log.ErrorContext(ctx, "task.error", ...)
}
```

**Patterns:**
- Structured logging with key-value pairs
- Context-aware via `InfoContext`/`ErrorContext`
- Duration recorded in milliseconds
- Logging is optional middleware, not mandatory
- In the worker's `handle()`, errors are not logged directly — left to middleware
- In the scheduler, errors are silently swallowed with explanatory comment: `// production impl should log; swallowing here keeps durable delivery alive`

## Comments

**When to Comment:**
- Package-level doc comments on every exported package (e.g., `// Package worker consumes tasks from the TASKS stream...`)
- Exported types and functions always have doc comments
- Uncommented internal functions for simple getters/setters
- Code comments explain _why_ not _what_ (e.g., shutdown ordering rationale in `worker.go`)
- Race-condition and ordering-critical sections have detailed comments with the full reasoning

**JSDoc/TSDoc:**
- Not applicable (Go). Standard Go doc comments with `//` style.
- No `godox` linter exclusion needed — comments are used substantively.

## Function Design

**Size:** Functions vary from 1-line accessors to ~60-line handlers. Core dispatch (`Worker.handle`) is ~70 lines. The scheduler `handleEvent` at ~55 lines. No excessively monolithic functions found.

**Parameters:**
- `ctx context.Context` is always the first parameter in handler functions registered with the registry
- Options passed via functional options pattern (e.g., `RegisterOpt`, `StepOption`, `DAGOption`, `StepOption`)
- Configuration via struct (e.g., `Options`, `Config`, `EnqueueOptions`)

**Return Values:**
- Handler signatures: `func(context.Context, args...) (T, error)` or `func(context.Context, args...) error`
- No bare returns — always explicit
- Named returns only for defer-based panics (`recover` middleware): `func(ctx context.Context, t *task.Task) (result []byte, err error)`

## Module Design

**Exports:**
- Mix of exported and unexported as needed
- Core API types/constructors are exported; internal implementation details are unexported
- Interface types exported for callers to implement (e.g., `StateStore`, `EventBus`, `StepHook`, `ClaimProvider`)
- `sync.Map`, `atomic.Pointer`, `atomic.Bool`, `atomic.Int32` used for concurrent fields

**Barrel Files:** Not used. Each package exposes its API directly from its source files.

## Common Patterns

**Functional Options pattern** — used in multiple packages:
```go
type RegisterOpt func(*regOptions)
type StepOption func(*Step)
type DAGOption func(*DAG)
```
Example consumers:
```go
Register(r, fn, WithName("custom.name"), Alias("old.name"))
dag.StepOpts("b", noopB, []StepOption{After(a)})
New(WithRetry(policy), WithDAGID("my-id"))
```

**Interface-based design for testability:**
- `StateStore` interface (`workflow/store.go`) — in-memory (`store_mem.go`) and NATS KV (`store_nats.go`) implementations
- `EventBus` interface (`workflow/events.go`) — channel-based (`events_mem.go`) and JetStream (`events_nats.go`)
- `Enqueuer` interface (`workflow/enqueuer.go`) — captured by `captureEnq` in tests
- `StepHook` interface (`worker/hook.go`) — implemented by `workflow.StepHook`
- `ClaimProvider` interface (`worker/claims.go`) — implemented via `ClaimsFunc` adapter or `mutableClaims`
- `LeaderElector` interface — tests use `toggleElector`

**CAS-based concurrency:**
- All store mutations use optimistic concurrency via `expectedRev` (uint64)
- `ErrStaleRevision` signals CAS failure; writers retry on stale
- Patterns for CAS loops in `workflow/hook.go::casUpdateStatus` and `workflow/scheduler.go::persistStatus`

**Value vs pointer receivers:**
- Value receivers for pure, stateless methods (e.g., `RetryPolicy.NextDelay()`, `RetryPolicy.ShouldRetry()`)
- Pointer receivers for state-mutating methods (e.g., `DAGState.MarkDone()`, `Registry.Register()`)
- Pointer receivers for worker/client with mutable state (e.g., `Worker.Run()`, `Worker.Use()`, `Client.Close()`)

**Configuration defaults pattern:**
```go
func (o *Options) setDefaults() {
    if o.Durable == "" { o.Durable = "ebind-worker" }
    if o.Concurrency <= 0 { o.Concurrency = 16 }
    // ...
}
```

**Test helper constructors:**
- `t.Helper()` called at the top of all test helper functions
- `t.Cleanup()` for resource teardown instead of manual `defer` in tests
- `t.TempDir()` for temporary directories

---

*Convention analysis: 2026-06-03*
