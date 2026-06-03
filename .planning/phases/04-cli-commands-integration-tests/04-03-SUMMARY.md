---
phase: 04-cli-commands-integration-tests
plan: 03
subsystem: cli, testing
tags: [cobra, integration-test, pausing, paused, workflow, cli]
requires:
  - phase: 04-01
    provides: pause.go, resume.go CLI commands
  - phase: 04-02
    provides: pause.go, resume.go dag command registration
provides:
  - CLI integration tests for dag pause and resume commands
  - CLI integration tests for invalid state transitions
  - CLI integration test for dag ls --status pausing|paused filter
affects: []
tech-stack:
  added: []
  patterns:
    - "CLI pause/resume tests use cobra in-process execution via cmddag.NewCmd(c).Execute()"
    - "DAG state seeded directly via wf.Store.PutMeta() in embedded NATS harness"
    - "Invalid state tests assert error contains both dagID and current status"
key-files:
  created: []
  modified:
    - cmd/ebctl/ebctl_integration_test.go
key-decisions:
  - "Added 'fmt' import and renamed shadowed local variable 'fmt' to 'v' in jsonRowsContain helper to accommodate fmt.Sprintf usage"
patterns-established:
  - "CLI pause/resume tests follow the same pattern as existing TestDagLsRmAndDlqFlow: testutil.SingleNode, newTestCtx, wf.Store.PutMeta to seed state, cmddag.NewCmd, bytes.Buffer for output capture, dagRoot.Execute()"
  - "Invalid state tests check both dagID and status string in error message via strings.Contains"
requirements-completed: [TST-05]
duration: 5min
completed: 2026-06-03
---

# Phase 04 Plan 03: CLI Command Integration Tests for Pause/Resume

**CLI command integration tests for pause/resume output formatting (`"paused <id>"`, `"resumed <id>"`), invalid state transitions, and `dag ls --status pausing|paused` filter**

## Performance

- **Duration:** 5 min
- **Completed:** 2026-06-03
- **Tasks:** 1
- **Files modified:** 1

## Accomplishments

- `TestDagPause` — verifies `ebctl dag pause <id>` outputs `"paused <id>"` and transitions running DAG to pausing/paused
- `TestDagResume` — verifies `ebctl dag resume <id>` outputs `"resumed <id>"` and transitions paused DAG back to running
- `TestDagPause_InvalidState` — verifies pause on a done DAG returns error containing dagID and "done" status
- `TestDagResume_InvalidState` — verifies resume on a running DAG returns error containing dagID and "running" status
- `TestDagLs_StatusFilterPaused` — verifies `--status paused` filter correctly includes paused DAGs and excludes running DAGs; unfiltered `dag ls` includes both

## Task Commits

Each task was committed atomically:

1. **Task 1: Add CLI command tests for pause/resume and dag ls filter** - `bfcc621` (test)

**Plan metadata:** (separate commit after state update)

## Files Created/Modified

- `cmd/ebctl/ebctl_integration_test.go` - Added 5 test functions for pause/resume and dag ls status filter; added `"fmt"` import; renamed shadowed local variable `fmt` to `v` in `jsonRowsContain`

## Decisions Made

- Added `"fmt"` import and renamed the shadowed local variable `fmt` in `jsonRowsContain` to `v` to avoid package shadowing conflict. The plan incorrectly stated `fmt` was already imported — it was used as a short variable name in the helper instead.

## Deviations from Plan

### Auto-fixed Issues

**1. [Rule 3 - Blocking] Added missing `"fmt"` import and renamed shadowed local variable**
- **Found during:** Task 1 (Add CLI command tests)
- **Issue:** The plan stated `"fmt"` was already imported, but it was not. The `jsonRowsContain` helper used `fmt` as a local variable name (`if fmt, ok := r[key]; ...`), which would conflict with importing the `fmt` package.
- **Fix:** Added `"fmt"` import after `"errors"` in the import block. Renamed the local variable `fmt` to `v` in `jsonRowsContain` to avoid shadowing the imported package.
- **Files modified:** `cmd/ebctl/ebctl_integration_test.go`
- **Verification:** `go build ./cmd/ebctl/...` passes, `go vet ./cmd/ebctl/...` passes
- **Committed in:** `bfcc621` (Task 1 commit)

---

**Total deviations:** 1 auto-fixed (1 blocking)
**Impact on plan:** Necessary fix to make the code compile. The test functions use `fmt.Sprintf` for expected output formatting.

## Issues Encountered

- `fmt` was not imported despite the plan stating it was — the existing `jsonRowsContain` helper used `fmt` as a local variable name, creating a shadowing conflict. Resolved by adding the import and renaming the variable.

## Known Stubs

None — all tests use real embedded NATS instances and exercise actual `Pause()`, `Resume()` API calls and `dag ls` command logic.

## Threat Flags

None — the only modified file is a test file. No new network endpoints, auth paths, or trust boundary changes introduced.

## Next Phase Readiness

- All CLI pause/resume commands have end-to-end integration tests
- dag ls `--status` filter tested with pausing/paused status values
- Ready for any follow-up work on CLI integration

---

## Self-Check: PASSED

- [x] `TestDagPause` — pause command outputs "paused \<id\>" and status transitions
- [x] `TestDagResume` — resume command outputs "resumed \<id\>" and status transitions
- [x] `TestDagPause_InvalidState` — error contains dagID and status
- [x] `TestDagResume_InvalidState` — error contains dagID and status
- [x] `TestDagLs_StatusFilterPaused` — `--status paused` filters correctly
- [x] All tests pass with `-race` detector: `go test ./cmd/ebctl/... -run TestDag -count=1 -race -timeout 120s` exits 0
- [x] `go build ./cmd/ebctl/...` passes
- [x] `go vet ./cmd/ebctl/...` passes
- [x] Existing short tests still pass: `go test ./cmd/ebctl/... -count=1 -race -short -timeout 120s` exits 0
- [x] Commit `bfcc621` exists
- [x] SUMMARY.md created at `.planning/phases/04-cli-commands-integration-tests/04-03-SUMMARY.md`

*Phase: 04-cli-commands-integration-tests*
*Completed: 2026-06-03*
