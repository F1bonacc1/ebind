# ebind

A Go task queue + DAG workflow engine built on NATS JetStream.

Function-first ergonomics (`Register(reg, MyFunc)`, `Enqueue(c, MyFunc, args...)`, `Await[T](ctx, fut)`) on top of a persistent queue with retries, dead-lettering, and optional DAG orchestration — all driven by a single NATS dependency that can run as an embedded in-process server (including 3-node HA cluster) or against an external JetStream deployment.

## Why ebind

- **NATS-native.** No Redis, no Postgres. If you already run NATS you already have ebind's dependencies.
- **Single-binary HA.** The `embed` package boots a 3-node JetStream cluster inside your process. One binary per machine, cluster.routes wired automatically.
- **Function-first API.** Pass your function reference, not a string name and a JSON schema. Reflection introspects the signature; runtime arg-type validation happens before publish.
- **Typed responses.** `Await[Profile](ctx, fut)` returns a typed value, not `interface{}`.
- **Durable DAG workflows.** Declare dependencies between steps; state lives in a NATS KV bucket so workflows survive producer restarts. Mandatory/optional steps, per-step retry policies, dynamic step addition from within handlers.

## Install

```sh
go get github.com/f1bonacc1/ebind
```

Requires Go 1.22+ and NATS JetStream 2.8+.

## Quickstart — standalone task queue

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/nats-io/nats.go"
    "github.com/nats-io/nats.go/jetstream"

    "github.com/f1bonacc1/ebind/client"
    "github.com/f1bonacc1/ebind/embed"
    "github.com/f1bonacc1/ebind/stream"
    "github.com/f1bonacc1/ebind/task"
    "github.com/f1bonacc1/ebind/worker"
)

// Any Go function with (context.Context, ...args) (T, error) or (context.Context, ...args) error.
func SendEmail(ctx context.Context, to, subject, body string) (string, error) {
    // ... actually send ...
    return "msg-id-42", nil
}

func main() {
    ctx := context.Background()

    // 1. Start an embedded NATS JetStream (dev). In prod, point at an external cluster
    //    or use embed.StartCluster(embed.ClusterConfig{Size: 3, ...}) for in-process HA.
    node, _ := embed.StartNode(embed.NodeConfig{Port: -1, StoreDir: "/tmp/ebind-demo"})
    defer node.Shutdown()
    nc, _ := nats.Connect(node.ClientURL())

    // 2. Create the ebind streams (TASKS, RESP, DLQ).
    js, _ := jetstream.New(nc)
    _ = stream.EnsureStreams(ctx, js, stream.Config{Replicas: 1})

    // 3. Register handlers + start worker.
    reg := task.NewRegistry()
    task.MustRegister(reg, SendEmail)

    w, _ := worker.New(nc, reg, worker.Options{Concurrency: 16})
    go w.Run(ctx)

    // 4. Enqueue from anywhere in the cluster — producer and worker can be different binaries.
    c, _ := client.New(ctx, nc, client.Options{})
    defer c.Close()

    fut, _ := client.Enqueue(c, SendEmail, "alice@example.com", "hello", "world")
    msgID, err := client.Await[string](ctx, fut)
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println("sent:", msgID)
}
```

Run the bundled end-to-end demo:

```sh
make demo
```

## Quickstart — DAG workflow

```go
import (
    "github.com/f1bonacc1/ebind/task"
    "github.com/f1bonacc1/ebind/workflow"
)

// Handlers are plain functions.
func FetchUser(ctx context.Context, id string) (User, error)        { /* ... */ }
func Enrich(ctx context.Context, id string) (Enriched, error)       { /* ... */ }
func Combine(ctx context.Context, u User, e Enriched) (Profile, error)

// In main(): after EnsureStreams + worker started, wire the workflow layer.
wf, _ := workflow.NewFromNATS(ctx, nc, 1 /* replicas */)
task.MustRegister(reg, FetchUser)
task.MustRegister(reg, Enrich)
task.MustRegister(reg, Combine)

// Attach the hook + middleware so the worker talks to the workflow layer.
w, _ := worker.New(nc, reg, worker.Options{
    StepHook:   wf.Hook(),
    Middleware: []worker.Middleware{wf.ContextMiddleware()},
})
go w.Run(ctx)
go wf.RunScheduler(ctx)

// Build + submit a DAG.
dag := workflow.New()
a := dag.Step("fetch",    FetchUser, userID)
b := dag.StepOpts("enrich", Enrich, []workflow.StepOption{workflow.Optional()}, userID)
c := dag.Step("combine",  Combine, a.Ref(), b.RefOrDefault(Enriched{}))

