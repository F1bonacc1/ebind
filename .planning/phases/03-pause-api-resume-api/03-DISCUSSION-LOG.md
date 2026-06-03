# Phase 3: Pause API + Resume API - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-06-03
**Phase:** 03-pause-api-resume-api
**Areas discussed:** API signature, Resume dispatch, sentinel errors, optional parameters

---

## API Signature

| Option | Description | Selected |
|--------|-------------|----------|
| Standalone functions | Pause(ctx, wf, dagID) / Resume(ctx, wf, dagID) — consistent with Cancel | ✓ |
| Workflow methods | wf.Pause(ctx, dagID) / wf.Resume(ctx, dagID) | |

**User's choice:** Standalone functions (same pattern as Cancel)
**Notes:** —

---

## Return type

| Option | Description | Selected |
|--------|-------------|----------|
| error only | Same as Cancel, simple | ✓ |
| (status, error) | Returns the resulting status | |

**User's choice:** error only
**Notes:** —

---

## Resume dispatch

| Option | Description | Selected |
|--------|-------------|----------|
| Direct enqueue in Resume() | Instant, bypasses scheduler, KV CAS + dedupe prevent double-enqueue | |
| Publish event for scheduler | Goes through scheduler's normal serialized path, higher latency | ✓ |

**User's choice:** Publish event for scheduler
**Notes:** Asked for more details first; chose event-based approach for consistency with event-driven architecture

---

## Sentinel errors

| Option | Description | Selected |
|--------|-------------|----------|
| New sentinel errors: ErrDAGNotRunning, ErrDAGNotPaused | Specific errors for invalid transitions | ✓ |
| Single ErrInvalidStateTransition | Less specific, fewer new errors | |
| No new sentinels — fmt.Errorf with context | Wrap existing errors | |

**User's choice:** New sentinel errors ErrDAGNotRunning, ErrDAGNotPaused
**Notes:** —

---

## Error format

| Option | Description | Selected |
|--------|-------------|----------|
| Include dagID + status | fmt.Errorf("ebind: cannot pause DAG %s: status is %s: %w") | ✓ |
| Just sentinel errors | Caller adds context | |

**User's choice:** Include dagID + status in wrapped error message
**Notes:** —

---

## Pause options struct

| Option | Description | Selected |
|--------|-------------|----------|
| No options now | Add later via functional options pattern | ✓ |
| Add PauseReason now | Simple string field | |
| Full PauseOptions struct | reason + force flags | |

**User's choice:** No options now — add later when needed
**Notes:** —

---

## Resume options struct

| Option | Description | Selected |
|--------|-------------|----------|
| No options now | Same as Cancel | ✓ |
| Add ResumeReason now | Simple string field | |

**User's choice:** No options now
**Notes:** —

---

## the agent's Discretion

- Exact resume event kind string
- Where to publish new sentinel errors (workflow/errors.go)
- Helper function for the common CAS pattern
- Test file name for API functions

## Deferred Ideas

- PauseReason/ResumeReason — can add as options later
- Force pause flag — out of scope
- Async pause with notification — v2

