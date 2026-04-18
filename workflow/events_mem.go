package workflow

import (
	"context"
	"strings"
	"sync"
)

// MemBus is an in-memory EventBus for tests. It fans out each published event
// to all matching subscribers. Ack/Nak are no-ops since nothing's durable.
type MemBus struct {
	mu   sync.Mutex
	subs []*memSub
}

type memSub struct {
	filter  string
	handler func(Event)
	stopped bool
}

func (ms *memSub) Stop() error {
	ms.stopped = true
	return nil
}

func NewMemBus() *MemBus { return &MemBus{} }

func (b *MemBus) Publish(_ context.Context, subject string, data []byte) error {
	ev, err := UnmarshalEvent(data)
	if err != nil {
		return err
	}
	// Attach no-op Ack/Nak for tests.
	ev.Ack = func() error { return nil }
	ev.Nak = func() error { return nil }

	b.mu.Lock()
	subs := make([]*memSub, 0, len(b.subs))
	for _, s := range b.subs {
		if !s.stopped && matchSubject(subject, s.filter) {
			subs = append(subs, s)
		}
	}
	b.mu.Unlock()

	for _, s := range subs {
		go s.handler(ev)
	}
	return nil
}

func (b *MemBus) Subscribe(_ context.Context, filter string, handler func(Event)) (Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := &memSub{filter: filter, handler: handler}
	b.subs = append(b.subs, s)
	return s, nil
}

// matchSubject implements a trimmed NATS-style subject matcher: `.` as separator,
// `>` matches one-or-more trailing tokens. Sufficient for tests.
func matchSubject(subject, filter string) bool {
	if filter == subject {
		return true
	}
	sp := strings.Split(subject, ".")
	fp := strings.Split(filter, ".")
	if len(fp) == 0 {
		return false
	}
	if fp[len(fp)-1] == ">" {
		if len(sp) < len(fp) {
			return false
		}
		for i := 0; i < len(fp)-1; i++ {
			if fp[i] != "*" && fp[i] != sp[i] {
				return false
			}
		}
		return true
	}
	if len(sp) != len(fp) {
		return false
	}
	for i := range sp {
		if fp[i] != "*" && fp[i] != sp[i] {
			return false
		}
	}
	return true
}
