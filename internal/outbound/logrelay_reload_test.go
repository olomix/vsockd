package outbound

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olomix/vsockd/internal/config"
	"github.com/olomix/vsockd/internal/metrics"
	"github.com/olomix/vsockd/internal/vsockconn"
)

// TestLogRelay_ReloadAddsListener verifies a SIGHUP reload that introduces a
// new log_relay port binds it and starts accepting, while the original
// listener keeps working.
func TestLogRelay_ReloadAddsListener(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const portA uint32 = 5140
	const portB uint32 = 5141
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.ndjson")
	pathB := filepath.Join(dir, "b.ndjson")

	cfgA := config.LogRelayListener{
		Port:   portA,
		Output: config.LogRelayOutputFile,
		Path:   pathA,
	}
	cfgB := config.LogRelayListener{
		Port:   portB,
		Output: config.LogRelayOutputFile,
		Path:   pathB,
	}

	m := metrics.New()
	s := startLogRelayServer(t, []config.LogRelayListener{cfgA},
		newLoopbackListenFunc(reg, hostCID), m, discardLogger())

	if err := s.Apply(nil, nil,
		[]config.LogRelayListener{cfgA, cfgB}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	sendLogLines(t, reg, logRelayCID, portB, `{"msg":"hi-b"}`)
	got := waitForFileLines(t, pathB, 1)
	if !strings.Contains(got[0], `"msg":"hi-b"`) {
		t.Fatalf("new listener output = %q", got[0])
	}

	// The original listener must still accept after the reload.
	sendLogLines(t, reg, logRelayCID, portA, `{"msg":"hi-a"}`)
	gotA := waitForFileLines(t, pathA, 1)
	if !strings.Contains(gotA[0], `"msg":"hi-a"`) {
		t.Fatalf("original listener output = %q", gotA[0])
	}
}

// TestLogRelay_ReloadRemovesListener verifies a reload that drops a
// log_relay port closes its accept loop and releases the file sink. After
// removal the port is gone from ListenerPorts and no longer dialable.
func TestLogRelay_ReloadRemovesListener(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const port uint32 = 5140
	path := filepath.Join(t.TempDir(), "a.ndjson")

	cfg := config.LogRelayListener{
		Port:   port,
		Output: config.LogRelayOutputFile,
		Path:   path,
	}

	m := metrics.New()
	s := startLogRelayServer(t, []config.LogRelayListener{cfg},
		newLoopbackListenFunc(reg, hostCID), m, discardLogger())

	// Sanity: it accepts before removal.
	sendLogLines(t, reg, logRelayCID, port, `{"msg":"before"}`)
	waitForFileLines(t, path, 1)

	if err := s.Apply(nil, nil, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if ports := s.ListenerPorts(); len(ports) != 0 {
		t.Fatalf("ListenerPorts after removal = %v, want empty", ports)
	}

	// The loopback backend unregisters a closed listener, so a fresh dial
	// to the removed port must fail rather than hang.
	if _, err := vsockconn.NewLoopbackDialer(reg, logRelayCID).
		Dial(hostCID, port); err == nil {
		t.Fatal("Dial to removed port succeeded, want error")
	}
}

// TestLogRelay_ReloadSwapSinkAndEnrich verifies that a same-port reload
// changing the sink path and enrichment routes new connections to the new
// sink/enrichment, while an in-flight relay keeps writing to the old sink
// with the old enrichment (plan decision 9). Serial handling means the
// in-flight connection is drained before the next one is accepted, so the
// two phases are observed in sequence on the same port.
func TestLogRelay_ReloadSwapSinkAndEnrich(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const port uint32 = 5140
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.ndjson")
	newPath := filepath.Join(dir, "new.ndjson")

	oldCfg := config.LogRelayListener{
		Port:   port,
		Output: config.LogRelayOutputFile,
		Path:   oldPath,
		Enrich: &config.LogRelayEnrich{CID: true, HostKey: "host"},
	}
	newCfg := config.LogRelayListener{
		Port:   port,
		Output: config.LogRelayOutputFile,
		Path:   newPath,
		Enrich: &config.LogRelayEnrich{
			CID:     true,
			HostKey: "host",
			Tags:    map[string]string{"region": "us-east-1"},
		},
	}

	m := metrics.New()
	s := startLogRelayServer(t, []config.LogRelayListener{oldCfg},
		newLoopbackListenFunc(reg, hostCID), m, discardLogger())

	// Open an in-flight relay and confirm its first line lands in the old
	// sink before the reload happens.
	conn, err := vsockconn.NewLoopbackDialer(reg, logRelayCID).
		Dial(hostCID, port)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if _, err := conn.Write([]byte(`{"n":1}` + "\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	waitForFileLines(t, oldPath, 1)

	// Reload to the new path + enrichment while the connection is open.
	if err := s.Apply(nil, nil,
		[]config.LogRelayListener{newCfg}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The in-flight relay must keep using the old sink (and old enrichment:
	// no region tag) for its remaining lines.
	if _, err := conn.Write([]byte(`{"n":2}` + "\n")); err != nil {
		t.Fatalf("Write after reload: %v", err)
	}
	oldLines := waitForFileLines(t, oldPath, 2)
	for _, ln := range oldLines {
		if strings.Contains(ln, "region") {
			t.Fatalf("in-flight relay picked up new enrichment: %q", ln)
		}
		if !strings.Contains(ln, `"cid":16`) {
			t.Fatalf("in-flight relay lost old enrichment: %q", ln)
		}
	}
	_ = conn.Close()

	// A new connection (accepted only after the serial in-flight one drains)
	// must land in the new sink with the new enrichment.
	sendLogLines(t, reg, logRelayCID, port, `{"n":3}`)
	newLines := waitForFileLines(t, newPath, 1)
	if !strings.Contains(newLines[0], "us-east-1") {
		t.Fatalf("new connection missing new enrichment: %q", newLines[0])
	}

	// And nothing from the new connection should have leaked into the old
	// sink (still exactly two lines there).
	if extra := nonEmptyLines(mustReadFile(t, oldPath)); len(extra) != 2 {
		t.Fatalf("old sink line count = %d, want 2", len(extra))
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// TestLogRelay_ReloadModeChangeRejected verifies that flipping a port
// between vsock_to_tcp and log_relay across a reload is refused (run()
// dispatches on a fixed mode), leaving the running listener intact and not
// leaking the staged sink.
func TestLogRelay_ReloadModeChangeRejected(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const port uint32 = 8080

	m := metrics.New()
	tcpCfg := []config.VsockToTCPListener{{
		Port:     port,
		Upstream: "127.0.0.1:9",
	}}
	s := startTCPServer(t, tcpCfg, newLoopbackListenFunc(reg, hostCID), m,
		discardLogger())

	logCfg := []config.LogRelayListener{{
		Port:   port,
		Output: config.LogRelayOutputFile,
		Path:   filepath.Join(t.TempDir(), "x.ndjson"),
	}}
	err := s.Apply(nil, nil, logCfg)
	if err == nil {
		t.Fatal("Apply with changed mode returned nil, want error")
	}
	if !strings.Contains(err.Error(), "cannot change mode") {
		t.Fatalf("Apply error = %q; want mention of mode change", err.Error())
	}

	// The original vsock_to_tcp listener must still be present and unchanged.
	if ports := s.ListenerPorts(); len(ports) != 1 || ports[0] != port {
		t.Fatalf("ListenerPorts after rejected reload = %v", ports)
	}
}
