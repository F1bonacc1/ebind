package workflow

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/task"
)

// NatsEnqueuer publishes a task.Task envelope to the TASKS stream.
// The envelope is marshaled and sent with Nats-Msg-Id = task.ID for dedupe.
type NatsEnqueuer struct {
	js jetstream.JetStream
}

func NewNatsEnqueuer(js jetstream.JetStream) *NatsEnqueuer { return &NatsEnqueuer{js: js} }

func (e *NatsEnqueuer) Enqueue(ctx context.Context, t task.Task) error {
	body, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("workflow: marshal task: %w", err)
	}
	_, err = e.js.Publish(ctx, stream.TaskSubjectPrefix+t.Name, body, jetstream.WithMsgID(t.ID))
	return err
}
