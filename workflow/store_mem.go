package workflow

import (
	"context"
	"sync"
)

// MemStore is an in-memory StateStore suitable for tests and local dev.
// It emulates NATS KV's revision-based CAS: each Put* increments the stored
// revision; callers must pass the matching expectedRev or get ErrStaleRevision.
type MemStore struct {
	mu      sync.Mutex
	steps   map[string]map[string]memEntry // dagID → stepID → entry
	results map[string]map[string][]byte   // dagID → stepID → bytes
	metas   map[string]memMetaEntry        // dagID → entry
	watches map[string][]chan []byte       // "dagID/stepID" → waiters
}

type memEntry struct {
	rec StepRecord
	rev uint64
}

type memMetaEntry struct {
	meta DAGMeta
	rev  uint64
}

func NewMemStore() *MemStore {
	return &MemStore{
		steps:   map[string]map[string]memEntry{},
		results: map[string]map[string][]byte{},
		metas:   map[string]memMetaEntry{},
		watches: map[string][]chan []byte{},
	}
}

func (s *MemStore) GetStep(_ context.Context, dagID, stepID string) (StepRecord, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.steps[dagID]
	if !ok {
		return StepRecord{}, 0, ErrStepNotFound
	}
	e, ok := bucket[stepID]
	if !ok {
		return StepRecord{}, 0, ErrStepNotFound
	}
	return e.rec, e.rev, nil
}

func (s *MemStore) PutStep(_ context.Context, dagID, stepID string, rec StepRecord, expectedRev uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.steps[dagID]
	if !ok {
		bucket = map[string]memEntry{}
		s.steps[dagID] = bucket
	}
	existing, exists := bucket[stepID]
	if expectedRev == 0 {
		if exists {
			return ErrStaleRevision
		}
		bucket[stepID] = memEntry{rec: rec, rev: 1}
		return nil
	}
	if !exists || existing.rev != expectedRev {
		return ErrStaleRevision
	}
	bucket[stepID] = memEntry{rec: rec, rev: existing.rev + 1}
	return nil
}

func (s *MemStore) ListSteps(_ context.Context, dagID string) ([]StepRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.steps[dagID]
	if !ok {
		return nil, nil
	}
	out := make([]StepRecord, 0, len(bucket))
	for _, e := range bucket {
		out = append(out, e.rec)
	}
	return out, nil
}

func (s *MemStore) GetResult(_ context.Context, dagID, stepID string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.results[dagID]
	if !ok {
		return nil, ErrStepNotFound
	}
	data, ok := bucket[stepID]
	if !ok {
		return nil, ErrStepNotFound
	}
	return append([]byte(nil), data...), nil
}

func (s *MemStore) PutResult(_ context.Context, dagID, stepID string, data []byte) error {
	s.mu.Lock()
	bucket, ok := s.results[dagID]
	if !ok {
		bucket = map[string][]byte{}
		s.results[dagID] = bucket
	}
	bucket[stepID] = append([]byte(nil), data...)
	key := dagID + "/" + stepID
	waiters := s.watches[key]
	delete(s.watches, key)
	s.mu.Unlock()
	for _, ch := range waiters {
		select {
		case ch <- append([]byte(nil), data...):
		default:
		}
		close(ch)
	}
	return nil
}

func (s *MemStore) GetMeta(_ context.Context, dagID string) (DAGMeta, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.metas[dagID]
	if !ok {
		return DAGMeta{}, 0, ErrDAGNotFound
	}
	return e.meta, e.rev, nil
}

func (s *MemStore) ListDAGs(_ context.Context) ([]DAGMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DAGMeta, 0, len(s.metas))
	for _, e := range s.metas {
		out = append(out, e.meta)
	}
	return out, nil
}

func (s *MemStore) PutMeta(_ context.Context, dagID string, meta DAGMeta, expectedRev uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, exists := s.metas[dagID]
	if expectedRev == 0 {
		if exists {
			return ErrStaleRevision
		}
		s.metas[dagID] = memMetaEntry{meta: meta, rev: 1}
		return nil
	}
	if !exists || existing.rev != expectedRev {
		return ErrStaleRevision
	}
	s.metas[dagID] = memMetaEntry{meta: meta, rev: existing.rev + 1}
	return nil
}

func (s *MemStore) DeleteMeta(_ context.Context, dagID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.metas, dagID)
	return nil
}

func (s *MemStore) DeleteStep(_ context.Context, dagID, stepID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bucket, ok := s.steps[dagID]; ok {
		delete(bucket, stepID)
		if len(bucket) == 0 {
			delete(s.steps, dagID)
		}
	}
	return nil
}

func (s *MemStore) DeleteResult(_ context.Context, dagID, stepID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bucket, ok := s.results[dagID]; ok {
		delete(bucket, stepID)
		if len(bucket) == 0 {
			delete(s.results, dagID)
		}
	}
	return nil
}

func (s *MemStore) WatchResult(ctx context.Context, dagID, stepID string) (<-chan []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan []byte, 1)
	if bucket, ok := s.results[dagID]; ok {
		if data, ok := bucket[stepID]; ok {
			ch <- append([]byte(nil), data...)
			close(ch)
			return ch, nil
		}
	}
	key := dagID + "/" + stepID
	s.watches[key] = append(s.watches[key], ch)
	go func() {
		<-ctx.Done()
		s.mu.Lock()
		waiters := s.watches[key]
		for i, w := range waiters {
			if w == ch {
				s.watches[key] = append(waiters[:i], waiters[i+1:]...)
				close(ch)
				break
			}
		}
		s.mu.Unlock()
	}()
	return ch, nil
}
