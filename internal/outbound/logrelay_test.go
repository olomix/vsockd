package outbound

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olomix/vsockd/internal/config"
	"github.com/olomix/vsockd/internal/metrics"
	"github.com/olomix/vsockd/internal/vsockconn"
)

const logRelayCID uint32 = 16

// startLogRelayServer wires a Server with only log_relay listeners over the
// loopback backend and tears it down on cleanup.
func startLogRelayServer(
	t *testing.T,
	cfgs []config.LogRelayListener,
	listenFn ListenFunc,
	m *metrics.Metrics,
	logger *slog.Logger,
) *Server {
	t.Helper()
	s, err := NewServer(nil, nil, cfgs, listenFn, m, logger)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(
			context.Background(), 2*time.Second)
		defer shutdownCancel()
		_ = s.Shutdown(shutdownCtx)
	})
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return s
}

// withStdoutSink redirects the package-level stdout sink writer for the
// duration of a test, mirroring withShuttleDrainTimeout.
func withStdoutSink(t *testing.T, w io.Writer) {
	t.Helper()
	prev := stdoutSinkWriter
	stdoutSinkWriter = w
	t.Cleanup(func() { stdoutSinkWriter = prev })
}

// sendLogLines dials a loopback log_relay port, writes each line terminated
// by '\n', and fully closes the connection so the handler reaches EOF.
func sendLogLines(
	t *testing.T, reg *vsockconn.Registry, cid, port uint32, lines ...string,
) {
	t.Helper()
	c, err := vsockconn.NewLoopbackDialer(reg, cid).Dial(hostCID, port)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	for _, ln := range lines {
		if _, err := c.Write([]byte(ln + "\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	_ = c.Close()
}

// sendRawLog dials a loopback log_relay port, writes payload verbatim (no
// added delimiter), and closes — used to exercise non-'\n'-terminated input.
func sendRawLog(
	t *testing.T, reg *vsockconn.Registry, cid, port uint32, payload string,
) {
	t.Helper()
	c, err := vsockconn.NewLoopbackDialer(reg, cid).Dial(hostCID, port)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if _, err := c.Write([]byte(payload)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = c.Close()
}

// waitForFileLines polls path until it holds at least n non-empty lines.
func waitForFileLines(t *testing.T, path string, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			lines := nonEmptyLines(b)
			if len(lines) >= n {
				return lines
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, _ := os.ReadFile(path)
	t.Fatalf("file %s did not reach %d lines; got: %q", path, n, b)
	return nil
}

func nonEmptyLines(b []byte) []string {
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		if ln != "" {
			out = append(out, ln)
		}
	}
	return out
}

// TestLogRelay_FileSplicesHostFields verifies a JSON object line gets the
// peer CID and host-tags object spliced in under the default host_key while
// every original byte — including a >2^53 int64 — is preserved verbatim,
// and the enclave's own "tags" object is left untouched (no merge).
func TestLogRelay_FileSplicesHostFields(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const port uint32 = 5140
	path := filepath.Join(t.TempDir(), "app.ndjson")

	cfgs := []config.LogRelayListener{{
		Port:   port,
		Output: config.LogRelayOutputFile,
		Path:   path,
		Enrich: &config.LogRelayEnrich{
			CID: true,
			Tags: map[string]string{
				"region":   "us-east-1",
				"instance": "i-0abc123",
			},
		},
	}}
	// config.Validate normally defaults host_key; set it explicitly here
	// since we bypass Validate by constructing the listener directly.
	cfgs[0].Enrich.HostKey = "host"

	startLogRelayServer(
		t, cfgs, newLoopbackListenFunc(reg, hostCID), metrics.New(),
		discardLogger())

	const in = `{"ts":"t1","src":"app","pid":9007199254740993,` +
		`"tags":{"service":"abc","version":"1.1.2"},"type":"log","msg":"hi"}`
	sendLogLines(t, reg, logRelayCID, port, in)

	got := waitForFileLines(t, path, 1)[0]

	if !strings.HasPrefix(got,
		`{"cid":16,"host":{"instance":"i-0abc123","region":"us-east-1"},`) {
		t.Fatalf("output missing spliced host prefix: %s", got)
	}
	// Big int64 must survive byte-for-byte (never decoded to float64).
	if !strings.Contains(got, `"pid":9007199254740993`) {
		t.Errorf("int64 not preserved: %s", got)
	}
	// Enclave tags must be untouched (no merge of host data into them).
	if !strings.Contains(got, `"tags":{"service":"abc","version":"1.1.2"}`) {
		t.Errorf("enclave tags mutated: %s", got)
	}

	var rec map[string]any
	if err := json.Unmarshal([]byte(got), &rec); err != nil {
		t.Fatalf("output not valid JSON: %v (%s)", err, got)
	}
	if rec["cid"] != float64(logRelayCID) {
		t.Errorf("cid = %v, want %d", rec["cid"], logRelayCID)
	}
	host, _ := rec["host"].(map[string]any)
	if host["region"] != "us-east-1" || host["instance"] != "i-0abc123" {
		t.Errorf("host tags = %v", host)
	}
	if rec["ts"] != "t1" {
		t.Errorf("original ts lost: %v", rec["ts"])
	}
}

// TestLogRelay_CustomHostKey verifies a configured host_key names the object
// the host tags land under.
func TestLogRelay_CustomHostKey(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const port uint32 = 5140
	path := filepath.Join(t.TempDir(), "app.ndjson")

	cfgs := []config.LogRelayListener{{
		Port:   port,
		Output: config.LogRelayOutputFile,
		Path:   path,
		Enrich: &config.LogRelayEnrich{
			CID:     true,
			HostKey: "meta",
			Tags:    map[string]string{"region": "eu-west-1"},
		},
	}}
	startLogRelayServer(
		t, cfgs, newLoopbackListenFunc(reg, hostCID), metrics.New(),
		discardLogger())

	sendLogLines(t, reg, logRelayCID, port, `{"x":1}`)
	got := waitForFileLines(t, path, 1)[0]

	if !strings.Contains(got, `"meta":{"region":"eu-west-1"}`) {
		t.Errorf("custom host_key not used: %s", got)
	}
	if strings.Contains(got, `"host":`) {
		t.Errorf("default host key leaked: %s", got)
	}
}

// TestLogRelay_ControlCharHostKey verifies a host_key with control bytes is
// JSON-encoded (not strconv.Quote), keeping output valid NDJSON.
func TestLogRelay_ControlCharHostKey(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const port uint32 = 5140
	path := filepath.Join(t.TempDir(), "app.ndjson")

	cfgs := []config.LogRelayListener{{
		Port:   port,
		Output: config.LogRelayOutputFile,
		Path:   path,
		Enrich: &config.LogRelayEnrich{
			HostKey: "h\x7fk",
			Tags:    map[string]string{"region": "eu-west-1"},
		},
	}}
	startLogRelayServer(
		t, cfgs, newLoopbackListenFunc(reg, hostCID), metrics.New(),
		discardLogger())

	sendLogLines(t, reg, logRelayCID, port, `{"x":1}`)
	got := waitForFileLines(t, path, 1)[0]

	if !json.Valid([]byte(got)) {
		t.Errorf("output is not valid JSON: %q", got)
	}
}

// TestLogRelay_FinalLineNoNewline verifies a record that arrives without a
// trailing newline (peer closes mid-stream) is still emitted and enriched —
// the EOF-with-buffered-data branch of handleLogRelay.
func TestLogRelay_FinalLineNoNewline(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const port uint32 = 5140
	path := filepath.Join(t.TempDir(), "app.ndjson")

	cfgs := []config.LogRelayListener{{
		Port:   port,
		Output: config.LogRelayOutputFile,
		Path:   path,
		Enrich: &config.LogRelayEnrich{CID: true, HostKey: "host"},
	}}
	startLogRelayServer(
		t, cfgs, newLoopbackListenFunc(reg, hostCID), metrics.New(),
		discardLogger())

	sendRawLog(t, reg, logRelayCID, port, `{"x":1}`)
	got := waitForFileLines(t, path, 1)[0]

	if want := `{"cid":16,"x":1}`; got != want {
		t.Fatalf("unterminated final line output = %q, want %q", got, want)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(got), &rec); err != nil {
		t.Fatalf("output not valid JSON: %v (%s)", err, got)
	}
}

// TestLogRelay_CRLFLineEnding verifies a "\r\n"-terminated record has the
// trailing '\r' stripped so the spliced object stays well-formed (dropNewline
// '\r' branch).
func TestLogRelay_CRLFLineEnding(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const port uint32 = 5140
	path := filepath.Join(t.TempDir(), "app.ndjson")

	cfgs := []config.LogRelayListener{{
		Port:   port,
		Output: config.LogRelayOutputFile,
		Path:   path,
		Enrich: &config.LogRelayEnrich{CID: true, HostKey: "host"},
	}}
	startLogRelayServer(
		t, cfgs, newLoopbackListenFunc(reg, hostCID), metrics.New(),
		discardLogger())

	sendRawLog(t, reg, logRelayCID, port, "{\"x\":1}\r\n")
	got := waitForFileLines(t, path, 1)[0]

	if want := `{"cid":16,"x":1}`; got != want {
		t.Fatalf("CRLF line output = %q, want %q", got, want)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(got), &rec); err != nil {
		t.Fatalf("output not valid JSON: %v (%s)", err, got)
	}
}

// TestLogRelay_EmptyObject verifies the empty-object splice path drops the
// trailing comma so output stays valid JSON.
func TestLogRelay_EmptyObject(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const port uint32 = 5140
	path := filepath.Join(t.TempDir(), "app.ndjson")

	cfgs := []config.LogRelayListener{{
		Port:   port,
		Output: config.LogRelayOutputFile,
		Path:   path,
		Enrich: &config.LogRelayEnrich{
			CID:     true,
			HostKey: "host",
			Tags:    map[string]string{"region": "us-east-1"},
		},
	}}
	startLogRelayServer(
		t, cfgs, newLoopbackListenFunc(reg, hostCID), metrics.New(),
		discardLogger())

	sendLogLines(t, reg, logRelayCID, port, `{}`)
	got := waitForFileLines(t, path, 1)[0]

	if want := `{"cid":16,"host":{"region":"us-east-1"}}`; got != want {
		t.Fatalf("empty object output = %q, want %q", got, want)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(got), &rec); err != nil {
		t.Fatalf("output not valid JSON: %v (%s)", err, got)
	}
}

// TestLogRelay_NonJSONWrappedAsRaw verifies a non-object line becomes a raw
// record carrying the original text in "msg".
func TestLogRelay_NonJSONWrappedAsRaw(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const port uint32 = 5140
	path := filepath.Join(t.TempDir(), "app.ndjson")

	cfgs := []config.LogRelayListener{{
		Port:   port,
		Output: config.LogRelayOutputFile,
		Path:   path,
		Enrich: &config.LogRelayEnrich{CID: true, HostKey: "host"},
	}}
	startLogRelayServer(
		t, cfgs, newLoopbackListenFunc(reg, hostCID), metrics.New(),
		discardLogger())

	sendLogLines(t, reg, logRelayCID, port, "plain log text")
	got := waitForFileLines(t, path, 1)[0]

	var rec map[string]any
	if err := json.Unmarshal([]byte(got), &rec); err != nil {
		t.Fatalf("raw output not valid JSON: %v (%s)", err, got)
	}
	if rec["type"] != "raw" {
		t.Errorf("type = %v, want raw", rec["type"])
	}
	if rec["msg"] != "plain log text" {
		t.Errorf("msg = %v, want original text", rec["msg"])
	}
	if rec["cid"] != float64(logRelayCID) {
		t.Errorf("raw record missing cid: %v", rec["cid"])
	}
}

// TestLogRelay_OverLongLineTruncated verifies a line exceeding max_line_bytes
// is emitted as a truncated raw record and the reader resyncs to the next
// line rather than wedging or dropping it.
func TestLogRelay_OverLongLineTruncated(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const port uint32 = 5140
	path := filepath.Join(t.TempDir(), "app.ndjson")

	cfgs := []config.LogRelayListener{{
		Port:         port,
		Output:       config.LogRelayOutputFile,
		Path:         path,
		MaxLineBytes: 32,
		Enrich:       &config.LogRelayEnrich{CID: true, HostKey: "host"},
	}}
	startLogRelayServer(
		t, cfgs, newLoopbackListenFunc(reg, hostCID), metrics.New(),
		discardLogger())

	long := strings.Repeat("A", 100)
	sendLogLines(t, reg, logRelayCID, port, long, `{"after":true}`)

	lines := waitForFileLines(t, path, 2)

	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("truncated output not valid JSON: %v (%s)", err, lines[0])
	}
	if first["type"] != "raw" {
		t.Errorf("truncated line type = %v, want raw", first["type"])
	}
	if first["truncated"] != true {
		t.Errorf("truncated flag = %v, want true", first["truncated"])
	}
	// Resync: the following well-formed line must be enriched normally.
	if !strings.Contains(lines[1], `"after":true`) {
		t.Errorf("reader did not resync to next line: %s", lines[1])
	}
}

// TestLogRelay_ExactMaxLineNotTruncated verifies the boundary: a line whose
// content is exactly max_line_bytes long is accepted and enriched normally,
// not flagged truncated (the reader buffer must hold the line plus its '\n').
func TestLogRelay_ExactMaxLineNotTruncated(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const port uint32 = 5140
	path := filepath.Join(t.TempDir(), "app.ndjson")

	// 32-byte JSON object: `{"v":"` + 24 'A' + `"}`.
	exact := `{"v":"` + strings.Repeat("A", 24) + `"}`
	if len(exact) != 32 {
		t.Fatalf("test setup: line is %d bytes, want 32", len(exact))
	}

	cfgs := []config.LogRelayListener{{
		Port:         port,
		Output:       config.LogRelayOutputFile,
		Path:         path,
		MaxLineBytes: 32,
		Enrich:       &config.LogRelayEnrich{CID: true, HostKey: "host"},
	}}
	startLogRelayServer(
		t, cfgs, newLoopbackListenFunc(reg, hostCID), metrics.New(),
		discardLogger())

	sendLogLines(t, reg, logRelayCID, port, exact)
	lines := waitForFileLines(t, path, 1)

	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("output not valid JSON: %v (%s)", err, lines[0])
	}
	if _, ok := rec["truncated"]; ok {
		t.Errorf("exact-length line wrongly flagged truncated: %s", lines[0])
	}
	if rec["type"] == "raw" {
		t.Errorf("exact-length object wrapped as raw: %s", lines[0])
	}
	if rec["v"] != strings.Repeat("A", 24) {
		t.Errorf("original content not preserved: %s", lines[0])
	}
}

// TestLogRelay_NoEnrichPassThrough verifies that without an enrich block the
// listener frames lines but leaves their content unmodified.
func TestLogRelay_NoEnrichPassThrough(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const port uint32 = 5140
	path := filepath.Join(t.TempDir(), "app.ndjson")

	cfgs := []config.LogRelayListener{{
		Port:   port,
		Output: config.LogRelayOutputFile,
		Path:   path,
	}}
	startLogRelayServer(
		t, cfgs, newLoopbackListenFunc(reg, hostCID), metrics.New(),
		discardLogger())

	sendLogLines(t, reg, logRelayCID, port, `{"a":1}`, "plain text")
	lines := waitForFileLines(t, path, 2)

	if lines[0] != `{"a":1}` {
		t.Errorf("object line modified: %q", lines[0])
	}
	if lines[1] != "plain text" {
		t.Errorf("plain line modified: %q", lines[1])
	}
}

// TestLogRelay_StdoutSink verifies a stdout listener writes enriched output
// to the injected writer.
func TestLogRelay_StdoutSink(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const port uint32 = 5141

	var buf safeBuffer
	withStdoutSink(t, &buf)

	cfgs := []config.LogRelayListener{{
		Port:   port,
		Output: config.LogRelayOutputStdout,
		Enrich: &config.LogRelayEnrich{CID: true, HostKey: "host"},
	}}
	startLogRelayServer(
		t, cfgs, newLoopbackListenFunc(reg, hostCID), metrics.New(),
		discardLogger())

	sendLogLines(t, reg, logRelayCID, port, `{"k":"v"}`)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), `"cid":16`) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	out := buf.String()
	if !strings.Contains(out, `"cid":16`) || !strings.Contains(out, `"k":"v"`) {
		t.Fatalf("stdout sink output = %q", out)
	}
}