_ = dag.Submit(ctx, wf)
profile, err := workflow.Await[Profile](ctx, wf, dag.ID(), c)
```

Key behaviors:

- `a.Ref()` — `combine` runs only if `fetch` succeeds; cascade-skips otherwise.
- `b.RefOrDefault(v)` — `combine` runs with `v` substituted if `enrich` fails or is skipped.
- `workflow.Optional()` — `enrich`'s failure does not fail the DAG.
- `workflow.WithRetry(policy)` — per-DAG default retry; `workflow.WithStepRetry(policy)` overrides per-step.
- From inside a handler, `workflow.FromContext(ctx).Step(...)` adds more steps dynamically.

### Resuming `Await` from another instance

DAG state + step results live in NATS KV. Workers keep running independently of whoever called `Await`. If the waiting process dies, the DAG continues; results land in KV and stay there. A different process (same NATS cluster) can resume the wait with only the DAG and step IDs:

```go
// Instance A — submitter. Persist these two strings somewhere (DB, Redis, file).
dagID  := dag.ID()
stepID := c.ID()            // the *Step you'd pass to Await
_ = dag.Submit(ctx, wf)
// ... instance A may exit now ...

// Instance B — resumer. No *Step handle needed.
wfB, _  := workflow.NewFromNATS(ctx, nc, 1)
result, err := workflow.AwaitByID[Profile](ctx, wfB, dagID, stepID)
```

`AwaitByID` uses NATS KV `IncludeHistory()` under the hood, so late subscribers still receive results that were written before they started watching. See [`examples/11-workflow-resume`](./examples/11-workflow-resume) for a runnable two-invocation demo.

## Architecture

```
┌──────────────┐        publish TASKS.<name>         ┌──────────────┐
│   Producer   │ ───────────────────────────────────▶│ NATS JetStrm │
│              │                                     │  TASKS       │
│ client.Enq   │                                     └──────┬───────┘
└──────┬───────┘                                            │ pull
       │                                                    ▼
       │ subscribe RESP.<client_id>.>     ┌──────────────────────┐
       │◀──────────────── RESP ───────────│      Worker(s)       │
       │                                  │  - reflect.Call      │
       ▼                                  │  - middleware chain  │
  Future.Get() / Await[T]                 │  - StepHook          │
                                          └──────┬───────────────┘
                                                 │ DAG events
                                                 ▼
                                    ┌────────────────────────────┐
                                    │  Scheduler (every worker,  │
                                    │   leader-gated)            │
                                    │  - state in NATS KV        │
                                    │  - resync on acquire       │
                                    └────────────────────────────┘
```

- **`task.Registry`** — name → reflect.Value map; `Register(fn)` introspects signature.
- **`client.Client`** — one response-consumer per client; routes responses to typed Futures.
- **`worker.Worker`** — pull consumer + middleware chain (`Recover`, `Log`, user) + per-task retry policy.
- **`embed.StartCluster(3)`** — in-process 3-node JetStream cluster with loopback routes.
- **`workflow`** — DAG builder + persistent state (KV bucket `ebind-dags`) + event-driven scheduler with leader-elector-gated sweep for stranded recovery.

See [CLAUDE.md](./CLAUDE.md) for the full architectural walk-through.

## Production concerns

| Concern | Handled by |
|---|---|
| At-least-once delivery | JetStream `AckExplicitPolicy` + `MaxDeliver` |
| Exactly-one enqueue on retries | JetStream `Nats-Msg-Id` dedupe with 5-min window |
| State consistency | NATS KV `Update(key, val, expectedRev)` CAS |
| Handler panics | `worker.Recover` middleware → `TaskError{Kind: "panic"}` |
| Retry control | `task.RetryPolicy` on envelope (per-task) or `worker.Options.MaxDeliver` default |
| Non-retryable errors | `RetryPolicy.NonRetryableErrorKinds` OR `TaskError{Retryable: false}` |
| Dead-lettering | `dlq.Publish` auto-called on final failure → `EBIND_DLQ` stream |
| Graceful shutdown | `worker.Run(ctx)` drains in-flight on ctx-cancel (configurable grace) |
| HA | 3-node embedded cluster with `Replicas: 3` streams & KV |
| Stranded DAG recovery | `Scheduler` sweep on `LeaderElector` false→true edge |

## Package layout

```
task/         envelope + registry + RetryPolicy
client/       Enqueue + Future + Await[T]
worker/       consume loop + middleware + StepHook
stream/       JetStream stream setup
dlq/          dead-letter publishing
embed/        in-process NATS server (single + cluster)
workflow/     DAG builder + scheduler + KV-backed state
internal/testutil/  harness for integration tests
cmd/demo/     single-process end-to-end demo
```

## Development

```sh
make help          # list all targets
make build         # compile everything
make test          # run all tests with -race
make test-count    # 3× runs, catch flakes
make lint          # golangci-lint
make cover         # HTML coverage report
make demo          # run cmd/demo end-to-end
```

## Status

- v1 (task queue): done — retries, DLQ, middleware, embedded HA cluster.
- v2 (DAG workflows): done — optional steps, retry policies, dynamic DAGs.
- v2.1 (stranded recovery): done — leader-acquisition sweep.
- v2+ (future): phantom-Running detection, cross-DAG signals, saga/compensation.

## License

MIT
