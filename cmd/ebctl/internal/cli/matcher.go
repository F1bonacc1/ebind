package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/workflow"
)

// MatcherFlags is the one flag set every `dag bp` subcommand that takes a
// breakpoint matcher shares: exactly one of --before/--after and exactly one
// of --step/--fn/--label. One matcher per invocation; there is no string
// syntax to parse.
type MatcherFlags struct {
	Before bool
	After  bool
	Step   string
	Fn     string
	Label  string
}

// AddMatcherFlags registers the matcher flags on cmd and returns the struct
// they fill. Call Matcher in RunE to build and validate the matcher.
func AddMatcherFlags(cmd *cobra.Command) *MatcherFlags {
	f := &MatcherFlags{}
	fs := cmd.Flags()
	fs.BoolVar(&f.Before, "before", false, "match the before-execution position (one of --before/--after)")
	fs.BoolVar(&f.After, "after", false, "match the after-execution position (one of --before/--after)")
	fs.StringVar(&f.Step, "step", "", "step ID glob, path.Match syntax: '*' does not cross '/' (one of --step/--fn/--label)")
	fs.StringVar(&f.Fn, "fn", "", "canonical function name glob, e.g. 'main.Transform*' (one of --step/--fn/--label)")
	fs.StringVar(&f.Label, "label", "", "breakpoint label declared with BreakBefore/BreakAfter, exact (one of --step/--fn/--label)")
	cmd.MarkFlagsMutuallyExclusive("before", "after")
	cmd.MarkFlagsMutuallyExclusive("step", "fn", "label")
	return f
}

// Matcher builds the matcher the flags describe. It checks the flag
// combination itself (so it does not depend on cobra having run its own
// group checks) and then the matcher's Validate.
func (f *MatcherFlags) Matcher() (workflow.BPMatcher, error) {
	var pos workflow.BPPosition
	switch {
	case f.Before && f.After:
		return workflow.BPMatcher{}, errors.New("--before and --after are mutually exclusive")
	case f.Before:
		pos = workflow.BPPositionBefore
	case f.After:
		pos = workflow.BPPositionAfter
	default:
		return workflow.BPMatcher{}, errors.New("one of --before or --after is required")
	}

	var m workflow.BPMatcher
	set := 0
	if f.Step != "" {
		set++
		m = workflow.BPMatcher{Kind: workflow.BPMatchStep, Pattern: f.Step}
	}
	if f.Fn != "" {
		set++
		m = workflow.BPMatcher{Kind: workflow.BPMatchFn, Pattern: f.Fn}
	}
	if f.Label != "" {
		set++
		m = workflow.BPMatcher{Kind: workflow.BPMatchLabel, Pattern: f.Label}
	}
	switch set {
	case 0:
		return workflow.BPMatcher{}, errors.New("one of --step, --fn or --label is required (with a non-empty pattern)")
	case 1:
	default:
		return workflow.BPMatcher{}, errors.New("--step, --fn and --label are mutually exclusive")
	}
	m.Position = pos
	if err := m.Validate(); err != nil {
		return workflow.BPMatcher{}, fmt.Errorf("invalid matcher: %w", err)
	}
	return m, nil
}
