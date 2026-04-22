// Package e2e runs an end-to-end scenario against vsockd with the loopback
// vsock backend. It covers the full inbound and outbound proxy paths plus a
// config reload in a single process so regressions that only show up when
// the wiring is complete are caught here.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olomix/vsockd/internal/app"
	"github.com/olomix/vsockd/internal/config"
	"github.com/olomix/vsockd/internal/outbound"
	"github.com/olomix/vsockd/internal/vsockconn"
)

const (
	// enclaveCID simulates a Nitro enclave's context ID. Chosen above the
	// reserved range (0..2) so it's a plausible real CID.
	enclaveCID uint32 = 16
	// hostCID is the source CID the inbound proxy identifies as when
	// dialing enclaves. Immaterial to the enclave in these tests.
	hostCID uint32 = 3
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
		Level: slog.LevelError,
	}))
}

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

// fakeEnclave serves HTTP over a loopback-vsock listener at (cid, port).
// Each accepted connection receives the handler's response. The listener
// lives until the test ends.
func fakeEnclave(
	t *testing.T,
	reg *vsockconn.Registry,
	cid, port uint32,
	handler func(conn net.Conn),
) {
	t.Helper()
	ln, err := vsockconn.ListenLoopback(reg, cid, port)
	if err != nil {
		t.Fatalf("fake enclave listen (cid=%d port=%d): %v", cid, port, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handler(c)
		}
	}()
}

// echoTCPServer spawns a 127.0.0.1 TCP listener that echoes every read back
// to its peer. It stands in for an external destination that the outbound
// proxy is authorized to reach. Returns the bound address and a cleanup.
func echoTCPServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
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
	return ln.Addr().String()
}

