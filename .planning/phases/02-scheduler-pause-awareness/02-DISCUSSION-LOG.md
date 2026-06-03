# Phase 2: Scheduler Pause Awareness - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-06-03
**Phase:** 02-scheduler-pause-awareness
**Areas discussed:** Event entry gate, pausing→paused auto-transition, sweep recovery, dispatch gate

---

## Event entry gate behavior

| Option | Description | Selected |
|--------|-------------|----------|
| Return early (same as Canceled) | handleEvent returns nil for pausing/paused, event acked normally | ✓ |
| Process but skip dispatch | Apply step status update but don't enqueue downstream | |

**User's choice:** Return early (same as Canceled pattern)
**Notes:** —

---

## Event gate scope

| Option | Description | Selected |
|--------|-------------|----------|
| Gate both | Both EventCompleted and EventStepAdded | ✓ |
| Gate only EventCompleted | step-added is small and harmless | |

**User's choice:** Gate both EventCompleted and EventStepAdded
**Notes:** —

---

## pausing→paused auto-transition location

| Option | Description | Selected |
|--------|-------------|----------|
| In onCompleted after step finishes | After MarkDone/MarkFailed, check if DAG is pausing and no more in-flight steps | ✓ |
| Only in sweep | Sweep is the reliable path | |
| Both onCompleted + sweep fallback | Event path for low latency, sweep for recovery | User selected onCompleted only, but sweep also recovers pausing→paused |

**User's choice:** In onCompleted after step finishes
**Notes:** Sweep also transitions pausing→paused as a fallback on leader acquisition

---

## CAS race: pausing→paused vs Resume

| Option | Description | Selected |
|--------|-------------|----------|
| Return nil (benign) | Someone else won the race, current state is valid | ✓ |
| Retry the CAS | The pause intent is still valid | |
| Reload and retry | Maybe Resume already set it back to running | |

**User's choice:** Return nil (benign CAS loss)
**Notes:** Asked for details first; chose nil on understanding

---

## Sweep recovery for pausing DAGs

| Option | Description | Selected |
|--------|-------------|----------|
| Transition pausing→paused + skip paused | Sweep recovers pausing DAGs, ignores paused | ✓ (partial) |
| Skip both, no transition | Don't touch pausing or paused | |
| Transition + enqueue recovery check | Also verify no stranded pending steps | |

**User's choice:** Transition pausing→paused and auto-finalize paused if all terminal
**Notes:** Also chose to auto-finalize paused DAGs if all steps are terminal

---

## Override D-04: auto-finalize paused if terminal

| Option | Description | Selected |
|--------|-------------|----------|
| Keep D-04 (stay paused) | Once paused, stays paused forever | |
| Auto-finalize if all terminal | Override D-04 for the case where all steps complete while paused | ✓ |

**User's choice:** Auto-finalize paused if all steps terminal
**Notes:** Explicitly overrides D-04 from Phase 1 context

---

## Dispatch gate (enqueueReady)

| Option | Description | Selected |
|--------|-------------|----------|
| Add pausing/paused alongside Canceled | enqueueReady skips all non-running DAGs | ✓ |
| Only guard for Canceled | pausing DAGs can still dispatch | |

**User's choice:** Add pausing/paused alongside Canceled
**Notes:** —

---

## Step-added gate

| Option | Description | Selected |
|--------|-------------|----------|
| Covered by enqueueReady guard | enqueueReady guard handles both | ✓ |
| Separate logic for step-added | Different behavior for step-added events | |

**User's choice:** Covered by enqueueReady guard
**Notes:** —

---

## the agent's Discretion

- Exact placement of pausing→paused CAS call in onCompleted
- Whether to add a helper function for the transition
- Error handling details for CAS race

## Deferred Ideas

None
