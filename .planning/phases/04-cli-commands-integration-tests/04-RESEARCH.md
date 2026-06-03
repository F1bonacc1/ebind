# Phase 4: CLI Commands + Integration Tests — Research

**Researched:** 2026-06-03
**Confidence:** HIGH (validated against running code + existing test patterns)

---

## Current Code State

Phases 1-3 completed and committed. All pause/resume infrastructure is in place:

- `workflow/state.go` — `DAGStatusPausing` and `DAGStatusPaused` constants, `CanPause()`, `CanResume()`, `HasInFlightSteps()`, `PausedAt` field
- `workflow/cancel.go` — Cancel handles pausing/paused states
- `workflow/scheduler.go` — Event entry gate (SG-01), dispatch gate (SG-02), pausing→paused auto-transition (SG-04), sweep recovery (SG-03)
- `workflow/pause.go` — `Pause(ctx, wf, dagID) error` and `Resume(ctx, wf, dagID) error` with CAS retry, sentinel errors
- `workflow/errors.go` — `ErrDAGNotRunning`, `ErrDAGNotPaused` sentinels
- `workflow/events.go` — `EventResumed EventKind = "resumed"` constant
- `workflow/pause_api_test.go` — 8 Pause/Resume API unit tests
- `workflow/scheduler_test.go` — 7 scheduler pause tests

## CLI Code (cmd/ebctl/) — Exact Line References

### cancel.go — Template for pause/resume (32 lines)

| Line | Pattern | Description |
|------|---------|-------------|
| 12 | `func newCancelCmd(c *cli.Context) *cobra.Command` | Factory function pattern |
| 13 | `return &cobra.Command{Use: "cancel <dag-id>", Short: ..., Args: cobra.ExactArgs(1), RunE: ...}` | Command definition |
| 18 | `ctx, cancel := c.Ctx()` | Timeout-bounded context |
| 20 | `wf, err := c.Workflow(ctx)` | Lazy workflow construction |
| 24 | `if err := c.Confirm(...); err != nil { return err }` | **NOT used for pause/resume** (D-33, D-34 — no confirmation) |
| 27 | `workflow.Cancel(ctx, wf, args[0])` | Direct API call |
| 30 | `c.Printer.Text(cmd.OutOrStdout(), fmt.Sprintf("canceled %s", args[0]))` | Output format |

**Key difference for pause/resume:** Skip the `c.Confirm()` call (per D-33, D-34). Output: `"paused <dag-id>"` and `"resumed <dag-id>"`.

### dag.go — Command Registration (23 lines)

| Line | Pattern | Phase 4 Change |
|------|---------|----------------|
| 15-21 | `cmd.AddCommand(newLsCmd(c))`, `cmd.AddCommand(newCancelCmd(c))` | Add `cmd.AddCommand(newPauseCmd(c))` and `cmd.AddCommand(newResumeCmd(c))` |

### ls.go — Status Display

| Line | Current | Phase 4 Change |
|------|---------|----------------|
| 84 | `string(m.Status)` | **No change needed** — automatically shows "pausing"/"paused" since DAGStatusPausing/Paused are already string constants |
| 93 | `"filter by status: running\|done\|failed\|canceled"` | Update to `"filter by status: running\|done\|failed\|canceled\|pausing\|paused"` |

The `string(m.Status)` call at line 84 uses Go's string conversion on the `DAGStatus` (which is `type DAGStatus string`). Both `DAGStatusPausing` ("pausing") and `DAGStatusPaused` ("paused") already exist as constants, so they display automatically. **No code change needed in the Status rendering**, only the `--status` filter help text at line 93.

### cli.go — Context & Wiring (171 lines)

| Pattern | Line | Used By |
|---------|------|---------|
| `c.Ctx()` | 48-51 | All commands — creates timeout context |
| `c.Workflow(ctx)` | 60-70 | All commands — lazy workflow init |
| `c.Confirm(prompt)` | 75-90 | Destructive ops — **NOT used for pause/resume** (D-33, D-34) |
| `c.Printer.Text(w, s)` | Used inline | Output formatting — pause/resume output confirmation |

## Integration Test Patterns

### wfHarness (workflow/integration_test.go)

The existing `wfHarness` at line 42-106 provides:
- `setup(t)` — creates embedded NATS node, JetStream streams, workflow, worker, scheduler
- Handlers: `hAdd`, `hDouble`, `hConcat`, `hFailMandatory`, `hFailOptional`
- Shared ordering observers for concurrency testing (`orderMu`, `orderSeen`)