// writeConfig renders body to path.
func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// TestEndToEnd exercises the inbound path, the outbound CONNECT path (both
// allowed and denied), and a SIGHUP-style reload that introduces a new
// inbound route. Everything runs in a single process against the loopback
// vsock backend.
func TestEndToEnd(t *testing.T) {
	// Addresses the test pins ahead of time.
	inPort := allocPort(t)
	outboundVsockPort := uint32(9000)
	enclaveInboundVsockPort := uint32(8080)

	// Upstream target for the outbound CONNECT allow-path.
	echoAddr := echoTCPServer(t)

	reg := vsockconn.NewRegistry()

	// Inbound proxy dialer: the host dialing enclaves.
	inboundDialer := vsockconn.NewLoopbackDialer(reg, hostCID)
	// Outbound proxy listener factory: exposes outbound ports at cid=0.
	outboundListen := outbound.ListenFunc(
		func(port uint32) (vsockconn.Listener, error) {
			return vsockconn.ListenLoopback(reg, 0, port)
		})

	// Fake enclave: inbound HTTP target.
	enclaveSaw := make(chan string, 4)
	fakeEnclave(t, reg, enclaveCID, enclaveInboundVsockPort, func(c net.Conn) {
		defer c.Close()
		br := bufio.NewReader(c)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		select {
		case enclaveSaw <- req.Host:
		default:
		}
		body := "enclave-response"
		resp := fmt.Sprintf(
			"HTTP/1.1 200 OK\r\nContent-Length: %d\r\n"+
				"Connection: close\r\n\r\n%s",
			len(body), body)
		_, _ = c.Write([]byte(resp))
	})

	// Generate initial config: one inbound listener for api.example.com,
	// one outbound listener allowing the enclave to reach the echo server.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "vsockd.yaml")
	initial := fmt.Sprintf(`
inbound:
  - bind: 127.0.0.1
    port: %d
    mode: http-host
    routes:
      - hostname: api.example.com
        cid: %d
        vsock_port: %d
outbound:
  - port: %d
    cids:
      - cid: %d
        allowed_hosts:
          - "%s"
shutdown_grace: 2s
`, inPort, enclaveCID, enclaveInboundVsockPort,
		outboundVsockPort, enclaveCID, echoAddr)
	writeConfig(t, cfgPath, initial)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	a, err := app.New(app.Options{
		ConfigPath:    cfgPath,
		Config:        cfg,
		Logger:        discardLogger(),
		VsockDialer:   inboundDialer,
		VsockListenFn: outboundListen,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		sctx, scancel := context.WithTimeout(
			context.Background(), 3*time.Second)
		defer scancel()
		_ = a.Shutdown(sctx)
	})
	if err := a.Start(ctx); err != nil {
		t.Fatalf("app.Start: %v", err)
	}

	t.Run("inbound_http_host_routing", func(t *testing.T) {
		addr := fmt.Sprintf("127.0.0.1:%d", inPort)
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial inbound: %v", err)
		}
		defer c.Close()

		req := "GET /hello HTTP/1.1\r\n" +
			"Host: api.example.com\r\n" +
			"Connection: close\r\n\r\n"
		if _, err := c.Write([]byte(req)); err != nil {
			t.Fatalf("client write: %v", err)
		}

		select {
		case host := <-enclaveSaw:
			if host != "api.example.com" {
				t.Errorf("enclave saw Host=%q, want api.example.com", host)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("enclave never saw the inbound request")
		}

		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		if !strings.Contains(string(body), "enclave-response") {
			t.Errorf("body = %q, want enclave-response", body)
		}
	})

	// Outbound tests: the enclave dials the outbound proxy via a loopback
	// dialer whose sourceCID equals the enclave's CID. That's what the
	// real vsock kernel would report on Accept.
	enclaveDialer := vsockconn.NewLoopbackDialer(reg, enclaveCID)

	t.Run("outbound_connect_allowed", func(t *testing.T) {
		c, err := enclaveDialer.Dial(0, outboundVsockPort)
		if err != nil {
			t.Fatalf("enclave dial outbound: %v", err)
		}
		defer c.Close()

		connect := fmt.Sprintf(
			"CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
		if _, err := c.Write([]byte(connect)); err != nil {
			t.Fatalf("CONNECT write: %v", err)
		}

		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		br := bufio.NewReader(c)
		status, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read status: %v", err)
		}
		if !strings.Contains(status, "200") {
			t.Fatalf("CONNECT status = %q, want 200", strings.TrimSpace(status))
		}
		// Consume the rest of the 200 response headers (terminating blank
		// line).
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("read headers: %v", err)
			}
			if strings.TrimSpace(line) == "" {
				break
			}
		}

		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		payload := []byte("ping-pong")
		if _, err := c.Write(payload); err != nil {
			t.Fatalf("tunnel write: %v", err)
		}
		buf := make([]byte, len(payload))
		if _, err := io.ReadFull(br, buf); err != nil {
			t.Fatalf("tunnel read: %v", err)
		}
		if string(buf) != string(payload) {
			t.Errorf("echoed = %q, want %q", buf, payload)
		}
	})

	t.Run("outbound_connect_denied", func(t *testing.T) {
		c, err := enclaveDialer.Dial(0, outboundVsockPort)
		if err != nil {
			t.Fatalf("enclave dial outbound: %v", err)
		}
		defer c.Close()

		connect := "CONNECT blocked.example.com:443 HTTP/1.1\r\n" +
			"Host: blocked.example.com:443\r\n\r\n"
		if _, err := c.Write([]byte(connect)); err != nil {
			t.Fatalf("CONNECT write: %v", err)
		}

		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("deny status = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("reload_adds_inbound_route", func(t *testing.T) {
		// Stage a second fake enclave for the route the reload introduces.
		const newEnclaveCID uint32 = 20
		const newEnclaveVsockPort uint32 = 8081
		newPort := allocPort(t)

		newSaw := make(chan string, 1)
		fakeEnclave(t, reg, newEnclaveCID, newEnclaveVsockPort, func(c net.Conn) {
			defer c.Close()
			br := bufio.NewReader(c)
			req, err := http.ReadRequest(br)
			if err != nil {
				return
			}
			select {
			case newSaw <- req.Host:
			default:
			}
			body := "new-route-ok"
			resp := fmt.Sprintf(
				"HTTP/1.1 200 OK\r\nContent-Length: %d\r\n"+
					"Connection: close\r\n\r\n%s",
				len(body), body)
			_, _ = c.Write([]byte(resp))
		})

		updated := fmt.Sprintf(`
inbound:
  - bind: 127.0.0.1
    port: %d
    mode: http-host
    routes:
      - hostname: api.example.com
        cid: %d
        vsock_port: %d
  - bind: 127.0.0.1
    port: %d
    mode: http-host
    routes:
      - hostname: new.example.com
        cid: %d
        vsock_port: %d
outbound:
  - port: %d
    cids:
      - cid: %d
        allowed_hosts:
          - "%s"
shutdown_grace: 2s
`, inPort, enclaveCID, enclaveInboundVsockPort,
			newPort, newEnclaveCID, newEnclaveVsockPort,
			outboundVsockPort, enclaveCID, echoAddr)
		writeConfig(t, cfgPath, updated)

		if err := a.Reload(); err != nil {
			t.Fatalf("Reload: %v", err)
		}

		keys := a.Inbound().ListenerKeys()
		if len(keys) != 2 {
			t.Fatalf("ListenerKeys after reload = %v, want 2", keys)
		}

		addr := fmt.Sprintf("127.0.0.1:%d", newPort)
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial new listener: %v", err)
		}
		defer c.Close()

		req := "GET /check HTTP/1.1\r\n" +
			"Host: new.example.com\r\n" +
			"Connection: close\r\n\r\n"
		if _, err := c.Write([]byte(req)); err != nil {
			t.Fatalf("client write: %v", err)
		}

		select {
		case host := <-newSaw:
			if host != "new.example.com" {
				t.Errorf("new enclave saw Host=%q, want new.example.com", host)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("new enclave never saw the post-reload request")
		}

		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		if !strings.Contains(string(body), "new-route-ok") {
			t.Errorf("body = %q, want new-route-ok", body)
		}
	})
}

