package inbound

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/olomix/vsockd/internal/config"
	"github.com/olomix/vsockd/internal/metrics"
	"github.com/olomix/vsockd/internal/vsockconn"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// discardLogger silences server logs during tests without losing the
// structured fields if a test needs to look at them later.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
		Level: slog.LevelError,
	}))
}

// startServer builds a Server, starts it, and returns the server plus a
// cleanup that stops it with a generous shutdown grace. The inbound
// listeners are configured on 127.0.0.1:0 so tests get ephemeral ports.
func startServer(
	t *testing.T,
	listeners []config.InboundListener,
	dialer vsockconn.Dialer,
	m *metrics.Metrics,
) *Server {
	t.Helper()
	s, err := NewServer(listeners, nil, dialer, m, discardLogger())
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

func fakeEnclave(
	t *testing.T,
	reg *vsockconn.Registry,
	cid, port uint32,
) (recv chan []byte, reply chan []byte, ln vsockconn.Listener) {
	t.Helper()
	ln, err := vsockconn.ListenLoopback(reg, cid, port)
	if err != nil {
		t.Fatalf("ListenLoopback: %v", err)
	}
	recv = make(chan []byte, 1)
	reply = make(chan []byte, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c vsockconn.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				n, rerr := c.Read(buf)
				if n > 0 {
					data := make([]byte, n)
					copy(data, buf[:n])
					select {
					case recv <- data:
					default:
					}
				}
				if rerr != nil && !errors.Is(rerr, io.EOF) {
					return
				}
				select {
				case resp := <-reply:
					_, _ = c.Write(resp)
				default:
				}
			}(c)
		}
	}()
	return recv, reply, ln
}

func counterValue(
	t *testing.T,
	vec *prometheus.CounterVec,
	labels ...string,
) float64 {
	t.Helper()
	c, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	m := &dto.Metric{}
	if err := c.Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if m.Counter == nil {
		t.Fatalf("metric has no counter payload")
	}
	return m.Counter.GetValue()
}

func waitForCounter(
	t *testing.T,
	vec *prometheus.CounterVec,
	want float64,
	labels ...string,
) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if counterValue(t, vec, labels...) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("counter %v did not reach %v within deadline; got %v",
		labels, want, counterValue(t, vec, labels...))
}

