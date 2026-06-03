# Technology Stack

**Analysis Date:** 2026-06-03

## Language

**Primary:**
- Go 1.26.1 (`go 1.26.1` in `go.mod`)
- Toolchain: `go1.26.3`
- Module path: `github.com/f1bonacc1/ebind`
- All source files across 12 packages and 2 commands

## Runtime

**Environment:**
- Compiled binary — no external runtime dependencies (single static binary)
- No interpreter or VM required

**Package Manager:**
- Go modules (`go.mod` + `go.sum`)
- Lockfile: `go.sum` present (3223 bytes)

## Build & Dev Tooling

**Build:**
- Standard `go build` (multi-package via `./...`)
- Makefile at `/home/pawel/repo/ebind/Makefile` — targets: `build`, `test`, `test-short`, `test-count`, `cover`, `lint`, `lint-fix`, `fmt`, `vet`, `tidy`, `demo`, `ebctl`, `ci`, `clean`
- `go build -o bin/ebctl ./cmd/ebctl` for the operator CLI

**CI:**
- GitHub Actions in `.github/workflows/ci.yml`:
  - Lint (`golangci-lint v2.11.3`)
  - Test (race + coverage via `make test`)
  - Build (compile all packages + smoke-test `ebctl`)
  - Vulnerability check (`govulncheck`)
  - PR title validation (conventional commits)
- Release pipeline: `.github/workflows/release.yml`
- CodeQL analysis: `.github/workflows/codeql.yml`
- Dependabot: `.github/dependabot.yml`

**Linting/Formatting:**
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

**Environment:**
- NATS connection URL via `EBIND_NATS_URL` env var (defaults to `nats://localhost:4222`) — `cmd/ebctl/internal/cli/cli.go`
- No other env vars required

**Build-time configuration:**
- `go.mod` — Go version, module path, dependency versions
- `.golangci.yaml` — lint rules

**Runtime configuration:**
- `worker.Options` — concurrency, backoff, middleware, claims, retry policy
- `stream.Config` — replicas, max ages, duplicate window
- `workflow.Workflow` — store, bus, enqueuer, elector, sweep timing
- `embed.NodeConfig` / `embed.ClusterConfig` — server name, host, port, store dir, ready wait

## Platform Requirements

**Development:**
- Go 1.26.1+
- Access to NATS (embedded or external)
- `golangci-lint` v2 for linting

**Production:**
- Single binary deployment
- Embedded NATS requires writable store directory (`StoreDir`)
- No external database or service dependencies beyond NATS itself
- 3-node cluster mode requires 3 loopback or routable addresses

---

*Stack analysis: 2026-06-03*
