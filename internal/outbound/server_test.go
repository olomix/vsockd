package outbound

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/olomix/vsockd/internal/config"
	"github.com/olomix/vsockd/internal/metrics"
	"github.com/olomix/vsockd/internal/vsockconn"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

const (
	// hostCID is the host side of the loopback vsock in these tests. The
	// real daemon does not care about this value because vsock.Listen is
	// not CID-scoped, but the loopback Registry needs a key and using 2
	// (VMADDR_CID_HOST) mirrors the Nitro parent-host convention.
	hostCID uint32 = 2
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
		Level: slog.LevelError,
	}))
}

// newLoopbackListenFunc returns a ListenFunc that binds the outbound
// server on loopback vsock ports, all hanging off the supplied Registry.
// Tests pass this wrapper instead of the real vsockconn.ListenVsock.
func newLoopbackListenFunc(
	reg *vsockconn.Registry,
	cid uint32,
) ListenFunc {
	return func(port uint32) (vsockconn.Listener, error) {
		return vsockconn.ListenLoopback(reg, cid, port)
	}
}

func startServer(
	t *testing.T,
	cfgs []config.OutboundListener,
	listenFn ListenFunc,
	m *metrics.Metrics,
) *Server {
	t.Helper()
	s, err := NewServer(cfgs, nil, listenFn, m, discardLogger())
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

// startEchoServer binds a 127.0.0.1 TCP listener that echoes every byte
// back. It closes on t.Cleanup.
func startEchoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return ln
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

// waitForCounterNonZero polls until the counter's value is > 0 or the
// deadline elapses. Use this when asserting byte-count metrics: the server
// increments them after the response has been written, so a naive
// counterValue check races against the test's resp.Body read.
func waitForCounterNonZero(
	t *testing.T,
	vec *prometheus.CounterVec,
	labels ...string,
) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if counterValue(t, vec, labels...) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("counter %v stayed at 0 within deadline", labels)
}

