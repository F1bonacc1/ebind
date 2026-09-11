package workflow

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	kvBucketName = "ebind-dags"

	// The stream behind the bucket and the subject prefix of its keys, for the
	// leader-served reads in listLive.
	kvStreamName    = "KV_" + kvBucketName
	kvSubjectPrefix = "$KV." + kvBucketName + "."

	keyMetaSuffix   = ".meta"
	keyStepPrefix   = ".step."
	keyResultPrefix = ".result."
	keySignalPrefix = ".signal."
)

// NatsStore is a JetStream KV-backed StateStore. Uses KV's built-in revision
// numbers for CAS; the revision returned by GetX matches what PutX expects in
// expectedRev.
type NatsStore struct {
	kv jetstream.KeyValue

	// nc, apiPrefix and timeout issue the raw JetStream API requests behind
	// listLive: the KV API reads only through direct gets, which any replica
	// may answer.
	nc        *nats.Conn
	apiPrefix string
	timeout   time.Duration
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
	opts := js.Options()
	timeout := opts.DefaultTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &NatsStore{kv: kv, nc: js.Conn(), apiPrefix: jsAPIPrefix(opts), timeout: timeout}, nil
}

// jsAPIPrefix derives the JetStream API subject prefix the way jetstream does:
// an explicit prefix, else the domain's, else the default.
func jsAPIPrefix(opts jetstream.JetStreamOptions) string {
	switch {
	case opts.APIPrefix != "":
		return strings.TrimSuffix(opts.APIPrefix, ".") + "."
	case opts.Domain != "":
		return fmt.Sprintf("$JS.%s.API.", opts.Domain)
	default:
		return jetstream.DefaultAPIPrefix
	}
}

func metaKey(dagID string) string           { return dagID + keyMetaSuffix }
func stepKey(dagID, stepID string) string   { return dagID + keyStepPrefix + stepID }
func resultKey(dagID, stepID string) string { return dagID + keyResultPrefix + stepID }

// signalKey base64url-encodes the signal name: names are arbitrary user text,
// while KV keys allow only [-/_=.a-zA-Z0-9]. The encoded alphabet ([A-Za-z0-9_-],
// no dots) also keeps signal keys disjoint from the .meta suffix scan in
// ListDAGs and the .step./.result. prefix scans. The raw name lives in the
// record value — never decode it from the key.
func signalKey(dagID, name string) string {
	return dagID + keySignalPrefix + base64.RawURLEncoding.EncodeToString([]byte(name))
}

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

func (s *NatsStore) PutStep(ctx context.Context, dagID, stepID string, rec StepRecord, expectedRev uint64) (uint64, error) {
	data, err := json.Marshal(rec)
	if err != nil {
		return 0, err
	}
	if expectedRev == 0 {
		rev, err := s.kv.Create(ctx, stepKey(dagID, stepID), data)
		if err != nil && (errors.Is(err, jetstream.ErrKeyExists) || strings.Contains(err.Error(), "wrong last sequence")) {
			return 0, ErrStaleRevision
		}
		return rev, err
	}
	rev, err := s.kv.Update(ctx, stepKey(dagID, stepID), data, expectedRev)
	if err != nil && strings.Contains(err.Error(), "wrong last sequence") {
		return 0, ErrStaleRevision
	}
	return rev, err
}

// ListDAGs returns the meta of every DAG in the bucket, with the key set read
// from the stream leader as in ListSteps. DAG IDs may contain dots and a
// subject filter cannot match on a suffix, so this lists every key in the
// bucket and keeps those ending in the meta suffix: O(bucket) per call, the
// same order as the ListKeys scan it replaces, but answered by the leader.
func (s *NatsStore) ListDAGs(ctx context.Context) ([]DAGMeta, error) {
	isMeta := func(key string) bool { return strings.HasSuffix(key, keyMetaSuffix) }
	vals, err := s.listLive(ctx, ">", isMeta)
	if err != nil {
		return nil, err
	}
	var out []DAGMeta
	for _, v := range vals {
		var meta DAGMeta
		if err := json.Unmarshal(v, &meta); err == nil {
			out = append(out, meta)
		}
	}
	return out, nil
}

// ListSteps returns every live step record of the DAG, in step-key order.
//
// A record missing from this list is not a stale read that the next poll
// corrects: Cancel, DeleteDAG, Pause and the scheduler act on "all steps", and
// a step the list omits is a step they never touch. So the key set comes from
// the stream leader, and a listed key the direct get misses is re-read from
// the leader rather than dropped (see listLive).
func (s *NatsStore) ListSteps(ctx context.Context, dagID string) ([]StepRecord, error) {
	vals, err := s.listLive(ctx, dagID+keyStepPrefix+">", nil)
	if err != nil {
		return nil, err
	}
	var out []StepRecord
	for _, v := range vals {
		var rec StepRecord
		if err := json.Unmarshal(v, &rec); err == nil {
			out = append(out, rec)
		}
	}
	return out, nil
}

