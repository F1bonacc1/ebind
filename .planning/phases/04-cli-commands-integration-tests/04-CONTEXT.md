# Phase 4: CLI Commands + Integration Tests - Context

**Gathered:** 2026-06-03
**Status:** Ready for planning

<domain>
## Phase Boundary

CLI commands for pause/resume, dag ls status column updates for PAUSING/PAUSED, integration tests with embedded NATS, and race condition tests.

</domain>

<decisions>
## Implementation Decisions

### CLI command design
- **D-33:** `ebctl dag pause <id>` — no confirmation (pause/resume are reversible), output: `"paused <dag-id>"`
- **D-34:** `ebctl dag resume <id>` — no confirmation, output: `"resumed <dag-id>"`
- **D-35:** Both commands follow the same cobra pattern as cancel.go (ExactArgs(1), Ctx(), Workflow() call)

### dag ls status display
- **D-36:** PAUSING and PAUSED appear in the STATUS column alongside existing statuses
- **D-37:** `--status` filter help text updated to include `pausing|paused`
- **D-38:** PausedAt NOT shown in `dag ls` — available via `dag get` if needed later

### Integration tests
- **D-39:** Full E2E cycle: submit DAG → pause → in-flight finishes → verify paused → resume → verify remaining steps execute → verify DAG completes (TST-03)

### Race condition tests
- **D-40:** Concurrent Pause vs Resume — verify CAS resolves race to valid state
- **D-41:** Concurrent Pause vs Cancel — verify one wins, DAG ends canceled or paused
- **D-42:** Pause + step completion event — verify event acked but no dispatch
- **D-43:** Resume + concurrent completion event — verify scheduler gate + resume interact correctly

### the agent's Discretion
- Exact `dag get` output for PausedAt if shown in detailed view
- Error message handling for CLI (wrapping API errors)
- Test file organization (new test file vs existing integration_test.go)

</decisions>

<canonical_refs>
## Canonical References

### CLI patterns
- `cmd/ebctl/internal/commands/dag/cancel.go` — Template for pause/resume CLI commands
- `cmd/ebctl/internal/commands/dag/ls.go` — Status column display, --status filter, stepsSummary
- `cmd/ebctl/internal/commands/dag/dag.go` — Command registration (add new subcommands here)
- `cmd/ebctl/internal/cli/cli.go` — CLI context with Ctx(), Workflow(), Confirm(), Printer

### API functions
- `workflow/pause.go` — Pause() and Resume() functions

### Integration test patterns
- `workflow/integration_test.go` — E2E tests with embedded NATS, test harness patterns
- `internal/testutil/harness.go` — SingleNode() test harness setup
- `workflow/scheduler_test.go` — MemStore + captureEnq + toggleElector test patterns

### Requirements
- `.planning/REQUIREMENTS.md` — CLI-01 through CLI-04, TST-03, TST-04, TST-05

</canonical_refs>

<code_context>
## Existing Code Insights

### CLI cancel command (template)
- cobra.Command with ExactArgs(1), Short description, RunE
- c.Ctx() + defer cancel(), c.Workflow(ctx), c.Confirm(), call workflow func
- Output: c.Printer.Text(cmd.OutOrStdout(), fmt.Sprintf("canceled %s", args[0]))

### dag ls status display
- STATUS column at line 84: `string(m.Status)` — automatically shows any DAGStatus string
- --status filter at line 93: currently lists `running|done|failed|canceled` — add `pausing|paused`
- New filter values will automatically match since DAGStatusPausing = "pausing"

### Integration test patterns
- `integration_test.go` uses testutil.SingleNode(), h.wf, h.worker
- Tests submit DAGs, await completion, check meta status
- Existing pattern: `h.submitDAG()`, `h.worker.Run()`, `h.waitForCompletion()`

### Registration
- `dag/dag.go` line 18: `cmd.AddCommand(newCancelCmd(c))` — add pause+resume next to cancel

</code_context>

<deferred>
## Deferred Ideas

- PausedAt in dag get — can add when get command is updated
- --reason flag for pause/resume CLI — v2 enhancement

</deferred>

---

*Phase: 04-cli-commands-integration-tests*
*Context gathered: 2026-06-03*
