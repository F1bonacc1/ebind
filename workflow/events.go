package workflow

import (
	"context"
	"encoding/json"
	"fmt"
)

// EventKind distinguishes the two scheduler-relevant events.
type EventKind string

const (
	EventCompleted EventKind = "completed"
	EventStepAdded EventKind = "step_added"
)

// Event is the payload delivered by EventBus to scheduler subscribers.
type Event struct {
	Kind      EventKind  `json:"kind"`
	DAGID     string     `json:"dag_id"`
	StepID    string     `json:"step_id"`
	Status    StepStatus `json:"status,omitempty"` // for EventCompleted
	ErrorKind string     `json:"error_kind,omitempty"`
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
		Kind      EventKind  `json:"kind"`
		DAGID     string     `json:"dag_id"`
		StepID    string     `json:"step_id"`
		Status    StepStatus `json:"status,omitempty"`
		ErrorKind string     `json:"error_kind,omitempty"`
	}{e.Kind, e.DAGID, e.StepID, e.Status, e.ErrorKind})
}

// UnmarshalEvent deserializes a wire payload into an Event. Ack/Nak are not populated
// here; the EventBus implementation must attach them before invoking the handler.
func UnmarshalEvent(data []byte) (Event, error) {
	var e Event
	err := json.Unmarshal(data, &e)
	return e, err
}
