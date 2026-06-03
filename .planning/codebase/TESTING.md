# Testing Patterns

**Analysis Date:** 2026-06-03

## Test Framework

**Runner:**
- Go standard library `testing` package — no third-party test runners
- Go version: 1.26.1 (toolchain 1.26.3)
- Config: none — pure `go test`

**Assertion Library:**
- Standard `testing.T` methods only — no testify, no assert/require
- Patterns: `t.Errorf("want X, got %v", got)`, `t.Fatalf("...")`, `t.Fatal(err)`, `t.Error(...)`

**Run Commands:**
```bash
make test                # go test -race -timeout 180s -coverprofile=coverage.out -covermode=atomic ./...
make test-short          # go test -race -short -timeout 60s ./... (skips embedded NATS tests)
make test-v              # go test -race -v -timeout 180s ./...
make test-count          # go test -race -count=3 -timeout 300s ./... (flake hunt, 3x runs)
make cover               # test + HTML coverage report
make cover-summary       # per-package coverage percentages
make bench               # go test -run=- -bench=. -benchmem ./...
```

## Test File Organization

**Location:**
- Co-located with source files in the same package directory (e.g., `task/retry_test.go` alongside `task/retry.go`)
- Integration tests that need the `_test` package suffix live in the same directory but use `package workflow_test` (e.g., `workflow/integration_test.go`)
- Test utilities in `internal/testutil/harness.go`

**Naming:**
- Test files: `<name>_test.go` (e.g., `scheduler_test.go`, `retry_test.go`, `worker_test.go`)
- Test functions: `Test<Package>_<Behavior>` or `Test<Package>_<Scenario>` (e.g., `TestRetryPolicy_NextDelay_Zero`, `TestState_MarkDone_UnlockDependent`)
- Integration tests: `TestIntegration_<Scenario>` (e.g., `TestIntegration_Linear`, `TestIntegration_FanOutFanIn`)

**Structure:**
```
task/
  retry_test.go          # Pure unit tests for RetryPolicy
  registry_test.go       # Unit tests for Register/Describe/Call
worker/
  worker_test.go         # Integration tests with embedded NATS
workflow/
  state_test.go          # Pure unit tests for DAGState
  ref_test.go            # Pure unit tests for Ref parsing/resolution
  dag_test.go            # Unit tests for DAG builder + Submit
  scheduler_test.go      # Unit tests with fakes (MemStore, MemBus, captureEnq)
  store_test.go          # Contract tests for StateStore implementations
  hook_test.go           # Unit tests for StepHook + truncateErrorMessage
  await_test.go          # Unit tests for Await/AwaitByID
  delete_test.go         # Unit tests for DeleteDAG
  debug_test.go          # Unit tests for Debug/DebugPrint
  integration_test.go    # Full integration tests with real embedded NATS
  errmsg_integration_test.go  # Integration tests for error message persistence
embed/
  cluster_test.go        # Cluster integration tests (3-node HA)
dlq/
  requeue_test.go        # Unit tests for DLQ requeue
```

## Test Taxonomy

### 1. Pure Unit Tests
No NATS, no goroutines beyond test runner. Test pure functions directly.

**Files:**
- `task/retry_test.go` — 14 boundary cases for `NextDelay` and `ShouldRetry`
- `workflow/state_test.go` — `DAGState` transition methods (MarkDone, MarkFailed, cascade, Terminal)
- `workflow/ref_test.go` — `Ref` marshal/unmarshal, `IsRef`, `DecodeRef`, `ResolveArgs`
- `workflow/dag_test.go` (most tests) — DAG builder, cycle detection, root enqueue
- `workflow/debug_test.go` — Debug counts, durations, blockers
- `workflow/await_test.go` — Await/AwaitByID with MemStore

**Pattern:**
```go
func TestRetryPolicy_NextDelay_ExponentialGrowth(t *testing.T) {
    p := RetryPolicy{
        InitialInterval:    time.Second,
        BackoffCoefficient: 2.0,
        MaximumInterval:    time.Hour,
    }
    cases := []struct {
        attempt int
        want    time.Duration
    }{
        {1, time.Second},
        {2, 2 * time.Second},
        // ...
    }
    for _, c := range cases {
        if got := p.NextDelay(c.attempt); got != c.want {
            t.Errorf("NextDelay(%d) = %v, want %v", c.attempt, got, c.want)
        }
    }
}
```

### 2. Unit Tests with Fakes
Use in-memory fakes (`MemStore`, `MemBus`, `captureEnq`) to test logic without NATS.

**Files:**
- `workflow/scheduler_test.go` — scheduler event handling, sweep, leader-to-follower behavior
- `workflow/store_test.go` — contract tests for StateStore via `runStoreContract`
- `workflow/hook_test.go` — StepHook CAS update, error message truncation
- `workflow/await_test.go` — Await with MemStore + async writes
- `workflow/delete_test.go` — DeleteDAG with MemStore

