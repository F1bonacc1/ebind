package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/f1bonacc1/ebind/workflow"
)

func TestMatcherFlags_Matcher(t *testing.T) {
	cases := []struct {
		name    string
		f       MatcherFlags
		want    workflow.BPMatcher
		wantErr string
	}{
		{"before step", MatcherFlags{Before: true, Step: "node-*"}, workflow.BeforeStep("node-*"), ""},
		{"after step", MatcherFlags{After: true, Step: "test"}, workflow.AfterStep("test"), ""},
		{"before fn", MatcherFlags{Before: true, Fn: "main.Charge"}, workflow.BeforeFn("main.Charge"), ""},
		{"after fn", MatcherFlags{After: true, Fn: "main.Pre*"}, workflow.AfterFn("main.Pre*"), ""},
		{"before label", MatcherFlags{Before: true, Label: "debug"}, workflow.BeforeLabel("debug"), ""},
		{"after label", MatcherFlags{After: true, Label: "debug"}, workflow.AfterLabel("debug"), ""},

		{"no position", MatcherFlags{Step: "x"}, workflow.BPMatcher{}, "--before or --after is required"},
		{"both positions", MatcherFlags{Before: true, After: true, Step: "x"}, workflow.BPMatcher{}, "mutually exclusive"},
		{"no kind", MatcherFlags{Before: true}, workflow.BPMatcher{}, "--step, --fn or --label is required"},
		{"two kinds", MatcherFlags{Before: true, Step: "x", Fn: "y"}, workflow.BPMatcher{}, "mutually exclusive"},
		{"three kinds", MatcherFlags{After: true, Step: "x", Fn: "y", Label: "z"}, workflow.BPMatcher{}, "mutually exclusive"},
		{"bad glob", MatcherFlags{Before: true, Step: "node-["}, workflow.BPMatcher{}, "invalid matcher"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.f.Matcher()
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("matcher = %+v, want %+v", got, c.want)
			}
		})
	}
	if _, err := (&MatcherFlags{Before: true, Fn: "["}).Matcher(); !errors.Is(err, workflow.ErrInvalidMatcher) {
		t.Fatalf("bad glob err = %v, want wrapping ErrInvalidMatcher", err)
	}
}

// runWithFlags parses args on a throwaway command that uses AddMatcherFlags,
// the way a real `bp` subcommand would.
func runWithFlags(args ...string) (workflow.BPMatcher, error) {
	var got workflow.BPMatcher
	cmd := &cobra.Command{Use: "t", SilenceUsage: true, SilenceErrors: true}
	f := AddMatcherFlags(cmd)
	cmd.RunE = func(*cobra.Command, []string) error {
		m, err := f.Matcher()
		got = m
		return err
	}
	cmd.SetArgs(args)
	err := cmd.Execute()
	return got, err
}

func TestAddMatcherFlags_Parse(t *testing.T) {
	m, err := runWithFlags("--after", "--fn", "main.Prepare")
	if err != nil {
		t.Fatal(err)
	}
	if m != workflow.AfterFn("main.Prepare") {
		t.Fatalf("matcher = %+v", m)
	}

	for _, args := range [][]string{
		{"--before", "--after", "--step", "x"},
		{"--before", "--step", "x", "--label", "y"},
		{"--before"},
		{"--step", "x"},
		{"--before", "--step", ""},
	} {
		if _, err := runWithFlags(args...); err == nil {
			t.Errorf("args %q: want error", args)
		}
	}
}
