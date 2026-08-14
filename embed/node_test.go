package embed

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStartNode_ReportsBindFailure is the regression test for a diagnosis, not
// for a behavior: a server that cannot bind must say so.
//
// A nightly e2e run lost an iteration to "cluster node 2 not ready within 30s"
// and nothing else — no cause anywhere in the log. Two things conspired.
// ReadyForConnections narrows the server's precise complaint down to a bool,
// and NoLog leaves the logger nil, which turns the Fatalf on the start path
// into a no-op. The cause existed and was thrown away twice.
//
// Occupying the port first makes the failure deterministic, so this asserts the
// error carries the reason through.
func TestStartNode_ReportsBindFailure(t *testing.T) {
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	node, err := StartNode(NodeConfig{
		ServerName: "test-occupied",
		Port:       port,
		StoreDir:   t.TempDir(),
		ReadyWait:  2 * time.Second,
	})
	if err == nil {
		node.Shutdown()
		t.Fatalf("StartNode on occupied port %d: want error, got nil", port)
	}
	if !strings.Contains(err.Error(), "address already in use") {
		t.Errorf("error does not name the cause: %v", err)
	}
}

// captureLogger collects the server's log output for assertions.
type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *captureLogger) add(format string, v ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, v...))
}

func (l *captureLogger) Noticef(f string, v ...any) { l.add(f, v...) }
func (l *captureLogger) Warnf(f string, v ...any)   { l.add(f, v...) }
func (l *captureLogger) Fatalf(f string, v ...any)  { l.add(f, v...) }
func (l *captureLogger) Errorf(f string, v ...any)  { l.add(f, v...) }
func (l *captureLogger) Debugf(f string, v ...any)  { l.add(f, v...) }
func (l *captureLogger) Tracef(f string, v ...any)  { l.add(f, v...) }

func (l *captureLogger) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// TestStartNode_LoggerReceivesOutput checks the escape hatch is actually wired.
// Clearing Options.NoLog is not sufficient — Server.Start never calls
// ConfigureLogger, so the logger would stay nil and the option would look
// enabled while dropping every line. Asserting on delivered output rather than
// on the option value is the only version of this test that can fail.
func TestStartNode_LoggerReceivesOutput(t *testing.T) {
	logger := &captureLogger{}
	node, err := StartNode(NodeConfig{
		ServerName: "test-logger",
		Port:       -1,
		StoreDir:   t.TempDir(),
		Logger:     logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Shutdown()

	if got := logger.snapshot(); len(got) == 0 {
		t.Error("Logger received no output from a started server")
	}
}

// TestStartNode_NilLoggerStaysSilent is the other half of the contract: the
// default must not start writing server logs into an embedding application.
func TestStartNode_NilLoggerStaysSilent(t *testing.T) {
	node, err := StartNode(NodeConfig{
		ServerName: "test-silent",
		Port:       -1,
		StoreDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Shutdown()

	if !node.opts.NoLog {
		t.Error("nil Logger left NoLog clear")
	}
}
