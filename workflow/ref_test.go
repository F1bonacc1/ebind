package workflow

import (
	"encoding/json"
	"testing"
)

func TestRef_MarshalUnmarshal(t *testing.T) {
	r := Ref{StepID: "a", Mode: RefModeRequired}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !IsRef(data) {
		t.Error("IsRef returned false for marshaled Ref")
	}
	decoded, ok := DecodeRef(data)
	if !ok {
		t.Fatal("DecodeRef failed")
	}
	if decoded.StepID != "a" || decoded.Mode != RefModeRequired {
		t.Errorf("decoded: %+v", decoded)
	}
}

func TestIsRef_NegativeCases(t *testing.T) {
	cases := []string{
		`42`,
		`"hello"`,
		`{"foo": "bar"}`,
		`[1, 2, 3]`,
		`null`,
		`true`,
	}
	for _, c := range cases {
		if IsRef(json.RawMessage(c)) {
			t.Errorf("IsRef(%s) = true, want false", c)
		}
	}
}

func TestResolveArgs_Literals(t *testing.T) {
	args := []json.RawMessage{
		json.RawMessage(`42`),
		json.RawMessage(`"x"`),
	}
	out, skip, err := ResolveArgs(args, nil, nil)
	if err != nil || skip {
		t.Fatalf("got err=%v skip=%v", err, skip)
	}
	if string(out[0]) != `42` || string(out[1]) != `"x"` {
		t.Errorf("literals changed: %v", out)
	}
}

func TestResolveArgs_Required_Success(t *testing.T) {
	ref, _ := json.Marshal(Ref{StepID: "up", Mode: RefModeRequired})
	args := []json.RawMessage{json.RawMessage(`1`), ref}
	results := map[string]json.RawMessage{"up": json.RawMessage(`"hello"`)}
	statuses := map[string]StepStatus{"up": StatusDone}

	out, skip, err := ResolveArgs(args, results, statuses)
	if err != nil || skip {
		t.Fatalf("err=%v skip=%v", err, skip)
	}
	if string(out[1]) != `"hello"` {
		t.Errorf("substitution failed: %s", out[1])
	}
}

func TestResolveArgs_Required_UpstreamFailed_Cascades(t *testing.T) {
	ref, _ := json.Marshal(Ref{StepID: "up", Mode: RefModeRequired})
	args := []json.RawMessage{ref}
	statuses := map[string]StepStatus{"up": StatusFailed}
	_, skip, err := ResolveArgs(args, nil, statuses)
	if err != nil {
		t.Fatal(err)
	}
	if !skip {
		t.Error("want skip=true")
	}
}

func TestResolveArgs_OrDefault_UpstreamFailed_Substitutes(t *testing.T) {
	ref, _ := json.Marshal(Ref{StepID: "up", Mode: RefModeOrDefault, Default: json.RawMessage(`99`)})
	args := []json.RawMessage{ref}
	statuses := map[string]StepStatus{"up": StatusFailed}
	out, skip, err := ResolveArgs(args, nil, statuses)
	if err != nil || skip {
		t.Fatalf("err=%v skip=%v", err, skip)
	}
	if string(out[0]) != `99` {
		t.Errorf("want default value, got %s", out[0])
	}
}

func TestResolveArgs_OrDefault_NoDefault_NullFallback(t *testing.T) {
	ref, _ := json.Marshal(Ref{StepID: "up", Mode: RefModeOrDefault})
	args := []json.RawMessage{ref}
	statuses := map[string]StepStatus{"up": StatusSkipped}
	out, _, err := ResolveArgs(args, nil, statuses)
	if err != nil {
		t.Fatal(err)
	}
	if string(out[0]) != `null` {
		t.Errorf("want null, got %s", out[0])
	}
}

func TestResolveArgs_UpstreamDoneButMissingResult(t *testing.T) {
	ref, _ := json.Marshal(Ref{StepID: "up", Mode: RefModeRequired})
	args := []json.RawMessage{ref}
	statuses := map[string]StepStatus{"up": StatusDone}
	_, _, err := ResolveArgs(args, nil, statuses)
	if err == nil {
		t.Error("want error for done-but-no-result")
	}
}

func TestResolveArgs_UpstreamNotTerminal(t *testing.T) {
	ref, _ := json.Marshal(Ref{StepID: "up", Mode: RefModeRequired})
	args := []json.RawMessage{ref}
	statuses := map[string]StepStatus{"up": StatusRunning}
	_, _, err := ResolveArgs(args, nil, statuses)
	if err == nil {
		t.Error("want error for non-terminal upstream")
	}
}

func TestResolveArgs_MixedLiteralsAndRefs(t *testing.T) {
	refA, _ := json.Marshal(Ref{StepID: "a", Mode: RefModeRequired})
	refB, _ := json.Marshal(Ref{StepID: "b", Mode: RefModeOrDefault, Default: json.RawMessage(`"fallback"`)})
	args := []json.RawMessage{
		json.RawMessage(`1`),
		refA,
		json.RawMessage(`"literal"`),
		refB,
	}
	results := map[string]json.RawMessage{"a": json.RawMessage(`42`)}
	statuses := map[string]StepStatus{"a": StatusDone, "b": StatusFailed}
	out, skip, err := ResolveArgs(args, results, statuses)
	if err != nil || skip {
		t.Fatalf("err=%v skip=%v", err, skip)
	}
	want := []string{`1`, `42`, `"literal"`, `"fallback"`}
	for i, w := range want {
		if string(out[i]) != w {
			t.Errorf("arg %d: got %s, want %s", i, out[i], w)
		}
	}
}
