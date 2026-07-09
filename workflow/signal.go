package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"
)

// Signal durably delivers the named external signal to a DAG. Steps gated on
// the name (WaitForSignal option or a SignalRef arg) become eligible to run;
// a SignalRef arg receives the JSON-marshaled payload.
//
// Semantics are one-shot, buffered, first-wins:
//   - The record is created exactly once per (DAG, name) and is immutable. A
//     repeat delivery is an idempotent no-op returning (false, nil) with the
//     original payload kept.
//   - A signal delivered before any step waits on it (including before a
//     dynamic step is added) is buffered for the DAG's lifetime.
//   - Delivery to a terminal DAG (done/failed/canceled) is a no-op returning
//     (false, nil) without writing. Inspect DAGMeta.Status to distinguish this
//     from a duplicate. An unknown DAG returns ErrDAGNotFound.
//
// Ordering: the signal record is created FIRST (the durable truth the
// schedulers' gate reads), then a wake-up EventSignal is ALWAYS published —
// even on duplicate — so a retry after a crash between write and publish
// converges. If the publish fails and is not retried, the leader sweep picks
// the step up from the durable record within SweepInterval (default 60s).
//
// A signal delivered while the DAG is paused or pausing is persisted and takes
// effect when Resume re-opens scheduling.
func Signal(ctx context.Context, wf *Workflow, dagID, name string, payload any) (bool, error) {
	if name == "" {
		return false, fmt.Errorf("workflow: signal name must be non-empty")
	}
	meta, _, err := wf.Store.GetMeta(ctx, dagID)
	if err != nil {
		return false, err
	}
	switch meta.Status {
	case DAGStatusDone, DAGStatusFailed, DAGStatusCanceled:
		return false, nil // nothing can wait in a terminal DAG; skip the write
	}
	data, err := marshalAny(payload)
	if err != nil {
		return false, fmt.Errorf("workflow: marshal signal payload: %w", err)
	}
	delivered := true
	err = wf.Store.PutSignal(ctx, dagID, SignalRecord{
		DAGID:       dagID,
		Name:        name,
		Payload:     data,
		DeliveredAt: time.Now().UTC(),
	})
	if errors.Is(err, ErrStaleRevision) {
		delivered = false // already delivered; first write wins
	} else if err != nil {
		return false, err
	}
	// Always publish, even on duplicate: a retry after a crash between the
	// create above and this publish must still trigger re-evaluation.
	if err := publishEvent(ctx, wf.Bus, Event{
		Kind: EventSignal, DAGID: dagID, StepID: dagID, SignalName: name,
	}); err != nil {
		return delivered, err
	}
	return delivered, nil
}

// signalMap indexes signal records by name for DAGState.Signals.
func signalMap(sigs []SignalRecord) map[string]SignalRecord {
	if len(sigs) == 0 {
		return nil
	}
	out := make(map[string]SignalRecord, len(sigs))
	for _, rec := range sigs {
		out[rec.Name] = rec
	}
	return out
}

// SignalInfo is the observability projection for one signal name in a DAG —
// delivered or still awaited.
type SignalInfo struct {
	Name        string          `json:"name"`
	Delivered   bool            `json:"delivered"`
	DeliveredAt time.Time       `json:"delivered_at,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	// Waiting lists pending steps gated on this name. Empty once delivered
	// (the gate is open) and always empty for terminal DAGs — mirroring
	// ComputeBreakpoints, a finished DAG reports nothing as waiting.
	Waiting []string `json:"waiting,omitempty"`
}

// ComputeSignals is a PURE projection of a DAG's signal state: the union of
// every name referenced by a step's WaitSignals and every delivered record.
// A buffered signal no step references yet still shows (delivered, no
// waiters). Sorted by name; Waiting sorted by step ID.
func ComputeSignals(meta DAGMeta, steps []StepRecord, sigs []SignalRecord) []SignalInfo {
	byName := signalMap(sigs)
	terminal := meta.Status == DAGStatusDone || meta.Status == DAGStatusFailed ||
		meta.Status == DAGStatusCanceled

	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	for _, s := range steps {
		names = append(names, s.WaitSignals...)
	}
	names = dedupeStrings(names)
	sort.Strings(names)

	out := make([]SignalInfo, 0, len(names))
	for _, name := range names {
		info := SignalInfo{Name: name}
		if rec, ok := byName[name]; ok {
			info.Delivered = true
			info.DeliveredAt = rec.DeliveredAt
			info.Payload = rec.Payload
		} else if !terminal {
			for _, s := range steps {
				if s.Status == StatusPending && slices.Contains(s.WaitSignals, name) {
					info.Waiting = append(info.Waiting, s.StepID)
				}
			}
			sort.Strings(info.Waiting)
		}
		out = append(out, info)
	}
	return out
}

// CountWaitingSteps counts distinct steps currently gated on any undelivered
// signal — the ⚑ badge number shared by Debug, `ebctl dag get`, and
// `ebctl dag ls`.
func CountWaitingSteps(infos []SignalInfo) int {
	seen := map[string]bool{}
	for _, si := range infos {
		for _, id := range si.Waiting {
			seen[id] = true
		}
	}
	return len(seen)
}

// ListSignalInfo loads a DAG's meta, steps, and signal records and projects
// them via ComputeSignals. Returns ErrDAGNotFound for an unknown DAG.
func ListSignalInfo(ctx context.Context, wf *Workflow, dagID string) ([]SignalInfo, error) {
	meta, _, err := wf.Store.GetMeta(ctx, dagID)
	if err != nil {
		return nil, err
	}
	steps, err := wf.Store.ListSteps(ctx, dagID)
	if err != nil {
		return nil, err
	}
	sigs, err := wf.Store.ListSignals(ctx, dagID)
	if err != nil {
		return nil, err
	}
	return ComputeSignals(meta, steps, sigs), nil
}
