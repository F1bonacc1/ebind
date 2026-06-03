---
phase: 04-cli-commands-integration-tests
plan: 01
subsystem: cli
tags: [cobra, cli, pause, resume, dag]

requires:
  - phase: 03-workflow-api
    provides: workflow.Pause() and workflow.Resume() API functions
provides:
  - ebctl dag pause &lt;id&gt; CLI command
  - ebctl dag resume &lt;id&gt; CLI command
  - dag ls --status filter support for pausing|paused
affects: [04-cli-commands-integration-tests]

tech-stack:
  added: []
  patterns:
    - "CLI commands follow cancel.go cobra pattern: ExactArgs(1), no confirmation for reversible ops"
    - "Unexported factory functions (newPauseCmd, newResumeCmd) registered via dag.go"

key-files:
  created:
    - cmd/ebctl/internal/commands/dag/pause.go
    - cmd/ebctl/internal/commands/dag/resume.go
  modified:
    - cmd/ebctl/internal/commands/dag/dag.go
    - cmd/ebctl/internal/commands/dag/ls.go

key-decisions:
  - "No c.Confirm() for pause/resume — both are reversible operations (D-33, D-34)"
  - "Pause registered before resume alphabetically, after cancel (semantic neighbor)"

patterns-established:
  - "New CLI subcommands follow cancel.go pattern: new*Cmd factory, ExactArgs(1), Ctx()+Workflow(), Printer.Text output"

requirements-completed: [CLI-01, CLI-02, CLI-03, CLI-04]

duration: 8min
completed: 2026-06-03
---

# Phase 04 Plan 01: Pause/Resume CLI Commands Summary

**ebctl dag pause &lt;id&gt; and dag resume &lt;id&gt; CLI commands following cancel.go pattern, with dag ls --status filter updated for pausing|paused**

## Performance

- **Duration:** 8 min
- **Started:** 2026-06-03T22:30:00Z
- **Completed:** 2026-06-03T22:38:00Z
- **Tasks:** 3
- **Files modified:** 4

## Accomplishments

- `ebctl dag pause <id>` command — calls `workflow.Pause()`, outputs "paused <id>", no confirmation needed
- `ebctl dag resume <id>` command — calls `workflow.Resume()`, outputs "resumed <id>", no confirmation needed
- Both commands use `ExactArgs(1)`, `Ctx()`, `Workflow()` following the existing cancel.go pattern
- `dag ls --status` filter help text includes `pausing|paused` — no rendering change needed as STATUS column already displays any DAGStatus value

## Task Commits

Each task was committed atomically:

1. **Task 1: Create pause.go and resume.go** - `73c99ae` (feat)
2. **Task 2: Register pause and resume commands in dag.go** - `76b9a5b` (feat)
3. **Task 3: Update dag ls --status filter help text** - `83cd73e` (feat)

## Files Created/Modified

- `cmd/ebctl/internal/commands/dag/pause.go` (new, 30 lines) — `newPauseCmd` cobra command, calls `workflow.Pause()`
- `cmd/ebctl/internal/commands/dag/resume.go` (new, 30 lines) — `newResumeCmd` cobra command, calls `workflow.Resume()`
- `cmd/ebctl/internal/commands/dag/dag.go` (modified) — registers `newPauseCmd` and `newResumeCmd` after cancel
- `cmd/ebctl/internal/commands/dag/ls.go` (modified) — `--status` filter help updated to include `pausing|paused`

## Decisions Made

- **No Confirm() for pause/resume** — both operations are reversible (a paused DAG can be resumed, a resumed DAG can be paused again). This differs from `cancel` which requires confirmation. (D-33, D-34)
- **Registration order** — `pause` before `resume` alphabetically, both after `cancel` as the closest semantic neighbor
- **Output format** — `"paused <id>"` / `"resumed <id>"` following the `"canceled <id>"` precedent from cancel.go

## Deviations from Plan

None — plan executed exactly as written.

## Issues Encountered

None.

## Threat Surface Scan

All four files (pause.go, resume.go, dag.go, ls.go) reference existing `workflow.Pause`/`workflow.Resume` API functions. No new network endpoints, auth paths, file access patterns, or schema changes introduced. The CLI commands pass user-provided dagID directly to the existing workflow API — threat disposition from the plan (T-04-01 accept, T-04-02 accept, T-04-03 accept) remains accurate with no new surface.

## User Setup Required

None — no external service configuration required.

## Next Phase Readiness

- CLI commands are ready for integration testing in 04-02
- workspace.Pause/Resume API functions are the only dependency — ensure they are implemented and exported from the workflow package

---

## Self-Check: PASSED

- [x] `cmd/ebctl/internal/commands/dag/pause.go` created (30 lines)
- [x] `cmd/ebctl/internal/commands/dag/resume.go` created (30 lines)
- [x] `cmd/ebctl/internal/commands/dag/dag.go` modified (newPauseCmd + newResumeCmd registered)
- [x] `cmd/ebctl/internal/commands/dag/ls.go` modified (status filter updated)
- [x] Commit `73c99ae` exists — Task 1
- [x] Commit `76b9a5b` exists — Task 2
- [x] Commit `83cd73e` exists — Task 3
- [x] `go build ./...` passes
- [x] `go vet ./...` passes

---

*Phase: 04-cli-commands-integration-tests*
*Completed: 2026-06-03*
