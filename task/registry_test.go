package task

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type sampleStruct struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

func noArgs(ctx context.Context) (string, error) {
	return "ok", nil
}

func oneArg(ctx context.Context, x int) (int, error) {
	return x * 2, nil
}

func multiArgs(ctx context.Context, count int, name string, meta sampleStruct) (string, error) {
	return name, nil
}

func errorOnly(ctx context.Context, who string) error {
	if who == "" {
		return errors.New("empty")
	}
	return nil
}

func returnsCustomError(ctx context.Context) error {
	return &TaskError{Kind: "custom", Message: "boom", Retryable: false}
}

func TestRegister_ValidSignatures(t *testing.T) {
	r := NewRegistry()
	cases := []any{noArgs, oneArg, multiArgs, errorOnly, returnsCustomError}
	for _, fn := range cases {
		if err := Register(r, fn); err != nil {
			t.Errorf("Register %T: unexpected error: %v", fn, err)
		}
	}
	if got := len(r.Names()); got != len(cases) {
		t.Errorf("want %d handlers, got %d (%v)", len(cases), got, r.Names())
	}
}

func TestRegister_InvalidSignatures(t *testing.T) {
	r := NewRegistry()
	bad := []any{
		"not a function",
		42,
		func() error { return nil },      // no ctx
		func(x int) error { return nil }, // first param not ctx
		func(ctx context.Context) {},     // no returns
		func(ctx context.Context) (int, int, error) { return 0, 0, nil }, // 3 returns
		func(ctx context.Context) int { return 0 },                       // last return not error
	}
	for _, fn := range bad {
		if err := Register(r, fn); err == nil {
			t.Errorf("Register %T: expected error, got nil", fn)
		}
	}
}

func TestRegister_Duplicate(t *testing.T) {
	r := NewRegistry()
	if err := Register(r, oneArg); err != nil {
		t.Fatal(err)
	}
	if err := Register(r, oneArg); err == nil {
		t.Fatal("expected duplicate error")
	}
}

func TestRegister_WithName(t *testing.T) {
	r := NewRegistry()
	if err := Register(r, oneArg, WithName("custom.name")); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("custom.name"); !ok {
		t.Fatal("custom name not registered")
	}
}

func TestRegister_Alias(t *testing.T) {
	r := NewRegistry()
	if err := Register(r, oneArg, Alias("old.name")); err != nil {
		t.Fatal(err)
	}
	d1, ok1 := r.Get("task.oneArg")
	d2, ok2 := r.Get("old.name")
	if !ok1 || !ok2 {
		t.Fatalf("canonical=%v alias=%v", ok1, ok2)
	}
	if d1 != d2 {
		t.Fatal("alias should point to same dispatcher")
	}
}

func TestDispatcher_Call_MultiArgs(t *testing.T) {
	r := NewRegistry()
	if err := Register(r, multiArgs); err != nil {
		t.Fatal(err)
	}
	d, _ := r.Get("task.multiArgs")

	payload, _ := json.Marshal([]any{7, "hello", sampleStruct{Tag: "t", Count: 3}})
	result, err := d.Call(context.Background(), payload)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got string
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatal(err)
	}
	if got != "hello" {
		t.Errorf("want hello, got %q", got)
	}
}

func TestDispatcher_Call_ErrorOnly(t *testing.T) {
	r := NewRegistry()
	if err := Register(r, errorOnly); err != nil {
		t.Fatal(err)
	}
	d, _ := r.Get("task.errorOnly")

	// Happy path.
	payload, _ := json.Marshal([]any{"alice"})
	result, err := d.Call(context.Background(), payload)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result != nil {
		t.Errorf("error-only handler should return nil result, got %q", result)
	}

	// Error path.
	payload2, _ := json.Marshal([]any{""})
	_, err = d.Call(context.Background(), payload2)
	if err == nil {
		t.Fatal("expected error")
	}
	var te *TaskError
	if !errors.As(err, &te) {
		t.Fatalf("want *TaskError, got %T", err)
	}
	if te.Kind != ErrKindHandler {
		t.Errorf("want Kind=handler, got %q", te.Kind)
	}
}

func TestDispatcher_Call_ArgCountMismatch(t *testing.T) {
	r := NewRegistry()
	Register(r, multiArgs)
	d, _ := r.Get("task.multiArgs")

	payload, _ := json.Marshal([]any{1, "only-two-args"}) // want 3
	_, err := d.Call(context.Background(), payload)
	var te *TaskError
	if !errors.As(err, &te) || te.Kind != ErrKindArgCount {
		t.Fatalf("want arg_count TaskError, got %v", err)
	}
}

func TestDispatcher_Call_CustomTaskErrorPreserved(t *testing.T) {
	r := NewRegistry()
	Register(r, returnsCustomError)
	d, _ := r.Get("task.returnsCustomError")
	payload, _ := json.Marshal([]any{})
	_, err := d.Call(context.Background(), payload)
	var te *TaskError
	if !errors.As(err, &te) {
		t.Fatalf("want *TaskError, got %T", err)
	}
	if te.Kind != "custom" || te.Message != "boom" {
		t.Errorf("TaskError fields lost: %+v", te)
	}
}

func TestDescribe_ValidateArgs(t *testing.T) {
	d, err := Describe(multiArgs)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ValidateArgs([]any{1, "x", sampleStruct{}}); err != nil {
		t.Errorf("valid args rejected: %v", err)
	}
	err = d.ValidateArgs([]any{"wrong", "x", sampleStruct{}})
	if err == nil || !strings.Contains(err.Error(), "arg 0") {
		t.Errorf("want arg 0 type error, got %v", err)
	}
	err = d.ValidateArgs([]any{1, "x"})
	if err == nil || !strings.Contains(err.Error(), "arg count") {
		t.Errorf("want arg count error, got %v", err)
	}
}

func TestCanonicalName(t *testing.T) {
	d, err := Describe(multiArgs)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "task.multiArgs" {
		t.Errorf("canonical name: got %q, want task.multiArgs", d.Name)
	}
}
