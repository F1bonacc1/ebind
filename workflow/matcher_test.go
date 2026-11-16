package workflow

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestBPMatcher_Constructors(t *testing.T) {
	cases := []struct {
		got  BPMatcher
		pos  BPPosition
		kind BPMatchKind
	}{
		{BeforeStep("p"), BPPositionBefore, BPMatchStep},
		{AfterStep("p"), BPPositionAfter, BPMatchStep},
		{BeforeFn("p"), BPPositionBefore, BPMatchFn},
		{AfterFn("p"), BPPositionAfter, BPMatchFn},
		{BeforeLabel("p"), BPPositionBefore, BPMatchLabel},
		{AfterLabel("p"), BPPositionAfter, BPMatchLabel},
	}
	for _, c := range cases {
		want := BPMatcher{Position: c.pos, Kind: c.kind, Pattern: "p"}
		if c.got != want {
			t.Errorf("constructor = %+v, want %+v", c.got, want)
		}
		if err := c.got.Validate(); err != nil {
			t.Errorf("%s: Validate: %v", c.got.Key(), err)
		}
	}
}

func TestBPMatcher_Matches(t *testing.T) {
	rec := StepRecord{
		StepID:      "upgrade-node-1",
		FnName:      "main.Upgrade",
		BreakBefore: []string{"risky"},
		BreakAfter:  []string{"inspect"},
	}
	nested := StepRecord{StepID: "node-1/upgrade", FnName: "ops/handlers.Upgrade"}

	cases := []struct {
		name string
		m    BPMatcher
		rec  StepRecord
		want bool
	}{
		{"step exact", BeforeStep("upgrade-node-1"), rec, true},
		{"step glob", BeforeStep("upgrade-node-*"), rec, true},
		{"step glob after", AfterStep("upgrade-node-?"), rec, true},
		{"step class", BeforeStep("upgrade-node-[12]"), rec, true},
		{"step miss", BeforeStep("deploy-*"), rec, false},
		{"step glob is anchored", BeforeStep("node-*"), rec, false},
		{"step star matches all", BeforeStep("*"), rec, true},

		// F8: path.Match never lets '*' or '?' cross '/'.
		{"slash: star does not cross", BeforeStep("node-*"), nested, false},
		{"slash: star does not cross (lone *)", BeforeStep("*"), nested, false},
		{"slash: explicit segment", BeforeStep("node-*/*"), nested, true},
		{"slash: question does not match slash", BeforeStep("node-1?upgrade"), nested, false},
		{"slash: fn star does not cross", BeforeFn("*.Upgrade"), nested, false},
		{"slash: fn segment", BeforeFn("ops/*.Upgrade"), nested, true},

		{"fn exact", BeforeFn("main.Upgrade"), rec, true},
		{"fn glob", AfterFn("main.Up*"), rec, true},
		{"fn miss", BeforeFn("main.Deploy"), rec, false},
		{"fn does not look at step id", BeforeFn("upgrade-node-*"), rec, false},
		{"step does not look at fn", BeforeStep("main.Upgrade"), rec, false},

		{"label before declared before", BeforeLabel("risky"), rec, true},
		{"label after declared after", AfterLabel("inspect"), rec, true},
		{"label before ignores after-only label", BeforeLabel("inspect"), rec, false},
		{"label after ignores before-only label", AfterLabel("risky"), rec, false},
		{"label is exact, not a glob", BeforeLabel("ris*"), rec, false},
		{"label on unlabeled step", BeforeLabel("risky"), nested, false},

		{"invalid: empty step pattern", BeforeStep(""), rec, false},
		{"invalid: empty label", BeforeLabel(""), StepRecord{BreakBefore: []string{""}}, false},
		{"invalid: bad glob", BeforeStep("upgrade-node-["), rec, false},
		{"invalid: unknown kind", BPMatcher{Position: BPPositionBefore, Kind: "tag", Pattern: "*"}, rec, false},
		{"invalid: unknown label position", BPMatcher{Position: "during", Kind: BPMatchLabel, Pattern: "risky"}, rec, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.m.Matches(c.rec); got != c.want {
				t.Errorf("%s.Matches(%q) = %v, want %v", c.m.Key(), c.rec.StepID, got, c.want)
			}
		})
	}
}

func TestBPMatcher_Validate(t *testing.T) {
	cases := []struct {
		name string
		m    BPMatcher
		ok   bool
	}{
		{"step glob", BeforeStep("a-*"), true},
		{"fn glob", AfterFn("pkg.F*"), true},
		{"label with glob chars is literal", BeforeLabel("a[b"), true},
		{"pattern with colon", BeforeStep("a:b"), true},
		{"empty step", BeforeStep(""), false},
		{"empty fn", AfterFn(""), false},
		{"empty label", AfterLabel(""), false},
		{"bad glob step", BeforeStep("a["), false},
		{"bad glob fn", BeforeFn("[]"), false},
		// path.Match must reject the malformed glob even though "" never
		// matches the literal prefix: Validate relies on that.
		{"bad glob after literal prefix", BeforeStep("abc[z-"), false},
		{"trailing backslash", BeforeStep(`a\`), false},
		{"unknown kind", BPMatcher{Position: BPPositionBefore, Kind: "tag", Pattern: "x"}, false},
		{"empty kind", BPMatcher{Position: BPPositionBefore, Pattern: "x"}, false},
		{"unknown position", BPMatcher{Position: "during", Kind: BPMatchStep, Pattern: "x"}, false},
		{"zero value", BPMatcher{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.m.Validate()
			if c.ok && err != nil {
				t.Fatalf("Validate(%+v) = %v, want nil", c.m, err)
			}
			if !c.ok && !errors.Is(err, ErrInvalidMatcher) {
				t.Fatalf("Validate(%+v) = %v, want ErrInvalidMatcher", c.m, err)
			}
		})
	}
}

func TestBPMatcher_Key(t *testing.T) {
	cases := []struct {
		m    BPMatcher
		want string
	}{
		{BeforeStep("upgrade-node-*"), "before:step:upgrade-node-*"},
		{AfterStep("test"), "after:step:test"},
		{BeforeFn("main.Charge"), "before:fn:main.Charge"},
		{AfterFn("main.Prepare"), "after:fn:main.Prepare"},
		{BeforeLabel("debug"), "before:label:debug"},
		{AfterLabel("a:b"), "after:label:a:b"},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		if got := c.m.Key(); got != c.want {
			t.Errorf("Key() = %q, want %q", got, c.want)
		}
		if seen[c.m.Key()] {
			t.Errorf("duplicate key %q", c.m.Key())
		}
		seen[c.m.Key()] = true
	}
	// The table above pins the exact strings (stability); any differing field
	// must give a different identity.
	for _, other := range []BPMatcher{AfterStep("x"), BeforeFn("x"), BeforeLabel("x"), BeforeStep("y")} {
		if other.Key() == BeforeStep("x").Key() {
			t.Errorf("%+v collides with before:step:x", other)
		}
	}
}

// The JSON shape is persisted from P5 on (DAGMeta.ArmedMatchers); pin it.
func TestBPMatcher_JSON(t *testing.T) {
	b, err := json.Marshal(BeforeFn("main.Charge"))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"position":"before","kind":"fn","pattern":"main.Charge"}`
	if string(b) != want {
		t.Fatalf("json = %s, want %s", b, want)
	}
	var back BPMatcher
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back != BeforeFn("main.Charge") {
		t.Fatalf("round trip = %+v", back)
	}
}