**Key fakes:**

`captureEnq` in `workflow/scheduler_test.go`:
```go
type captureEnq struct {
    mu    sync.Mutex
    tasks []task.Task
}
func (c *captureEnq) Enqueue(_ context.Context, t task.Task) error {
    c.mu.Lock(); defer c.mu.Unlock()
    c.tasks = append(c.tasks, t)
    return nil
}
```

**Contract test pattern** in `workflow/store_test.go`:
```go
func runStoreContract(t *testing.T, newStore func(t *testing.T) StateStore) {
    t.Run("PutMeta_CreateOnce", func(t *testing.T) {
        s := newStore(t)
        // test CAS semantics
    })
    t.Run("Step_CRUD", func(t *testing.T) { /* ... */ })
    t.Run("ConcurrentCAS", func(t *testing.T) { /* ... */ })
}

func TestMemStore_Contract(t *testing.T) {
    runStoreContract(t, func(t *testing.T) StateStore { return NewMemStore() })
}
```

### 3. Integration Tests (Embedded NATS)
Start a real in-process NATS JetStream server via `embed.StartNode`. Test the full stack.

**Files:**
- `worker/worker_test.go` — worker round-trip, panic recovery, retry, StepHook
- `workflow/integration_test.go` — DAG workflows: linear, fan-out/in, failure cascade, retry, targeted placement, cancel
- `workflow/errmsg_integration_test.go` — error message persistence end-to-end

**Harness pattern** (`workflow/integration_test.go`):
```go
type wfHarness struct {
    nc     *nats.Conn
    js     jetstream.JetStream
    wf     *workflow.Workflow
    reg    *task.Registry
    worker *worker.Worker
    cancel context.CancelFunc
}

func setup(t *testing.T) *wfHarness {
    t.Helper()
    storeDir, err := os.MkdirTemp("", "ebind-wf-*")
    // ... start node, connect, create streams, create workflow, worker, scheduler
    return &wfHarness{...}
}
```

### 4. Cluster Integration Tests
Start a 3-node HA cluster via `embed.StartCluster`. Test quorum formation, failover.

**Files:**
- `embed/cluster_test.go` — `TestCluster_FormsQuorum`, `TestCluster_SurvivesOneNodeLoss`

**Pattern:**
```go
func TestCluster_FormsQuorum(t *testing.T) {
    c, err := StartCluster(ClusterConfig{Size: 3, Name: "test-quorum"})
    // ...
    waitClusterReady(t, c, 15*time.Second)
    // publish, check stream replicas
}
```

### 5. CLI Integration Tests
Test the `ebctl` CLI against an embedded NATS.

**Files:**
- `cmd/ebctl/ebctl_integration_test.go`

## Test Structure Patterns

**Setup helpers with `t.Helper()` and `t.Cleanup()`:**
```go
func setupDAG(t *testing.T) (*Workflow, *DAG, *captureEnq) {
    t.Helper()
    enq := &captureEnq{}
    wf := NewWorkflow(NewMemStore(), NewMemBus(), enq)
    dag := New()
    return wf, dag, enq
}
```

**Shared test state with package-level atomics** (for integration test handler functions):
```go
var (
    orderMu    sync.Mutex
    orderSeen  []string
    orderStart = map[string]time.Time{}
    orderEnd   = map[string]time.Time{}
)

func resetOrdering() {
    orderMu.Lock()
    orderSeen = nil
    orderStart = map[string]time.Time{}
    orderEnd = map[string]time.Time{}
    orderMu.Unlock()
}
```

**Timeout + context pattern** for async tests:
```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
```

**Polling pattern** for eventual consistency:
```go
deadline := time.Now().Add(2 * time.Second)
for time.Now().Before(deadline) {
    rec, _, err := wf.Store.GetStep(ctx, dag.ID(), "a")
    if err == nil && rec.Status == workflow.StatusDone {
        break
    }
    time.Sleep(50 * time.Millisecond)
}
```

**Channel synchronization** for handler ordering:
```go
func newBlockedWhereHandler() (func(context.Context) (string, error), <-chan struct{}, chan<- struct{}) {
    started := make(chan struct{})
    release := make(chan struct{})
    fn := func(ctx context.Context) (string, error) {
        close(started)  // signal test that handler started
        <-release       // block until test says go
        return workflow.CurrentWorkerID(ctx), nil
    }
    return fn, started, release
}
```

## Mocking

**Framework:** No mocking framework. Fakes are hand-written as structs implementing interfaces.

**Patterns:**
```go
// toggleElector implements LeaderElector for test control
type toggleElector struct {
    mu     sync.Mutex
    leader bool
}
func (t *toggleElector) IsLeader() bool { ... }
func (t *toggleElector) set(v bool) { ... }
```

**What to Mock:**
- `StateStore` — `MemStore`
- `EventBus` — `MemBus`
- `Enqueuer` — `captureEnq`
- `LeaderElector` — `toggleElector`
- `ClaimProvider` — `mutableClaims`