// TestConnect_AllowedTunnels verifies an allowlisted CONNECT gets 200 and
// bytes flow bidirectionally through the tunnel to a local echo server.
func TestConnect_AllowedTunnels(t *testing.T) {
	echo := startEchoServer(t)

	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080

	m := metrics.New()
	cfgs := []config.OutboundListener{{
		Port: vsockPort,
		CIDs: []config.OutboundCID{{
			CID:          enclaveCID,
			AllowedHosts: []string{echo.Addr().String()},
		}},
	}}
	startServer(t, cfgs, newLoopbackListenFunc(reg, hostCID), m)

	d := vsockconn.NewLoopbackDialer(reg, enclaveCID)
	c, err := d.Dial(hostCID, vsockPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	connectLine := fmt.Sprintf(
		"CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n",
		echo.Addr().String(), echo.Addr().String())
	if _, err := c.Write([]byte(connectLine)); err != nil {
		t.Fatalf("Write CONNECT: %v", err)
	}

	br := bufio.NewReader(c)
	// Pass a fake CONNECT request so ReadResponse treats the response as
	// bodiless per RFC 7230 §3.3.3. Without this, the stdlib would try to
	// consume tunnel bytes as the response body.
	resp, err := http.ReadResponse(br,
		&http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Use the tunnel: write bytes, expect echo back.
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatalf("tunnel Write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatalf("tunnel Read: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("tunnel echo = %q, want %q", buf, "ping")
	}

	waitForCounter(t, m.OutboundConnections, 1,
		metrics.FormatCID(enclaveCID), metrics.OutboundResultAllowed)
	// Byte counters are updated only once io.Copy unblocks, which happens
	// on close. Close the client side to trigger that path.
	_ = c.Close()
	waitForCounter(t, m.OutboundBytes, 4,
		metrics.FormatCID(enclaveCID), metrics.DirectionOut)
	waitForCounter(t, m.OutboundBytes, 4,
		metrics.FormatCID(enclaveCID), metrics.DirectionIn)
}

// TestConnect_DeniedByAllowlist verifies a CONNECT to a host not on the
// allowlist returns 403 and never dials upstream.
func TestConnect_DeniedByAllowlist(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080

	m := metrics.New()
	cfgs := []config.OutboundListener{{
		Port: vsockPort,
		CIDs: []config.OutboundCID{{
			CID:          enclaveCID,
			AllowedHosts: []string{"allowed.example.com:443"},
		}},
	}}
	startServer(t, cfgs, newLoopbackListenFunc(reg, hostCID), m)

	d := vsockconn.NewLoopbackDialer(reg, enclaveCID)
	c, err := d.Dial(hostCID, vsockPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	connectLine := "CONNECT blocked.example.com:443 HTTP/1.1\r\n" +
		"Host: blocked.example.com:443\r\n\r\n"
	if _, err := c.Write([]byte(connectLine)); err != nil {
		t.Fatalf("Write CONNECT: %v", err)
	}

	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	waitForCounter(t, m.OutboundConnections, 1,
		metrics.FormatCID(enclaveCID), metrics.OutboundResultDenied)
}

// TestProxy_AbsoluteURIGet verifies that an absolute-URI GET is rewritten
// to origin form, the upstream receives it with the correct Host header,
// and the response streams back to the enclave.
func TestProxy_AbsoluteURIGet(t *testing.T) {
	var sawRequestURI, sawHost string
	upstream := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sawRequestURI = r.RequestURI
			sawHost = r.Host
			fmt.Fprintf(w, "hello from %s", r.Host)
		}))
	t.Cleanup(upstream.Close)

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080

	m := metrics.New()
	cfgs := []config.OutboundListener{{
		Port: vsockPort,
		CIDs: []config.OutboundCID{{
			CID:          enclaveCID,
			AllowedHosts: []string{u.Host},
		}},
	}}
	startServer(t, cfgs, newLoopbackListenFunc(reg, hostCID), m)

	d := vsockconn.NewLoopbackDialer(reg, enclaveCID)
	c, err := d.Dial(hostCID, vsockPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	reqLine := fmt.Sprintf(
		"GET %s/hello HTTP/1.1\r\nHost: %s\r\n\r\n",
		upstream.URL, u.Host)
	if _, err := c.Write([]byte(reqLine)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	want := "hello from " + u.Host
	if string(body) != want {
		t.Errorf("body = %q, want %q", body, want)
	}
	if sawRequestURI != "/hello" {
		t.Errorf("upstream RequestURI = %q, want %q", sawRequestURI, "/hello")
	}
	if sawHost != u.Host {
		t.Errorf("upstream Host = %q, want %q", sawHost, u.Host)
	}

	waitForCounter(t, m.OutboundConnections, 1,
		metrics.FormatCID(enclaveCID), metrics.OutboundResultAllowed)
	// Non-zero byte counts in both directions. Poll to avoid racing the
	// server's post-write counter increment.
	waitForCounterNonZero(t, m.OutboundBytes,
		metrics.FormatCID(enclaveCID), metrics.DirectionOut)
	waitForCounterNonZero(t, m.OutboundBytes,
		metrics.FormatCID(enclaveCID), metrics.DirectionIn)
}

// TestProxy_HostHeaderRewritten verifies RFC 7230 §5.4: when the enclave
// sends an absolute-URI request whose Host header disagrees with the
// request-target, the proxy must ignore the received Host header and
// forward the request-target's host. This also defends against using a
// mismatched Host header to reach a different vhost on an allowlisted IP.
func TestProxy_HostHeaderRewritten(t *testing.T) {
	var sawHost string
	upstream := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sawHost = r.Host
			fmt.Fprintln(w, "ok")
		}))
	t.Cleanup(upstream.Close)

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080

	m := metrics.New()
	cfgs := []config.OutboundListener{{
		Port: vsockPort,
		CIDs: []config.OutboundCID{{
			CID:          enclaveCID,
			AllowedHosts: []string{u.Host},
		}},
	}}
	startServer(t, cfgs, newLoopbackListenFunc(reg, hostCID), m)

	d := vsockconn.NewLoopbackDialer(reg, enclaveCID)
	c, err := d.Dial(hostCID, vsockPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	reqLine := fmt.Sprintf(
		"GET %s/ HTTP/1.1\r\nHost: attacker.example.com\r\n\r\n",
		upstream.URL)
	if _, err := c.Write([]byte(reqLine)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if sawHost != u.Host {
		t.Errorf("upstream Host = %q, want %q (received Host header "+
			"must be replaced with request-target host)", sawHost, u.Host)
	}
}

// TestProxy_DeniedByAllowlist verifies absolute-URI GET to a disallowed
// destination gets 403.
func TestProxy_DeniedByAllowlist(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080

	m := metrics.New()
	cfgs := []config.OutboundListener{{
		Port: vsockPort,
		CIDs: []config.OutboundCID{{
			CID:          enclaveCID,
			AllowedHosts: []string{"allowed.example.com:80"},
		}},
	}}
	startServer(t, cfgs, newLoopbackListenFunc(reg, hostCID), m)

	d := vsockconn.NewLoopbackDialer(reg, enclaveCID)
	c, err := d.Dial(hostCID, vsockPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	reqLine := "GET http://blocked.example.com/path HTTP/1.1\r\n" +
		"Host: blocked.example.com\r\n\r\n"
	if _, err := c.Write([]byte(reqLine)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	waitForCounter(t, m.OutboundConnections, 1,
		metrics.FormatCID(enclaveCID), metrics.OutboundResultDenied)
}

// TestWrongPeerCID_Closed verifies a connection from a CID not listed on
// the port is dropped without reading any request bytes.
func TestWrongPeerCID_Closed(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const allowedCID uint32 = 16
	const unknownCID uint32 = 99
	const vsockPort uint32 = 8080

	m := metrics.New()
	cfgs := []config.OutboundListener{{
		Port: vsockPort,
		CIDs: []config.OutboundCID{{
			CID:          allowedCID,
			AllowedHosts: []string{"*"},
		}},
	}}
	startServer(t, cfgs, newLoopbackListenFunc(reg, hostCID), m)

	d := vsockconn.NewLoopbackDialer(reg, unknownCID)
	c, err := d.Dial(hostCID, vsockPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	n, err := c.Read(buf)
	if err == nil && n > 0 {
		t.Fatalf("expected close, got %d bytes (%q)", n, buf[:n])
	}

	waitForCounter(t, m.OutboundConnections, 1,
		metrics.CIDLabelUnauthorized, metrics.OutboundResultDenied)
	// The unauthorized-CID label keeps metric cardinality bounded.
	if v := counterValue(t, m.OutboundConnections,
		metrics.FormatCID(unknownCID), metrics.OutboundResultDenied); v != 0 {
		t.Errorf("raw unknownCID must not appear as a metric label, got %v", v)
	}
	// Counter for the legitimate CID must stay at zero.
	if v := counterValue(t, m.OutboundConnections,
		metrics.FormatCID(allowedCID),
		metrics.OutboundResultAllowed); v != 0 {
		t.Errorf("allowed counter for legitimate CID = %v, want 0", v)
	}
}

// TestMalformedRequest_400 verifies the server writes a 400 on an
// unparseable request line.
func TestMalformedRequest_400(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080

	m := metrics.New()
	cfgs := []config.OutboundListener{{
		Port: vsockPort,
		CIDs: []config.OutboundCID{{
			CID:          enclaveCID,
			AllowedHosts: []string{"*"},
		}},
	}}
	startServer(t, cfgs, newLoopbackListenFunc(reg, hostCID), m)

	d := vsockconn.NewLoopbackDialer(reg, enclaveCID)
	c, err := d.Dial(hostCID, vsockPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	// Three tokens after the first space make parseRequestLine reject
	// the version field.
	if _, err := c.Write([]byte(
		"NOT A VALID HTTP REQUEST\r\n\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	waitForCounter(t, m.OutboundConnections, 1,
		metrics.FormatCID(enclaveCID), metrics.OutboundResultError)
}

// TestProxy_OriginFormRejected verifies a plain (non-proxy) HTTP request
// with only a path gets 400.
func TestProxy_OriginFormRejected(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const enclaveCID uint32 = 16
	const vsockPort uint32 = 8080

	m := metrics.New()
	cfgs := []config.OutboundListener{{
		Port: vsockPort,
		CIDs: []config.OutboundCID{{
			CID:          enclaveCID,
			AllowedHosts: []string{"*"},
		}},
	}}
	startServer(t, cfgs, newLoopbackListenFunc(reg, hostCID), m)

	d := vsockconn.NewLoopbackDialer(reg, enclaveCID)
	c, err := d.Dial(hostCID, vsockPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if _, err := c.Write([]byte(
		"GET /path HTTP/1.1\r\nHost: example.com\r\n\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestConnect_DialFailureReturns502 verifies that a failure to dial the
// upstream TCP endpoint surfaces as 502 Bad Gateway.
func TestConnect_DialFailureReturns502(t *testing.T) {
	// Claim a port, get its address, then close so the address is dead.
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
	cfgs := []config.OutboundListener{{
		Port: vsockPort,
		CIDs: []config.OutboundCID{{
			CID:          enclaveCID,
			AllowedHosts: []string{deadAddr},
		}},
	}}
	startServer(t, cfgs, newLoopbackListenFunc(reg, hostCID), m)

	d := vsockconn.NewLoopbackDialer(reg, enclaveCID)
	c, err := d.Dial(hostCID, vsockPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if _, err := c.Write([]byte(fmt.Sprintf(
		"CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n",
		deadAddr, deadAddr))); err != nil {
		t.Fatalf("Write: %v", err)
	}

	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	waitForCounter(t, m.OutboundConnections, 1,
		metrics.FormatCID(enclaveCID), metrics.OutboundResultError)
}

// TestServer_MultiplePorts exercises two listeners on different vsock
// ports with distinct CID sets and verifies they route independently.
func TestServer_MultiplePorts(t *testing.T) {
	echo := startEchoServer(t)

	reg := vsockconn.NewRegistry()
	const cidA uint32 = 16
	const cidB uint32 = 20
	const portA uint32 = 8080
	const portB uint32 = 8081

	m := metrics.New()
	cfgs := []config.OutboundListener{
		{
			Port: portA,
			CIDs: []config.OutboundCID{{
				CID: cidA, AllowedHosts: []string{echo.Addr().String()},
			}},
		},
		{
			Port: portB,
			CIDs: []config.OutboundCID{{
				CID: cidB, AllowedHosts: []string{echo.Addr().String()},
			}},
		},
	}
	startServer(t, cfgs, newLoopbackListenFunc(reg, hostCID), m)

	// CID A on port A must work.
	cA, err := vsockconn.NewLoopbackDialer(reg, cidA).Dial(hostCID, portA)
	if err != nil {
		t.Fatalf("Dial A: %v", err)
	}
	defer cA.Close()
	fmt.Fprintf(cA, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n",
		echo.Addr().String(), echo.Addr().String())
	brA := bufio.NewReader(cA)
	respA, err := http.ReadResponse(brA,
		&http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("ReadResponse A: %v", err)
	}
	if respA.StatusCode != http.StatusOK {
		t.Errorf("A status = %d", respA.StatusCode)
	}

	// CID A on port B must be denied (CID A is not in port B's CID set).
	cAWrong, err := vsockconn.NewLoopbackDialer(reg, cidA).Dial(hostCID, portB)
	if err != nil {
		t.Fatalf("Dial A on port B: %v", err)
	}
	defer cAWrong.Close()
	_ = cAWrong.SetReadDeadline(time.Now().Add(1 * time.Second))
	buf := make([]byte, 1)
	n, _ := cAWrong.Read(buf)
	if n != 0 {
		t.Errorf("expected close, got %d bytes", n)
	}
	waitForCounter(t, m.OutboundConnections, 1,
		metrics.CIDLabelUnauthorized, metrics.OutboundResultDenied)
}

// TestNewServerRejectsNilDependencies locks down the required arguments.
func TestNewServerRejectsNilDependencies(t *testing.T) {
	reg := vsockconn.NewRegistry()
	listenFn := newLoopbackListenFunc(reg, hostCID)

	if _, err := NewServer(nil, nil, nil, metrics.New(),
		discardLogger()); err == nil {
		t.Error("expected error on nil listen func")
	}
	if _, err := NewServer(nil, nil, listenFn, nil,
		discardLogger()); err == nil {
		t.Error("expected error on nil metrics")
	}
}

// TestNewServerRejectsDuplicateCID catches a port with the same CID twice.
// The full config validator (config.Validate) catches cross-port duplicates;
// this check guards the single-port case.
func TestNewServerRejectsDuplicateCID(t *testing.T) {
	reg := vsockconn.NewRegistry()
	_, err := NewServer(
		[]config.OutboundListener{{
			Port: 8080,
			CIDs: []config.OutboundCID{
				{CID: 16, AllowedHosts: []string{"*"}},
				{CID: 16, AllowedHosts: []string{"*"}},
			},
		}},
		nil,
		newLoopbackListenFunc(reg, hostCID),
		metrics.New(),
		discardLogger(),
	)
	if err == nil {
		t.Fatal("expected duplicate-cid error")
	}
	if !strings.Contains(err.Error(), "duplicate cid") {
		t.Errorf("error %v; want duplicate cid wording", err)
	}
}

// TestNewServerRejectsBadAllowlist catches malformed allowlist patterns
// surfacing via allowlist.New.
func TestNewServerRejectsBadAllowlist(t *testing.T) {
	reg := vsockconn.NewRegistry()
	_, err := NewServer(
		[]config.OutboundListener{{
			Port: 8080,
			CIDs: []config.OutboundCID{{
				CID:          16,
				AllowedHosts: []string{"not-a-pattern"},
			}},
		}},
		nil,
		newLoopbackListenFunc(reg, hostCID),
		metrics.New(),
		discardLogger(),
	)
	if err == nil {
		t.Fatal("expected allowlist error")
	}
}

// TestServerShutdownGraceful verifies an idle server shuts down cleanly.
func TestServerShutdownGraceful(t *testing.T) {
	reg := vsockconn.NewRegistry()
	m := metrics.New()
	cfgs := []config.OutboundListener{{
		Port: 8080,
		CIDs: []config.OutboundCID{{
			CID: 16, AllowedHosts: []string{"*"},
		}},
	}}
	s, err := NewServer(cfgs, nil, newLoopbackListenFunc(reg, hostCID),
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
}

// TestStartBindConflict verifies Start returns an error when the vsock
// port is already in use.
func TestStartBindConflict(t *testing.T) {
	reg := vsockconn.NewRegistry()
	const vsockPort uint32 = 8080

	// Hold the port first so the server cannot bind it.
	held, err := vsockconn.ListenLoopback(reg, hostCID, vsockPort)
	if err != nil {
		t.Fatalf("pre-listen: %v", err)
	}
	defer held.Close()

	s, err := NewServer(
		[]config.OutboundListener{{
			Port: vsockPort,
			CIDs: []config.OutboundCID{{
				CID: 16, AllowedHosts: []string{"*"},
			}},
		}},
		nil,
		newLoopbackListenFunc(reg, hostCID),
		metrics.New(),
		discardLogger(),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("expected bind conflict")
	}
}

// TestStripHopByHop covers the header-stripping helper directly. The
// Connection token list must also drive header removal.
func TestStripHopByHop(t *testing.T) {
	h := http.Header{}
	h.Set("Proxy-Connection", "keep-alive")
	h.Set("Keep-Alive", "timeout=5")
	h.Set("Upgrade", "websocket")
	h.Set("X-Custom", "keep-me")
	h.Set("Connection", "close, X-Drop-Me")
	h.Set("X-Drop-Me", "should-disappear")

	stripHopByHop(h)

	for _, name := range []string{
		"Proxy-Connection", "Keep-Alive", "Upgrade",
		"Connection", "X-Drop-Me",
	} {
		if got := h.Get(name); got != "" {
			t.Errorf("header %q not stripped, got %q", name, got)
		}
	}
	if got := h.Get("X-Custom"); got != "keep-me" {
		t.Errorf("X-Custom = %q, want keep-me", got)
	}
}

// hangingListener mimics the shutdown behaviour of mdlayher/vsock v1.2.1:
// after Close it returns an error whose string matches net.ErrClosed but
// whose value does not (errors.Is returns false). Used to reproduce the
// outbound-shutdown spin-loop bug without a real AF_VSOCK socket.
type hangingListener struct {
	accept chan error
	closed atomic.Bool
	addr   net.Addr
}

func newHangingListener() *hangingListener {
	return &hangingListener{
		accept: make(chan error, 1),
		addr:   &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0},
	}
}

func (h *hangingListener) Accept() (vsockconn.Conn, error) {
	err, ok := <-h.accept
	if !ok {
		return nil, errors.New("use of closed network connection")
	}
	return nil, err
}

func (h *hangingListener) Close() error {
	if h.closed.CompareAndSwap(false, true) {
		// mdlayher/vsock v1.2.1 opError wraps the close-side error as a
		// freshly constructed errors.New with this exact message, so the
		// string matches net.ErrClosed but errors.Is does not.
		h.accept <- errors.New("use of closed network connection")
		close(h.accept)
	}
	return nil
}

func (h *hangingListener) Addr() net.Addr { return h.addr }

// TestServerShutdownHandlesNonErrClosedAcceptError reproduces the bug
// where Shutdown spins forever because the outbound accept loop does not
// recognise mdlayher/vsock's rewrapped close error as net.ErrClosed.
// Pre-fix, Shutdown hangs until its ctx deadline and the test times out.
// Post-fix, Shutdown returns nil well inside the 500 ms budget because
// the per-listener done channel signals the accept loop to exit.
func TestServerShutdownHandlesNonErrClosedAcceptError(t *testing.T) {
	hanging := newHangingListener()
	listenFn := func(port uint32) (vsockconn.Listener, error) {
		return hanging, nil
	}
	cfgs := []config.OutboundListener{{
		Port: 8080,
		CIDs: []config.OutboundCID{{
			CID: 16, AllowedHosts: []string{"*"},
		}},
	}}
	s, err := NewServer(cfgs, nil, listenFn, metrics.New(), discardLogger())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(
		context.Background(), 500*time.Millisecond)
	defer cancel()
	// Run Shutdown off the test goroutine: pre-fix, Shutdown blocks
	// indefinitely on its internal <-done because the accept loop never
	// exits. Without this goroutine the whole test would hang until the
	// package test timeout rather than fail with a useful message.
	errCh := make(chan error, 1)
	start := time.Now()
	go func() { errCh <- s.Shutdown(shutdownCtx) }()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Shutdown: %v (elapsed %s)", err, time.Since(start))
		}
		if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
			t.Fatalf("Shutdown took %s; want well under 500ms", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Shutdown did not return within 2s; accept loop spinning on non-ErrClosed error")
	}
}
