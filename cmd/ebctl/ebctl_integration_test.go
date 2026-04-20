package main_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/cli"
	cmddag "github.com/f1bonacc1/ebind/cmd/ebctl/internal/commands/dag"
	cmddlq "github.com/f1bonacc1/ebind/cmd/ebctl/internal/commands/dlq"
	cmdstream "github.com/f1bonacc1/ebind/cmd/ebctl/internal/commands/stream"
	"github.com/f1bonacc1/ebind/cmd/ebctl/internal/format"
	"github.com/f1bonacc1/ebind/internal/testutil"
	"github.com/f1bonacc1/ebind/worker"
	"github.com/f1bonacc1/ebind/workflow"
)

// newTestCtx wires a cli.Context around a running NATS harness and a JSON printer,
// so table-less invocations are easy to assert.
func newTestCtx(t *testing.T, h *testutil.Harness, fmtName string) *cli.Context {
	t.Helper()
	p, err := format.New(fmtName)
	if err != nil {
		t.Fatal(err)
	}
	c := cli.NewContext()
	c.NC = h.Conn
	c.JS = h.JS
	c.Printer = p
	c.Yes = true
	c.Timeout = 5 * time.Second
	return c
}

func TestDagLsRmAndDlqFlow(t *testing.T) {
	h := testutil.SingleNode(t, worker.Options{Concurrency: 1})
	c := newTestCtx(t, h, "json")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	wf, err := c.Workflow(ctx)
	if err != nil {
		t.Fatal(err)
	}

	const dagID = "dag-ebctl-1"
	if err := wf.Store.PutMeta(ctx, dagID, workflow.DAGMeta{
		ID: dagID, Status: workflow.DAGStatusDone, CreatedAt: time.Now().UTC(),
	}, 0); err != nil {
		t.Fatal(err)
	}
	if err := wf.Store.PutStep(ctx, dagID, "a", workflow.StepRecord{
		DAGID: dagID, StepID: "a", Status: workflow.StatusDone,
		AddedAt: time.Now().UTC(), FnName: "test.noop",
	}, 0); err != nil {
		t.Fatal(err)
	}
	if err := wf.Store.PutResult(ctx, dagID, "a", []byte(`"done"`)); err != nil {
		t.Fatal(err)
	}

	// `dag ls` — must include our seeded DAG
	dagRoot := cmddag.NewCmd(c)
	var ls bytes.Buffer
	dagRoot.SetOut(&ls)
	dagRoot.SetArgs([]string{"ls"})
	if err := dagRoot.Execute(); err != nil {
		t.Fatalf("dag ls: %v", err)
	}
	if !strings.Contains(ls.String(), dagID) {
		t.Errorf("dag ls did not list our dag: %s", ls.String())
	}

	// `dag get` — must render the step we created
	dagRoot2 := cmddag.NewCmd(c)
	var getOut bytes.Buffer
	dagRoot2.SetOut(&getOut)
	dagRoot2.SetArgs([]string{"get", dagID})
	if err := dagRoot2.Execute(); err != nil {
		t.Fatalf("dag get: %v", err)
	}
	if !strings.Contains(getOut.String(), "\"step_id\": \"a\"") {
		t.Errorf("dag get output missing step: %s", getOut.String())
	}

	// `dag rm` — must delete all KV records
	dagRoot3 := cmddag.NewCmd(c)
	var rm bytes.Buffer
	dagRoot3.SetOut(&rm)
	dagRoot3.SetArgs([]string{"rm", dagID})
	if err := dagRoot3.Execute(); err != nil {
		t.Fatalf("dag rm: %v", err)
	}
	if _, _, err := wf.Store.GetMeta(ctx, dagID); err != workflow.ErrDAGNotFound {
		t.Errorf("meta still present after rm: %v", err)
	}
}

func TestStreamLs(t *testing.T) {
	h := testutil.SingleNode(t, worker.Options{Concurrency: 1})
	c := newTestCtx(t, h, "json")

	root := cmdstream.NewStreamCmd(c)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"ls"})
	if err := root.Execute(); err != nil {
		t.Fatalf("stream ls: %v", err)
	}
	for _, want := range []string{"EBIND_TASKS", "EBIND_RESP", "EBIND_DLQ"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stream ls missing %s:\n%s", want, out.String())
		}
	}
}

func TestDlqEmpty(t *testing.T) {
	h := testutil.SingleNode(t, worker.Options{Concurrency: 1})
	c := newTestCtx(t, h, "pretty")

	root := cmddlq.NewCmd(c)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"ls"})
	if err := root.Execute(); err != nil {
		t.Fatalf("dlq ls: %v", err)
	}
	if !strings.Contains(out.String(), "empty") {
		t.Errorf("expected empty-DLQ message: %s", out.String())
	}
}

// jsonRowsContain is a readability helper — looks for a key/value pair inside
// the printed JSON array. Used to keep assertion intent close to the test body.
func jsonRowsContain(t *testing.T, raw []byte, key, val string) bool {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		return false
	}
	for _, r := range rows {
		if fmt, ok := r[key]; ok && fmt == val {
			return true
		}
	}
	return false
}

var _ jetstream.JetStream // keep import aligned with the harness
var _ = jsonRowsContain