// logRecord mirrors the unexported helper in the per-package tests: a
// decoded JSON record captured through a slog JSON handler so e2e
// assertions can key off stable attribute shapes instead of slog's
// internal Value types.
type logRecord map[string]any

// captureHandler funnels every slog record through an internal JSON
// handler and appends it to a shared buffer, so e2e assertions can scan
// the full log tape produced by app.New's logger.
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

func countMessage(recs []logRecord, target string) int {
	n := 0
	for _, r := range recs {
		if m, _ := r["msg"].(string); m == target {
			n++
		}
	}
	return n
}

func findMessage(recs []logRecord, target string) logRecord {
	for _, r := range recs {
		if m, _ := r["msg"].(string); m == target {
			return r
		}
	}
	return nil
}

// startPrefixEcho runs a newline-framed TCP echo on 127.0.0.1. Each read
// line is echoed back prefixed with prefix+":" and terminated with "\n".
// The prefix lets a reload sub-case tell which upstream (pre or post
// reload) ultimately answered a given connection.
func startPrefixEcho(t *testing.T, prefix string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("prefix echo listen: %v", err)
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
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					_, err := c.Write([]byte(
						prefix + ":" + sc.Text() + "\n"))
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

// fakeVsockEcho registers a loopback vsock listener at (cid, port) whose
// accepted connections byte-echo everything they read. Stands in for an
// enclave TCP service sitting at the far end of the vsock link in the
// inbound mode: tcp path.
func fakeVsockEcho(
	t *testing.T,
	reg *vsockconn.Registry,
	cid, port uint32,
) {
	t.Helper()
	ln, err := vsockconn.ListenLoopback(reg, cid, port)
	if err != nil {
		t.Fatalf("fake vsock echo listen (cid=%d port=%d): %v",
			cid, port, err)
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
}

// waitForCount polls capH until countMessage for each named message hits
// its expected value or the deadline expires. Returns the snapshot
// records so the caller can assert on attributes without re-reading.
func waitForCount(
	t *testing.T,
	capH *captureHandler,
	expect map[string]int,
	timeout time.Duration,
) []logRecord {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var recs []logRecord
	for {
		recs = capH.records(t)
		done := true
		for msg, want := range expect {
			if countMessage(recs, msg) < want {
				done = false
				break
			}
		}
		if done {
			return recs
		}
		if time.Now().After(deadline) {
			for msg, want := range expect {
				got := countMessage(recs, msg)
				if got < want {
					t.Errorf(
						"log %q count = %d, want >= %d", msg, got, want)
				}
			}
			return recs
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestEndToEnd_TCPPassthrough drives both mode=tcp directions through
// app.New/app.Start end-to-end:
//
//   - inbound TCP listener on 127.0.0.1 forwarding to a loopback-vsock
//     echo (the fake enclave)
//   - outbound vsock listener forwarding to a real 127.0.0.1 TCP echo
//
// It then reloads the config with a new outbound upstream and verifies
// new vsock connections land on the new echo while an already-dialed
// connection keeps draining through the pre-reload upstream. The shared
// debug logger is captured so the per-connection open/close pairs and
// their total_bytes attrs can be asserted.
func TestEndToEnd_TCPPassthrough(t *testing.T) {
	const (
		inVsockPort  uint32 = 8090
		outVsockPort uint32 = 9100
	)
	inTCPPort := allocPort(t)

	echoA := startPrefixEcho(t, "A")

	reg := vsockconn.NewRegistry()
	inboundDialer := vsockconn.NewLoopbackDialer(reg, hostCID)
	outboundListen := outbound.ListenFunc(
		func(port uint32) (vsockconn.Listener, error) {
			return vsockconn.ListenLoopback(reg, 0, port)
		})

	fakeVsockEcho(t, reg, enclaveCID, inVsockPort)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "vsockd.yaml")
	initial := fmt.Sprintf(`
log_level: debug
tcp_to_vsock:
  - bind: 127.0.0.1
    port: %d
    vsock_cid: %d
    vsock_port: %d
vsock_to_tcp:
  - port: %d
    upstream: "%s"
shutdown_grace: 2s
`, inTCPPort, enclaveCID, inVsockPort, outVsockPort, echoA)
	writeConfig(t, cfgPath, initial)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	capH := newCaptureHandler()
	logger := slog.New(capH)

	a, err := app.New(app.Options{
		ConfigPath:    cfgPath,
		Config:        cfg,
		Logger:        logger,
		VsockDialer:   inboundDialer,
		VsockListenFn: outboundListen,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		sctx, scancel := context.WithTimeout(
			context.Background(), 3*time.Second)
		defer scancel()
		_ = a.Shutdown(sctx)
	})
	if err := a.Start(ctx); err != nil {
		t.Fatalf("app.Start: %v", err)
	}

	// Counts of connections opened per direction — the final assertion
	// block uses these to verify one open/close log pair exists per
	// accepted connection.
	var (
		inboundConns  int
		outboundConns int
	)

	t.Run("inbound_tcp_passthrough", func(t *testing.T) {
		addr := fmt.Sprintf("127.0.0.1:%d", inTCPPort)
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial inbound tcp: %v", err)
		}
		defer c.Close()

		payload := []byte("hello-inbound-tcp")
		if _, err := c.Write(payload); err != nil {
			t.Fatalf("write: %v", err)
		}
		// Half-close the write side so the vsock echo's io.Copy returns
		// EOF and the inbound shuttle unblocks its reverse direction.
		if err := c.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}

		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		got, err := io.ReadAll(c)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("echo = %q, want %q", got, payload)
		}
		inboundConns++
	})

	t.Run("outbound_tcp_passthrough_pre_reload", func(t *testing.T) {
		enclaveDialer := vsockconn.NewLoopbackDialer(reg, enclaveCID)
		c, err := enclaveDialer.Dial(0, outVsockPort)
		if err != nil {
			t.Fatalf("enclave dial outbound: %v", err)
		}
		defer c.Close()

		// Line-framed round-trip: write "alpha\n", expect "A:alpha\n".
		if _, err := c.Write([]byte("alpha\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		br := bufio.NewReader(c)
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if line != "A:alpha\n" {
			t.Fatalf("pre-reload echo = %q, want %q", line, "A:alpha\n")
		}
		// Closing triggers the close log we'll assert on later.
		_ = c.Close()
		outboundConns++
	})

	t.Run("reload_changes_tcp_upstream", func(t *testing.T) {
		echoB := startPrefixEcho(t, "B")
		updated := fmt.Sprintf(`
log_level: debug
tcp_to_vsock:
  - bind: 127.0.0.1
    port: %d
    vsock_cid: %d
    vsock_port: %d
vsock_to_tcp:
  - port: %d
    upstream: "%s"
shutdown_grace: 2s
`, inTCPPort, enclaveCID, inVsockPort, outVsockPort, echoB)
		writeConfig(t, cfgPath, updated)

		// Open an outbound connection BEFORE the reload so its upstream
		// dial resolves to echoA, then verify it keeps draining through
		// echoA after the reload fires. This is the "existing ones
		// drain" assertion from the plan.
		enclaveDialer := vsockconn.NewLoopbackDialer(reg, enclaveCID)
		existing, err := enclaveDialer.Dial(0, outVsockPort)
		if err != nil {
			t.Fatalf("pre-reload dial: %v", err)
		}
		defer existing.Close()
		if _, err := existing.Write([]byte("drain1\n")); err != nil {
			t.Fatalf("drain1 write: %v", err)
		}
		_ = existing.SetReadDeadline(time.Now().Add(2 * time.Second))
		existBR := bufio.NewReader(existing)
		line, err := existBR.ReadString('\n')
		if err != nil {
			t.Fatalf("drain1 read: %v", err)
		}
		if line != "A:drain1\n" {
			t.Fatalf(
				"pre-reload existing echo = %q, want %q",
				line, "A:drain1\n")
		}

		if err := a.Reload(); err != nil {
			t.Fatalf("Reload: %v", err)
		}

		// Existing conn: another round-trip must still go through echoA
		// because its upstream TCP dial happened before the swap.
		if _, err := existing.Write([]byte("drain2\n")); err != nil {
			t.Fatalf("drain2 write: %v", err)
		}
		_ = existing.SetReadDeadline(time.Now().Add(2 * time.Second))
		line, err = existBR.ReadString('\n')
		if err != nil {
			t.Fatalf("drain2 read: %v", err)
		}
		if line != "A:drain2\n" {
			t.Fatalf(
				"post-reload existing echo = %q, want %q (should drain through old upstream)",
				line, "A:drain2\n")
		}
		_ = existing.Close()
		outboundConns++

		// New conn: must resolve to echoB because upstreamSnapshot at
		// the new accept reads the swapped atomic.
		fresh, err := enclaveDialer.Dial(0, outVsockPort)
		if err != nil {
			t.Fatalf("post-reload dial: %v", err)
		}
		defer fresh.Close()
		if _, err := fresh.Write([]byte("switched\n")); err != nil {
			t.Fatalf("switched write: %v", err)
		}
		_ = fresh.SetReadDeadline(time.Now().Add(2 * time.Second))
		freshBR := bufio.NewReader(fresh)
		line, err = freshBR.ReadString('\n')
		if err != nil {
			t.Fatalf("switched read: %v", err)
		}
		if line != "B:switched\n" {
			t.Fatalf(
				"post-reload new echo = %q, want %q (upstream not swapped)",
				line, "B:switched\n")
		}
		_ = fresh.Close()
		outboundConns++
	})

	// Poll until every opened connection has produced its expected
	// open/close log pair, then assert exact counts. Close logs are
	// emitted asynchronously from the handler goroutine so a short
	// settle window is needed.
	recs := waitForCount(t, capH, map[string]int{
		"tcp_to_vsock connection opened": inboundConns,
		"tcp_to_vsock connection closed": inboundConns,
		"vsock_to_tcp connection opened": outboundConns,
		"vsock_to_tcp connection closed": outboundConns,
	}, 3*time.Second)

	if got := countMessage(recs, "tcp_to_vsock connection opened"); got != inboundConns {
		t.Errorf("tcp_to_vsock connection opened count = %d, want %d",
			got, inboundConns)
	}
	if got := countMessage(recs, "tcp_to_vsock connection closed"); got != inboundConns {
		t.Errorf("tcp_to_vsock connection closed count = %d, want %d",
			got, inboundConns)
	}
	if got := countMessage(recs, "vsock_to_tcp connection opened"); got != outboundConns {
		t.Errorf("vsock_to_tcp connection opened count = %d, want %d",
			got, outboundConns)
	}
	if got := countMessage(recs, "vsock_to_tcp connection closed"); got != outboundConns {
		t.Errorf("vsock_to_tcp connection closed count = %d, want %d",
			got, outboundConns)
	}

	// Byte totals sanity: every close log carries total_bytes >= 0, and
	// at least one of each direction recorded a positive total (the
	// round-trip actually transferred bytes).
	assertPositiveTotal := func(msg string) {
		t.Helper()
		var anyPositive bool
		for _, r := range recs {
			if m, _ := r["msg"].(string); m != msg {
				continue
			}
			v, ok := r["total_bytes"].(float64)
			if !ok {
				t.Errorf("%q missing total_bytes: %v", msg, r)
				continue
			}
			if v < 0 {
				t.Errorf("%q total_bytes negative: %v", msg, v)
			}
			if v > 0 {
				anyPositive = true
			}
		}
		if !anyPositive {
			t.Errorf("no %q record had total_bytes > 0", msg)
		}
	}
	assertPositiveTotal("tcp_to_vsock connection closed")
	assertPositiveTotal("vsock_to_tcp connection closed")

	// Spot-check a representative close log for the vsock side carries
	// a plausible CID (enclaveCID from the dialer).
	if rec := findMessage(recs, "vsock_to_tcp connection closed"); rec != nil {
		if got := rec["cid"]; got != float64(enclaveCID) {
			t.Errorf("vsock close cid = %v, want %v", got, enclaveCID)
		}
		if got := rec["listen_port"]; got != float64(outVsockPort) {
			t.Errorf("vsock close listen_port = %v, want %v",
				got, outVsockPort)
		}
	}
}