**E2E test pattern** (from existing cancel/rm integration tests):
```go
func TestPauseResumeE2E(t *testing.T) {
    h := setup(t)
    // Register handlers
    task.MustRegister(h.reg, hAdd)
    task.MustRegister(h.reg, hDouble)
    // Build a DAG with multiple steps
    dag := workflow.New()
    dag.Step("a", hAdd, task.WithArgs(1, 2))
    dag.Step("b", hDouble, task.Ref("a"), task.WithArgs(1))
    dag.Step("c", hConcat, task.Ref("b"), task.WithArgs("result", 1))
    // Submit
    ctx := context.Background()
    _, err := dag.Submit(ctx, h.wf)
    // Resume provides the "running" starting state for pause tests
    // Pause → complete in-flight → verify paused → resume → verify completion
}
```

### testutil.SingleNode (internal/testutil/harness.go)

For CLI tests, `testutil.SingleNode(t, opts)` at line 31 provides:
- NATS embedded server + JetStream + streams
- Worker with `StepHook` integration
- Client for task enqueue

**CLI test pattern** (from ebctl_integration_test.go):
```go
h := testutil.SingleNode(t, worker.Options{Concurrency: 1})
c := newTestCtx(t, h, "json")

dagRoot := cmddag.NewCmd(c)
var buf bytes.Buffer
dagRoot.SetOut(&buf)
dagRoot.SetArgs([]string{"ls"})
if err := dagRoot.Execute(); err != nil { t.Fatal(err) }
```

## Pause/Resume API Reference

### Pause(ctx, wf, dagID) — workflow/pause.go:18-50

```
Behavior:
- 5-attempt CAS retry loop
- If no in-flight steps → running→paused directly (sets PausedAt)
- If in-flight steps → running→pausing (scheduler auto-transitions to paused)
- Returns ErrDAGNotRunning if status is not running (including pausing/paused)
- Returns ErrStaleRevision after 5 CAS failures

Input: context.Context, *Workflow, string (dagID)
Output: error (nil on success, sentinel on invalid transition)
```

### Resume(ctx, wf, dagID) — workflow/pause.go:59-89

```
Behavior:
- 5-attempt CAS retry loop
- Transition paused→running
- Publishes EventResumed on bus → scheduler re-evaluates ready steps
- Returns ErrDAGNotPaused if status is not paused (including running/pausing)
- Returns ErrStaleRevision after 5 CAS failures

Input: context.Context, *Workflow, string (dagID)
Output: error (nil on success, sentinel on invalid transition)
```

### Sentinel Errors

| Error | Condition |
|-------|-----------|
| `ErrDAGNotRunning` | Pause on done/failed/canceled/pausing/paused |
| `ErrDAGNotPaused` | Resume on running/pausing/done/failed/canceled |
| `ErrStaleRevision` | CAS exhausted 5 retries |

Both API functions wrap errors with dagID and current status:
```
"ebind: cannot pause DAG <id>: status is <status>: %w"
"ebind: cannot resume DAG <id>: status is <status>: %w"
```

## CLI Design Per User Decisions

### pause.go — Command Design
```
Use:   "pause <dag-id>"
Short: "Pause a running DAG (in-flight steps finish, pending stay pending)"
Args:  cobra.ExactArgs(1)
RunE:
  ctx, cancel := c.Ctx()       // timeout context
  defer cancel()
  wf, err := c.Workflow(ctx)   // lazy workflow
  if err != nil { return err }
  if err := workflow.Pause(ctx, wf, args[0]); err != nil {
    return err                  // cobra displays the wrapped error
  }
  return c.Printer.Text(cmd.OutOrStdout(), fmt.Sprintf("paused %s", args[0]))
```

Note: No `c.Confirm()` per D-33. Pause/resume are reversible.

### resume.go — Command Design
```
Use:   "resume <dag-id>"
Short: "Resume a paused DAG (pending steps will be enqueued)"
Args:  cobra.ExactArgs(1)
RunE:
  ctx, cancel := c.Ctx()
  defer cancel()
  wf, err := c.Workflow(ctx)
  if err != nil { return err }
  if err := workflow.Resume(ctx, wf, args[0]); err != nil {
    return err
  }
  return c.Printer.Text(cmd.OutOrStdout(), fmt.Sprintf("resumed %s", args[0]))
```

## Test Plan

### E2E Integration Test (TST-03, D-39)

Full cycle in `workflow/integration_test.go`:

