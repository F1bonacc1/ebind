package main_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if _, _, err := wf.Store.GetMeta(ctx, dagID); !errors.Is(err, workflow.ErrDAGNotFound) {
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

func TestDagPause(t *testing.T) {
	h := testutil.SingleNode(t, worker.Options{Concurrency: 1, AckWait: time.Second})
	c := newTestCtx(t, h, "pretty")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	wf, err := c.Workflow(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Seed a running DAG
	const dagID = "dag-pause-test"
	if err := wf.Store.PutMeta(ctx, dagID, workflow.DAGMeta{
		ID: dagID, Status: workflow.DAGStatusRunning, CreatedAt: time.Now().UTC(),
	}, 0); err != nil {
		t.Fatal(err)
	}

	// Execute: ebctl dag pause <dag-id>
	dagRoot := cmddag.NewCmd(c)
	var buf bytes.Buffer
	dagRoot.SetOut(&buf)
	dagRoot.SetArgs([]string{"pause", dagID})
	if err := dagRoot.Execute(); err != nil {
		t.Fatalf("dag pause: %v", err)
	}

	// Verify output
	output := strings.TrimSpace(buf.String())
	expected := fmt.Sprintf("paused %s", dagID)
	if output != expected {
		t.Errorf("expected output %q, got %q", expected, output)
	}

	// Verify DAG status transitioned
	meta, _, err := wf.Store.GetMeta(ctx, dagID)
	if err != nil {
		t.Fatal(err)
	}
	// Should be pausing or paused (no in-flight steps)
	if meta.Status != workflow.DAGStatusPausing && meta.Status != workflow.DAGStatusPaused {
		t.Errorf("expected pausing or paused, got %s", meta.Status)
	}
}

func TestDagResume(t *testing.T) {
	h := testutil.SingleNode(t, worker.Options{Concurrency: 1, AckWait: time.Second})
	c := newTestCtx(t, h, "pretty")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	wf, err := c.Workflow(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Seed a paused DAG
	const dagID = "dag-resume-test"
	if err := wf.Store.PutMeta(ctx, dagID, workflow.DAGMeta{
		ID: dagID, Status: workflow.DAGStatusPaused, CreatedAt: time.Now().UTC(), PausedAt: time.Now().UTC(),
	}, 0); err != nil {
		t.Fatal(err)
	}

	// Execute: ebctl dag resume <dag-id>
	dagRoot := cmddag.NewCmd(c)
	var buf bytes.Buffer
	dagRoot.SetOut(&buf)
	dagRoot.SetArgs([]string{"resume", dagID})
	if err := dagRoot.Execute(); err != nil {
		t.Fatalf("dag resume: %v", err)
	}

	// Verify output
	output := strings.TrimSpace(buf.String())
	expected := fmt.Sprintf("resumed %s", dagID)
	if output != expected {
		t.Errorf("expected output %q, got %q", expected, output)
	}

	// Verify DAG status transitioned
	meta, _, err := wf.Store.GetMeta(ctx, dagID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Status != workflow.DAGStatusRunning {
		t.Errorf("expected running, got %s", meta.Status)
	}
}

func TestDagPause_InvalidState(t *testing.T) {
	h := testutil.SingleNode(t, worker.Options{Concurrency: 1})
	c := newTestCtx(t, h, "pretty")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	wf, err := c.Workflow(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Test pause on a DAG that is already done
	const dagID = "dag-pause-invalid"
	if err := wf.Store.PutMeta(ctx, dagID, workflow.DAGMeta{
		ID: dagID, Status: workflow.DAGStatusDone, CreatedAt: time.Now().UTC(),
	}, 0); err != nil {
		t.Fatal(err)
	}

	dagRoot := cmddag.NewCmd(c)
	var buf bytes.Buffer
	dagRoot.SetOut(&buf)
	dagRoot.SetArgs([]string{"pause", dagID})
	err = dagRoot.Execute()
	if err == nil {
		t.Fatal("expected error for pause on done DAG")
	}
	// Error should contain dagID and current status (from the wrapped API error)
	if !strings.Contains(err.Error(), dagID) {
		t.Errorf("error missing dagID: %v", err)
	}
	if !strings.Contains(err.Error(), "done") {
		t.Errorf("error missing status 'done': %v", err)
	}
}

func TestDagResume_InvalidState(t *testing.T) {
	h := testutil.SingleNode(t, worker.Options{Concurrency: 1})
	c := newTestCtx(t, h, "pretty")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	wf, err := c.Workflow(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Test resume on a done DAG — terminal states reject resume.
	// (Resume on a running DAG is idempotent and succeeds by design.)
	const dagID = "dag-resume-invalid"
	if err := wf.Store.PutMeta(ctx, dagID, workflow.DAGMeta{
		ID: dagID, Status: workflow.DAGStatusDone, CreatedAt: time.Now().UTC(),
	}, 0); err != nil {
		t.Fatal(err)
	}

	dagRoot := cmddag.NewCmd(c)
	var buf bytes.Buffer
	dagRoot.SetOut(&buf)
	dagRoot.SetArgs([]string{"resume", dagID})
	err = dagRoot.Execute()
	if err == nil {
		t.Fatal("expected error for resume on done DAG")
	}
	// Error should contain dagID and current status
	if !strings.Contains(err.Error(), dagID) {
		t.Errorf("error missing dagID: %v", err)
	}
	if !strings.Contains(err.Error(), "done") {
		t.Errorf("error missing status 'done': %v", err)
	}
}

func TestDagLs_StatusFilterPaused(t *testing.T) {
	h := testutil.SingleNode(t, worker.Options{Concurrency: 1})
	c := newTestCtx(t, h, "json")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	wf, err := c.Workflow(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Seed two DAGs: one running, one paused
	runningID := "dag-ls-running"
	pausedID := "dag-ls-paused"
	if err := wf.Store.PutMeta(ctx, runningID, workflow.DAGMeta{
		ID: runningID, Status: workflow.DAGStatusRunning, CreatedAt: time.Now().UTC(),
	}, 0); err != nil {
		t.Fatal(err)
	}
	if err := wf.Store.PutMeta(ctx, pausedID, workflow.DAGMeta{
		ID: pausedID, Status: workflow.DAGStatusPaused, CreatedAt: time.Now().UTC(),
	}, 0); err != nil {
		t.Fatal(err)
	}

	// Test: ebctl dag ls --status paused — must show only the paused DAG
	dagRoot := cmddag.NewCmd(c)
	var buf bytes.Buffer
	dagRoot.SetOut(&buf)
	dagRoot.SetArgs([]string{"ls", "--status", "paused"})
	if err := dagRoot.Execute(); err != nil {
		t.Fatalf("dag ls --status paused: %v", err)
	}

	raw := buf.Bytes()
	if !jsonRowsContain(t, raw, "id", pausedID) {
		t.Errorf("dag ls --status paused should include %s:\n%s", pausedID, string(raw))
	}
	if jsonRowsContain(t, raw, "id", runningID) {
		t.Errorf("dag ls --status paused should NOT include %s:\n%s", runningID, string(raw))
	}

	// Test: ebctl dag ls without filter — both should show
	dagRoot2 := cmddag.NewCmd(c)
	var buf2 bytes.Buffer
	dagRoot2.SetOut(&buf2)
	dagRoot2.SetArgs([]string{"ls"})
	if err := dagRoot2.Execute(); err != nil {
		t.Fatalf("dag ls: %v", err)
	}
	raw2 := buf2.Bytes()
	if !jsonRowsContain(t, raw2, "id", runningID) {
		t.Errorf("dag ls should include %s:\n%s", runningID, string(raw2))
	}
	if !jsonRowsContain(t, raw2, "id", pausedID) {
		t.Errorf("dag ls should include %s:\n%s", pausedID, string(raw2))
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
		if v, ok := r[key]; ok && v == val {
			return true
		}
	}
	return false
}

var _ jetstream.JetStream // keep import aligned with the harness
var _ = jsonRowsContain