// listLive returns the current value of every live KV key that matches
// pattern and, when keep is non-nil, satisfies keep, in key order.
//
// The key set is read from the stream leader: KV ListKeys runs on an ordered
// consumer that the server places on a random replica, which may be behind.
// Values are read with direct gets. A listed key the direct get cannot find is
// either a delete/purge tombstone or a replica that has not applied the write
// yet, and only the leader can tell which.
func (s *NatsStore) listLive(ctx context.Context, pattern string, keep func(key string) bool) ([][]byte, error) {
	subjects, err := s.leaderSubjects(ctx, kvSubjectPrefix+pattern)
	if err != nil {
		return nil, err
	}
	var out [][]byte
	for _, subj := range subjects {
		key := strings.TrimPrefix(subj, kvSubjectPrefix)
		if keep != nil && !keep(key) {
			continue
		}
		entry, err := s.kv.Get(ctx, key)
		if err == nil {
			out = append(out, entry.Value())
			continue
		}
		if !errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, err
		}
		val, live, err := s.leaderLastMsg(ctx, subj)
		if err != nil {
			return nil, err
		}
		if live {
			out = append(out, val)
		}
	}
	return out, nil
}

// leaderSubjects returns the sorted stream subjects matching filter, read
// through STREAM.INFO, which only the stream leader answers. Pages are merged
// by subject the way jetstream's Stream.Info does. That method is not used
// directly because it rewrites the Stream's cached info without a lock, so a
// shared handle would race under concurrent list calls.
func (s *NatsStore) leaderSubjects(ctx context.Context, filter string) ([]string, error) {
	seen := make(map[string]struct{})
	for {
		var resp struct {
			Error *jetstream.APIError `json:"error,omitempty"`
			Total int                 `json:"total"`
			State struct {
				Subjects map[string]uint64 `json:"subjects"`
			} `json:"state"`
		}
		req := map[string]any{"subjects_filter": filter, "offset": len(seen)}
		if err := s.apiRequest(ctx, "STREAM.INFO."+kvStreamName, req, &resp); err != nil {
			return nil, err
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		before := len(seen)
		for subj := range resp.State.Subjects {
			seen[subj] = struct{}{}
		}
		// A page that adds nothing new ends the scan too, so a subject set
		// shifting under the offset cannot loop forever.
		if len(seen) == before || len(seen) >= resp.Total {
			break
		}
	}
	out := make([]string, 0, len(seen))
	for subj := range seen {
		out = append(out, subj)
	}
	slices.Sort(out)
	return out, nil
}

// leaderLastMsg reads the last message on subj through the non-direct
// STREAM.MSG.GET API, which only the stream leader answers. jetstream's
// Stream.GetLastMsgForSubject cannot stand in: it switches to a direct get
// whenever the stream allows them, and KV buckets always do. The bool is false
// for a delete/purge tombstone or a subject with no message left.
func (s *NatsStore) leaderLastMsg(ctx context.Context, subj string) ([]byte, bool, error) {
	var resp struct {
		Error   *jetstream.APIError `json:"error,omitempty"`
		Message *struct {
			Header []byte `json:"hdrs,omitempty"`
			Data   []byte `json:"data,omitempty"`
		} `json:"message,omitempty"`
	}
	req := map[string]string{"last_by_subj": subj}
	if err := s.apiRequest(ctx, "STREAM.MSG.GET."+kvStreamName, req, &resp); err != nil {
		return nil, false, err
	}
	switch {
	case resp.Error != nil && resp.Error.ErrorCode == jetstream.JSErrCodeMessageNotFound:
		return nil, false, nil
	case resp.Error != nil:
		return nil, false, resp.Error
	case resp.Message == nil:
		return nil, false, jetstream.ErrInvalidJetStreamResponse
	}
	if len(resp.Message.Header) > 0 {
		hdr, err := nats.DecodeHeadersMsg(resp.Message.Header)
		if err != nil {
			return nil, false, err
		}
		if op := hdr.Get("KV-Operation"); op == "DEL" || op == "PURGE" {
			return nil, false, nil
		}
	}
	return resp.Message.Data, true, nil
}

// apiRequest sends a JetStream API request and decodes the JSON reply into
// resp, bounded by the JetStream default timeout when ctx has no deadline.
func (s *NatsStore) apiRequest(ctx context.Context, api string, req, resp any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	msg, err := s.nc.RequestWithContext(ctx, s.apiPrefix+api, body)
	if err != nil {
		return err
	}
	return json.Unmarshal(msg.Data, resp)
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

func (s *NatsStore) GetSignal(ctx context.Context, dagID, name string) (SignalRecord, error) {
	entry, err := s.kv.Get(ctx, signalKey(dagID, name))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return SignalRecord{}, ErrSignalNotFound
		}
		return SignalRecord{}, err
	}
	var rec SignalRecord
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return SignalRecord{}, err
	}
	return rec, nil
}

func (s *NatsStore) PutSignal(ctx context.Context, dagID string, rec SignalRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = s.kv.Create(ctx, signalKey(dagID, rec.Name), data)
	if err != nil && (errors.Is(err, jetstream.ErrKeyExists) || strings.Contains(err.Error(), "wrong last sequence")) {
		return ErrStaleRevision
	}
	return err
}

// ListSignals returns every delivered signal of the DAG, with the key set read
// from the stream leader as in ListSteps. The scheduler gates waiting steps on
// this list, so a record it drops keeps a step blocked after its signal was
// delivered. A signal key holds exactly one token after the prefix (see
// signalKey), hence the single-token wildcard.
func (s *NatsStore) ListSignals(ctx context.Context, dagID string) ([]SignalRecord, error) {
	vals, err := s.listLive(ctx, dagID+keySignalPrefix+"*", nil)
	if err != nil {
		return nil, err
	}
	var out []SignalRecord
	for _, v := range vals {
		var rec SignalRecord
		if err := json.Unmarshal(v, &rec); err == nil {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (s *NatsStore) DeleteSignal(ctx context.Context, dagID, name string) error {
	if err := s.kv.Delete(ctx, signalKey(dagID, name)); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
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
