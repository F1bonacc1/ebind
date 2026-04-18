// Package dlq publishes poison tasks (those that exhausted MaxDeliver or failed non-retryably)
// to the DLQ stream for operator review.
package dlq

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/task"
)

type Entry struct {
	Task       task.Task      `json:"task"`
	Error      task.TaskError `json:"error"`
	FinalAttempt int          `json:"final_attempt"`
	DeadLetteredAt time.Time  `json:"dead_lettered_at"`
}

// Publish sends a dead-letter entry to DLQ.<task_name>.
func Publish(ctx context.Context, js jetstream.JetStream, t *task.Task, taskErr *task.TaskError) error {
	entry := Entry{
		Task:           *t,
		Error:          *taskErr,
		FinalAttempt:   t.Attempt,
		DeadLetteredAt: time.Now().UTC(),
	}
	body, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = js.Publish(pubCtx, stream.DLQSubjectPrefix+t.Name, body)
	return err
}
