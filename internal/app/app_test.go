package app_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olomix/vsockd/internal/app"
	"github.com/olomix/vsockd/internal/config"
	"github.com/olomix/vsockd/internal/metrics"
	"github.com/olomix/vsockd/internal/outbound"
	"github.com/olomix/vsockd/internal/vsockconn"

	dto "github.com/prometheus/client_model/go"
)

// discardLogger silences app + server logs during tests.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
		Level: slog.LevelError,
	}))
}

// allocPort binds :0 on 127.0.0.1, reads back the ephemeral port, then
// frees it so the caller can write it into a config file. Small TOCTOU
// window but adequate for single-process tests.
func allocPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("alloc port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// writeConfig renders a config YAML to path and returns the path.
func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "vsockd.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// loopbackBackend returns an in-process vsock backend backed by a single
// registry so the inbound dialer and outbound listener can talk to each
// other inside one process.
func loopbackBackend(sourceCID uint32) (
	*vsockconn.Registry, vsockconn.Dialer, outbound.ListenFunc,
) {
	reg := vsockconn.NewRegistry()
	dial := vsockconn.NewLoopbackDialer(reg, sourceCID)
	listen := func(port uint32) (vsockconn.Listener, error) {
		return vsockconn.ListenLoopback(reg, 0, port)
	}
	return reg, dial, listen
}

// fakeEnclave listens on the loopback registry at (cid, port), accepts one
// connection, and echoes whatever it receives as an HTTP 200 response. It
// shuts down when the returned Listener is closed. The echoBody channel is
// closed after the first request is seen so the test can synchronize.
func fakeEnclave(
	t *testing.T,
	reg *vsockconn.Registry,
	cid, port uint32,
) (vsockconn.Listener, <-chan []byte) {
	t.Helper()
	ln, err := vsockconn.ListenLoopback(reg, cid, port)
	if err != nil {
		t.Fatalf("ListenLoopback: %v", err)
	}
	seen := make(chan []byte, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c vsockconn.Conn) {
				defer c.Close()
				buf := make([]byte, 2048)
				n, _ := c.Read(buf)
				if n > 0 {
					b := make([]byte, n)
					copy(b, buf[:n])
					select {
					case seen <- b:
					default:
					}
				}
				resp := "HTTP/1.1 200 OK\r\n" +
					"Content-Length: 2\r\nConnection: close\r\n\r\nok"
				_, _ = c.Write([]byte(resp))
			}(c)
		}
	}()
	return ln, seen
}

// counterValue returns the current value for the labelled counter series.
// It does not fail if the series has not been observed yet — it returns 0
// in that case, which matches Prometheus's "not yet incremented" semantics.
func counterValue(
	t *testing.T,
	m *metrics.Metrics,
	name string,
	labels map[string]string,
) float64 {
	t.Helper()
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, metric := range f.GetMetric() {
			if labelSubset(metric.GetLabel(), labels) {
				if metric.Counter != nil {
					return metric.Counter.GetValue()
				}
			}
		}
	}
	return 0
}

