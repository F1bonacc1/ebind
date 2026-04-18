package workflow

import "github.com/f1bonacc1/ebind/task"

// Step represents a single node in a DAG. Returned by DAG.Step for the caller
// to chain into dependent steps via Ref() or RefOrDefault().
type Step struct {
	id       string
	fn       any
	args     []any // may contain Ref values from upstream steps
	optional bool
	policy   *task.RetryPolicy

	// afterDeps: explicit required temporal deps (from After()). Step won't start
	// until each is terminal; if any failed/skipped, this step cascade-skips.
	afterDeps []string
	// afterAnyDeps: explicit optional temporal deps (from AfterAny()). Step waits
	// for each to be terminal but runs regardless of their outcome.
	afterAnyDeps []string
}

// ID returns the stable step ID within its DAG.
func (s *Step) ID() string { return s.id }

// Ref returns a Required-mode reference. Downstream steps using this Ref will
// be cascade-skipped if this step fails or is itself skipped.
func (s *Step) Ref() Ref { return Ref{StepID: s.id, Mode: RefModeRequired} }

// RefOrDefault returns an OrDefault-mode reference. If this step fails or is
// skipped, the downstream step runs with the provided default value substituted
// for this step's output.
func (s *Step) RefOrDefault(defaultValue any) Ref {
	defJSON, _ := marshalAny(defaultValue)
	return Ref{StepID: s.id, Mode: RefModeOrDefault, Default: defJSON}
}

// StepOption configures a Step at construction time.
type StepOption func(*Step)

// Optional marks a step as non-critical — its failure does not fail the DAG.
// Downstream steps choose whether to cascade-skip (via Ref) or substitute
// (via RefOrDefault).
func Optional() StepOption { return func(s *Step) { s.optional = true } }

// WithStepRetry overrides the DAG's default retry policy for this step.
func WithStepRetry(p task.RetryPolicy) StepOption {
	return func(s *Step) { pc := p; s.policy = &pc }
}

// After declares explicit temporal-only dependencies on the given upstream steps.
// This step waits until every upstream is terminal (done/failed/skipped) before
// it runs. If any upstream ended in failed/skipped, this step is cascade-skipped
// (same semantics as referencing via Ref()).
//
// Use After when you need ordering but the current step's handler doesn't
// consume any upstream output. Equivalent to adding a Ref(upstream) arg that
// the handler ignores, but without contaminating the handler's signature.
func After(steps ...*Step) StepOption {
	return func(s *Step) {
		for _, up := range steps {
			if up == nil {
				continue
			}
			s.afterDeps = append(s.afterDeps, up.id)
		}
	}
}

// AfterAny declares optional temporal-only dependencies. This step waits until
// every upstream is terminal (done/failed/skipped) but runs regardless of
// whether they succeeded. Upstream failure does NOT cascade-skip this step.
//
// Use AfterAny for ordering a "best-effort" or cleanup step that should run
// after some other work, whether that work succeeded or not.
func AfterAny(steps ...*Step) StepOption {
	return func(s *Step) {
		for _, up := range steps {
			if up == nil {
				continue
			}
			s.afterAnyDeps = append(s.afterAnyDeps, up.id)
		}
	}
}
