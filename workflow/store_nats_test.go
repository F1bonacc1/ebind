package workflow

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/embed"
)

func newTestNatsStore(t *testing.T) *NatsStore {
	t.Helper()
	node, err := embed.StartNode(embed.NodeConfig{
		ServerName: "store-" + t.Name(),
		Port:       -1,
		StoreDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(node.Shutdown)
	nc, err := nats.Connect(node.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := NewNatsStore(ctx, js, 1)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// flakyGetKV makes kv.Get of one key fail with err. With
// jetstream.ErrKeyNotFound it answers the way a direct get served by a replica
// that has not applied the key's create yet does.
type flakyGetKV struct {
	jetstream.KeyValue
	key string
	err error
}

func (kv *flakyGetKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if key == kv.key {
		return nil, kv.err
	}
	return kv.KeyValue.Get(ctx, key)
}

func listedStepIDs(steps []StepRecord) []string {
	ids := make([]string, 0, len(steps))
	for _, s := range steps {
		ids = append(ids, s.StepID)
	}
	slices.Sort(ids)
	return ids
}

func putTestSteps(t *testing.T, s StateStore, dagID string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := s.PutStep(context.Background(), dagID, id, StepRecord{DAGID: dagID, StepID: id}, 0); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNatsStore_ListSteps_DirectGetMiss_ReadsFromLeader(t *testing.T) {
	s := newTestNatsStore(t)
	putTestSteps(t, s, "d", "a", "b")
	s.kv = &flakyGetKV{KeyValue: s.kv, key: stepKey("d", "b"), err: jetstream.ErrKeyNotFound}

	steps, err := s.ListSteps(context.Background(), "d")
	if err != nil {
		t.Fatal(err)
	}
	if got := listedStepIDs(steps); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("ListSteps = %v, want [a b]: a key the direct get missed must not vanish from the list", got)
	}
}

func TestNatsStore_ListSteps_GetErrorIsReturned(t *testing.T) {
	s := newTestNatsStore(t)
	putTestSteps(t, s, "d", "a", "b")
	boom := errors.New("boom")
	s.kv = &flakyGetKV{KeyValue: s.kv, key: stepKey("d", "b"), err: boom}

	if steps, err := s.ListSteps(context.Background(), "d"); !errors.Is(err, boom) {
		t.Errorf("ListSteps = %v, %v; want the Get error, not a partial list", listedStepIDs(steps), err)
	}
}

func TestNatsStore_ListSteps_ScopedToDAGAndSkipsDeleted(t *testing.T) {
	s := newTestNatsStore(t)
	ctx := context.Background()
	if err := s.PutMeta(ctx, "d", DAGMeta{ID: "d", Status: DAGStatusRunning}, 0); err != nil {
		t.Fatal(err)
	}
	putTestSteps(t, s, "d", "c", "a", "b")
	putTestSteps(t, s, "dd", "x") // DAG id sharing a prefix with "d"
	if err := s.PutResult(ctx, "d", "a", []byte(`1`)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSignal(ctx, "d", SignalRecord{Name: "go"}); err != nil {
		t.Fatal(err)
	}
	// The delete tombstone stays in the stream, so the leader still lists the
	// key: only the leader-served read can tell it is gone.
	if err := s.DeleteStep(ctx, "d", "b"); err != nil {
		t.Fatal(err)
	}

	steps, err := s.ListSteps(ctx, "d")
	if err != nil {
		t.Fatal(err)
	}
	if got := listedStepIDs(steps); !slices.Equal(got, []string{"a", "c"}) {
		t.Errorf("ListSteps(d) = %v, want [a c]", got)
	}
	if steps, err := s.ListSteps(ctx, "none"); err != nil || len(steps) != 0 {
		t.Errorf("ListSteps(none) = %v, %v; want empty, nil", listedStepIDs(steps), err)
	}
}

func listedSignalNames(sigs []SignalRecord) []string {
	names := make([]string, 0, len(sigs))
	for _, s := range sigs {
		names = append(names, s.Name)
	}
	slices.Sort(names)
	return names
}

func listedDAGIDs(metas []DAGMeta) []string {
	ids := make([]string, 0, len(metas))
	for _, m := range metas {
		ids = append(ids, m.ID)
	}
	slices.Sort(ids)
	return ids
}

func putTestSignals(t *testing.T, s StateStore, dagID string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := s.PutSignal(context.Background(), dagID, SignalRecord{DAGID: dagID, Name: n}); err != nil {
			t.Fatal(err)
		}
	}
}

func putTestMetas(t *testing.T, s StateStore, dagIDs ...string) {
	t.Helper()
	for _, id := range dagIDs {
		if err := s.PutMeta(context.Background(), id, DAGMeta{ID: id, Status: DAGStatusRunning}, 0); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNatsStore_ListSignals_DirectGetMiss_ReadsFromLeader(t *testing.T) {
	s := newTestNatsStore(t)
	putTestSignals(t, s, "d", "a", "b")
	s.kv = &flakyGetKV{KeyValue: s.kv, key: signalKey("d", "b"), err: jetstream.ErrKeyNotFound}

	sigs, err := s.ListSignals(context.Background(), "d")
	if err != nil {
		t.Fatal(err)
	}
	if got := listedSignalNames(sigs); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("ListSignals = %v, want [a b]: a signal the direct get missed must not vanish from the list", got)
	}
}

func TestNatsStore_ListSignals_ScopedToDAGAndSkipsDeleted(t *testing.T) {
	s := newTestNatsStore(t)
	ctx := context.Background()
	putTestMetas(t, s, "d")
	putTestSteps(t, s, "d", "s1")
	putTestSignals(t, s, "d", "c", "a", "b")
	putTestSignals(t, s, "dd", "x") // DAG id sharing a prefix with "d"
	if err := s.DeleteSignal(ctx, "d", "b"); err != nil {
		t.Fatal(err)
	}

	sigs, err := s.ListSignals(ctx, "d")
	if err != nil {
		t.Fatal(err)
	}
	if got := listedSignalNames(sigs); !slices.Equal(got, []string{"a", "c"}) {
		t.Errorf("ListSignals(d) = %v, want [a c]", got)
	}
}

func TestNatsStore_ListDAGs_DirectGetMiss_ReadsFromLeader(t *testing.T) {
	s := newTestNatsStore(t)
	putTestMetas(t, s, "d1", "d2")
	s.kv = &flakyGetKV{KeyValue: s.kv, key: metaKey("d2"), err: jetstream.ErrKeyNotFound}

	metas, err := s.ListDAGs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := listedDAGIDs(metas); !slices.Equal(got, []string{"d1", "d2"}) {
		t.Errorf("ListDAGs = %v, want [d1 d2]: a meta the direct get missed must not vanish from the list", got)
	}
}

func TestNatsStore_ListDAGs_MetasOnlyIncludingDottedIDs(t *testing.T) {
	s := newTestNatsStore(t)
	ctx := context.Background()
	putTestMetas(t, s, "d1", "a.b", "gone") // DAG IDs may contain dots
	putTestSteps(t, s, "d1", "s1")
	putTestSignals(t, s, "d1", "meta")
	if err := s.PutResult(ctx, "d1", "s1", []byte(`1`)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMeta(ctx, "gone"); err != nil {
		t.Fatal(err)
	}

	metas, err := s.ListDAGs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := listedDAGIDs(metas); !slices.Equal(got, []string{"a.b", "d1"}) {
		t.Errorf("ListDAGs = %v, want [a.b d1]", got)
	}
}
