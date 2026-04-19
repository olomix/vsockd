// Package e2e runs an end-to-end scenario against vsockd with the loopback
// vsock backend. It covers the full inbound and outbound proxy paths plus a
// config reload in a single process so regressions that only show up when
// the wiring is complete are caught here.
package e2e

import (
	"bufio"
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

// httpEnclaveHandler consumes an HTTP request from c and writes a fixed
// 200 response. Used as the default enclave behaviour for inbound tests.
func httpEnclaveHandler(body string) func(net.Conn) {
	return func(c net.Conn) {
		defer c.Close()
		br := bufio.NewReader(c)
		_, _ = http.ReadRequest(br)
		resp := fmt.Sprintf(
			"HTTP/1.1 200 OK\r\nContent-Length: %d\r\n"+
				"Connection: close\r\n\r\n%s",
			len(body), body)
		_, _ = c.Write([]byte(resp))
	}
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
