# External Integrations

**Analysis Date:** 2026-06-03

## Overview

This project integrates with exactly **one external system**: [NATS](https://nats.io/). There are no other external services, databases, or APIs. The library can embed its own NATS server or connect to an existing deployment.

---

## NATS JetStream

**The single integration.** Everything runs on NATS JetStream — no Redis, no Postgres, no external databases.

### Connection Mode

The library accepts an existing `*nats.Conn` and creates a `jetstream.JetStream` handle from it. This is done in every package that needs NATS:

| Package | File | Pattern |
|---------|------|---------|
| `worker/` | `worker/worker.go` | `js, err := jetstream.New(nc)` in `New()` |
| `client/` | `client/client.go` | `js, err := jetstream.New(nc)` in `New()` |
| `workflow/` | `workflow/nats.go` | `js, err := jetstream.New(nc)` in `NewFromNATS()` |
| `cmd/ebctl/` | `cmd/ebctl/internal/cli/cli.go` | `js, err := jetstream.New(nc)` in PersistentPreRun |

### NATS Server Modes

**Three modes supported:**

1. **Embedded single-node** — `embed.StartNode(NodeConfig)` at `/home/pawel/repo/ebind/embed/node.go`
   - In-process NATS server with JetStream enabled
   - Configurable host, port, server name, store directory
   - Used by tests (`internal/testutil/harness.go`), demo (`cmd/demo/main.go`), and examples
   - Example: `embed/embed_test.go` tests

2. **Embedded 3-node HA cluster** — `embed.StartCluster(ClusterConfig)` at `/home/pawel/repo/ebind/embed/cluster.go`
   - Three NATS servers on loopback with route formation
   - Random free ports via `net.ListenTCP` + port reservation
   - `WaitReady()` polls for meta-leader election + peer discovery
   - Requires `stream.WaitMetaLeader` before stream creation
   - Example: `examples/04-cluster-ha/main.go`

3. **External NATS** — Connect to any running NATS server
   - `nats.Connect(url)` with standard options
   - Used by `ebctl` CLI to manage existing deployments
   - `EBIND_NATS_URL` env var (default `nats://localhost:4222`)

### JetStream Streams

Four streams, defined in `/home/pawel/repo/ebind/stream/setup.go`:

| Stream Name | Type | Subjects | Retention | Purpose |
|-------------|------|----------|-----------|---------|
| `EBIND_TASKS` | Task queue | `TASKS.>`, `TASKS_TARGET.>` | WorkQueue | Task envelopes consumed by workers |
| `EBIND_RESP` | Response channel | `RESP.>` | Limits | Per-client response envelopes for `Future.Get/Await` |
| `EBIND_DLQ` | Dead letter queue | `DLQ.>` | Limits | Failed/dead-lettered tasks (7d default max age) |
| `EBIND_DAG_EVENTS` | Workflow events | `DAG.>` | WorkQueue | Scheduler events (`completed`, `step_added`) |

**Stream configuration defaults** (from `stream.Config`):
- `TaskMaxAge`: 24h
- `ResponseMaxAge`: 1h
- `DLQMaxAge`: 7d
- `DuplicateWindow`: 5min
- `Replicas`: 1 (dev) / 3 (HA cluster)

All streams use **FileStorage** — no MemoryStorage paths.

### JetStream KeyValue Store

Single KV bucket `ebind-dags`, used exclusively by the workflow package:

| Key Pattern | Value | Purpose |
|-------------|-------|---------|
| `<dag_id>/meta` | `DAGMeta` | DAG-level metadata (status, default policy, timestamps) |
| `<dag_id>/step/<step_id>` | `StepRecord` | Per-step state (fn name, args, deps, status, retry policy, error info) |
| `<dag_id>/result/<step_id>` | raw bytes | Per-step result output |

**All writes use CAS** (`Create`/`Update` with revision checks) — fundamental to correctness without leader election. Implemented at `/home/pawel/repo/ebind/workflow/store_nats.go`.

### JetStream Consumers

| Consumer | Stream | Type | Scope |
|----------|--------|------|-------|
| `ebind-worker` (durable) | `EBIND_TASKS` | Filter: `TASKS.>` | General task consumption |
| `ebind-targets-<claim>` (durable) | `EBIND_TASKS` | Filter: `TASKS_TARGET.<token>.>` | Targeted/claimed task consumption |
| `ebind-resp-<client_id>` (ephemeral) | `EBIND_RESP` | Filter: `RESP.<client_id>.>` | Per-client response routing |
| `ebind-scheduler` (durable) | `EBIND_DAG_EVENTS` | Filter: `DAG.>` | DAG scheduler event processing |

### JetStream Features Used

- **WorkQueuePolicy** — for task and event streams (at-most-once consumer semantics)
- **LimitsPolicy** — for response and DLQ streams (retention by age/bytes)
- **AckExplicitPolicy** — all consumers require explicit ack
- **MaxDeliver** — configurable per consumer (default -1 = infinite for worker)
- **Nats-Msg-Id deduplication** — 5-minute window; task UUID for ad-hoc, `<dag_id>:<step_id>` for DAG steps
- **FileStorage** — all streams
- **KV CAS** — `Create`/`Update` with revision checks
- **KV Watch** — `WatchResult` for `Await[T]` functionality
- **KV ListKeys** — `ListDAGs`, `ListSteps` for scheduler sweep + CLI introspection
- **Consumer filter subjects** — targeted delivery via `TASKS_TARGET.` prefix
- **`GetMsg`** / **`DeleteMsg`** — DLQ entry inspection and requeue
- **Stream-level `DeleteMsg`** — `dlq/requeue.go`

### Subject Naming Conventions

```
TASKS.<handler_name>              — general task subject
TASKS_TARGET.<base64_target>.<name>  — targeted task subject
RESP.<client_id>.<task_id>        — response subject
DLQ.<handler_name>                — dead letter subject
DAG.<dag_id>.<kind>.<step_id>     — workflow event subject
```

---

## APIs Exposed

### Go Library API

This project exposes a **pure Go library API** — no HTTP, gRPC, or REST endpoints.

| Package | Entry Point | Purpose |
|---------|-------------|---------|
| `task` | `NewRegistry()`, `Register()`, `MustRegister()` | Register handler functions with reflection-based dispatch |
| `client` | `New()`, `Enqueue()`, `EnqueueOpts()`, `EnqueueAsync()`, `Await[T]()` | Produce tasks and await results |
| `worker` | `New()`, `Run()` | Consume tasks, dispatch to handlers, manage retries |
| `workflow` | `New()`, `Step()`, `Submit()`, `RunScheduler()`, `Await[T]()` | Build and run DAG workflows |
| `workflow` | `Cancel()`, `DeleteDAG()`, `DAGInfo()` | Manage DAG lifecycle |
| `stream` | `EnsureStreams()` | Bootstraps required JetStream streams |
| `dlq` | `Publish()`, `Fetch()`, `Requeue()` | Dead letter queue management |
| `embed` | `StartNode()`, `StartCluster()` | In-process NATS server |

### CLI Tool (`ebctl`)

Command-line operator tool, built with cobra at `/home/pawel/repo/ebind/cmd/ebctl/main.go`:

```
ebctl
├── version           — Print version info
├── dag
│   ├── ls            — List DAGs
│   ├── get           — Get DAG details
│   ├── tree          — Show DAG as dependency tree
│   ├── step          — Get/result details of a step
│   ├── watch         — Watch DAG events live
│   ├── cancel        — Cancel a running DAG
│   └── rm            — Delete a DAG
├── dlq
│   ├── ls            — List DLQ entries
│   ├── show          — Show DLQ entry details
│   ├── watch         — Watch DLQ live
│   ├── requeue       — Requeue DLQ entry to TASKS
│   └── purge         — Purge DLQ
└── stream
    ├── ls            — List streams
    ├── info          — Stream info
    ├── consumer      — List consumers
    ├── peek          — Peek stream messages
    ├── purge         — Purge stream
    └── rmmsg         — Remove specific message
```

**Global flags:** `--server`/`-s` (NATS URL), `--output`/`-o` (pretty|json), `--timeout`, `--yes`, `--verbose`, `--replicas`

### Demo Application

Single-process end-to-end at `/home/pawel/repo/ebind/cmd/demo/main.go` — embeds NATS, registers handlers, enqueues tasks, awaits responses.

### Examples

13 example programs in `/home/pawel/repo/ebind/examples/` demonstrating: basic usage, retry policies, fire-and-forget, cluster HA, middleware, linear workflows, fan-out, optional steps, dynamic steps, temporal deps, resume, placement, cancel.

---

## Serialization

**Format:** JSON exclusively across all wire protocols.

| Data | Type | Location |
|------|------|----------|
| Task envelope | `task.Task` (JSON struct) | `task/task.go` |
| Task args | JSON array of values | `client/client.go` — `json.Marshal(args)` |
| Task results | JSON-encoded single value | `task/registry.go` — `json.Marshal(results[0])` |
| DAG events | JSON `Event` struct | `workflow/events.go` |
| Step records | JSON `StepRecord` struct | `workflow/state.go` |
| DAG meta | JSON `DAGMeta` struct | `workflow/state.go` |
| DLQ entries | JSON `Entry` struct | `dlq/dlq.go` |
| Ref markers | JSON with `__ebind_ref__` sentinel | `workflow/ref.go` |
| CLI output | Pretty (tab-aligned) or JSON | `cmd/ebctl/internal/format/` |

---

## Monitoring & Observability

**Not bundled.** The library provides hooks:
- `worker.Metrics` interface — wire to Prometheus/OpenTelemetry via `worker.WithMetrics()` at `/home/pawel/repo/ebind/worker/middleware.go`
- `worker.Log` middleware — structured logging via `log/slog` at `/home/pawel/repo/ebind/worker/middleware.go`
- `worker.StepHook` — observe step completion/failure at `/home/pawel/repo/ebind/worker/hook.go`

No built-in metrics export, tracing, or health endpoints.

---

## Secrets & Environment

**Required env vars:**
- `EBIND_NATS_URL` — NATS server URL (optional, defaults to `nats://localhost:4222`)

**No secrets stored in repo.** NATS credentials are passed through the existing `nats.Connect` options (token, JWT, TLS certs) — the library does not manage them.

---

*Integration audit: 2026-06-03*
