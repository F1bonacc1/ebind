# Phase 1: State Machine & Pure Logic - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-06-03
**Phase:** 01-state-machine-pure-logic
**Areas discussed:** Status values + transitions, maybeFinalize + Terminal behavior, Cancel ↔ Pause interaction, DAGMeta audit fields

---

## Status values + transitions

| Option | Description | Selected |
|--------|-------------|----------|
| Skip to paused directly | When zero in-flight steps, DagPause goes directly to paused | ✓ |
| Always pausing first | Always go through pausing state first | |

**User's choice:** Skip to paused directly when no steps in-flight
**Notes:** — 

---

## Naming

| Option | Description | Selected |
|--------|-------------|----------|
| DAGStatusPausing/Paused | Full constant names consistent with existing | ✓ |
| Pausing/Paused | Shorter constant names | |

**User's choice:** DAGStatusPausing, DAGStatusPaused (consistent with existing)
**Notes:** —

---

## Visibility

| Option | Description | Selected |
|--------|-------------|----------|
| Unexported | HasInFlightSteps is internal only | ✓ |
| Exported | Callers may want to check | |

**User's choice:** Unexported
**Notes:** —

---

## maybeFinalize behavior for pausing/paused

| Option | Description | Selected |
|--------|-------------|----------|
| Leave pausing/paused alone | Once paused, stays paused until explicit resume or cancel | ✓ |
| Auto-finalize pausing→paused only | Transition pausing→paused but stop there | |
| Finalize regardless | Let Terminal() finalize to done/failed even for paused | |

**User's choice:** Leave pausing/paused alone (no auto-transition)
**Notes:** —

---

## Terminal() for pausing

| Option | Description | Selected |
|--------|-------------|----------|
| Not terminal | pausing is not terminal (treated like running) | ✓ |
| Terminal if steps done | pausing is terminal if all steps are done | |

**User's choice:** Not terminal
**Notes:** —

---

## Cancel + pausing

| Option | Description | Selected |
|--------|-------------|----------|
| Cancel pausing DAGs | Transition pausing→canceled | ✓ |
| Wait then cancel | Block until paused then cancel | |

**User's choice:** Cancel pausing DAGs
**Notes:** —

---

## Cancel + paused

| Option | Description | Selected |
|--------|-------------|----------|
| Cancel paused DAGs | Transition paused→canceled | ✓ |
| Error, resume first | Must resume before canceling | |

**User's choice:** Cancel paused DAGs
**Notes:** —

---

## Idempotency for DagPause on terminal DAGs

| Option | Description | Selected |
|--------|-------------|----------|
| Return error | Descriptive error for invalid pause attempts | ✓ |
| Return nil | Idempotent no-op | |

**User's choice:** Return error on done/failed/canceled DAGs
**Notes:** User asked about consequences first, then chose explicit error

---

## Audit fields on DAGMeta

| Option | Description | Selected |
|--------|-------------|----------|
| Add PausedAt now | Timestamp field added in Phase 1 | ✓ |
| Defer audit fields | Keep DAGMeta minimal | |

**User's choice:** Add PausedAt to DAGMeta now
**Notes:** —

---

## the agent's Discretion

- Exact error message wording for invalid pause/resume attempts
- Internal helper function signatures beyond the required public API

## Deferred Ideas

None
