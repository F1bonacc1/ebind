# Research Summary: Pause/Resume for ebind DAG Engine

**Domain:** Go DAG workflow engine pause/resume architecture
**Researched:** 2026-06-03
**Overall confidence:** HIGH

## Executive Summary

Pause/resume is an architectural feature that must integrate cleanly into ebind's existing event-driven scheduler with CAS-based state, leader-elected failover, and a pure in-memory state machine. The research confirms the approach from PROJECT.md is sound: two new DAG-level statuses (`pausing`, `paused`) following the existing lowercase convention, with no changes to step-level statuses.

The critical architectural finding is that pause is **not cancel**. Cancel is immediate and terminal (pending→canceled, runs to completion with side effects ignored). Pause is gradual and non-terminal (in-flight steps complete naturally, pending steps remain pending). This fundamental difference means pause/resume cannot reuse Cancel's code path — it needs its own scheduler gates, its own CAS transitions, and its own recovery path in the sweep on leader failover.

The pausing→paused transition is the hardest problem. It must be distributed and event-driven: the scheduler detects "last running step finished" on a completion event and CAS-transitions the meta from pausing to paused. The sweep on leader acquisition provides failover recovery. CAS contention with concurrent Resume is resolved by one-wins semantics — the user's explicit Resume intention naturally wins over the auto-pausing-transition.

## Key Findings

| Area | Finding |
|------|---------|
| **DAG statuses** | Two new: `pausing` (transient, in-flight draining), `paused` (idle, no in-flight, no dispatch) |
| **Step statuses** | Unchanged — pending stays pending, running stays running |
| **Scheduler gates** | Three: event entry (Ack-if-paused/pausing), dispatch (skip-if-not-running), sweep (handle pausing→paused) |
| **pausing→paused** | Triggered by scheduler on last completion event; failsafe via sweep on leader acquisition |
| **Resume** | CAS meta→running + direct enqueue of ReadyToRun steps (bypasses scheduler for low latency) |
| **CAS model** | Same retry-until-deadline pattern as Cancel — 5 attempts, ErrStaleRevision on exhaustion |
| **Failover** | Sweep handles pausing→paused recovery; sweep skips paused DAGs (zero CPU) |
| **Testing** | MemStore + captureEnq + toggleElector pattern works unchanged; need ~12 new test cases across state/scheduler/integration |

## Implications for Roadmap

Based on research, suggested phase structure:

1. **State Machine & Pure Logic** — Add `pausing`/`paused` constants, `HasInFlightSteps()`, `CanPause()`, `CanResume()` pure functions, update `maybeFinalize`. Zero IO. Pure unit tests.
2. **Pause API + CLI** — `Pause()` function with CAS meta→pausing, `ebctl dag pause`, CLI display updates. Testable with captureEnq.
3. **Scheduler Pause Awareness** — Three scheduler gates, pausing→paused auto-transition in `handleEvent`, sweep recovery. Integration tests.
4. **Resume API + CLI** — `Resume()` function with CAS→running + direct enqueue, `ebctl dag resume`. Full end-to-end integration tests.

**Phase ordering rationale:**
- Pure logic must exist before anything else (Phase 1)
- Pause must exist before scheduler can react to it (Phase 2 before 3)
- Pause must work fully before resume makes sense (Phase 3 before 4)
- CLI display for pausing/paused needs the statuses (Phase 2 before 4)
- Some CLI display work can overlap with Phase 2

**Research flags for phases:**
- Phase 3: The pausing→paused race with Resume needs careful design review. Both CAS on meta with the same initial revision — the winner determines whether the DAG goes running or paused. This is correct by CAS semantics but the "loser" sees a benign error that must be handled gracefully.
- Phase 4: Resume's direct enqueue of ready steps outside scheduler's `s.mu` is a new concurrent access pattern. CAS + dedupe provides safety, but needs explicit stress testing.

## Confidence Assessment

| Area | Confidence | Notes |
|------|------------|-------|
| Stack | HIGH | No new dependencies needed — all changes are in `workflow/` package and `cmd/ebctl/`. Cross-engine research (STACK.md) confirmed status-field pattern (Prefect, Argo, Airflow). Temporal's signal-based approach is inapplicable to ebind's declarative DAG model. |
| Features | HIGH | Requirements well-defined in PROJECT.md. No ambiguity. |
| Architecture | HIGH | Design is a natural extension of Cancel pattern + scheduler gates. Three-gate model is comprehensive. |
| Pitfalls | MEDIUM | The pausing→paused race window and 5-retry CAS ceiling are known concerns from existing Cancel implementation. These apply equally. |

## Gaps to Address

- **5-retry CAS ceiling** — CONCERNS.md identifies Cancel's 5-retry as fragile under contention. Pause/Resume inherits this. Should the ceiling be increased or should a write-through pattern be used? Mitigation: document that pause/resume under heavy concurrent Cancel/Resume/Scheduler contention may fail with ErrStaleRevision. Recommend increasing retries or using exponential backoff if this becomes an operational issue.
- **pausing→paused if all steps terminal** — Edge case: user pauses a DAG where the last running step's completion triggers the pausing→paused transition, but all steps are now terminal. Should the DAG auto-finalize to done/failed or stay paused? Research recommends: **stay paused** (user paused it for a reason). They can cancel or delete explicitly.
- **Dynamic step addition during pause** — Confirmed safe: step stays pending, dispatches on resume. But should we warn users? Mitigation: document the behavior.
- **No wire-format changes** — Because we're only changing DAG meta status (a string field) and adding no new step-level fields, existing in-flight tasks and stored DAGs are fully backward compatible. Old DAGs without pause support (status=running only) continue to work.

---

## Files Created

| File | Purpose |
|------|---------|
| `.planning/research/ARCHITECTURE.md` | Full architecture design: state machine, scheduler gates, CAS matrix, testing strategy, build order |
| `.planning/research/STACK.md` | Stack assessment — no new dependencies |
| `.planning/research/FEATURES.md` | Feature breakdown with phase mapping |
| `.planning/research/PITFALLS.md` | Domain pitfalls with mitigations |
| `.planning/research/SUMMARY.md` | This file — executive summary and roadmap implications |
