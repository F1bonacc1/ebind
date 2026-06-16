package workflow

import (
	"context"
	"encoding/json"
	"fmt"
)

// EventKind distinguishes the scheduler-relevant event types.
type EventKind string

const (
	EventCompleted EventKind = "completed"
	EventStepAdded EventKind = "step_added"
	// EventResumed is published by Resume() to trigger step re-evaluation in the scheduler.
	EventResumed EventKind = "resumed"
	// EventBPHit and EventBPResumed are INFORMATIONAL: published when a step
	// stops at an armed breakpoint / is released by ResumeBreakpoint, so live
	// observers (ebctl dag watch) see breakpoint transitions as they happen.
	// Schedulers Ack-drop them; nothing load-bearing may depend on their
	// delivery — the durable truth is the step records (dag bp ls).
	EventBPHit     EventKind = "bp_hit"
	EventBPResumed EventKind = "bp_resumed"
)

// Event is the payload delivered by EventBus to scheduler subscribers.
type Event struct {
	Kind         EventKind  `json:"kind"`
	DAGID        string     `json:"dag_id"`
	StepID       string     `json:"step_id"`
	Status       StepStatus `json:"status,omitempty"` // for EventCompleted
	ErrorKind    string     `json:"error_kind,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
	BPPosition   BPPosition `json:"bp_position,omitempty"` // for EventBPHit / EventBPResumed
	// BPLabels: for EventBPHit, the position's full label config; for
	// EventBPResumed, the single label the resume was issued with.
	BPLabels []string `json:"bp_labels,omitempty"`
	// Ack acknowledges the event has been processed. Subscribers must call this
	// on every delivered event. Nak redelivers after the implementation-specific delay.
	Ack func() error `json:"-"`
	Nak func() error `json:"-"`
}

// Subscription is returned by EventBus.Subscribe and is stopped via Stop().
type Subscription interface {
	Stop() error
}

// EventBus carries scheduler events between workers.
type EventBus interface {
	// Publish sends a single event payload on the given subject.
	// Subjects follow the convention DAG.<dag_id>.<kind>.<step_id>.
	Publish(ctx context.Context, subject string, data []byte) error
	// Subscribe delivers events matching the filter. The handler must call
	// Event.Ack or Event.Nak exactly once per delivery.
	Subscribe(ctx context.Context, filter string, handler func(Event)) (Subscription, error)
}

// EventSubject builds the on-the-wire subject for an event.
func EventSubject(e Event) string {
	return fmt.Sprintf("DAG.%s.%s.%s", e.DAGID, e.Kind, e.StepID)
}

// MarshalEvent serializes an Event for on-wire delivery (without Ack/Nak).
func MarshalEvent(e Event) ([]byte, error) {
	return json.Marshal(struct {
		Kind         EventKind  `json:"kind"`
		DAGID        string     `json:"dag_id"`
		StepID       string     `json:"step_id"`
		Status       StepStatus `json:"status,omitempty"`
		ErrorKind    string     `json:"error_kind,omitempty"`
		ErrorMessage string     `json:"error_message,omitempty"`
		BPPosition   BPPosition `json:"bp_position,omitempty"`
		BPLabels     []string   `json:"bp_labels,omitempty"`
	}{e.Kind, e.DAGID, e.StepID, e.Status, e.ErrorKind, e.ErrorMessage, e.BPPosition, e.BPLabels})
}

// UnmarshalEvent deserializes a wire payload into an Event. Ack/Nak are not populated
// here; the EventBus implementation must attach them before invoking the handler.
func UnmarshalEvent(data []byte) (Event, error) {
	var e Event
	err := json.Unmarshal(data, &e)
	return e, err
}

// publishEvent marshals e and publishes it on its canonical subject. Whether
// the returned error matters is the caller's call: sites whose correctness
// depends on delivery propagate it; sites with a backstop (the leader sweep)
// or purely informational events (bp_hit/bp_resumed) discard it.
func publishEvent(ctx context.Context, bus EventBus, e Event) error {
	data, err := MarshalEvent(e)
	if err != nil {
		return err
	}
	return bus.Publish(ctx, EventSubject(e), data)
}
