# Feature Landscape: Pause/Resume

**Domain:** Go DAG workflow engine pause/resume
**Researched:** 2026-06-03

## Table Stakes

These are features users expect from a DAG workflow engine's pause/resume capability. Missing any makes the feature feel incomplete.

| Feature | Why Expected | Complexity | Notes |
|---------|--------------|------------|-------|
| **Pause blocks new dispatches** | Core purpose of pause | Low | Gate in `enqueueReady` |
| **In-flight steps complete normally** | Clean pause, not hard abort | Low | No step status changes during pause |
| **Paused state survives restarts** | Durability in a production system | Low | Persisted in existing KV bucket via CAS |
| **Resume re-dispatches ready steps** | Core purpose of resume | Low | `ReadyToRun()` after CAS→running |
| **Atomic state transitions** | Must not leave partial state | Low | CAS on meta, same as Cancel |
| **Cancel during pause works** | Admin escape hatch | Low | Cancel already handles pending→canceled |
| **CLI commands** | Operator accessibility | Low | Two new cobra commands |
| **Status display** | Operators need visibility | Low | Status column in `dag ls` |

## Differentiators

Features that set ebind's pause/resume apart. Not expected in every DAG engine, but valuable.

| Feature | Value Proposition | Complexity | Notes |
|---------|-------------------|------------|-------|
| **No new infrastructure** | Pause/resume uses existing KV bucket, no new streams/consumers | None | Architectural advantage of CAS-based design |
| **Zero CPU for paused DAGs** | Paused DAGs are skipped in scheduler sweep; only one status check per event | Low | Gate 3: sweep skips paused DAGs |
| **Event-driven pausing→paused** | No polling, no ticker; transition on last completion event | Medium | Requires careful race handling with Resume |
| **Failover-safe pause tracking** | Leader crash during pausing→paused doesn't lose state; sweep recovers | Low | Sweep handles pausing DAGs on acquisition |
| **Dynamic step addition during pause** | Handlers can add steps to paused DAGs; they execute on resume | Low | Step-added events are ACKed but not dispatched until resume |
| **Early resume (while still draining)** | User can resume before all in-flight steps finish | Low | Resume works from both pausing and paused |
| **Backward compatible** | Existing DAGs without pause support continue working | None | Only new statuses in existing meta string field |

## Anti-Features

Features explicitly NOT building into pause/resume.

| Anti-Feature | Why Avoid | What to Do Instead |
|--------------|-----------|-------------------|
| **Auto-resume timer** | Scope creep; "pause for X then resume" can be built by caller watching the DAG status | Document pattern for external timer-based resume |
| **Pause timeout / auto-cancel** | Same as above; premature optimization | Document that caller should use Cancel on timeout |
| **Pause from within handler** | Would require new event type and complex state coordination in StepHook | Only via explicit API/CLI call |
| **Canceling in-flight tasks on pause** | Violates "clean pause" requirement | Let them finish naturally |
| **Per-step pause** | Entirely different feature scope (individual step control) | Not needed for v1 |
| **Pause reason / annotation** | Extra metadata field with unclear ROI | Could add in future as optional meta field |

## Feature Dependencies

```
State machine constants (pausing, paused)
  │
  ├──► Pause() API function
  │       │
  │       ├──► CLI pause command
  │       │
  │       └──► Scheduler pause gates
  │               │
  │               ├──► onEvent gate (Ack if paused/pausing)
  │               ├──► enqueueReady gate (skip if not running)
  │               └──► sweep handler (pausing→paused recovery)
  │
  └──► Resume() API function
          │
          ├──► CLI resume command
          │
          └──► (no new scheduler gates needed for resume)
```

Arrows mean "depends on". Scheduler gates depend on Pause API (need the pausing status to check against). Resume depends on Pause (nothing to resume if nothing was paused).

## MVP Recommendation

Phase 1: **State Machine + Pure Logic** — constants, pure functions, unit tests
Phase 2: **Pause API + CLI** — `Pause()` function, `ebctl dag pause`, status display
Phase 3: **Scheduler Integration** — all three gates, pausing→paused transition, sweep recovery
Phase 4: **Resume API + CLI** + integration tests — completes the feature

## Sources

- PROJECT.md — feature requirements and constraints (HIGH confidence)
- workflow/state.go, workflow/scheduler.go, workflow/cancel.go — codebase analysis (HIGH confidence)
- Temporal.io docs — pattern validation for "let in-flight complete" design (MEDIUM confidence)
