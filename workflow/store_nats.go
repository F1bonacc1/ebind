package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
)

const (
	kvBucketName = "ebind-dags"

	keyMetaSuffix   = ".meta"
	keyStepPrefix   = ".step."
	keyResultPrefix = ".result."
)

// NatsStore is a JetStream KV-backed StateStore. Uses KV's built-in revision
// numbers for CAS; the revision returned by GetX matches what PutX expects in
// expectedRev.
type NatsStore struct {
	kv jetstream.KeyValue
}

// NewNatsStore creates (or opens) the KV bucket and returns a NatsStore.
func NewNatsStore(ctx context.Context, js jetstream.JetStream, replicas int) (*NatsStore, error) {
	if replicas <= 0 {
		replicas = 1
	}
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:   kvBucketName,
		Replicas: replicas,
	})
	if err != nil {
		return nil, fmt.Errorf("workflow: create/open KV: %w", err)
	}
	return &NatsStore{kv: kv}, nil
}

func metaKey(dagID string) string           { return dagID + keyMetaSuffix }
func stepKey(dagID, stepID string) string   { return dagID + keyStepPrefix + stepID }
func resultKey(dagID, stepID string) string { return dagID + keyResultPrefix + stepID }

func (s *NatsStore) GetMeta(ctx context.Context, dagID string) (DAGMeta, uint64, error) {
	entry, err := s.kv.Get(ctx, metaKey(dagID))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return DAGMeta{}, 0, ErrDAGNotFound
		}
		return DAGMeta{}, 0, err
	}
	var meta DAGMeta
	if err := json.Unmarshal(entry.Value(), &meta); err != nil {
		return DAGMeta{}, 0, err
	}
	return meta, entry.Revision(), nil
}

func (s *NatsStore) PutMeta(ctx context.Context, dagID string, meta DAGMeta, expectedRev uint64) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if expectedRev == 0 {
		_, err := s.kv.Create(ctx, metaKey(dagID), data)
		if err != nil && (errors.Is(err, jetstream.ErrKeyExists) || strings.Contains(err.Error(), "wrong last sequence")) {
			return ErrStaleRevision
		}
		return err
	}
	_, err = s.kv.Update(ctx, metaKey(dagID), data, expectedRev)
	if err != nil && strings.Contains(err.Error(), "wrong last sequence") {
		return ErrStaleRevision
	}
	return err
}

func (s *NatsStore) GetStep(ctx context.Context, dagID, stepID string) (StepRecord, uint64, error) {
	entry, err := s.kv.Get(ctx, stepKey(dagID, stepID))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return StepRecord{}, 0, ErrStepNotFound
		}
		return StepRecord{}, 0, err
	}
	var rec StepRecord
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return StepRecord{}, 0, err
	}
	return rec, entry.Revision(), nil
}

func (s *NatsStore) PutStep(ctx context.Context, dagID, stepID string, rec StepRecord, expectedRev uint64) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if expectedRev == 0 {
		_, err := s.kv.Create(ctx, stepKey(dagID, stepID), data)
		if err != nil && (errors.Is(err, jetstream.ErrKeyExists) || strings.Contains(err.Error(), "wrong last sequence")) {
			return ErrStaleRevision
		}
		return err
	}
	_, err = s.kv.Update(ctx, stepKey(dagID, stepID), data, expectedRev)
	if err != nil && strings.Contains(err.Error(), "wrong last sequence") {
		return ErrStaleRevision
	}
	return err
}

func (s *NatsStore) ListDAGs(ctx context.Context) ([]DAGMeta, error) {
	keys, err := s.kv.ListKeys(ctx)
	if err != nil {
		return nil, err
	}
	var out []DAGMeta
	for k := range keys.Keys() {
		if !strings.HasSuffix(k, keyMetaSuffix) {
			continue
		}
		entry, err := s.kv.Get(ctx, k)
		if err != nil {
			continue
		}
		var meta DAGMeta
		if err := json.Unmarshal(entry.Value(), &meta); err == nil {
			out = append(out, meta)
		}
	}
	return out, nil
}

func (s *NatsStore) ListSteps(ctx context.Context, dagID string) ([]StepRecord, error) {
	keys, err := s.kv.ListKeys(ctx)
	if err != nil {
		return nil, err
	}
	prefix := dagID + keyStepPrefix
	var out []StepRecord
	for k := range keys.Keys() {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		entry, err := s.kv.Get(ctx, k)
		if err != nil {
			continue
		}
		var rec StepRecord
		if err := json.Unmarshal(entry.Value(), &rec); err == nil {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (s *NatsStore) GetResult(ctx context.Context, dagID, stepID string) ([]byte, error) {
	entry, err := s.kv.Get(ctx, resultKey(dagID, stepID))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, ErrStepNotFound
		}
		return nil, err
	}
	return entry.Value(), nil
}

func (s *NatsStore) PutResult(ctx context.Context, dagID, stepID string, data []byte) error {
	_, err := s.kv.Put(ctx, resultKey(dagID, stepID), data)
	return err
}

func (s *NatsStore) DeleteMeta(ctx context.Context, dagID string) error {
	if err := s.kv.Delete(ctx, metaKey(dagID)); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return err
	}
	return nil
}

func (s *NatsStore) DeleteStep(ctx context.Context, dagID, stepID string) error {
	if err := s.kv.Delete(ctx, stepKey(dagID, stepID)); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return err
	}
	return nil
}

func (s *NatsStore) DeleteResult(ctx context.Context, dagID, stepID string) error {
	if err := s.kv.Delete(ctx, resultKey(dagID, stepID)); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return err
	}
	return nil
}

func (s *NatsStore) WatchResult(ctx context.Context, dagID, stepID string) (<-chan []byte, error) {
	key := resultKey(dagID, stepID)
	w, err := s.kv.Watch(ctx, key, jetstream.IncludeHistory())
	if err != nil {
		return nil, err
	}
	out := make(chan []byte, 1)
	go func() {
		defer close(out)
		defer func() { _ = w.Stop() }()
		for {
			select {
			case <-ctx.Done():
				return
			case entry, ok := <-w.Updates():
				if !ok {
					return
				}
				if entry == nil {
					continue // init boundary marker
				}
				select {
				case out <- entry.Value():
				case <-ctx.Done():
				}
				return
			}
		}
	}()
	return out, nil
}
