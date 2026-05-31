package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/f1bonacc1/ebind/task"
)

// seedRunningStep puts a Running step record so the hook's CAS update has a
// non-terminal record to transition.
func seedRunningStep(t *testing.T, store StateStore, dagID, stepID string) {
	t.Helper()
	rec := StepRecord{DAGID: dagID, StepID: stepID, Status: StatusRunning}
	if err := store.PutStep(context.Background(), dagID, stepID, rec, 0); err != nil {
		t.Fatalf("seed step: %v", err)
	}
}

// TestStepHook_OnStepFailed_PersistsErrorMessage is the core of the feature:
// a failed step's error message must be persisted into the durable step record,
// not only the error kind.
func TestStepHook_OnStepFailed_PersistsErrorMessage(t *testing.T) {
	store := NewMemStore()
	hook := (&Workflow{Store: store, Bus: NewMemBus()}).Hook()
	seedRunningStep(t, store, "dag1", "s1")

	taskErr := &task.TaskError{Kind: "handler", Message: "boom: connection refused", Retryable: false}
	tk := &task.Task{DAGID: "dag1", StepID: "s1"}
	if err := hook.OnStepFailed(context.Background(), tk, taskErr); err != nil {
		t.Fatalf("OnStepFailed: %v", err)
	}

	rec, _, err := store.GetStep(context.Background(), "dag1", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != StatusFailed {
		t.Errorf("status = %s, want failed", rec.Status)
	}
	if rec.ErrorKind != "handler" {
		t.Errorf("ErrorKind = %q, want handler", rec.ErrorKind)
	}
	if rec.ErrorMessage != "boom: connection refused" {
		t.Errorf("ErrorMessage = %q, want full message", rec.ErrorMessage)
	}
}

// TestStepHook_OnStepFailed_EventCarriesMessage verifies the completion event
// (consumed by the scheduler) also carries the message so the in-memory state
// stays consistent with the persisted record.
func TestStepHook_OnStepFailed_EventCarriesMessage(t *testing.T) {
	store := NewMemStore()
	bus := NewMemBus()
	got := make(chan Event, 1)
	if _, err := bus.Subscribe(context.Background(), "DAG.>", func(ev Event) {
		got <- ev
		_ = ev.Ack()
	}); err != nil {
		t.Fatal(err)
	}
	hook := (&Workflow{Store: store, Bus: bus}).Hook()
	seedRunningStep(t, store, "dag1", "s1")

	taskErr := &task.TaskError{Kind: "handler", Message: "kaboom"}
	if err := hook.OnStepFailed(context.Background(), &task.Task{DAGID: "dag1", StepID: "s1"}, taskErr); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-got:
		if ev.Status != StatusFailed || ev.ErrorKind != "handler" || ev.ErrorMessage != "kaboom" {
			t.Errorf("event = %+v, want failed/handler/kaboom", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for completion event")
	}
}

func TestStepHook_OnStepFailed_TruncatesLongMessage(t *testing.T) {
	store := NewMemStore()
	hook := (&Workflow{Store: store, Bus: NewMemBus(), MaxStepErrorBytes: 16}).Hook()
	seedRunningStep(t, store, "dag1", "s1")

	long := strings.Repeat("x", 1000)
	if err := hook.OnStepFailed(context.Background(), &task.Task{DAGID: "dag1", StepID: "s1"},
		&task.TaskError{Kind: "handler", Message: long}); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := store.GetStep(context.Background(), "dag1", "s1")
	if want := strings.Repeat("x", 16) + "…"; rec.ErrorMessage != want {
		t.Errorf("ErrorMessage = %q (len %d), want %q", rec.ErrorMessage, len(rec.ErrorMessage), want)
	}
}

// TestStepHook_OnStepFailed_DisabledMessage covers the compliance opt-out:
// negative MaxStepErrorBytes keeps the kind but stores no message.
func TestStepHook_OnStepFailed_DisabledMessage(t *testing.T) {
	store := NewMemStore()
	hook := (&Workflow{Store: store, Bus: NewMemBus(), MaxStepErrorBytes: -1}).Hook()
	seedRunningStep(t, store, "dag1", "s1")

	if err := hook.OnStepFailed(context.Background(), &task.Task{DAGID: "dag1", StepID: "s1"},
		&task.TaskError{Kind: "handler", Message: "secret://creds"}); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := store.GetStep(context.Background(), "dag1", "s1")
	if rec.ErrorKind != "handler" {
		t.Errorf("kind should still be stored, got %q", rec.ErrorKind)
	}
	if rec.ErrorMessage != "" {
		t.Errorf("ErrorMessage should be empty when disabled, got %q", rec.ErrorMessage)
	}
}

func TestTruncateErrorMessage(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"under default", "short", 0, "short"},
		{"disabled", "anything", -1, ""},
		{"exact", "abcd", 4, "abcd"},
		{"over", "abcdef", 4, "abcd…"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := truncateErrorMessage(c.in, c.max); got != c.want {
				t.Errorf("truncateErrorMessage(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
			}
		})
	}
}