**What NOT to Mock:**
- The task registry (use `task.NewRegistry()` + `task.MustRegister()`)
- Handler functions (define real handler functions in test file)
- JetStream interactions in integration tests (use real embedded NATS)

## Fixtures and Factories

**Test Data:**
- `defaultTestPolicy()` in `workflow/scheduler_test.go` — returns a fast RetryPolicy for tests:
```go
func defaultTestPolicy() task.RetryPolicy {
    return task.RetryPolicy{
        InitialInterval:    10 * time.Millisecond,
        BackoffCoefficient: 2.0,
        MaximumInterval:    100 * time.Millisecond,
        MaximumAttempts:    3,
    }
}
```

- Helper constructors in test files:
  - `makeState(steps ...StepRecord) *DAGState` — builds DAGState for state tests
  - `refArgs(refs ...Ref) json.RawMessage` — produces JSON args with Ref envelopes
  - `seedRunningStep(t, store, dagID, stepID)` — inserts a Running step for hook tests
  - `seedDAG(t, steps...) (*Workflow, string)` — seeds store with meta+steps for debug tests
  - `publishCompletion(t, wf, dagID, stepID, status, errKind)` — simulates hook events
  - `emulateHook(t, wf, dagID, stepID, result, te)` — drives scheduler without real worker
  - `waitEnqueued(t, enq, want, timeout)` — polls captureEnq for expected task count

**Location:**
- Inline in test files (no separate `testdata/` or `fixtures/` directory)
- `internal/testutil/harness.go` provides `Harness.SingleNode()` for reusable worker integration test setup

## Coverage

**Requirements:** None enforced as minimum, but coverage HTML report is generated:
```bash
make cover  # writes coverage.out + coverage.html in project root
```
Per-package coverage available via `make cover-summary`.

## Test Types Summary

| Category | Files | What's tested | NATS needed |
|---|---|---|---|
| Pure unit | `retry_test.go`, `ref_test.go`, `state_test.go`, parts of `dag_test.go`, `debug_test.go`, `await_test.go` | Pure functions, state transitions, argument resolution | No |
| Unit with fakes | `scheduler_test.go`, `store_test.go`, `hook_test.go`, `delete_test.go` | Scheduler event handling, store CAS contract, hook persistence | No |
| Integration | `worker_test.go`, `integration_test.go`, `errmsg_integration_test.go` | Full worker round-trip, DAG execution, retry, placement | Yes (embedded) |
| Cluster | `cluster_test.go` | Quorum formation, failover | Yes (3-node embedded) |
| CLI integration | `ebctl_integration_test.go` | CLI commands against real NATS | Yes (embedded) |

## Short Test Mode

The `-short` flag is checked by testing convention. Tests that require embedded NATS will have the `-short` guard (not implemented in current tests — tests use `testutil.SingleNode` which starts NATS; use `make test-short` to skip integration-heavy tests when fast feedback is needed).

## Common Patterns

**Async testing with channels:**
```go
func TestAwaitByID_AsyncResult(t *testing.T) {
    store := NewMemStore()
    wf := NewWorkflow(store, NewMemBus(), &captureEnq{})
    ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
    defer cancel()
    // store initial state
    // start goroutine that writes result after delay
    go func() {
        time.Sleep(100 * time.Millisecond)
        // update step to Done then write result
        rec, rev, _ := store.GetStep(ctx, "d", "a")
        rec.Status = StatusDone
        _ = store.PutStep(ctx, "d", "a", rec, rev)
        _ = store.PutResult(ctx, "d", "a", []byte(`42`))
    }()
    got, err := AwaitByID[int](ctx, wf, "d", "a")
    // assert
}
```

**Error sentinel testing:**
```go
if !errors.Is(err, workflow.ErrStepFailed) {
    t.Errorf("want ErrStepFailed, got %v", err)
}
```

**Custom TaskError assertion:**
```go
var te *task.TaskError
if !errors.As(err, &te) {
    t.Fatalf("want *TaskError, got %T: %v", err, err)
}
if te.Kind != "validation" {
    t.Errorf("want kind=validation, got %q", te.Kind)
}
```

**Table-driven tests:**
```go
func TestTruncateErrorMessage(t *testing.T) {
    cases := []struct {
        name string
        in   string
        max  int
        want string
    }{
        {"under default", "short", 0, "short"},
        {"disabled", "anything", -1, ""},
        {"exact", "abcd", 4, "abcd"},
        {"over", "abcdef", 4, "abcd…"},
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            if got := truncateErrorMessage(c.in, c.max); got != c.want {
                t.Errorf("truncateErrorMessage(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
            }
        })
    }
}
```

**Race detection:** All `make test*` targets use `-race` flag. The `test-count` target runs `-count=3` for flake detection.

---

*Testing analysis: 2026-06-03*
