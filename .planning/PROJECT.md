# ebind

## What This Is

ebind is a Go library that provides a task queue and DAG workflow engine over NATS JetStream. It enables durable, multi-step workflows with step dependencies, retries, and dynamic step addition — with no external dependencies beyond NATS itself. Applications embed ebind in-process for production-grade orchestration without separate infrastructure.

## Core Value

Durable, reliable workflow execution on a single dependency. A paused DAG should stay paused across process restarts, with no CPU consumed, and resume exactly where it left off.

## Requirements

### Validated

- ✓ Task queue with reflection-based function dispatch — existing
- ✓ DAG workflow engine with step dependencies (After/AfterAny) — existing
- ✓ DAG submission, cancellation, and deletion — existing
- ✓ Dynamic step addition via workflow context — existing
- ✓ Step retry policies with exponential backoff — existing
- ✓ Cascade skipping on upstream failure — existing
- ✓ Await[T] for subscribing to step results — existing
- ✓ DAG state persistence in NATS KV bucket — existing
- ✓ Event-driven scheduler with CAS-based state transitions — existing
- ✓ Leader election for scheduler failover — existing
- ✓ CLI (ebctl) for dag inspection (ls/get/tree/step/cancel/rm) — existing
- ✓ DLQ management via ebctl — existing
- ✓ Embedded NATS (single-node and 3-node cluster) — existing
- ✓ Middleware chain for worker customization — existing
- ✓ Placement/targeted delivery for step placement — existing
- ✓ Error kind and message persistence on step failure — existing
- ✓ Pause DAG execution — ongoing step finishes, new dispatches blocked, state becomes PAUSED — v1.0
- ✓ Resume DAG execution — PAUSED → ready steps dispatched, state becomes IN PROGRESS — v1.0
- ✓ API functions: Pause(ctx, wf, id), Resume(ctx, wf, id) on Workflow type — v1.0
- ✓ CLI commands: ebctl dag pause &lt;id&gt;, ebctl dag resume &lt;id&gt; — v1.0
- ✓ CLI display: dag ls shows PAUSING/PAUSED in status column — v1.0
- ✓ Scheduler respects PAUSED state: no event processing, no CPU, no sweep re-enqueue — v1.0
- ✓ Cancel transitions pausing/paused → canceled — v1.0
- ✓ State machine with DAGStatusPausing/Paused, HasInFlightSteps, CanPause, CanResume — v1.0
- ✓ Full E2E integration tests + race condition tests — v1.0

### Active

_(New features for next milestone — TBD)_

### Out of Scope

- Time-based auto-resume (e.g., "pause for 1h then auto-resume") — simple manual pause/resume is sufficient
- Pause timeout / auto-cancel on prolonged pause — not needed for initial feature
- Canceling in-flight tasks on pause — in-flight tasks complete naturally
- Pause from within a handler (workflow context) — only via explicit API/CLI call
- UI dashboard — this is a Go library with a CLI, not a web service

## Context

**Shipped v1.0 (2026-06-03):** Pause/Resume DAG Execution — 4 phases, 8 plans, 23 requirements — all delivered in a single session.

The ebind codebase has a mature DAG engine with states: running, done, failed, canceled, pausing, paused. The pause/resume feature adds two new DAG-level states: `pausing` (transient, while in-flight step finishes) and `paused` (persisted, dormant, no CPU consumed). Step-level states remain unchanged — pending steps stay pending while paused.

The scheduler gates event processing, step dispatch, and sweep recovery for pausing/paused DAGs. Paused DAGs consume zero scheduler CPU beyond the status check in the event loop. The PAUSED state is persisted in KV via CAS, surviving restarts.

On resume, ready steps are re-evaluated and dispatched, and the DAG transitions back to running.

**Next area:** TBD — project open for new feature planning.

## Next Milestone Goals

The next milestone will define ebind's next development cycle. Potential areas (not committed):
- Security hardening / audit
- Observability improvements (metrics, tracing)
- Additional API features
- Performance tuning

*Define via `/gsd-new-milestone`*

## Constraints

- **Compatibility**: New DAG statuses must follow existing lowercase naming convention (`pausing`, `paused`)
- **Compatibility**: Pause must use KV CAS for state transitions, consistent with existing Cancel() pattern
- **Compatibility**: PAUSED state must be backward-compatible with existing stores — old DAGs without pause support continue working
- **Behavior**: Paused DAGs must consume no scheduler CPU resources beyond the status check in the event loop

## Key Decisions

| Decision | Rationale | Outcome |
|----------|-----------|---------|
| New DAG statuses: `pausing`, `paused` | Consistent with existing lowercase naming (running, done, failed, canceled) | ✓ Good (Phase 1) |
| Pause blocks new dispatches, lets in-flight finish | Users want clean pause, not hard abort | ✓ Good (Phase 1) |
| PAUSED survives restarts | Persisted in KV bucket via CAS | ✓ Good (Phase 1) |
| Both API + CLI entry points | Library users and operators both need access | ✓ Good (Phase 3-4) |
| CLI status column for PAUSED | Simple, fits existing dag ls output | ✓ Good (Phase 4) |
| No auto-resume or pause timeout | Keep scope tight; can add later | ✓ Good (all phases) |
| PausedAt audit field on DAGMeta | CLI display and audit trail | ✓ Good (Phase 1) |
| Cancel transitions pausing/paused → canceled | Admin escape hatch works through pause state | ✓ Good (Phase 1) |
| maybeFinalize leaves pausing/paused alone | Once paused, stays paused until explicit resume or cancel | ✓ Good (Phase 1), overridden for auto-finalize (Phase 2) |
| Terminal() treats pausing/paused as non-terminal | Both keep the DAG alive | ✓ Good (Phase 1) |
| Pause publishes event for scheduler dispatch | Goes through scheduler's normal serialized path | ✓ Good (Phase 3) |
| Sweep auto-finalizes paused DAGs if terminal | Zero CPU for completed-but-paused DAGs | ✓ Good (Phase 2) |
| CLI no confirmation for pause/resume | Pause/resume are reversible | ✓ Good (Phase 4) |

## Evolution

This document evolves at phase transitions and milestone boundaries.

**After each phase transition** (via `/gsd-transition`):
1. Requirements invalidated? → Move to Out of Scope with reason
2. Requirements validated? → Move to Validated with phase reference
3. New requirements emerged? → Add to Active
4. Decisions to log? → Add to Key Decisions
5. "What This Is" still accurate? → Update if drifted

**After each milestone** (via `/gsd-complete-milestone`):
1. Full review of all sections
2. Core Value check — still the right priority?
3. Audit Out of Scope — reasons still valid?
4. Update Context with current state

---
*Last updated: 2026-06-08 after v1.0 milestone completion*
