# Milestones

## v1.0 — Pause/Resume DAG Execution

**Shipped:** 2026-06-03
**Phases:** 4 | **Plans:** 8 | **Commits:** 66

**Key accomplishments:**

1. Added `DAGStatusPausing`/`DAGStatusPaused` constants, `PausedAt` audit field, pure state functions (`HasInFlightSteps`, `CanPause`, `CanResume`), updated `Terminal()`/`Cancel()`/`maybeFinalize` guards, and 13 pure unit tests
2. Implemented four scheduler pause gates — event entry guard, dispatch guard, pausing→paused auto-transition, and sweep recovery (including two critical bug fixes found during testing)
3. Built `Pause()` and `Resume()` API functions with 5-attempt CAS retry, sentinel errors (`ErrDAGNotRunning`, `ErrDAGNotPaused`), `EventResumed` constant, and comprehensive 8-test suite
4. Added `ebctl dag pause <id>` and `ebctl dag resume <id>` CLI commands, `dag ls --status` filter for `pausing|paused`, plus E2E integration tests, race condition tests, and CLI command tests

**Delivered:** Durable pause/resume for DAG workflows. Paused DAGs persist across restarts, consume zero CPU, and resume exactly where they left off — all on a single NATS dependency.

**Features:**
- State machine: DAGStatusPausing/Paused, HasInFlightSteps, CanPause/CanResume, PausedAt audit field
- Scheduler: Event entry gate, dispatch gate, auto-transition pausing→paused, sweep recovery
- API: Pause()/Resume() with CAS retry, sentinel errors
- CLI: ebctl dag pause/resume commands, dag ls status filter
- Tests: 13 unit tests, 7 scheduler tests, 8 API tests, E2E integration, race conditions, CLI tests

**File stats:** 39 files changed, +6,557 / -47 lines

---

*Last updated: 2026-06-08*
