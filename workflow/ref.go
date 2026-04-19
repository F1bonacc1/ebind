package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Ref marks a position in a step's argument list that should be substituted with
// another step's result at scheduling time. Refs are JSON-serialized into the step
// record's ArgsJSON; the scheduler fetches the upstream result from the store and
// substitutes it in place before enqueueing.
//
// The magic key "__ebind_ref__" makes Refs distinguishable from ordinary JSON values.
// The Mode controls what happens when the upstream step failed/skipped.
type Ref struct {
	StepID  string          `json:"step_id"`
	Mode    RefMode         `json:"mode"`
	Default json.RawMessage `json:"default,omitempty"` // used when Mode == RefOrDefault
}

type RefMode string

const (
	// RefModeRequired: upstream must be `done`. If `failed`/`skipped`, this step is skipped (cascade).
	RefModeRequired RefMode = "required"
	// RefModeOrDefault: if upstream `failed`/`skipped`, substitute the Default value and run anyway.
	RefModeOrDefault RefMode = "or_default"
)

const refMarker = "__ebind_ref__"

// refEnvelope is the on-the-wire JSON shape for a Ref embedded in an args array.
type refEnvelope struct {
	Marker string          `json:"__ebind_ref__"`
	StepID string          `json:"step_id"`
	Mode   RefMode         `json:"mode"`
	Def    json.RawMessage `json:"default,omitempty"`
}

// MarshalJSON serializes Ref using the refEnvelope shape so scheduler can find it.
func (r Ref) MarshalJSON() ([]byte, error) {
	return json.Marshal(refEnvelope{
		Marker: refMarker,
		StepID: r.StepID,
		Mode:   r.Mode,
		Def:    r.Default,
	})
}

// UnmarshalJSON accepts the refEnvelope shape.
func (r *Ref) UnmarshalJSON(data []byte) error {
	var env refEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return err
	}
	r.StepID = env.StepID
	r.Mode = env.Mode
	r.Default = env.Def
	return nil
}

// IsRef inspects a raw JSON value to see if it's a Ref envelope.
// Fast path: checks for the marker string without a full decode.
func IsRef(raw json.RawMessage) bool {
	return bytes.Contains(raw, []byte(`"`+refMarker+`"`))
}

// DecodeRef parses a raw JSON value as a Ref if it is one; otherwise returns (nil, false).
func DecodeRef(raw json.RawMessage) (*Ref, bool) {
	if !IsRef(raw) {
		return nil, false
	}
	var r Ref
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, false
	}
	return &r, true
}

// StepStatus is used by ResolveArgs to decide how to handle each Ref.
type StepStatus string

const (
	StatusPending  StepStatus = "pending"
	StatusRunning  StepStatus = "running"
	StatusDone     StepStatus = "done"
	StatusFailed   StepStatus = "failed"
	StatusSkipped  StepStatus = "skipped"
	StatusCanceled StepStatus = "canceled"
)

// ResolveArgs substitutes each Ref in rawArgs with the actual upstream result bytes
// from results (keyed by step ID). Returns one of:
//   - resolved args (all Refs substituted; literal values unchanged)
//   - ShouldSkip=true if any RefModeRequired pointed at a failed/skipped upstream
//     (caller should mark the step skipped and cascade)
//   - error if a required upstream result is missing or a Ref is malformed
//
// ResolveArgs is PURE — no IO, deterministic. 100% unit-testable.
func ResolveArgs(
	rawArgs []json.RawMessage,
	results map[string]json.RawMessage,
	statuses map[string]StepStatus,
) (resolved []json.RawMessage, shouldSkip bool, err error) {
	out := make([]json.RawMessage, len(rawArgs))
	for i, raw := range rawArgs {
		ref, isRef := DecodeRef(raw)
		if !isRef {
			out[i] = raw
			continue
		}
		status := statuses[ref.StepID]
		switch status {
		case StatusDone:
			upstream, ok := results[ref.StepID]
			if !ok {
				return nil, false, fmt.Errorf("ResolveArgs: upstream %q is done but no result", ref.StepID)
			}
			out[i] = upstream
		case StatusFailed, StatusSkipped, StatusCanceled:
			switch ref.Mode {
			case RefModeRequired:
				return nil, true, nil // cascade skip
			case RefModeOrDefault:
				if len(ref.Default) == 0 {
					out[i] = []byte("null")
				} else {
					out[i] = ref.Default
				}
			default:
				return nil, false, fmt.Errorf("ResolveArgs: unknown RefMode %q for %q", ref.Mode, ref.StepID)
			}
		default:
			return nil, false, fmt.Errorf("ResolveArgs: upstream %q not terminal (status=%s)", ref.StepID, status)
		}
	}
	return out, false, nil
}
