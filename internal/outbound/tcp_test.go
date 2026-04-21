package outbound

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olomix/vsockd/internal/config"
	"github.com/olomix/vsockd/internal/metrics"
	"github.com/olomix/vsockd/internal/vsockconn"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// logRecord is a JSON-shaped slog record captured by captureHandler. The
// tests assert on message text and specific attrs, so the payload is kept
// generic to avoid coupling to slog's internal value representation.
type logRecord map[string]any

// captureHandler is a slog handler that appends every emitted record to
// an internal slice. Records are serialized through slog.JSONHandler so
// the attribute values land in a stable, comparable shape.
type captureHandler struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	json slog.Handler
}

func newCaptureHandler() *captureHandler {
	h := &captureHandler{}
	h.json = slog.NewJSONHandler(&h.buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	return h
}

func (h *captureHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.json.Enabled(ctx, l)
}

func (h *captureHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.json.Handle(ctx, r)
}

func (h *captureHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &captureHandler{json: h.json.WithAttrs(as)}
}

func (h *captureHandler) WithGroup(n string) slog.Handler {
	return &captureHandler{json: h.json.WithGroup(n)}
}

// records returns the emitted records as decoded JSON objects. Safe to
// call while the server is running; each call takes a snapshot of the
// buffer so the caller can retry a polling check without losing earlier
// entries.
func (h *captureHandler) records(t *testing.T) []logRecord {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []logRecord
	dec := json.NewDecoder(bytes.NewReader(h.buf.Bytes()))
	for {
		var r logRecord
		if err := dec.Decode(&r); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode log: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// findMessage returns the first record whose "msg" equals target.
func findMessage(recs []logRecord, target string) logRecord {
	for _, r := range recs {
		if m, _ := r["msg"].(string); m == target {
			return r
		}
	}
	return nil
}

func startTCPServer(
	t *testing.T,
	cfgs []config.VsockToTCPListener,
	listenFn ListenFunc,
	m *metrics.Metrics,
	logger *slog.Logger,
) *Server {
	t.Helper()
	s, err := NewServer(nil, cfgs, listenFn, m, logger)
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

// TestTCP_Passthrough_RoundTrip verifies a vsock_to_tcp listener shuttles
// bytes bidirectionally between the vsock peer and the TCP upstream and
// captures accurate byte totals in the debug close log.
func TestTCP_Passthrough_RoundTrip(t *testing.T) {
	echo := startEchoServer(t)

	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080

	capH := newCaptureHandler()
	logger := slog.New(capH)

	m := metrics.New()
	cfgs := []config.VsockToTCPListener{{
		Port:     vsockPort,
		Upstream: echo.Addr().String(),
	}}
	startTCPServer(t, cfgs, newLoopbackListenFunc(reg, hostCID), m, logger)

	c, err := vsockconn.NewLoopbackDialer(reg, enclaveCID).
		Dial(hostCID, vsockPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	payload := []byte("ping-pong-0123456789")
	if _, err := c.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Half-close write so the upstream echo's io.Copy returns and the
	// server-side shuttle unblocks; this also mirrors how enclave
	// clients would signal end-of-request on a request/response
	// protocol.
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		if err := cw.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
	} else {
		t.Fatalf("loopback conn does not support CloseWrite")
	}

	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo = %q, want %q", got, payload)
	}
	_ = c.Close()

	// Poll for the close log since the handler finishes asynchronously.
	var closeLog logRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		closeLog = findMessage(
			capH.records(t), "vsock connection closed")
		if closeLog != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if closeLog == nil {
		t.Fatalf("close log not emitted; records: %v", capH.records(t))
	}

	// Byte totals: one full echo round-trip moves len(payload) bytes in
	// each direction, so total_bytes must be 2 * len(payload).
	wantTotal := float64(2 * len(payload))
	if got := closeLog["total_bytes"]; got != wantTotal {
		t.Errorf("total_bytes = %v, want %v", got, wantTotal)
	}
	if got := closeLog["listen_port"]; got != float64(vsockPort) {
		t.Errorf("listen_port = %v, want %v", got, vsockPort)
	}
	if got := closeLog["cid"]; got != float64(enclaveCID) {
		t.Errorf("cid = %v, want %v", got, enclaveCID)
	}

	openLog := findMessage(capH.records(t), "inbound vsock connection")
	if openLog == nil {
		t.Fatalf("open log not emitted")
	}
	if got := openLog["cid"]; got != float64(enclaveCID) {
		t.Errorf("open cid = %v, want %v", got, enclaveCID)
	}
	if got := openLog["listen_port"]; got != float64(vsockPort) {
		t.Errorf("open listen_port = %v, want %v", got, vsockPort)
	}
}

// TestTCP_Passthrough_DialFailure verifies the handler logs a warn and
// closes the vsock connection cleanly when the TCP upstream is
// unreachable. No panic, no byte transfer.
func TestTCP_Passthrough_DialFailure(t *testing.T) {
	// Claim a port, close it, and use the now-dead address as upstream.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := dead.Addr().String()
	_ = dead.Close()

	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080

	capH := newCaptureHandler()
	logger := slog.New(capH)

	m := metrics.New()
	cfgs := []config.VsockToTCPListener{{
		Port:     vsockPort,
		Upstream: deadAddr,
	}}
	startTCPServer(t, cfgs, newLoopbackListenFunc(reg, hostCID), m, logger)

	c, err := vsockconn.NewLoopbackDialer(reg, enclaveCID).
		Dial(hostCID, vsockPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	// The vsock conn must be closed by the server once the upstream dial
	// fails. A Read blocks until that close propagates.
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	n, _ := c.Read(buf)
	if n != 0 {
		t.Errorf("expected empty read on dial-fail close, got %d bytes", n)
	}

	// Poll for the expected log pair: warn on dial-fail, debug on close.
	var failLog, closeLog logRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		recs := capH.records(t)
		failLog = findMessage(recs, "outbound tcp dial failed")
		closeLog = findMessage(recs, "vsock connection closed")
		if failLog != nil && closeLog != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if failLog == nil {
		t.Fatalf("expected 'outbound tcp dial failed' log")
	}
	if got := failLog["upstream"]; got != deadAddr {
		t.Errorf("upstream attr = %v, want %v", got, deadAddr)
	}
	if closeLog == nil {
		t.Fatalf("expected 'vsock connection closed' log even on dial fail")
	}
	// No bytes transferred → total_bytes must be zero.
	if got := closeLog["total_bytes"]; got != float64(0) {
		t.Errorf("total_bytes on dial fail = %v, want 0", got)
	}
}

// withShuttleDrainTimeout temporarily overrides shuttleDrainTimeout so a
// test can exercise the force-close path without stalling the suite.
func withShuttleDrainTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := shuttleDrainTimeout
	shuttleDrainTimeout = d
	t.Cleanup(func() { shuttleDrainTimeout = prev })
}

// TestTCP_Passthrough_ClientDisconnectMidStream verifies a peer that
// drops the vsock connection mid-transfer still lets the handler
// unblock, tear down the upstream conn, and emit a close log.
func TestTCP_Passthrough_ClientDisconnectMidStream(t *testing.T) {
	// Shrink the drain window so the silent-upstream force-close path
	// fires well inside the poll deadline below.
	withShuttleDrainTimeout(t, 300*time.Millisecond)
	// This server never echoes; it just holds the connection so the
	// handler's io.Copy(upstream, client) direction is blocked on the
	// client side and the reverse direction is blocked on the upstream
	// side. Closing the client then unblocks both via EOF/Close.
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	var upstreamConn net.Conn
	upstreamReady := make(chan struct{})
	go func() {
		c, err := upstream.Accept()
		if err != nil {
			return
		}
		upstreamConn = c
		close(upstreamReady)
		// Drain forever; do not write anything back.
		_, _ = io.Copy(io.Discard, c)
	}()

	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080

	capH := newCaptureHandler()
	logger := slog.New(capH)

	m := metrics.New()
	cfgs := []config.VsockToTCPListener{{
		Port:     vsockPort,
		Upstream: upstream.Addr().String(),
	}}
	startTCPServer(t, cfgs, newLoopbackListenFunc(reg, hostCID), m, logger)

	c, err := vsockconn.NewLoopbackDialer(reg, enclaveCID).
		Dial(hostCID, vsockPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	payload := []byte("partial-stream")
	if _, err := c.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Wait for the silent upstream to observe its incoming connection
	// before slamming the client; this guarantees both io.Copy
	// directions are active and exercises the mid-stream abort path.
	select {
	case <-upstreamReady:
	case <-time.After(2 * time.Second):
		t.Fatalf("upstream never accepted")
	}

	// Hard-close the client side without a half-close: forces both
	// io.Copy goroutines to return via error rather than EOF.
	_ = c.Close()

	var closeLog logRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		closeLog = findMessage(
			capH.records(t), "vsock connection closed")
		if closeLog != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if closeLog == nil {
		t.Fatalf("close log not emitted after client disconnect")
	}
	// The upstream-facing copy direction saw at least the initial write.
	if got, _ := closeLog["total_bytes"].(float64); got < float64(len(payload)) {
		t.Errorf("total_bytes = %v, want >= %v", got, len(payload))
	}
	if upstreamConn != nil {
		_ = upstreamConn.Close()
	}
}

// TestTCP_Passthrough_ContextCancelViaShutdown verifies that closing the
// server releases every in-flight TCP passthrough connection inside the
// shutdown grace window.
func TestTCP_Passthrough_ContextCancelViaShutdown(t *testing.T) {
	// A silent upstream keeps the bidirectional copy parked until the
	// server tears things down. upstreamReady signals that the handler's
	// upstream dial landed, so the shuttleTCP is guaranteed to be running
	// before we call Shutdown — without this, the test races the accept
	// loop and can see a no-op Shutdown that returns before its deadline.
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	upstreamReady := make(chan struct{}, 1)
	go func() {
		for {
			c, err := upstream.Accept()
			if err != nil {
				return
			}
			select {
			case upstreamReady <- struct{}{}:
			default:
			}
			// Hold until close; no echo, no writes.
			go func(c net.Conn) {
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
			}(c)
		}
	}()

	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080

	m := metrics.New()
	cfgs := []config.VsockToTCPListener{{
		Port:     vsockPort,
		Upstream: upstream.Addr().String(),
	}}
	s, err := NewServer(nil, cfgs, newLoopbackListenFunc(reg, hostCID), m,
		discardLogger())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	c, err := vsockconn.NewLoopbackDialer(reg, enclaveCID).
		Dial(hostCID, vsockPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Gate on the upstream accept so handleTCP is definitely parked in
	// shuttleTCP before Shutdown races the accept loop.
	select {
	case <-upstreamReady:
	case <-time.After(2 * time.Second):
		t.Fatalf("upstream never accepted")
	}

	// Shutdown with an expired deadline must return a ctx error — the
	// handler's io.Copy is otherwise parked forever — and the
	// force-close path in Shutdown drains tracked connections.
	shutdownCtx, shutdownCancel := context.WithTimeout(
		context.Background(), 100*time.Millisecond)
	defer shutdownCancel()
	if err := s.Shutdown(shutdownCtx); err == nil {
		t.Fatalf("expected shutdown deadline error, got nil")
	}

	// The peer side must now see EOF rather than stay blocked.
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := c.Read(buf); err == nil {
		t.Errorf("expected Read error after forced shutdown, got nil")
	}
}

// TestTCP_Passthrough_NewListenerAcceptsVsockToTCP sanity-checks that the
// direct constructor path builds a vsock_to_tcp listener without going
// through config.Validate.
func TestTCP_Passthrough_NewListenerAcceptsVsockToTCP(t *testing.T) {
	_, err := newVsockToTCPListener(config.VsockToTCPListener{
		Port:     8080,
		Upstream: "127.0.0.1:1",
	}, &Server{})
	if err != nil {
		t.Fatalf("expected TCP listener to build, got %v", err)
	}
}

// TestTCP_Passthrough_UpstreamSwap verifies replaceUpstream — the helper
// the reload path hooks into — atomically publishes a new upstream
// visible to subsequent upstreamSnapshot reads.
func TestTCP_Passthrough_UpstreamSwap(t *testing.T) {
	l, err := newVsockToTCPListener(config.VsockToTCPListener{
		Port:     8080,
		Upstream: "127.0.0.1:1",
	}, &Server{})
	if err != nil {
		t.Fatalf("newVsockToTCPListener: %v", err)
	}
	if got := l.upstreamSnapshot(); got != "127.0.0.1:1" {
		t.Errorf("initial upstream = %q", got)
	}
	const replacement = "10.0.0.5:5432"
	l.replaceUpstream(replacement)
	if got := l.upstreamSnapshot(); got != replacement {
		t.Errorf("after swap upstream = %q, want %q", got, replacement)
	}
}

// plainCounterValue reads the current value out of a labelless
// prometheus.Counter, paralleling counterValue for CounterVec helpers.
func plainCounterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	dm := &dto.Metric{}
	if err := c.Write(dm); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if dm.Counter == nil {
		t.Fatalf("metric has no counter payload")
	}
	return dm.Counter.GetValue()
}

// waitForPlainCounter polls until the labelless counter reaches want or
// the deadline elapses. Byte counters are incremented after the handler
// releases the connection, so a naive read races the test's ReadAll.
func waitForPlainCounter(t *testing.T, c prometheus.Counter, want float64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if plainCounterValue(t, c) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("plain counter did not reach %v; got %v",
		want, plainCounterValue(t, c))
}

// TestTCP_Passthrough_MetricsIncrement verifies the vsock_to_tcp handler
// increments the counters: one connection, byte totals matching the echo
// round-trip, no error counters touched.
func TestTCP_Passthrough_MetricsIncrement(t *testing.T) {
	echo := startEchoServer(t)

	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080

	m := metrics.New()
	cfgs := []config.VsockToTCPListener{{
		Port:     vsockPort,
		Upstream: echo.Addr().String(),
	}}
	startTCPServer(t, cfgs, newLoopbackListenFunc(reg, hostCID), m,
		discardLogger())

	c, err := vsockconn.NewLoopbackDialer(reg, enclaveCID).
		Dial(hostCID, vsockPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	payload := []byte("metrics-payload-0123")
	if _, err := c.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadAll(c); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	_ = c.Close()

	// Connections counter records one accept even though the dial and the
	// close happen asynchronously; bytes are incremented after both
	// shuttle directions finish, so poll on those.
	waitForPlainCounter(t, m.VsockToTCPConnections, 1)
	waitForCounter(t, m.VsockToTCPBytes, float64(len(payload)),
		metrics.DirectionUp)
	waitForCounter(t, m.VsockToTCPBytes, float64(len(payload)),
		metrics.DirectionDown)

	if got := counterValue(t, m.VsockToTCPErrors, metrics.TCPErrorDial); got != 0 {
		t.Errorf("dial error counter = %v, want 0", got)
	}
	if got := counterValue(t, m.VsockToTCPErrors, metrics.TCPErrorCopy); got != 0 {
		t.Errorf("copy error counter = %v, want 0", got)
	}
}

// TestTCP_Passthrough_MetricsDialFail verifies the dial_fail counter
// increments exactly once when the upstream dial cannot complete, and
// that no bytes are recorded in that case.
func TestTCP_Passthrough_MetricsDialFail(t *testing.T) {
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := dead.Addr().String()
	_ = dead.Close()

	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080

	m := metrics.New()
	cfgs := []config.VsockToTCPListener{{
		Port:     vsockPort,
		Upstream: deadAddr,
	}}
	startTCPServer(t, cfgs, newLoopbackListenFunc(reg, hostCID), m,
		discardLogger())

	c, err := vsockconn.NewLoopbackDialer(reg, enclaveCID).
		Dial(hostCID, vsockPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	_, _ = c.Read(buf)
	_ = c.Close()

	waitForPlainCounter(t, m.VsockToTCPConnections, 1)
	waitForCounter(t, m.VsockToTCPErrors, 1, metrics.TCPErrorDial)

	if got := counterValue(t, m.VsockToTCPBytes, metrics.DirectionUp); got != 0 {
		t.Errorf("DirectionUp bytes = %v, want 0", got)
	}
	if got := counterValue(t, m.VsockToTCPBytes, metrics.DirectionDown); got != 0 {
		t.Errorf("DirectionDown bytes = %v, want 0", got)
	}
}

// TestTCP_Passthrough_ConcurrentConnections exercises the race detector
// by running multiple simultaneous TCP-passthrough connections. This
// catches lock-ordering or missed-lock bugs in trackConn/untrackConn and
// the shuttle goroutines.
func TestTCP_Passthrough_ConcurrentConnections(t *testing.T) {
	echo := startEchoServer(t)

	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080

	m := metrics.New()
	cfgs := []config.VsockToTCPListener{{
		Port:     vsockPort,
		Upstream: echo.Addr().String(),
	}}
	startTCPServer(t, cfgs, newLoopbackListenFunc(reg, hostCID), m,
		discardLogger())

	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(id int) {
			defer wg.Done()
			c, err := vsockconn.NewLoopbackDialer(reg, enclaveCID).
				Dial(hostCID, vsockPort)
			if err != nil {
				t.Errorf("worker %d Dial: %v", id, err)
				return
			}
			defer c.Close()
			msg := []byte(strings.Repeat("x", 64))
			if _, err := c.Write(msg); err != nil {
				t.Errorf("worker %d Write: %v", id, err)
				return
			}
			if cw, ok := c.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			got, err := io.ReadAll(c)
			if err != nil {
				t.Errorf("worker %d ReadAll: %v", id, err)
				return
			}
			if !bytes.Equal(got, msg) {
				t.Errorf("worker %d echo mismatch", id)
			}
		}(i)
	}
	wg.Wait()
}

// TestTCP_Passthrough_ApplyModeChangeRejected verifies that swapping a
// port between the HTTP forward-proxy (outbound) and raw passthrough
// (vsock_to_tcp) sections via Apply/SIGHUP is refused with a descriptive
// error and leaves the running listener intact. Silently accepting this
// reload would leave run() dispatching on the stale mode while the swap
// wipes the matcher table — denying every subsequent connection with no
// operator signal.
func TestTCP_Passthrough_ApplyModeChangeRejected(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const vsockPort uint32 = 8080

	m := metrics.New()
	httpCfg := []config.OutboundListener{{
		Port: vsockPort,
		CIDs: []config.OutboundCID{
			{CID: 16, AllowedHosts: []string{"example.com:443"}},
		},
	}}
	s := startServer(t, httpCfg, newLoopbackListenFunc(reg, hostCID), m)

	tcpCfg := []config.VsockToTCPListener{{
		Port:     vsockPort,
		Upstream: "127.0.0.1:9",
	}}
	err := s.Apply(nil, tcpCfg)
	if err == nil {
		t.Fatal("Apply with changed mode returned nil, want error")
	}
	if !strings.Contains(err.Error(), "cannot change mode") {
		t.Fatalf("Apply error = %q; want mention of mode change",
			err.Error())
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.listeners) != 1 {
		t.Fatalf("listeners len = %d, want 1", len(s.listeners))
	}
	if s.listeners[0].mode != modeHTTPProxy {
		t.Fatalf("listener mode = %q; want original HTTP mode preserved",
			s.listeners[0].mode)
	}
	if got := s.listeners[0].matchersSnapshot(); len(got) != 1 {
		t.Fatalf("matcher table size = %d; want 1 (original CID preserved)",
			len(got))
	}
}
