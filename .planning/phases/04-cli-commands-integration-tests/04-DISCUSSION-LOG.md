# Phase 4: CLI Commands + Integration Tests - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-06-03
**Phase:** 04-cli-commands-integration-tests
**Areas discussed:** CLI design, ls display, integration tests, race condition tests

---

## CLI confirmation

| Option | Description | Selected |
|--------|-------------|----------|
| No confirmation | Pause/resume are reversible | ✓ |
| Require confirmation | Consistent with cancel | |

**User's choice:** No confirmation
**Notes:** —

---

## CLI output format

| Option | Description | Selected |
|--------|-------------|----------|
| Match cancel output | "paused <id>" / "resumed <id>" | ✓ |
| More verbose | Include status transition | |

**User's choice:** Match cancel output
**Notes:** —

---

## dag ls STATUS display

| Option | Description | Selected |
|--------|-------------|----------|
| PAUSING/PAUSED in STATUS + filter update | Show in status column, add to --status filter help | ✓ |
| Just STATUS column | Don't update filter | |
| Separate column | New column for pause status | |

**User's choice:** STATUS column + filter update
**Notes:** —

---

## PausedAt in dag ls

| Option | Description | Selected |
|--------|-------------|----------|
| Don't show in ls | dag get can show details | ✓ |
| Separate PAUSED column | New column when status is paused | |
| Show in AGE column | Replace age with paused duration | |

**User's choice:** Don't show in ls
**Notes:** —

---

## Integration test: E2E cycle

| Option | Description | Selected |
|--------|-------------|----------|
| Full pause→complete→resume→done | Submit, pause, in-flight finishes, verify paused, resume, verify complete | ✓ |
| Pause then cancel | Pause running DAG then cancel | Not selected |
| Pause with in-flight steps | Verify in-flight complete, pending stay pending | Not selected |
| Resume all-done | Resume with zero ready steps | Not selected |
| Rapid toggle cycle | Pause→resume→pause→resume | Not selected |

**User's choice:** Full E2E cycle only
**Notes:** User selected only the full cycle test from integration options

## Race condition tests

| Option | Description | Selected |
|--------|-------------|----------|
| Concurrent Pause vs Resume | CAS resolves to valid state | ✓ |
| Concurrent Pause vs Cancel | One wins, DAG ends canceled or paused | ✓ |
| Pause + step completion event | Event acked but no dispatch | ✓ |
| Resume + concurrent completion event | Scheduler gate + resume interact correctly | ✓ |

**User's choice:** All 4 race condition tests
**Notes:** Selected "all"

## the agent's Discretion

- Error message handling for CLI
- Test file organization
- Exact dag get output

## Deferred Ideas

- PausedAt in dag get — can add later
- --reason flag for pause/resume CLI — v2

