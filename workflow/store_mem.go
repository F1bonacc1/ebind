package workflow

import (
	"context"
	"sync"
)

// MemStore is an in-memory StateStore suitable for tests and local dev.
// It emulates NATS KV's revision-based CAS: each Put* increments the stored
// revision; callers must pass the matching expectedRev or get ErrStaleRevision.
type MemStore struct {
	mu       sync.Mutex
	steps    map[string]map[string]memEntry     // dagID → stepID → entry
	results  map[string]map[string][]byte       // dagID → stepID → bytes
	metas    map[string]memMetaEntry            // dagID → entry
	watches  map[string][]chan []byte           // "dagID/stepID" → waiters
	signals  map[string]map[string]SignalRecord // dagID → signal name → record
	dagWatch map[string][]*memDAGWatcher        // dagID → WatchDAG subscribers
}

// memDAGWatchBuffer is how many live changes a WatchDAG subscriber may lag
// behind before its channel is closed (it then re-watches). Writers never
// block on a slow subscriber.
const memDAGWatchBuffer = 256

type memDAGWatcher struct {
	ch     chan DAGChange
	closed bool
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
		steps:    map[string]map[string]memEntry{},
		results:  map[string]map[string][]byte{},
		metas:    map[string]memMetaEntry{},
		watches:  map[string][]chan []byte{},
		signals:  map[string]map[string]SignalRecord{},
		dagWatch: map[string][]*memDAGWatcher{},
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

func (s *MemStore) PutStep(_ context.Context, dagID, stepID string, rec StepRecord, expectedRev uint64) (uint64, error) {
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
			return 0, ErrStaleRevision
		}
		bucket[stepID] = memEntry{rec: rec, rev: 1}
		s.notifyLocked(dagID, DAGChange{Step: &rec})
		return 1, nil
	}
	if !exists || existing.rev != expectedRev {
		return 0, ErrStaleRevision
	}
	newRev := existing.rev + 1
	bucket[stepID] = memEntry{rec: rec, rev: newRev}
	s.notifyLocked(dagID, DAGChange{Step: &rec})
	return newRev, nil
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

// GetMetaLatest is GetMeta: MemStore reads are always current.
func (s *MemStore) GetMetaLatest(ctx context.Context, dagID string) (DAGMeta, uint64, error) {
	return s.GetMeta(ctx, dagID)
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
		s.notifyLocked(dagID, DAGChange{Meta: &meta})
		return nil
	}
	if !exists || existing.rev != expectedRev {
		return ErrStaleRevision
	}
	s.metas[dagID] = memMetaEntry{meta: meta, rev: existing.rev + 1}
	s.notifyLocked(dagID, DAGChange{Meta: &meta})
	return nil
}

func (s *MemStore) DeleteMeta(_ context.Context, dagID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.metas[dagID]; ok {
		delete(s.metas, dagID)
		s.notifyLocked(dagID, DAGChange{Deleted: DeletedMeta})
	}
	return nil
}

func (s *MemStore) DeleteStep(_ context.Context, dagID, stepID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bucket, ok := s.steps[dagID]; ok {
		if _, exists := bucket[stepID]; exists {
			delete(bucket, stepID)
			s.notifyLocked(dagID, DAGChange{Deleted: stepID})
		}
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

func (s *MemStore) GetSignal(_ context.Context, dagID, name string) (SignalRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bucket, ok := s.signals[dagID]; ok {
		if rec, ok := bucket[name]; ok {
			return rec, nil
		}
	}
	return SignalRecord{}, ErrSignalNotFound
}

func (s *MemStore) PutSignal(_ context.Context, dagID string, rec SignalRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.signals[dagID]
	if !ok {
		bucket = map[string]SignalRecord{}
		s.signals[dagID] = bucket
	}
	if _, exists := bucket[rec.Name]; exists {
		return ErrStaleRevision
	}
	bucket[rec.Name] = rec
	return nil
}

func (s *MemStore) ListSignals(_ context.Context, dagID string) ([]SignalRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.signals[dagID]
	if !ok {
		return nil, nil
	}
	out := make([]SignalRecord, 0, len(bucket))
	for _, rec := range bucket {
		out = append(out, rec)
	}
	return out, nil
}

func (s *MemStore) DeleteSignal(_ context.Context, dagID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bucket, ok := s.signals[dagID]; ok {
		delete(bucket, name)
		if len(bucket) == 0 {
			delete(s.signals, dagID)
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

// WatchDAG implements StateStore. The snapshot and the Synced marker are
// queued under the store lock, so no write can slip between the snapshot and
// the first live change.
func (s *MemStore) WatchDAG(ctx context.Context, dagID string) (<-chan DAGChange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	steps := s.steps[dagID]
	w := &memDAGWatcher{ch: make(chan DAGChange, len(steps)+2+memDAGWatchBuffer)}
	if e, ok := s.metas[dagID]; ok {
		meta := e.meta
		w.ch <- DAGChange{Meta: &meta}
	}
	for _, e := range steps {
		rec := e.rec
		w.ch <- DAGChange{Step: &rec}
	}
	w.ch <- DAGChange{Synced: true}
	s.dagWatch[dagID] = append(s.dagWatch[dagID], w)
	go func() {
		<-ctx.Done()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.dropWatcherLocked(dagID, w)
	}()
	return w.ch, nil
}

// notifyLocked fans a change out to dagID's watchers without blocking; a
// watcher whose buffer is full is closed and dropped. Caller holds s.mu.
func (s *MemStore) notifyLocked(dagID string, c DAGChange) {
	for _, w := range s.dagWatch[dagID] {
		select {
		case w.ch <- c:
		default:
			s.dropWatcherLocked(dagID, w)
		}
	}
}

func (s *MemStore) dropWatcherLocked(dagID string, w *memDAGWatcher) {
	if w.closed {
		return
	}
	w.closed = true
	close(w.ch)
	ws := s.dagWatch[dagID]
	for i, x := range ws {
		if x == w {
			// Copy rather than append in place: notifyLocked may be ranging
			// over the old slice right now.
			next := make([]*memDAGWatcher, 0, len(ws)-1)
			next = append(next, ws[:i]...)
			next = append(next, ws[i+1:]...)
			if len(next) == 0 {
				delete(s.dagWatch, dagID)
			} else {
				s.dagWatch[dagID] = next
			}
			break
		}
	}
}
