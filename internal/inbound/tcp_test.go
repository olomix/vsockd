package inbound

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// logRecord is a JSON-shaped slog record captured by captureHandler. Tests
// assert on message text and specific attrs, so the payload is kept
// generic to avoid coupling to slog's internal value representation.
type logRecord map[string]any

// captureHandler is a slog handler that funnels every record through an
// internal JSON handler so attribute values land in a stable, comparable
// shape for test assertions.
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
// buffer so a polling caller does not lose earlier entries.
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

// startTCPEchoTarget registers a loopback vsock listener that reads bytes
// and writes them back to the caller, half-closing on EOF so the
// shuttle's read side returns cleanly.
func startTCPEchoTarget(
	t *testing.T,
	reg *vsockconn.Registry,
	cid, port uint32,
) vsockconn.Listener {
	t.Helper()
	ln, err := vsockconn.ListenLoopback(reg, cid, port)
	if err != nil {
		t.Fatalf("ListenLoopback: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c vsockconn.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return ln
}

func startTCPInboundServer(
	t *testing.T,
	listeners []config.InboundListener,
	dialer vsockconn.Dialer,
	m *metrics.Metrics,
	logger *slog.Logger,
) *Server {
	t.Helper()
	s, err := NewServer(listeners, dialer, m, logger)
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

// TestInboundTCP_Passthrough_RoundTrip verifies a TCP-mode inbound
// listener shuttles bytes bidirectionally between a TCP peer and the
// configured vsock target, captures accurate byte totals in the debug
// close log, and emits the expected open log.
func TestInboundTCP_Passthrough_RoundTrip(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080
	startTCPEchoTarget(t, reg, enclaveCID, vsockPort)

	capH := newCaptureHandler()
	logger := slog.New(capH)

	m := metrics.New()
	cfg := []config.InboundListener{{
		Bind:       "127.0.0.1",
		Port:       0,
		Mode:       config.ModeTCP,
		TargetCID:  enclaveCID,
		TargetPort: vsockPort,
	}}
	s := startTCPInboundServer(t, cfg,
		vsockconn.NewLoopbackDialer(reg, 2), m, logger)

	addr := s.Addr(0)
	if addr == nil {
		t.Fatal("Addr(0) returned nil")
	}

	client, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	payload := []byte("ping-pong-0123456789")
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Half-close write so the vsock echo's io.Copy returns and the
	// server-side shuttle unblocks: this also mirrors how real clients
	// signal end-of-request on a request/response protocol.
	tcpClient := client.(*net.TCPConn)
	if err := tcpClient.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo = %q, want %q", got, payload)
	}
	_ = client.Close()

	// Poll for the close log since the handler completes asynchronously.
	var closeLog logRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		closeLog = findMessage(capH.records(t), "tcp connection closed")
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
	if gotTotal := closeLog["total_bytes"]; gotTotal != wantTotal {
		t.Errorf("total_bytes = %v, want %v", gotTotal, wantTotal)
	}
	if gotListen := closeLog["listen"]; gotListen != addr.String() {
		t.Errorf("listen = %v, want %v", gotListen, addr.String())
	}
	if _, ok := closeLog["remote"].(string); !ok {
		t.Errorf("remote attr missing or wrong type: %v", closeLog["remote"])
	}

	openLog := findMessage(capH.records(t), "inbound tcp connection")
	if openLog == nil {
		t.Fatalf("open log not emitted")
	}
	if gotListen := openLog["listen"]; gotListen != addr.String() {
		t.Errorf("open listen = %v, want %v", gotListen, addr.String())
	}
	if _, ok := openLog["remote"].(string); !ok {
		t.Errorf("open remote attr missing or wrong type: %v", openLog["remote"])
	}
}

// TestInboundTCP_Passthrough_DialFailure verifies the handler logs a warn
// and closes the TCP connection cleanly when the vsock target is not
// registered. No panic, no byte transfer.
func TestInboundTCP_Passthrough_DialFailure(t *testing.T) {
	// Registry has nothing registered — every vsock Dial refuses.
	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080

	capH := newCaptureHandler()
	logger := slog.New(capH)

	m := metrics.New()
	cfg := []config.InboundListener{{
		Bind:       "127.0.0.1",
		Port:       0,
		Mode:       config.ModeTCP,
		TargetCID:  enclaveCID,
		TargetPort: vsockPort,
	}}
	s := startTCPInboundServer(t, cfg,
		vsockconn.NewLoopbackDialer(reg, 2), m, logger)

	client, err := net.Dial("tcp", s.Addr(0).String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	// The TCP conn must be closed by the server once the vsock dial
	// fails. A Read blocks until that close propagates.
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	n, _ := client.Read(buf)
	if n != 0 {
		t.Errorf("expected empty read on dial-fail close, got %d bytes", n)
	}

	// Poll for the expected log pair: warn on dial-fail, debug on close.
	var failLog, closeLog logRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		recs := capH.records(t)
		failLog = findMessage(recs, "inbound tcp dial failed")
		closeLog = findMessage(recs, "tcp connection closed")
		if failLog != nil && closeLog != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if failLog == nil {
		t.Fatalf("expected 'inbound tcp dial failed' log")
	}
	if got := failLog["target_cid"]; got != float64(enclaveCID) {
		t.Errorf("target_cid attr = %v, want %v", got, enclaveCID)
	}
	if got := failLog["target_port"]; got != float64(vsockPort) {
		t.Errorf("target_port attr = %v, want %v", got, vsockPort)
	}
	if closeLog == nil {
		t.Fatalf("expected 'tcp connection closed' log even on dial fail")
	}
	if got := closeLog["total_bytes"]; got != float64(0) {
		t.Errorf("total_bytes on dial fail = %v, want 0", got)
	}
}

// withInboundShuttleDrainTimeout temporarily overrides shuttleDrainTimeout
// so a test can exercise the force-close path without stalling the suite.
func withInboundShuttleDrainTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := shuttleDrainTimeout
	shuttleDrainTimeout = d
	t.Cleanup(func() { shuttleDrainTimeout = prev })
}

// TestInboundTCP_Passthrough_ClientDisconnectMidStream verifies a TCP
// peer that drops the connection mid-transfer still lets the handler
// unblock, tear down the vsock conn, and emit a close log.
func TestInboundTCP_Passthrough_ClientDisconnectMidStream(t *testing.T) {
	withInboundShuttleDrainTimeout(t, 300*time.Millisecond)

	// Register a vsock target that silently holds the connection: it
	// accepts, reads forever, but never writes back. This parks the
	// handler's upstream→client copy until something closes the upstream.
	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080
	targetLn, err := vsockconn.ListenLoopback(reg, enclaveCID, vsockPort)
	if err != nil {
		t.Fatalf("ListenLoopback: %v", err)
	}
	t.Cleanup(func() { _ = targetLn.Close() })

	targetReady := make(chan struct{})
	var targetConn vsockconn.Conn
	go func() {
		c, err := targetLn.Accept()
		if err != nil {
			return
		}
		targetConn = c
		close(targetReady)
		_, _ = io.Copy(io.Discard, c)
	}()

	capH := newCaptureHandler()
	logger := slog.New(capH)

	m := metrics.New()
	cfg := []config.InboundListener{{
		Bind:       "127.0.0.1",
		Port:       0,
		Mode:       config.ModeTCP,
		TargetCID:  enclaveCID,
		TargetPort: vsockPort,
	}}
	s := startTCPInboundServer(t, cfg,
		vsockconn.NewLoopbackDialer(reg, 2), m, logger)

	client, err := net.Dial("tcp", s.Addr(0).String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	payload := []byte("partial-stream")
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Wait for the silent target to observe its connection before
	// slamming the client; this guarantees both io.Copy directions are
	// active and exercises the mid-stream abort path.
	select {
	case <-targetReady:
	case <-time.After(2 * time.Second):
		t.Fatalf("target never accepted")
	}

	// Hard-close the client side without half-close: forces both
	// io.Copy goroutines to return via error rather than EOF.
	_ = client.Close()

	var closeLog logRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		closeLog = findMessage(capH.records(t), "tcp connection closed")
		if closeLog != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if closeLog == nil {
		t.Fatalf("close log not emitted after client disconnect")
	}
	// The upstream-facing copy saw at least the initial write.
	if got, _ := closeLog["total_bytes"].(float64); got < float64(len(payload)) {
		t.Errorf("total_bytes = %v, want >= %v", got, len(payload))
	}
	if targetConn != nil {
		_ = targetConn.Close()
	}
}

// TestInboundTCP_Passthrough_ContextCancelViaShutdown verifies that
// shutting down the server tears down in-flight TCP passthrough
// connections inside the shutdown grace window.
func TestInboundTCP_Passthrough_ContextCancelViaShutdown(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080
	targetLn, err := vsockconn.ListenLoopback(reg, enclaveCID, vsockPort)
	if err != nil {
		t.Fatalf("ListenLoopback: %v", err)
	}
	t.Cleanup(func() { _ = targetLn.Close() })
	accepted := make(chan struct{}, 1)
	go func() {
		for {
			c, err := targetLn.Accept()
			if err != nil {
				return
			}
			select {
			case accepted <- struct{}{}:
			default:
			}
			go func(c vsockconn.Conn) {
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
			}(c)
		}
	}()

	m := metrics.New()
	cfg := []config.InboundListener{{
		Bind:       "127.0.0.1",
		Port:       0,
		Mode:       config.ModeTCP,
		TargetCID:  enclaveCID,
		TargetPort: vsockPort,
	}}
	s, err := NewServer(cfg, vsockconn.NewLoopbackDialer(reg, 2),
		m, discardLogger())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	client, err := net.Dial("tcp", s.Addr(0).String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Wait for the target to accept the upstream vsock conn before
	// calling Shutdown: this guarantees the handler has crossed
	// dialUpstream into shuttleTCP and both io.Copy directions are
	// parked. Otherwise a fast Shutdown could race the handler through
	// an early exit path and spuriously pass.
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("target never accepted upstream conn")
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

	// The peer side must now see EOF rather than stay blocked forever.
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := client.Read(buf); err == nil {
		t.Errorf("expected Read error after forced shutdown, got nil")
	}
}

// TestInboundTCP_NewListenerAcceptsTCPMode sanity-checks the constructor
// accepts a mode=tcp config and stores the atomic target fields.
func TestInboundTCP_NewListenerAcceptsTCPMode(t *testing.T) {
	const cid uint32 = 7
	const port uint32 = 4242
	l, err := newListener(config.InboundListener{
		Bind:       "127.0.0.1",
		Port:       5432,
		Mode:       config.ModeTCP,
		TargetCID:  cid,
		TargetPort: port,
	}, &Server{})
	if err != nil {
		t.Fatalf("newListener: %v", err)
	}
	if got := l.targetCID.Load(); got != cid {
		t.Errorf("targetCID = %d, want %d", got, cid)
	}
	if got := l.targetPort.Load(); got != port {
		t.Errorf("targetPort = %d, want %d", got, port)
	}
	// TCP listeners skip the route map; routesSnapshot must return nil
	// so the HTTP path's behaviour is never accidentally triggered.
	if rm := l.routesSnapshot(); rm != nil {
		t.Errorf("routesSnapshot for TCP mode = %v, want nil", rm)
	}
}

// plainCounterValue reads the current value out of a labelless
// prometheus.Counter, paralleling the counterValue helper used for
// CounterVec in server_test.go.
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
// the deadline elapses.
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

// TestInboundTCP_Passthrough_MetricsIncrement verifies the mode=tcp
// inbound handler increments the new counters: one connection, byte
// totals matching the echo round-trip, no error counters touched.
func TestInboundTCP_Passthrough_MetricsIncrement(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080
	startTCPEchoTarget(t, reg, enclaveCID, vsockPort)

	m := metrics.New()
	cfg := []config.InboundListener{{
		Bind:       "127.0.0.1",
		Port:       0,
		Mode:       config.ModeTCP,
		TargetCID:  enclaveCID,
		TargetPort: vsockPort,
	}}
	s := startTCPInboundServer(t, cfg,
		vsockconn.NewLoopbackDialer(reg, 2), m, discardLogger())

	client, err := net.Dial("tcp", s.Addr(0).String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	payload := []byte("metrics-payload-0123")
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = client.(*net.TCPConn).CloseWrite()
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadAll(client); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	_ = client.Close()

	waitForPlainCounter(t, m.TCPInboundConnections, 1)
	waitForCounter(t, m.TCPInboundBytes, float64(len(payload)),
		metrics.DirectionUp)
	waitForCounter(t, m.TCPInboundBytes, float64(len(payload)),
		metrics.DirectionDown)

	if got := counterValue(t, m.TCPInboundErrors, metrics.TCPErrorDial); got != 0 {
		t.Errorf("dial error counter = %v, want 0", got)
	}
	if got := counterValue(t, m.TCPInboundErrors, metrics.TCPErrorCopy); got != 0 {
		t.Errorf("copy error counter = %v, want 0", got)
	}
}

// TestInboundTCP_Passthrough_MetricsDialFail verifies the dial_fail
// counter increments exactly once when the vsock target refuses, and
// that no bytes are recorded for that connection.
func TestInboundTCP_Passthrough_MetricsDialFail(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080

	m := metrics.New()
	cfg := []config.InboundListener{{
		Bind:       "127.0.0.1",
		Port:       0,
		Mode:       config.ModeTCP,
		TargetCID:  enclaveCID,
		TargetPort: vsockPort,
	}}
	s := startTCPInboundServer(t, cfg,
		vsockconn.NewLoopbackDialer(reg, 2), m, discardLogger())

	client, err := net.Dial("tcp", s.Addr(0).String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	_, _ = client.Read(buf)
	_ = client.Close()

	waitForPlainCounter(t, m.TCPInboundConnections, 1)
	waitForCounter(t, m.TCPInboundErrors, 1, metrics.TCPErrorDial)

	if got := counterValue(t, m.TCPInboundBytes, metrics.DirectionUp); got != 0 {
		t.Errorf("DirectionUp bytes = %v, want 0", got)
	}
	if got := counterValue(t, m.TCPInboundBytes, metrics.DirectionDown); got != 0 {
		t.Errorf("DirectionDown bytes = %v, want 0", got)
	}
}

// TestInboundTCP_Passthrough_ConcurrentConnections exercises the race
// detector by running multiple simultaneous TCP passthrough connections,
// catching any missed locking in trackConn/untrackConn and the shuttle
// goroutines.
func TestInboundTCP_Passthrough_ConcurrentConnections(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080
	startTCPEchoTarget(t, reg, enclaveCID, vsockPort)

	m := metrics.New()
	cfg := []config.InboundListener{{
		Bind:       "127.0.0.1",
		Port:       0,
		Mode:       config.ModeTCP,
		TargetCID:  enclaveCID,
		TargetPort: vsockPort,
	}}
	s := startTCPInboundServer(t, cfg,
		vsockconn.NewLoopbackDialer(reg, 2), m, discardLogger())

	addr := s.Addr(0).String()
	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(id int) {
			defer wg.Done()
			client, err := net.Dial("tcp", addr)
			if err != nil {
				t.Errorf("worker %d Dial: %v", id, err)
				return
			}
			defer client.Close()
			msg := []byte(strings.Repeat("x", 64))
			if _, err := client.Write(msg); err != nil {
				t.Errorf("worker %d Write: %v", id, err)
				return
			}
			if tc, ok := client.(*net.TCPConn); ok {
				_ = tc.CloseWrite()
			}
			_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
			got, err := io.ReadAll(client)
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

// TestInboundTCP_DialCancelledByShutdown verifies that Shutdown's grace
// window bounds time spent waiting on an outstanding vsock Dial. The
// blockingDialer never returns on its own, so the only way the handleTCP
// goroutine can exit within the grace is for Shutdown to cancel the
// dial via dialCtx.
func TestInboundTCP_DialCancelledByShutdown(t *testing.T) {
	dialer := &blockingDialer{
		release: make(chan struct{}),
		started: make(chan struct{}, 1),
	}
	defer close(dialer.release)

	m := metrics.New()
	ephemeralPort := func() int {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		p := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		return p
	}()
	s, err := NewServer(
		[]config.InboundListener{{
			Bind:       "127.0.0.1",
			Port:       ephemeralPort,
			Mode:       config.ModeTCP,
			TargetCID:  16,
			TargetPort: 1,
		}},
		dialer, m, discardLogger(),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	client, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", ephemeralPort))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	select {
	case <-dialer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("blockingDialer.Dial was never called")
	}

	grace := 200 * time.Millisecond
	sctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	start := time.Now()
	err = s.Shutdown(sctx)
	dur := time.Since(start)

	if dur > grace+2*time.Second {
		t.Errorf("Shutdown took %v; dial was not cancelled within grace", dur)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown err = %v; want context.DeadlineExceeded", err)
	}
}

