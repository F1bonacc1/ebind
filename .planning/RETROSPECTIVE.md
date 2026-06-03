# Project Retrospective

*A living document updated after each milestone. Lessons feed forward into future planning.*

## Milestone: v1.0 — Pause/Resume DAG Execution

**Shipped:** 2026-06-03
**Phases:** 4 | **Plans:** 8 | **Sessions:** 1 (contiguous ~1.5h execution)

### What Was Built
- State machine: DAGStatusPausing/Paused, HasInFlightSteps, CanPause/CanResume, PausedAt audit field
- Scheduler: Event entry gate, dispatch gate, auto-transition pausing→paused, sweep recovery
- API: Pause()/Resume() with 5-attempt CAS retry, sentinel errors, EventResumed
- CLI: ebctl dag pause/resume, dag ls --status filter for pausing|paused
- Tests: 13 unit + 7 scheduler + 8 API + E2E integration + race conditions + CLI command tests

### What Worked
- Research-driven phase ordering (State→Scheduler→API→CLI) proved correct — no rework needed
- Yolo mode with auto-approve kept momentum high through 4 phases in a single session
- Plan-generated test code surfaced 3 bugs in the implementation (handleEvent gate, sweep auto-finalize, NATS subject validity)
- All 23 v1 requirements shipped as specified — no scope creep or dropped items

### What Was Inefficient
- Plan code blocks sometimes had API inaccuracies (non-existent functions, wrong signatures) requiring auto-fixes during execution
- Project docs were manually initialized rather than using `/gsd-new-milestone` — some STATE.md fields became stale

### Patterns Established
- **Auto-fix discovery**: Running tests from plans surfaces implementation bugs the plan author couldn't anticipate
- **Blocking handlers for timing**: E2E tests for pause scenarios need blocking handlers + 3-step DAGs for deterministic timing
- **Event gate precision**: handleEvent must distinguish event types (EventCompleted passes for pausing DAGs, EventStepAdded is gated)

### Key Lessons
1. Write test plans with the exact API surface — plan-provided test code had non-existent APIs (task.WithArgs, task.Ref) that needed correction
2. Fast arithmetic handlers complete before pause can be called — use blocking handlers + multi-step DAGs for deterministic timing in pause tests
3. state.Terminal() returns false for paused meta status by design — sweep auto-finalize must check step terminality directly

### Cost Observations
- Model mix: 100% claude (single session, no model switching)
- Sessions: 1 contiguous execution session (~1.5h)
- Notable: All 4 phases executed consecutively in ~1.5h with zero rework — research-driven phase ordering was the key efficiency driver

---

## Cross-Milestone Trends

### Process Evolution

| Milestone | Sessions | Phases | Key Change |
|-----------|----------|--------|------------|
| v1.0 | 1 | 4 | Initial milestone — yolo mode, discuss→plan→execute per phase |

### Cumulative Quality

| Milestone | Test Functions | Zero-Dep Additions |
|-----------|---------------|-------------------|
| v1.0 | 30+ new tests (13 unit + 7 scheduler + 8 API + E2E + race + CLI) | 0 |

### Top Lessons (Verified Across Milestones)

1. *(First milestone — no cross-milestone data yet)*

---

*Last updated: 2026-06-08*