| Step | Action | Expected State |
|------|--------|----------------|
| 1 | Submit a→b DAG (a: hAdd(1,2)→3, b: hDouble(3)→6) | a running, b pending |
| 2 | Wait for step a to deliver (consume it) | a done, b running |
| 3 | Call `workflow.Pause(ctx, h.wf, dagID)` | DAG → pausing |
| 4 | Wait for step b to complete (in-flight) | a done, b done |
| 5 | Verify meta status | DAGStatusPaused |
| 6 | Call `workflow.Resume(ctx, h.wf, dagID)` | DAG → running, EventResumed published |
| 7 | Wait for scheduler to re-enqueue and deliver remaining steps | all steps done |
| 8 | Verify DAG completed | DAGStatusDone |

**Infrastructure:** Uses `setup()` from existing `integration_test.go`. Wait for step delivery via polling worker/step status. The scheduler auto-transition (SG-04) handles step 4→5.

### Race Condition Tests (TST-04, D-40 through D-43)

All in `workflow/integration_test.go` or a new `workflow/race_test.go`:

**1. TestPauseResume_Race_PauseVsResume (D-40)**
- Start a DAG with a blocking handler (blockedWhere pattern)
- Spawn goroutines: `workflow.Pause()` and `workflow.Resume()`
- Verify DAG ends in valid state (running or paused — CAS one-wins)
- Both can succeed? No — Pause transitions running→pausing, Resume transitions paused→running. CAS one-wins, the other gets ErrStaleRevision (which already returns wrapped sentinel). Acceptable.

Wait, actually this race is more nuanced. If both run concurrently:
- Resume on a running DAG returns ErrDAGNotPaused (status is running)
- Pause on a running DAG succeeds (transitions to pausing)
- If Resume happens after Pause put paused→running, then Pause fails

The test should verify the DAG ends in a valid state (running or pausing/paused, not corrupted).

**2. TestPauseResume_Race_PauseVsCancel (D-41)**
- Start DAG with blocking handler
- Spawn goroutines: `workflow.Pause()` and `workflow.Cancel()`
- Verify DAG ends in running→pausing/paused OR canceled (CAS one-wins)
- Cast: Pause works on running, Cancel works on running/pausing/paused
- Acceptable outcomes: pausing, paused, or canceled

**3. TestPauseResume_Race_PauseAndStepComplete (D-42)**
- Start DAG with blocking handler
- Call Pause (→pausing), release handler to complete step
- Wait for auto-transition → paused
- Verify no new dispatches happen (the remaining pending step stays pending)

**4. TestPauseResume_Race_ResumeAndComplete (D-43)**
- Start DAG, pause it, verify paused
- Call Resume (→running, EventResumed published)
- Immediately complete the remaining in-flight step
- Verify scheduler gate + resume interact correctly (either step completes normally or is re-enqueued)

### CLI Command Tests (TST-05)

In `cmd/ebctl/ebctl_integration_test.go`:

**1. TestDagPause (CLI-01, CLI-04)**
- Seed a DAG with status=Running via `wf.Store.PutMeta()`
- Run `ebctl dag pause <id>` via `cmddag.NewCmd(c)`
- Verify output: `"paused <dag-id>"`
- Verify DAG status changed to pausing/paused

**2. TestDagResume (CLI-02, CLI-04)**
- Seed a DAG with status=Paused via `wf.Store.PutMeta()`
- Run `ebctl dag resume <id>` via cobra
- Verify output: `"resumed <dag-id>"`
- Verify DAG status changed to running

**3. TestDagPause_InvalidState (CLI-04)**
- Seed DAGs with done/failed/canceled status
- Run `ebctl dag pause <id>`
- Verify error output contains dagID and current status (from wrapped API error)

**4. TestDagResume_InvalidState (CLI-04)**
- Seed DAGs with running/done/failed/canceled/pausing status
- Run `ebctl dag resume <id>`
- Verify error output contains dagID and current status

**5. TestDagLs_StatusFilter (CLI-03)**
- Seed DAGs with pausing and paused statuses
- Run `ebctl dag ls --status pausing`
- Verify the pausing DAG appears
- Run `ebctl dag ls --status paused`
- Verify the paused DAG appears

## Summary

Phase 4 touches:
1. **New files:**
   - `cmd/ebctl/internal/commands/dag/pause.go` (~30 lines) — Pause CLI command
   - `cmd/ebctl/internal/commands/dag/resume.go` (~30 lines) — Resume CLI command

2. **Modified files:**
   - `cmd/ebctl/internal/commands/dag/dag.go` (+2 lines) — Register pause + resume commands
   - `cmd/ebctl/internal/commands/dag/ls.go` (+0/-1 lines) — Update `--status` filter help text
   - `workflow/integration_test.go` (+~150-200 lines) — E2E integration test + race condition tests
   - `cmd/ebctl/ebctl_integration_test.go` (+~150-200 lines) — CLI command tests
