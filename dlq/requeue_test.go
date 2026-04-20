package dlq_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/client"
	"github.com/f1bonacc1/ebind/dlq"
	"github.com/f1bonacc1/ebind/internal/testutil"
	"github.com/f1bonacc1/ebind/stream"
	"github.com/f1bonacc1/ebind/task"
	"github.com/f1bonacc1/ebind/worker"
)

func reflectValueOf(v any) reflect.Value { return reflect.ValueOf(v) }

func TestRequeue_RepublishesAndDeletes(t *testing.T) {
	var attempts atomic.Int32
	fail := func(_ context.Context, msg string) (string, error) {
		n := attempts.Add(1)
		if n == 1 {
			return "", errors.New("forced fail")
		}
		return "ok:" + msg, nil
	}

	h := testutil.SingleNode(t, worker.Options{Concurrency: 1, MaxDeliver: 1})
	task.MustRegister(h.Reg, fail)
	fnName := task.CanonicalName(reflectValueOf(fail))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fut, err := client.Enqueue(h.Client, fail, "hello")
	if err != nil {
		t.Fatal(err)
	}
	var out string
	if err := fut.Get(ctx, &out); err == nil {
		t.Fatal("expected first attempt to fail")
	}

	seq, entry := waitForDLQ(t, ctx, h.JS, fnName)

	if entry.Error.Message != "forced fail" {
		t.Errorf("unexpected error message: %q", entry.Error.Message)
	}

	if err := dlq.Requeue(ctx, h.JS, seq); err != nil {
		t.Fatalf("Requeue: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if attempts.Load() >= 2 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("requeued task never reran; attempts=%d", attempts.Load())
}

func TestRequeue_NotFound(t *testing.T) {
	h := testutil.SingleNode(t, worker.Options{Concurrency: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := dlq.Requeue(ctx, h.JS, 99999); !errors.Is(err, dlq.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func waitForDLQ(t *testing.T, ctx context.Context, js jetstream.JetStream, fnName string) (uint64, dlq.Entry) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		s, err := js.Stream(ctx, stream.DLQStream)
		if err != nil {
			t.Fatal(err)
		}
		info, err := s.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.State.Msgs > 0 {
			for seq := info.State.FirstSeq; seq <= info.State.LastSeq; seq++ {
				msg, err := s.GetMsg(ctx, seq)
				if err != nil {
					continue
				}
				var e dlq.Entry
				if err := json.Unmarshal(msg.Data, &e); err != nil {
					continue
				}
				if e.Task.Name == fnName {
					return seq, e
				}
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s in DLQ", fnName)
	return 0, dlq.Entry{}
}