// TestServerHTTP_RoutesMatchingHost drives an http-host listener with a
// matching Host header and verifies the fake enclave receives the full
// request verbatim and that metrics reflect the successful proxy.
func TestServerHTTP_RoutesMatchingHost(t *testing.T) {
	reg := vsockconn.NewRegistry()
	recv, reply, ln := fakeEnclave(t, reg, 16, 8080)
	defer ln.Close()

	m := metrics.New()
	cfg := []config.InboundListener{{
		Bind: "127.0.0.1",
		Port: 0,
		Mode: config.ModeHTTPHost,
		Routes: []config.Route{
			{Hostname: "api.example.com", CID: 16, VsockPort: 8080},
		},
	}}
	s := startServer(t, cfg, vsockconn.NewLoopbackDialer(reg, 2), m)

	addr := s.Addr(0)
	if addr == nil {
		t.Fatal("Addr(0) returned nil")
	}

	client, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	req := "GET /hello HTTP/1.1\r\n" +
		"Host: api.example.com\r\n" +
		"User-Agent: test\r\n\r\n"
	if _, err := client.Write([]byte(req)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	respBody := []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
	reply <- respBody

	select {
	case got := <-recv:
		if !bytes.Equal(got, []byte(req)) {
			t.Errorf("enclave got %q\nwant %q", got, req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake enclave did not receive request")
	}

	// Read response that the fake enclave echoed back.
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, len(respBody))
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("ReadFull response: %v", err)
	}
	if !bytes.Equal(buf, respBody) {
		t.Errorf("client response %q, want %q", buf, respBody)
	}

	waitForCounter(t, m.InboundConnections, 1, "api.example.com")
	// Replayed sniff bytes alone should exceed zero; both directions should
	// eventually accumulate some bytes.
	waitForCounter(t, m.InboundBytes, 1, "api.example.com", metrics.DirectionIn)
	waitForCounter(t, m.InboundBytes, 1, "api.example.com", metrics.DirectionOut)
}

// TestServerHTTP_NonMatchingHostCloses verifies a Host-header miss drops
// the connection without dialing any upstream. The counting dialer fails if
// anyone Dial()s it.
func TestServerHTTP_NonMatchingHostCloses(t *testing.T) {
	m := metrics.New()
	dialer := &counterDialer{fail: true}

	cfg := []config.InboundListener{{
		Bind: "127.0.0.1",
		Port: 0,
		Mode: config.ModeHTTPHost,
		Routes: []config.Route{
			{Hostname: "api.example.com", CID: 16, VsockPort: 8080},
		},
	}}
	s := startServer(t, cfg, dialer, m)

	client, err := net.Dial("tcp", s.Addr(0).String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	req := "GET / HTTP/1.1\r\nHost: other.example.com\r\n\r\n"
	if _, err := client.Write([]byte(req)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Server should close without writing a response.
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	n, err := client.Read(buf)
	if err == nil && n > 0 {
		t.Fatalf("expected connection close, got %d bytes", n)
	}

	waitForCounter(t, m.InboundErrors, 1,
		routeLabelUnknown, metrics.InboundErrorRoute)

	if dialer.called() != 0 {
		t.Errorf("dialer was called %d times; expected 0", dialer.called())
	}
}

// TestServerTLS_RoutesMatchingSNI sends a recorded ClientHello carrying SNI
// "api.example.com" to a tls-sni listener and verifies the fake enclave
// receives the full record verbatim.
func TestServerTLS_RoutesMatchingSNI(t *testing.T) {
	reg := vsockconn.NewRegistry()
	recv, _, ln := fakeEnclave(t, reg, 20, 8443)
	defer ln.Close()

	m := metrics.New()
	cfg := []config.InboundListener{{
		Bind: "127.0.0.1",
		Port: 0,
		Mode: config.ModeTLSSNI,
		Routes: []config.Route{
			{Hostname: "api.example.com", CID: 20, VsockPort: 8443},
		},
	}}
	s := startServer(t, cfg, vsockconn.NewLoopbackDialer(reg, 2), m)

	hello := recordClientHello(t, "api.example.com")

	client, err := net.Dial("tcp", s.Addr(0).String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	if _, err := client.Write(hello); err != nil {
		t.Fatalf("Write ClientHello: %v", err)
	}

	select {
	case got := <-recv:
		// The enclave may see more bytes than just the ClientHello (the
		// buffered sniff + the Read side of the client socket), but the
		// leading bytes MUST equal the ClientHello exactly.
		if !bytes.HasPrefix(got, hello) {
			t.Errorf("enclave did not receive ClientHello prefix; got %d bytes, want prefix %d",
				len(got), len(hello))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake enclave did not receive ClientHello")
	}

	waitForCounter(t, m.InboundConnections, 1, "api.example.com")
	waitForCounter(t, m.InboundBytes, float64(len(hello)),
		"api.example.com", metrics.DirectionIn)
}

// TestServerTLS_NonMatchingSNICloses verifies a SNI miss drops without
// dialing upstream.
func TestServerTLS_NonMatchingSNICloses(t *testing.T) {
	m := metrics.New()
	dialer := &counterDialer{fail: true}

	cfg := []config.InboundListener{{
		Bind: "127.0.0.1",
		Port: 0,
		Mode: config.ModeTLSSNI,
		Routes: []config.Route{
			{Hostname: "api.example.com", CID: 20, VsockPort: 8443},
		},
	}}
	s := startServer(t, cfg, dialer, m)

	hello := recordClientHello(t, "other.example.com")

	client, err := net.Dial("tcp", s.Addr(0).String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	if _, err := client.Write(hello); err != nil {
		t.Fatalf("Write ClientHello: %v", err)
	}

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	n, err := client.Read(buf)
	if err == nil && n > 0 {
		t.Fatalf("expected connection close, got %d bytes", n)
	}

	waitForCounter(t, m.InboundErrors, 1,
		routeLabelUnknown, metrics.InboundErrorRoute)
	if dialer.called() != 0 {
		t.Errorf("dialer was called %d times; expected 0", dialer.called())
	}
}

// TestServerUpstreamDialFailure verifies that a vsock dial failure is
// reported as a dial-kind error and the client is closed cleanly.
func TestServerUpstreamDialFailure(t *testing.T) {
	reg := vsockconn.NewRegistry() // nothing registered — every Dial refuses

	m := metrics.New()
	cfg := []config.InboundListener{{
		Bind: "127.0.0.1",
		Port: 0,
		Mode: config.ModeHTTPHost,
		Routes: []config.Route{
			{Hostname: "api.example.com", CID: 16, VsockPort: 8080},
		},
	}}
	s := startServer(t, cfg, vsockconn.NewLoopbackDialer(reg, 2), m)

	client, err := net.Dial("tcp", s.Addr(0).String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	req := "GET / HTTP/1.1\r\nHost: api.example.com\r\n\r\n"
	if _, err := client.Write([]byte(req)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	n, err := client.Read(buf)
	if err == nil && n > 0 {
		t.Fatalf("expected close, got %d bytes", n)
	}

	waitForCounter(t, m.InboundErrors, 1,
		"api.example.com", metrics.InboundErrorDial)
	// Connection counter still increments even when the dial fails; this
	// records a decision was attempted for this route.
	waitForCounter(t, m.InboundConnections, 1, "api.example.com")
}

// TestServerShutdownGraceful verifies Shutdown returns cleanly when no
// connections are in flight.
func TestServerShutdownGraceful(t *testing.T) {
	reg := vsockconn.NewRegistry()
	_, _, ln := fakeEnclave(t, reg, 16, 8080)
	defer ln.Close()

	m := metrics.New()
	cfg := []config.InboundListener{{
		Bind: "127.0.0.1",
		Port: 0,
		Mode: config.ModeHTTPHost,
		Routes: []config.Route{
			{Hostname: "api.example.com", CID: 16, VsockPort: 8080},
		},
	}}
	s, err := NewServer(cfg, nil, vsockconn.NewLoopbackDialer(reg, 2),
		m, discardLogger())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(
		context.Background(), time.Second)
	defer shutdownCancel()
	if err := s.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// After Shutdown, a fresh Dial to the old addr should fail.
	if _, err := net.DialTimeout("tcp", s.Addr(0).String(),
		200*time.Millisecond); err == nil {
		t.Fatal("expected Dial to fail after Shutdown")
	}
}

// TestServerShutdownForceClosesStuckConn verifies that a connection still
// in-flight when Shutdown's context expires is force-closed rather than
// hanging the daemon forever. The fake enclave deliberately never replies,
// and the client keeps its half open; only the forced close can unblock it.
func TestServerShutdownForceClosesStuckConn(t *testing.T) {
	reg := vsockconn.NewRegistry()

	// Register a listener that accepts and then silently holds the
	// connection open, never replying. This pins the io.Copy reading from
	// upstream on the server side until someone closes the upstream.
	enclaveReady := make(chan struct{})
	enclaveHold := make(chan struct{})
	enclaveLn, err := vsockconn.ListenLoopback(reg, 16, 8080)
	if err != nil {
		t.Fatalf("ListenLoopback: %v", err)
	}
	defer enclaveLn.Close()
	go func() {
		for {
			c, err := enclaveLn.Accept()
			if err != nil {
				return
			}
			close(enclaveReady)
			<-enclaveHold
			c.Close()
		}
	}()
	defer close(enclaveHold)

	m := metrics.New()
	cfg := []config.InboundListener{{
		Bind: "127.0.0.1",
		Port: 0,
		Mode: config.ModeHTTPHost,
		Routes: []config.Route{
			{Hostname: "api.example.com", CID: 16, VsockPort: 8080},
		},
	}}
	s, err := NewServer(cfg, nil, vsockconn.NewLoopbackDialer(reg, 2),
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
	req := "GET / HTTP/1.1\r\nHost: api.example.com\r\n\r\n"
	if _, err := client.Write([]byte(req)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case <-enclaveReady:
	case <-time.After(2 * time.Second):
		t.Fatal("enclave did not accept")
	}

	// The client is still holding its half open. A Shutdown call with a
	// very short grace must return ctx.Err() and force-close the conn.
	shutdownCtx, shutdownCancel := context.WithTimeout(
		context.Background(), 100*time.Millisecond)
	defer shutdownCancel()
	start := time.Now()
	err = s.Shutdown(shutdownCtx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected Shutdown to return context error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Shutdown took %v; expected quick force-close", elapsed)
	}
}

// TestServerMultipleListeners starts one http-host and one tls-sni listener
// on distinct ports and verifies both route correctly.
func TestServerMultipleListeners(t *testing.T) {
	reg := vsockconn.NewRegistry()
	httpRecv, httpReply, lnHTTP := fakeEnclave(t, reg, 16, 8080)
	defer lnHTTP.Close()
	tlsRecv, _, lnTLS := fakeEnclave(t, reg, 20, 8443)
	defer lnTLS.Close()

	m := metrics.New()
	cfg := []config.InboundListener{
		{
			Bind: "127.0.0.1",
			Port: 0,
			Mode: config.ModeHTTPHost,
			Routes: []config.Route{
				{Hostname: "api.example.com", CID: 16, VsockPort: 8080},
			},
		},
		{
			Bind: "127.0.0.1",
			Port: 0,
			Mode: config.ModeTLSSNI,
			Routes: []config.Route{
				{Hostname: "api.example.com", CID: 20, VsockPort: 8443},
			},
		},
	}
	s := startServer(t, cfg, vsockconn.NewLoopbackDialer(reg, 2), m)
	httpAddr := s.Addr(0).String()
	tlsAddr := s.Addr(1).String()
	if httpAddr == tlsAddr {
		t.Fatal("two listeners should bind distinct ports")
	}

	// HTTP path.
	c1, err := net.Dial("tcp", httpAddr)
	if err != nil {
		t.Fatalf("HTTP Dial: %v", err)
	}
	defer c1.Close()
	httpReq := "GET /h HTTP/1.1\r\nHost: api.example.com\r\n\r\n"
	if _, err := c1.Write([]byte(httpReq)); err != nil {
		t.Fatalf("HTTP Write: %v", err)
	}
	httpReply <- []byte("HTTP/1.1 204 No Content\r\n\r\n")

	select {
	case got := <-httpRecv:
		if !strings.HasPrefix(string(got), "GET /h") {
			t.Errorf("http enclave got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("http enclave did not receive")
	}

	// TLS path.
	c2, err := net.Dial("tcp", tlsAddr)
	if err != nil {
		t.Fatalf("TLS Dial: %v", err)
	}
	defer c2.Close()
	hello := recordClientHello(t, "api.example.com")
	if _, err := c2.Write(hello); err != nil {
		t.Fatalf("TLS Write: %v", err)
	}

	select {
	case got := <-tlsRecv:
		if !bytes.HasPrefix(got, hello) {
			t.Errorf("tls enclave did not receive ClientHello prefix")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tls enclave did not receive")
	}
}

// TestServerNoListenersConfigured verifies we accept empty input cleanly —
// Start is a no-op, Shutdown does nothing.
func TestServerNoListenersConfigured(t *testing.T) {
	m := metrics.New()
	s, err := NewServer(nil, nil, vsockconn.NewLoopbackDialer(
		vsockconn.NewRegistry(), 2), m, discardLogger())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(
		context.Background(), 500*time.Millisecond)
	defer shutdownCancel()
	if err := s.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// TestNewServerRejectsUnknownMode verifies mode validation happens in
// NewServer so invalid configs do not reach Start.
func TestNewServerRejectsUnknownMode(t *testing.T) {
	m := metrics.New()
	_, err := NewServer(
		[]config.InboundListener{{
			Bind: "127.0.0.1",
			Port: 0,
			Mode: "bogus",
			Routes: []config.Route{
				{Hostname: "x", CID: 16, VsockPort: 1},
			},
		}},
		nil,
		vsockconn.NewLoopbackDialer(vsockconn.NewRegistry(), 2),
		m, discardLogger(),
	)
	if err == nil {
		t.Fatal("expected NewServer to reject unknown mode")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error = %v; want mention of bogus mode", err)
	}
}

// TestNewServerRejectsNilDependencies locks down the required arguments.
func TestNewServerRejectsNilDependencies(t *testing.T) {
	reg := vsockconn.NewRegistry()
	if _, err := NewServer(nil, nil, nil,
		metrics.New(), discardLogger()); err == nil {
		t.Error("expected error on nil dialer")
	}
	if _, err := NewServer(nil, nil,
		vsockconn.NewLoopbackDialer(reg, 2), nil, discardLogger()); err == nil {
		t.Error("expected error on nil metrics")
	}
}

// TestServerBindConflict verifies Start surfaces bind errors.
func TestServerBindConflict(t *testing.T) {
	// Hold a port so the server can't bind it.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer held.Close()

	port := held.Addr().(*net.TCPAddr).Port
	m := metrics.New()
	s, err := NewServer(
		[]config.InboundListener{{
			Bind: "127.0.0.1",
			Port: port,
			Mode: config.ModeHTTPHost,
			Routes: []config.Route{
				{Hostname: "x", CID: 16, VsockPort: 1},
			},
		}},
		nil,
		vsockconn.NewLoopbackDialer(vsockconn.NewRegistry(), 2),
		m, discardLogger(),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("expected bind conflict error")
	}
}

// TestApplyModeChangeSameAddrReturnsClearError verifies that swapping the
// sniff mode on an already-bound bind:port is rejected with a descriptive
// error rather than bubbling up a confusing EADDRINUSE from net.Listen —
// and that the running listener stays intact when the reload is refused.
func TestApplyModeChangeSameAddrReturnsClearError(t *testing.T) {
	port := func() int {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		p := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		return p
	}()

	m := metrics.New()
	cfg := []config.InboundListener{{
		Bind: "127.0.0.1",
		Port: port,
		Mode: config.ModeHTTPHost,
		Routes: []config.Route{
			{Hostname: "x", CID: 16, VsockPort: 1},
		},
	}}
	s := startServer(t, cfg, vsockconn.NewLoopbackDialer(vsockconn.NewRegistry(), 2), m)

	updated := []config.InboundListener{{
		Bind: "127.0.0.1",
		Port: port,
		Mode: config.ModeTLSSNI,
		Routes: []config.Route{
			{Hostname: "x", CID: 16, VsockPort: 1},
		},
	}}
	err := s.Apply(updated, nil)
	if err == nil {
		t.Fatal("Apply with changed mode returned nil, want error")
	}
	if !strings.Contains(err.Error(), "cannot change mode") {
		t.Fatalf("Apply error = %q; want mention of mode change", err.Error())
	}

	// Running listener must stay on the original mode.
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.listeners) != 1 {
		t.Fatalf("listeners len = %d, want 1", len(s.listeners))
	}
	if s.listeners[0].mode != config.ModeHTTPHost {
		t.Fatalf("listener mode = %q; want original %q preserved",
			s.listeners[0].mode, config.ModeHTTPHost)
	}
}

// blockingDialer hangs every Dial call until release is closed. Used to
// exercise the dial-cancellation path in Shutdown: without dialCtx being
// plumbed through, handle() would stay blocked here indefinitely and
// Shutdown would hang past its grace window.
type blockingDialer struct {
	release chan struct{}
	started chan struct{}
}

func (d *blockingDialer) Dial(cid, port uint32) (net.Conn, error) {
	select {
	case d.started <- struct{}{}:
	default:
	}
	<-d.release
	return nil, errors.New("blockingDialer: released")
}

// TestShutdownCancelsPendingUpstreamDial verifies that Shutdown's grace
// window bounds time spent waiting on an outstanding vsock Dial. The
// blockingDialer never returns on its own, so the only way the handle
// goroutine can exit within the shutdown grace is for Shutdown to cancel
// the dial via dialCtx.
func TestShutdownCancelsPendingUpstreamDial(t *testing.T) {
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
			Bind: "127.0.0.1",
			Port: ephemeralPort,
			Mode: config.ModeHTTPHost,
			Routes: []config.Route{
				{Hostname: "x", CID: 16, VsockPort: 1},
			},
		}},
		nil,
		dialer, m, discardLogger(),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", ephemeralPort)
	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	_, _ = client.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))

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

// counterDialer is a Dialer stub used by tests that must not actually dial.
// When fail is true, every Dial returns an error; counts remain visible via
// called() so tests can assert "dialer never touched".
type counterDialer struct {
	fail  bool
	count int
}

func (d *counterDialer) Dial(cid, port uint32) (net.Conn, error) {
	d.count++
	if d.fail {
		return nil, fmt.Errorf("counterDialer: configured to fail")
	}
	// Return a no-op conn; we'll not actually exercise the proxy path in
	// these tests.
	a, b := net.Pipe()
	go func() { _ = b.Close() }()
	return a, nil
}

func (d *counterDialer) called() int { return d.count }
