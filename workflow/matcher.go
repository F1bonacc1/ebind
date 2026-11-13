package workflow

import (
	"fmt"
	"path"
	"slices"
)

// BPMatchKind selects which step attribute a BPMatcher's pattern is matched
// against.
type BPMatchKind string

const (
	// BPMatchStep matches the step ID with a path.Match glob.
	BPMatchStep BPMatchKind = "step"
	// BPMatchFn matches the step's canonical function name with a path.Match glob.
	BPMatchFn BPMatchKind = "fn"
	// BPMatchLabel matches a breakpoint label declared on the step with
	// BreakBefore/BreakAfter, exactly.
	BPMatchLabel BPMatchKind = "label"
)

// BPMatcher selects the steps a breakpoint applies to and the position
// (before or after execution) it stops them at. It is a plain value: compare
// matchers with Key, not ==, when the intent is "the same breakpoint".
//
// Step and fn patterns use path.Match glob syntax, where '*' and '?' never
// match '/': BeforeStep("node-*") does not match the step ID "node-1/upgrade";
// use "node-*/*" for that. Label patterns are exact strings, not globs.
type BPMatcher struct {
	Position BPPosition  `json:"position"`
	Kind     BPMatchKind `json:"kind"`
	Pattern  string      `json:"pattern"`
}

// BeforeStep stops every step whose ID matches glob before it runs.
// glob uses path.Match syntax: '*' does not cross '/'.
func BeforeStep(glob string) BPMatcher {
	return BPMatcher{Position: BPPositionBefore, Kind: BPMatchStep, Pattern: glob}
}

// AfterStep stops the dependents of every step whose ID matches glob after
// that step completes. glob uses path.Match syntax: '*' does not cross '/'.
func AfterStep(glob string) BPMatcher {
	return BPMatcher{Position: BPPositionAfter, Kind: BPMatchStep, Pattern: glob}
}

// BeforeFn stops every step whose function's canonical name matches glob
// before it runs. The canonical name is the one recorded at submit (e.g.
// "handlers.Charge"); registry aliases are not consulted. glob uses
// path.Match syntax: '*' does not cross '/'.
func BeforeFn(glob string) BPMatcher {
	return BPMatcher{Position: BPPositionBefore, Kind: BPMatchFn, Pattern: glob}
}

// AfterFn stops the dependents of every step whose function's canonical name
// matches glob after that step completes. See BeforeFn for the name and glob
// rules.
func AfterFn(glob string) BPMatcher {
	return BPMatcher{Position: BPPositionAfter, Kind: BPMatchFn, Pattern: glob}
}

// BeforeLabel stops every step that declares label with BreakBefore, before
// it runs. A label declared only with BreakAfter does not match.
func BeforeLabel(label string) BPMatcher {
	return BPMatcher{Position: BPPositionBefore, Kind: BPMatchLabel, Pattern: label}
}

// AfterLabel stops the dependents of every step that declares label with
// BreakAfter, after that step completes. A label declared only with
// BreakBefore does not match.
func AfterLabel(label string) BPMatcher {
	return BPMatcher{Position: BPPositionAfter, Kind: BPMatchLabel, Pattern: label}
}

// Key is the matcher's stable identity, "<position>:<kind>:<pattern>" (e.g.
// "before:step:upgrade-node-*"). Arming two matchers with the same Key arms one
// breakpoint. Position and kind never contain ':', so the first two colons
// always delimit the fields even when the pattern contains colons.
func (m BPMatcher) Key() string {
	return string(m.Position) + ":" + string(m.Kind) + ":" + m.Pattern
}

// Validate reports a matcher no API should accept: an unknown position or
// kind, an empty pattern, or a malformed glob for the step and fn kinds. The
// error wraps ErrInvalidMatcher.
func (m BPMatcher) Validate() error {
	switch m.Position {
	case BPPositionBefore, BPPositionAfter:
	default:
		return fmt.Errorf("%w: unknown position %q (want %q or %q)", ErrInvalidMatcher, m.Position, BPPositionBefore, BPPositionAfter)
	}
	if m.Pattern == "" {
		return fmt.Errorf("%w: %s %s: empty pattern", ErrInvalidMatcher, m.Position, m.Kind)
	}
	switch m.Kind {
	case BPMatchStep, BPMatchFn:
		if _, err := path.Match(m.Pattern, ""); err != nil {
			return fmt.Errorf("%w: %s: bad glob: %w", ErrInvalidMatcher, m.Key(), err)
		}
	case BPMatchLabel:
	default:
		return fmt.Errorf("%w: unknown kind %q (want %q, %q or %q)", ErrInvalidMatcher, m.Kind, BPMatchStep, BPMatchFn, BPMatchLabel)
	}
	return nil
}

// Matches reports whether rec is selected by m. It looks only at immutable
// step config (ID, function name, declared labels), never at status, so the
// answer for a given step never changes. An invalid matcher matches nothing.
func (m BPMatcher) Matches(rec StepRecord) bool {
	switch m.Kind {
	case BPMatchStep:
		return globMatch(m.Pattern, rec.StepID)
	case BPMatchFn:
		return globMatch(m.Pattern, rec.FnName)
	case BPMatchLabel:
		if m.Pattern == "" {
			return false
		}
		switch m.Position {
		case BPPositionBefore:
			return slices.Contains(rec.BreakBefore, m.Pattern)
		case BPPositionAfter:
			return slices.Contains(rec.BreakAfter, m.Pattern)
		}
	}
	return false
}

func globMatch(pattern, name string) bool {
	if pattern == "" {
		return false
	}
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}
