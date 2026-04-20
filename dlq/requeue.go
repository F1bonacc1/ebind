package dlq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/stream"
)

// ErrNotFound is returned when the given DLQ sequence does not exist.
var ErrNotFound = errors.New("dlq: entry not found")

// Fetch returns the DLQ Entry at the given stream sequence. Useful for CLIs
// that want to inspect an entry before choosing to requeue or purge.
func Fetch(ctx context.Context, js jetstream.JetStream, streamSeq uint64) (Entry, error) {
	s, err := js.Stream(ctx, stream.DLQStream)
	if err != nil {
		return Entry{}, fmt.Errorf("dlq: open stream: %w", err)
	}
	msg, err := s.GetMsg(ctx, streamSeq)
	if err != nil {
		if errors.Is(err, jetstream.ErrMsgNotFound) {
			return Entry{}, ErrNotFound
		}
		return Entry{}, fmt.Errorf("dlq: get msg %d: %w", streamSeq, err)
	}
	var entry Entry
	if err := json.Unmarshal(msg.Data, &entry); err != nil {
		return Entry{}, fmt.Errorf("dlq: decode entry: %w", err)
	}
	return entry, nil
}

// Requeue republishes the DLQ entry at streamSeq back onto EBIND_TASKS and,
// on success, deletes the DLQ message.
//
// The republished envelope keeps the original ID, Name, Payload, DAGID/StepID,
// Target, and RetryPolicy but resets Attempt and EnqueuedAt. Because the task
// may have been dead-lettered a long time ago, the Nats-Msg-Id is a freshly
// generated UUID: reusing the original task.ID could collide with the
// 5-minute JetStream dedupe window if the caller requeues after a retry storm.
//
// Returns ErrNotFound if streamSeq no longer exists in the DLQ.
func Requeue(ctx context.Context, js jetstream.JetStream, streamSeq uint64) error {
	entry, err := Fetch(ctx, js, streamSeq)
	if err != nil {
		return err
	}

	t := entry.Task
	t.Attempt = 0
	t.EnqueuedAt = time.Now().UTC()
	t.WorkerID = ""

	body, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("dlq: marshal task: %w", err)
	}
	subject := stream.TaskPublishSubject(t.Name, t.Target)
	msgID := uuid.NewString()
	if _, err := js.Publish(ctx, subject, body, jetstream.WithMsgID(msgID)); err != nil {
		return fmt.Errorf("dlq: republish: %w", err)
	}

	s, err := js.Stream(ctx, stream.DLQStream)
	if err != nil {
		return fmt.Errorf("dlq: open stream: %w", err)
	}
	if err := s.DeleteMsg(ctx, streamSeq); err != nil {
		return fmt.Errorf("dlq: delete msg %d after requeue: %w", streamSeq, err)
	}
	return nil
}