// TestLogRelay_SerialHandling verifies a second connection is not processed
// while the first is still open: log_relay handles one connection at a time
// per listener (plan decision 4).
func TestLogRelay_SerialHandling(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const port uint32 = 5140
	path := filepath.Join(t.TempDir(), "app.ndjson")

	cfgs := []config.LogRelayListener{{
		Port:   port,
		Output: config.LogRelayOutputFile,
		Path:   path,
	}}
	startLogRelayServer(
		t, cfgs, newLoopbackListenFunc(reg, hostCID), metrics.New(),
		discardLogger())

	// conn1 stays open after one line, holding the accept loop.
	c1, err := vsockconn.NewLoopbackDialer(reg, logRelayCID).Dial(hostCID, port)
	if err != nil {
		t.Fatalf("Dial c1: %v", err)
	}
	if _, err := c1.Write([]byte("first\n")); err != nil {
		t.Fatalf("Write c1: %v", err)
	}
	_ = waitForFileLines(t, path, 1)

	// conn2 connects and sends, but must not be serviced until c1 closes.
	c2, err := vsockconn.NewLoopbackDialer(reg, logRelayCID).Dial(hostCID, port)
	if err != nil {
		t.Fatalf("Dial c2: %v", err)
	}
	if _, err := c2.Write([]byte("second\n")); err != nil {
		t.Fatalf("Write c2: %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	if lines := nonEmptyLines(mustRead(t, path)); len(lines) != 1 {
		t.Fatalf("second connection serviced before first closed: %v", lines)
	}

	_ = c1.Close()
	lines := waitForFileLines(t, path, 2)
	_ = c2.Close()
	if lines[0] != "first" || lines[1] != "second" {
		t.Fatalf("serial order wrong: %v", lines)
	}
}

// TestLogRelay_ShutdownForceClosesInFlight verifies Shutdown releases an
// in-flight relay within its grace window.
func TestLogRelay_ShutdownForceClosesInFlight(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const port uint32 = 5140
	path := filepath.Join(t.TempDir(), "app.ndjson")

	cfgs := []config.LogRelayListener{{
		Port:   port,
		Output: config.LogRelayOutputFile,
		Path:   path,
	}}
	s, err := NewServer(
		nil, nil, cfgs, newLoopbackListenFunc(reg, hostCID), metrics.New(),
		discardLogger())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	c, err := vsockconn.NewLoopbackDialer(reg, logRelayCID).Dial(hostCID, port)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	// Send one line, then hold the connection open so the handler parks on
	// its next read inside the accept loop.
	if _, err := c.Write([]byte("held\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = waitForFileLines(t, path, 1)

	shutdownCtx, shutdownCancel := context.WithTimeout(
		context.Background(), 200*time.Millisecond)
	defer shutdownCancel()
	if err := s.Shutdown(shutdownCtx); err == nil {
		t.Fatal("expected shutdown deadline error for parked relay")
	}

	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := c.Read(buf); err == nil {
		t.Error("expected read error after forced shutdown, got nil")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return b
}

// safeBuffer is a goroutine-safe bytes.Buffer for capturing sink output the
// handler writes from another goroutine.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