func labelSubset(got []*dto.LabelPair, want map[string]string) bool {
	for k, v := range want {
		found := false
		for _, l := range got {
			if l.GetName() == k && l.GetValue() == v {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// startApp is the canonical setup: write cfgBody to disk, load it, build
// an app with a loopback backend, Start, and arrange Shutdown on cleanup.
func startApp(t *testing.T, cfgBody, metricsAddr string) (
	*app.App, *vsockconn.Registry, string,
) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := writeConfig(t, dir, cfgBody)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	reg, dialer, listenFn := loopbackBackend(2)
	a, err := app.New(app.Options{
		ConfigPath:    cfgPath,
		Config:        cfg,
		Logger:        discardLogger(),
		MetricsAddr:   metricsAddr,
		VsockDialer:   dialer,
		VsockListenFn: listenFn,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		sctx, scancel := context.WithTimeout(
			context.Background(), 2*time.Second)
		defer scancel()
		_ = a.Shutdown(sctx)
	})
	if err := a.Start(ctx); err != nil {
		t.Fatalf("app.Start: %v", err)
	}
	return a, reg, cfgPath
}

// TestMetricsEndpoint starts the app with a minimal config and asserts the
// /metrics endpoint returns 200 with the expected metric families present.
//
// Prometheus CounterVec families are only exposed once at least one series
// per vector has been observed, so the test primes each counter with a
// representative label set before scraping.
func TestMetricsEndpoint(t *testing.T) {
	inPort := allocPort(t)
	metricsPort := allocPort(t)
	body := fmt.Sprintf(`
inbound:
  - bind: 127.0.0.1
    port: %d
    mode: http-host
    routes:
      - hostname: api.example.com
        cid: 16
        vsock_port: 8080
shutdown_grace: 2s
`, inPort)
	metricsAddr := fmt.Sprintf("127.0.0.1:%d", metricsPort)
	a, _, _ := startApp(t, body, metricsAddr)

	m := a.Metrics()
	m.InboundConnections.WithLabelValues("api.example.com")
	m.InboundBytes.WithLabelValues("api.example.com", metrics.DirectionIn)
	m.InboundErrors.WithLabelValues("api.example.com", metrics.InboundErrorSniff)
	m.OutboundConnections.WithLabelValues("16", metrics.OutboundResultAllowed)
	m.OutboundBytes.WithLabelValues("16", metrics.DirectionOut)
	m.ConfigReloads.WithLabelValues(metrics.ReloadResultSuccess)

	url := fmt.Sprintf("http://%s/metrics", metricsAddr)
	deadline := time.Now().Add(2 * time.Second)
	var resp *http.Response
	var err error
	for time.Now().Before(deadline) {
		resp, err = http.Get(url)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	text := string(b)
	wantNames := []string{
		"inbound_connections_total",
		"inbound_bytes_total",
		"inbound_errors_total",
		"outbound_connections_total",
		"outbound_bytes_total",
		"config_reloads_total",
	}
	for _, n := range wantNames {
		if !strings.Contains(text, n) {
			t.Errorf("metrics output missing %q", n)
		}
	}
}

// TestReloadAddAndRemoveListener drives a SIGHUP-equivalent Reload: the
// new config adds an inbound listener and removes the original one. We
// verify the diff landed by inspecting ListenerKeys and by opening a TCP
// connection to the new listener.
func TestReloadAddAndRemoveListener(t *testing.T) {
	oldPort := allocPort(t)
	newPort := allocPort(t)

	initial := fmt.Sprintf(`
inbound:
  - bind: 127.0.0.1
    port: %d
    mode: http-host
    routes:
      - hostname: api.example.com
        cid: 16
        vsock_port: 8080
shutdown_grace: 2s
`, oldPort)
	a, _, cfgPath := startApp(t, initial, "")

	initialKeys := a.Inbound().ListenerKeys()
	if len(initialKeys) != 1 {
		t.Fatalf("initial ListenerKeys = %v, want 1 listener", initialKeys)
	}

	updated := fmt.Sprintf(`
inbound:
  - bind: 127.0.0.1
    port: %d
    mode: http-host
    routes:
      - hostname: other.example.com
        cid: 20
        vsock_port: 8081
shutdown_grace: 2s
`, newPort)
	if err := os.WriteFile(cfgPath, []byte(updated), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := a.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	keys := a.Inbound().ListenerKeys()
	if len(keys) != 1 {
		t.Fatalf("after reload ListenerKeys = %v, want 1 listener", keys)
	}
	if !strings.Contains(keys[0], fmt.Sprint(newPort)) {
		t.Fatalf("expected new port %d in %q", newPort, keys[0])
	}

	// Old port should no longer accept connections.
	oldAddr := fmt.Sprintf("127.0.0.1:%d", oldPort)
	if c, err := net.DialTimeout("tcp", oldAddr, 200*time.Millisecond); err == nil {
		_ = c.Close()
		t.Fatalf("old listener still accepting on %s", oldAddr)
	}

	// New port should accept.
	newAddr := fmt.Sprintf("127.0.0.1:%d", newPort)
	c, err := net.DialTimeout("tcp", newAddr, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("dial new listener %s: %v", newAddr, err)
	}
	_ = c.Close()

	// Successful reload counter advanced.
	if v := counterValue(t, a.Metrics(), "config_reloads_total",
		map[string]string{"result": "success"}); v != 1 {
		t.Errorf("config_reloads_total{success} = %v, want 1", v)
	}
}

// TestReloadInvalidConfigKeepsRunning verifies that a bad config on
// reload leaves the running state unchanged and bumps the failure counter.
func TestReloadInvalidConfigKeepsRunning(t *testing.T) {
	inPort := allocPort(t)
	initial := fmt.Sprintf(`
inbound:
  - bind: 127.0.0.1
    port: %d
    mode: http-host
    routes:
      - hostname: api.example.com
        cid: 16
        vsock_port: 8080
shutdown_grace: 2s
`, inPort)
	a, _, cfgPath := startApp(t, initial, "")

	if err := os.WriteFile(cfgPath, []byte("not: [valid yaml"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := a.Reload(); err == nil {
		t.Fatal("Reload with bad config returned nil, want error")
	}

	// Original listener still present and reachable.
	addr := fmt.Sprintf("127.0.0.1:%d", inPort)
	c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("original listener unreachable after failed reload: %v", err)
	}
	_ = c.Close()

	if v := counterValue(t, a.Metrics(), "config_reloads_total",
		map[string]string{"result": "failure"}); v != 1 {
		t.Errorf("config_reloads_total{failure} = %v, want 1", v)
	}
}

// TestReloadRoutesSwapAtomically verifies the kept-listener path: when an
// inbound listener's (bind, port, mode) stays the same but its routes
// change, the running TCP listener is preserved and the new routes take
// effect for subsequent connections.
func TestReloadRoutesSwapAtomically(t *testing.T) {
	inPort := allocPort(t)
	initial := fmt.Sprintf(`
inbound:
  - bind: 127.0.0.1
    port: %d
    mode: http-host
    routes:
      - hostname: old.example.com
        cid: 16
        vsock_port: 8080
shutdown_grace: 2s
`, inPort)
	a, reg, cfgPath := startApp(t, initial, "")

	_, _ = fakeEnclave(t, reg, 16, 8080)
	_, seenNew := fakeEnclave(t, reg, 20, 8081)

	updated := fmt.Sprintf(`
inbound:
  - bind: 127.0.0.1
    port: %d
    mode: http-host
    routes:
      - hostname: new.example.com
        cid: 20
        vsock_port: 8081
shutdown_grace: 2s
`, inPort)
	if err := os.WriteFile(cfgPath, []byte(updated), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := a.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	// Listener at the same port should still be bound (kept path).
	keys := a.Inbound().ListenerKeys()
	if len(keys) != 1 || !strings.Contains(keys[0], fmt.Sprint(inPort)) {
		t.Fatalf("kept listener missing: %v", keys)
	}

	// A connection using the NEW route should succeed.
	addr := fmt.Sprintf("127.0.0.1:%d", inPort)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	req := "GET / HTTP/1.1\r\n" +
		"Host: new.example.com\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	select {
	case got := <-seenNew:
		if !strings.Contains(string(got), "Host: new.example.com") {
			t.Errorf("fake enclave received unexpected request: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake enclave never received the post-reload request")
	}
}

// TestShutdownGraceful verifies that Shutdown returns within the grace
// window when there is nothing pending.
func TestShutdownGraceful(t *testing.T) {
	inPort := allocPort(t)
	body := fmt.Sprintf(`
inbound:
  - bind: 127.0.0.1
    port: %d
    mode: http-host
    routes:
      - hostname: api.example.com
        cid: 16
        vsock_port: 8080
shutdown_grace: 1s
`, inPort)
	dir := t.TempDir()
	cfgPath := writeConfig(t, dir, body)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, dialer, listenFn := loopbackBackend(2)
	a, err := app.New(app.Options{
		ConfigPath:    cfgPath,
		Config:        cfg,
		Logger:        discardLogger(),
		VsockDialer:   dialer,
		VsockListenFn: listenFn,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- a.Shutdown(context.Background())
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Shutdown returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return within grace window")
	}

	// After shutdown, the port should no longer accept connections.
	addr := fmt.Sprintf("127.0.0.1:%d", inPort)
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		_ = c.Close()
		t.Errorf("listener still accepting after Shutdown")
	}
}

// TestReloadExistingConnSurvives opens an inbound connection that holds
// an in-flight upstream copy, triggers a reload that removes the original
// listener, then completes the HTTP exchange. The already-established
// connection must not be torn down by the reload.
func TestReloadExistingConnSurvives(t *testing.T) {
	inPort := allocPort(t)

	initial := fmt.Sprintf(`
inbound:
  - bind: 127.0.0.1
    port: %d
    mode: http-host
    routes:
      - hostname: api.example.com
        cid: 16
        vsock_port: 8080
shutdown_grace: 2s
`, inPort)
	a, reg, cfgPath := startApp(t, initial, "")

	// Stage an enclave that waits for the complete HTTP request before
	// responding. The test drives timing so the reload happens while the
	// bidirectional copy is active.
	upstreamReady := make(chan net.Conn, 1)
	ln, err := vsockconn.ListenLoopback(reg, 16, 8080)
	if err != nil {
		t.Fatalf("ListenLoopback: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		upstreamReady <- c
	}()

	addr := fmt.Sprintf("127.0.0.1:%d", inPort)
	client, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	req := "GET / HTTP/1.1\r\n" +
		"Host: api.example.com\r\n\r\n"
	if _, err := client.Write([]byte(req)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var upstream net.Conn
	select {
	case upstream = <-upstreamReady:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the connection")
	}
	defer upstream.Close()

	// Reload the config to remove the original listener. The in-flight
	// connection must not be affected.
	newPort := allocPort(t)
	updated := fmt.Sprintf(`
inbound:
  - bind: 127.0.0.1
    port: %d
    mode: http-host
    routes:
      - hostname: different.example.com
        cid: 17
        vsock_port: 9999
shutdown_grace: 2s
`, newPort)
	if err := os.WriteFile(cfgPath, []byte(updated), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := a.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	// Enclave writes a response; the original client should still read it.
	resp := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
	if _, err := upstream.Write([]byte(resp)); err != nil {
		t.Fatalf("upstream Write: %v", err)
	}
	upstream.Close()

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1024)
	n, err := client.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("client Read: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "200 OK") {
		t.Errorf("client did not receive response; got %q", buf[:n])
	}

	// New listener is bound on the new port.
	newAddr := fmt.Sprintf("127.0.0.1:%d", newPort)
	c, err := net.DialTimeout("tcp", newAddr, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("new listener not reachable: %v", err)
	}
	_ = c.Close()
}
