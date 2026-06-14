package outbound

import (
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olomix/vsockd/internal/config"
	"github.com/olomix/vsockd/internal/metrics"
	"github.com/olomix/vsockd/internal/vsockconn"
)

// TestLogRelay_MetricsHappyPath verifies the connection, line, and byte
// counters advance: one accept, one line per emitted record, and the byte
// total matching what landed in the file sink.
func TestLogRelay_MetricsHappyPath(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const port uint32 = 5140
	path := filepath.Join(t.TempDir(), "app.ndjson")

	m := metrics.New()
	cfgs := []config.LogRelayListener{{
		Port:   port,
		Output: config.LogRelayOutputFile,
		Path:   path,
		Enrich: &config.LogRelayEnrich{CID: true, HostKey: "host"},
	}}
	startLogRelayServer(
		t, cfgs, newLoopbackListenFunc(reg, hostCID), m, discardLogger())

	sendLogLines(t, reg, logRelayCID, port, `{"a":1}`, `{"b":2}`)
	_ = waitForFileLines(t, path, 2)

	waitForPlainCounter(t, m.LogRelayConnections, 1)
	waitForPlainCounter(t, m.LogRelayLines, 2)

	// Output bytes recorded must equal the bytes written to the file sink.
	wantBytes := float64(len(mustRead(t, path)))
	waitForPlainCounter(t, m.LogRelayBytes, wantBytes)
	if got := plainCounterValue(t, m.LogRelayBytes); got != wantBytes {
		t.Errorf("LogRelayBytes = %v, want %v", got, wantBytes)
	}

	if got := counterValue(
		t, m.LogRelayErrors, metrics.LogRelayErrorRead); got != 0 {
		t.Errorf("read_error = %v, want 0", got)
	}
	if got := counterValue(
		t, m.LogRelayErrors, metrics.LogRelayErrorLineTooLong); got != 0 {
		t.Errorf("line_too_long = %v, want 0", got)
	}
}

// TestLogRelay_MetricsLineTooLong verifies the line_too_long error counter
// increments once when a line exceeds max_line_bytes.
func TestLogRelay_MetricsLineTooLong(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const port uint32 = 5140
	path := filepath.Join(t.TempDir(), "app.ndjson")

	m := metrics.New()
	cfgs := []config.LogRelayListener{{
		Port:         port,
		Output:       config.LogRelayOutputFile,
		Path:         path,
		MaxLineBytes: 32,
		Enrich:       &config.LogRelayEnrich{CID: true, HostKey: "host"},
	}}
	startLogRelayServer(
		t, cfgs, newLoopbackListenFunc(reg, hostCID), m, discardLogger())

	long := strings.Repeat("A", 100)
	sendLogLines(t, reg, logRelayCID, port, long, `{"after":true}`)
	_ = waitForFileLines(t, path, 2)

	waitForCounter(t, m.LogRelayErrors, 1, metrics.LogRelayErrorLineTooLong)
}

// TestLogRelay_MetricsSinkOpenFailure verifies that a sink that cannot be
// opened both fails server construction and increments the sink_open error
// counter.
func TestLogRelay_MetricsSinkOpenFailure(t *testing.T) {
	reg := vsockconn.NewRegistry()
	// A path under a non-existent directory cannot be opened for append.
	badPath := filepath.Join(t.TempDir(), "no-such-dir", "app.ndjson")

	m := metrics.New()
	cfgs := []config.LogRelayListener{{
		Port:   5140,
		Output: config.LogRelayOutputFile,
		Path:   badPath,
	}}
	_, err := NewServer(
		nil, nil, cfgs, newLoopbackListenFunc(reg, hostCID), m, discardLogger())
	if err == nil {
		t.Fatal("expected NewServer error for unopenable sink")
	}
	if got := counterValue(
		t, m.LogRelayErrors, metrics.LogRelayErrorSink); got != 1 {
		t.Errorf("sink_open = %v, want 1", got)
	}
}

// TestLogRelay_MetricsReadError verifies the read_error counter increments
// when the connection ends with a reset rather than a clean EOF.
func TestLogRelay_MetricsReadError(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const port uint32 = 5140
	path := filepath.Join(t.TempDir(), "app.ndjson")

	m := metrics.New()
	cfgs := []config.LogRelayListener{{
		Port:   port,
		Output: config.LogRelayOutputFile,
		Path:   path,
	}}
	startLogRelayServer(
		t, cfgs, newLoopbackListenFunc(reg, hostCID), m, discardLogger())

	c, err := vsockconn.NewLoopbackDialer(reg, logRelayCID).Dial(hostCID, port)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	// Send one complete line so the handler is parked in its read loop.
	if _, err := c.Write([]byte("first\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = waitForFileLines(t, path, 1)

	// Force a TCP RST (SetLinger(0) + Close) so the handler's next read
	// returns a reset error rather than EOF.
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = c.Close()

	waitForCounter(t, m.LogRelayErrors, 1, metrics.LogRelayErrorRead)
}
