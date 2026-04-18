# ebind examples

Each subdirectory is a self-contained runnable program. Every example starts its own embedded NATS JetStream so you can run any of them with no external setup.

```sh
go run ./examples/01-basic
go run ./examples/07-workflow-fanout
# ...
```

## Index

### Task queue basics

| # | Example | Demonstrates |
|---|---|---|
| 01 | [`01-basic`](./01-basic) | Register a handler, enqueue a task, await the typed result |
| 02 | [`02-retry-policy`](./02-retry-policy) | Task-level `RetryPolicy` with backoff + attempt cap + non-retryable kinds |
| 03 | [`03-fire-and-forget`](./03-fire-and-forget) | `EnqueueAsync` — publish without subscribing for a response |
| 04 | [`04-cluster-ha`](./04-cluster-ha) | Start a 3-node in-process NATS cluster; survive a non-leader node loss |
| 05 | [`05-middleware`](./05-middleware) | Compose built-in (`worker.Log`) + a custom timing middleware |

### Workflow (DAG)

| # | Example | Demonstrates |
|---|---|---|
| 06 | [`06-workflow-linear`](./06-workflow-linear) | Linear A → B → C pipeline with typed data flow via `.Ref()` |
| 07 | [`07-workflow-fanout`](./07-workflow-fanout) | Parallel fan-out + fan-in — two roots run concurrently, third joins |
| 08 | [`08-workflow-optional`](./08-workflow-optional) | `Optional()` step whose failure doesn't fail DAG; downstream uses `RefOrDefault(v)` |
| 09 | [`09-workflow-dynamic`](./09-workflow-dynamic) | Handler adds per-page step dynamically via `workflow.FromContext(ctx).Step(...)` |
| 10 | [`10-workflow-temporal-deps`](./10-workflow-temporal-deps) | Time-only deps via `After()` (cascade on fail) and `AfterAny()` (always run) |

## Reading order

If you're new to ebind, walk the examples in order — each adds one concept on top of the previous:

1. **01 → 03**: basic task queue mechanics
2. **04**: HA deployment topology
3. **05**: extending the worker pipeline
4. **06 → 09**: workflow layer, from simple linear to fully dynamic

## Common shape

Every example follows the same skeleton so you can diff them to see which parts vary:

```go
// 1. Embedded NATS + connect
node, _ := embed.StartNode(...)
nc, _ := nats.Connect(node.ClientURL())

// 2. Streams
js, _ := jetstream.New(nc)
stream.EnsureStreams(ctx, js, stream.Config{Replicas: 1})

// 3. Register handlers + start worker
reg := task.NewRegistry()
task.MustRegister(reg, MyHandler)
w, _ := worker.New(nc, reg, worker.Options{...})
go w.Run(ctx)

// 4. (workflow only) Wire workflow layer
wf, _ := workflow.NewFromNATS(ctx, nc, 1)
// + StepHook + ContextMiddleware in worker.Options
go wf.RunScheduler(ctx)

// 5. Enqueue + await
fut, _ := client.Enqueue(c, MyHandler, args...)
result, _ := client.Await[T](ctx, fut)
```

In production you wouldn't embed NATS in the producer binary — you'd point `nats.Connect` at an existing cluster. The embed package is for dev, tests, and single-binary HA deployments where the app _is_ the NATS node.
