package workflow

import (
	"fmt"

	"github.com/f1bonacc1/ebind/worker"
)

type PlacementMode string

const (
	PlacementDirect     PlacementMode = "direct"
	PlacementColocate   PlacementMode = "colocate_with"
	PlacementFollow     PlacementMode = "follow_target_of"
	PlacementHere       PlacementMode = "colocate_here"
)

// PlacementSpec describes how a step chooses its execution target.
type PlacementSpec struct {
	Mode   PlacementMode `json:"mode,omitempty"`
	Target string        `json:"target,omitempty"`
	StepID string        `json:"step_id,omitempty"`
}

// OnTarget binds a step to a logical or concrete target claim.
func OnTarget(target string) StepOption {
	return func(s *Step) {
		s.placement = &PlacementSpec{Mode: PlacementDirect, Target: target}
	}
}

// ColocateWith binds a step to the concrete worker that executed another step.
// The dependency is also made explicit so the referenced step has already run by
// the time placement is resolved.
func ColocateWith(step *Step) StepOption {
	return func(s *Step) {
		if step == nil {
			return
		}
		s.afterDeps = append(s.afterDeps, step.id)
		s.placement = &PlacementSpec{Mode: PlacementColocate, StepID: step.id}
	}
}

// FollowTargetOf binds a step to the same logical target expression as another
// step, but re-resolves that target at execution time.
func FollowTargetOf(step *Step) StepOption {
	return func(s *Step) {
		if step == nil {
			return
		}
		s.afterDeps = append(s.afterDeps, step.id)
		s.placement = &PlacementSpec{Mode: PlacementFollow, StepID: step.id}
	}
}

// ColocateHere is only valid for dynamic steps created from a running handler
// via workflow.FromContext(ctx).StepOpts(...). It binds the new step to the
// concrete worker that is currently executing.
func ColocateHere() StepOption {
	return func(s *Step) {
		s.placement = &PlacementSpec{Mode: PlacementHere}
	}
}

func resolvePlacementTarget(rec StepRecord, steps map[string]StepRecord, seen map[string]struct{}) (string, error) {
	if rec.Placement == nil {
		return "", nil
	}
	if seen == nil {
		seen = map[string]struct{}{}
	}
	key := rec.StepID + ":" + string(rec.Placement.Mode)
	if _, ok := seen[key]; ok {
		return "", fmt.Errorf("workflow: placement cycle at step %q", rec.StepID)
	}
	seen[key] = struct{}{}

	switch rec.Placement.Mode {
	case PlacementDirect:
		return rec.Placement.Target, nil
	case PlacementHere:
		return "", fmt.Errorf("workflow: step %q uses ColocateHere outside a running handler", rec.StepID)
	case PlacementColocate:
		upstream, ok := steps[rec.Placement.StepID]
		if !ok {
			return "", fmt.Errorf("workflow: step %q colocates with unknown step %q", rec.StepID, rec.Placement.StepID)
		}
		if upstream.WorkerID == "" {
			return "", fmt.Errorf("workflow: step %q cannot colocate with %q before it has an executor", rec.StepID, rec.Placement.StepID)
		}
		return worker.ConcreteTarget(upstream.WorkerID), nil
	case PlacementFollow:
		upstream, ok := steps[rec.Placement.StepID]
		if !ok {
			return "", fmt.Errorf("workflow: step %q follows unknown step %q", rec.StepID, rec.Placement.StepID)
		}
		if upstream.Placement == nil {
			return "", fmt.Errorf("workflow: step %q cannot follow target of %q without placement", rec.StepID, rec.Placement.StepID)
		}
		return resolvePlacementTarget(upstream, steps, seen)
	default:
		return "", fmt.Errorf("workflow: step %q has unknown placement mode %q", rec.StepID, rec.Placement.Mode)
	}
}
