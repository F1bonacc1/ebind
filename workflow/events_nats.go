package workflow

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

const (
	DAGEventsStream = "EBIND_DAG_EVENTS"
	dagEventSubject = "DAG.>"
)

// NatsBus is a JetStream-backed EventBus. Events durable in a dedicated stream;
// subscribers use a shared durable consumer so each event is handled once cluster-wide.
type NatsBus struct {
	js           jetstream.JetStream
	consumerName string
	eventsStream jetstream.Stream
}

// NewNatsBus creates (or opens) the DAG events stream.
func NewNatsBus(ctx context.Context, js jetstream.JetStream, replicas int) (*NatsBus, error) {
	if replicas <= 0 {
		replicas = 1
	}
	stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      DAGEventsStream,
		Subjects:  []string{dagEventSubject},
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.FileStorage,
		Replicas:  replicas,
		MaxAge:    24 * time.Hour,
	})
	if err != nil {
		return nil, fmt.Errorf("workflow: create events stream: %w", err)
	}
	return &NatsBus{js: js, eventsStream: stream, consumerName: "ebind-scheduler"}, nil
}

func (b *NatsBus) Publish(ctx context.Context, subject string, data []byte) error {
	_, err := b.js.Publish(ctx, subject, data)
	return err
}

func (b *NatsBus) Subscribe(ctx context.Context, filter string, handler func(Event)) (Subscription, error) {
	cons, err := b.eventsStream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       b.consumerName,
		FilterSubject: filter,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    -1, // retry forever; scheduler's non-leader path Naks on purpose
	})
	if err != nil {
		return nil, err
	}
	consumeCtx, err := cons.Consume(func(msg jetstream.Msg) {
		ev, err := UnmarshalEvent(msg.Data())
		if err != nil {
			_ = msg.Term()
			return
		}
		ev.Ack = func() error { return msg.Ack() }
		ev.Nak = func() error { return msg.NakWithDelay(time.Second) }
		handler(ev)
	})
	if err != nil {
		return nil, err
	}
	return &natsSub{cc: consumeCtx}, nil
}

type natsSub struct {
	cc jetstream.ConsumeContext
}

func (s *natsSub) Stop() error { s.cc.Stop(); return nil }
