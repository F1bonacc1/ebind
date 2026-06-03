# Research: Phase 1 — State Machine & Pure Logic

**Researched:** 2026-06-03
**Mode:** Project-level research synthesis (project-level research in `.planning/research/`)
**Confidence:** HIGH — pure state logic extending well-established codebase patterns

---

## Scope

Phase 1 covers zero-IO, pure state machine changes only:
1. Add `DAGStatusPausing` and `DAGStatusPaused` constants
2. Add `HasInFlightSteps`, `CanPause`, `CanResume` pure functions on `DAGState`
3. Update `Terminal()` to NOT treat `pausing`/`paused` as terminal
4. Update `maybeFinalize` to skip finalization for `pausing`/`paused` DAGs
5. Update `Cancel()` CAS to handle `pausing`/`paused` states (transition to canceled)
6. Add `PausedAt time.Time` field to `DAGMeta`
7. Pure unit tests for all new/updated functions

No IO, no scheduler changes, no API/CLI.

## Key Findings

### Patterns to Follow

| Pattern | Location | Rationale |
|---------|----------|-----------|
| Constant naming | `workflow/state.go:13-18` | `DAGStatusPausing DAGStatus = "pausing"` — lowercase string constants, same file |
| Pure state functions | `workflow/state.go:72-83` | `ReadyToRun()` on `DAGState` returns data, no IO |
| CAS retry loop | `workflow/cancel.go:13-30` | 5 attempts, `errors.Is(err, ErrStaleRevision)`, break on success |
| makeState test helper | `workflow/state_test.go:11-20` | Creates `DAGState` from `StepRecord` variadic |
| refArgs test helper | `workflow/state_test.go:22-30` | Builds JSON ref arguments |
| Cancel pattern | `workflow/cancel.go:12-56` | GetMeta → CAS switch → PutMeta; then step iteration |
| maybeFinalize guard | `workflow/scheduler.go:308-309` | Currently guards `DAGStatusCanceled`; must add `pausing`/`paused` |
| StepRecord.IsTerminal | `workflow/state.go:59-61` | Checks step statuses (not DAG status) — no change needed |
| DAGMeta struct | `workflow/state.go:21-27` | Add `PausedAt` field with `json:"paused_at,omitempty"` |

### Phase 1-Specific Pitfalls (from `.planning/research/PITFALLS.md`)

| Pitfall | Risk | Mitigation |
|---------|------|------------|
| Status string mismatch (`"PAUSED"` vs `"paused"`) | Compile-time checkable | Use typed `DAGStatus` constants, not string literals |
| maybeFinalize overwrites pause | DAG auto-finalizes while paused | Guard `maybeFinalize` (scheduler.go:308) against `DAGStatusPausing`/`DAGStatusPaused` |
| Forgetting Cancel handles pausing/paused | Paused DAG can't be canceled | Add explicit cases to Cancel() switch stmt |
| DAGStatusRunning comparisons miss new states | String comparisons instead of constants | Audit all `== DAGStatusRunning` comparisons (sweep, enqueueReady) |

### Required Code Changes (Summary)

**File: `workflow/state.go`**
- Add `DAGStatusPausing` and `DAGStatusPaused` constants
- Add `PausedAt time.Time \`json:"paused_at,omitempty"\`` to `DAGMeta`
- Add `HasInFlightSteps() bool` method on `*DAGState`
- Add `CanPause() bool` method on `*DAGState`
- Add `CanResume() bool` method on `*DAGState`
- Update `Terminal()` to exclude `pausing`/`paused` from all-terminal computation — ensure they don't incorrectly trigger terminal transitions while paused

**File: `workflow/cancel.go`**
- Add `DAGStatusPausing` and `DAGStatusPaused` to the Cancel switch (both → canceled)

**File: `workflow/scheduler.go`**
- Update `maybeFinalize` — add guard for `DAGStatusPausing` and `DAGStatusPaused` (line 298-316)

**File: `workflow/state_test.go`** (new tests)
- Test `HasInFlightSteps`: true when running, false when none running
- Test `CanPause`: true for running, false for paused/terminal
- Test `CanResume`: true for paused, false for running/terminal
- Test `Terminal()` with pausing/paused steps (should not trigger terminal)
- Test `Cancel()` on pausing DAG → canceled
- Test `Cancel()` on paused DAG → canceled
- Test `maybeFinalize` guard (though this is in scheduler_test.go or state_test.go)
- Test `PausedAt` is set on DAGMeta

### What NOT to Change

- Step-level statuses are untouched — no new step statuses needed
- `StepRecord.IsTerminal()` — unchanged (only checks step statuses)
- `ReadyToRun()` — unchanged (only checks deps, not DAG meta status)
- `enqueueReady` guard — Phase 2 concern
- `handleEvent` event gate — Phase 2 concern
- `sweep` filter — Phase 2 concern
- No new files — all changes in existing `workflow/state.go`, `workflow/cancel.go`, `workflow/scheduler.go`
- No new dependencies

### Validation Architecture

All changes must be pure unit-testable:
1. Test DAGState methods with `makeState` helper → no NATS needed
2. Test `Cancel()` behavior via pure state assertions (or minimal Workflow setup)
3. Run `go test -race ./workflow/` after changes

---

## Research Sources

- `.planning/research/ARCHITECTURE.md` — Full architecture including state machine, Terminal() impact, Cancel interaction
- `.planning/research/SUMMARY.md` — Executive summary with phase ordering rationale
- `.planning/research/FEATURES.md` — Feature dependencies, MVP recommendation
- `.planning/research/PITFALLS.md` — Phase 1-specific warnings (P9, P11)
- `.planning/research/STACK.md` — Stack assessment (no new deps)
- Codebase: `workflow/state.go`, `workflow/cancel.go`, `workflow/scheduler.go`, `workflow/state_test.go`
