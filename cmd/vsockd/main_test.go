package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestMain lets the test binary re-exec itself as the real vsockd entry
// point when VSOCKD_TEST_SUBPROCESS=1. Classic Go idiom for CLI smoke
// tests: avoids needing a separate `go build` step in CI.
func TestMain(m *testing.M) {
	if os.Getenv("VSOCKD_TEST_SUBPROCESS") == "1" {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func runSubprocess(t *testing.T, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "VSOCKD_TEST_SUBPROCESS=1")
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exitCode = 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("unexpected exec error: %v", err)
		}
	}
	return outBuf.String(), errBuf.String(), exitCode
}

func TestHelpExitsZero(t *testing.T) {
	_, _, code := runSubprocess(t, "-help")
	if code != 0 {
		t.Fatalf("-help exit code = %d, want 0", code)
	}
}

func TestVersionPrintsAndExitsZero(t *testing.T) {
	stdout, _, code := runSubprocess(t, "-version")
	if code != 0 {
		t.Fatalf("-version exit code = %d, want 0", code)
	}
	if stdout == "" {
		t.Fatalf("-version produced no stdout")
	}
}

func TestUnknownFlagExitsNonZero(t *testing.T) {
	_, _, code := runSubprocess(t, "-totally-unknown-flag")
	if code == 0 {
		t.Fatalf("unknown flag exit code = 0, want non-zero")
	}
}

// allocPort returns an ephemeral 127.0.0.1 port and frees it, so the
// caller can pass it to a subprocess that will bind it.
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

// writeConfigFile writes body to a vsockd.yaml in a scratch temp dir and
// returns its path. The directory is cleaned up with the test.
func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "vsockd.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// runningProcess wraps a live vsockd subprocess plus the buffers capturing
// its output. Tests start one, interact with it over OS signals, and Wait
// for clean exit.
type runningProcess struct {
	cmd    *exec.Cmd
	stderr *syncBuffer

	// exited is closed exactly once, when cmd.Wait returns. Receiving
	// from it is therefore safe to do multiple times — tests and the
	// cleanup function both need that property.
	exited chan struct{}
	mu     sync.Mutex
	err    error
}

func (r *runningProcess) waitErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// syncBuffer is a bytes.Buffer that serializes writes so goroutine-driven
// reads from the subprocess's stderr do not race with test assertions.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func startVsockd(t *testing.T, cfgPath, metricsAddr string) *runningProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0],
		"-config", cfgPath,
		"-metrics-addr", metricsAddr,
		"-log-format", "json",
	)
	cmd.Env = append(os.Environ(),
		"VSOCKD_TEST_SUBPROCESS=1",
		"VSOCKD_BACKEND=loopback",
	)
	stderr := &syncBuffer{}
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start vsockd: %v", err)
	}
	rp := &runningProcess{
		cmd:    cmd,
		stderr: stderr,
		exited: make(chan struct{}),
	}
	go func() {
		err := cmd.Wait()
		rp.mu.Lock()
		rp.err = err
		rp.mu.Unlock()
		close(rp.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-rp.exited:
			return
		default:
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-rp.exited:
			return
		case <-time.After(5 * time.Second):
		}
		_ = cmd.Process.Kill()
		<-rp.exited
	})
	return rp
}

// waitForListen repeatedly dials addr until a connection succeeds or the
// timeout elapses. Used to synchronize on the subprocess binding its TCP
// listeners before the test makes assertions.
func waitForListen(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(30 * time.Millisecond)
	}
	return fmt.Errorf("no listener on %s after %s", addr, timeout)
}

// waitForLog polls the stderr buffer for substring s within timeout.
func waitForLog(b *syncBuffer, s string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(b.String(), s) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// TestMetricsEndpointSmokeTest starts the real binary with a minimal
// config and asserts /metrics returns 200.
func TestMetricsEndpointSmokeTest(t *testing.T) {
	inPort := allocPort(t)
	metricsPort := allocPort(t)
	cfg := fmt.Sprintf(`
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
	cfgPath := writeConfigFile(t, cfg)
	metricsAddr := fmt.Sprintf("127.0.0.1:%d", metricsPort)

	rp := startVsockd(t, cfgPath, metricsAddr)

	if err := waitForListen(metricsAddr, 3*time.Second); err != nil {
		t.Fatalf("metrics listener: %v\nstderr: %s", err, rp.stderr.String())
	}

	resp, err := http.Get("http://" + metricsAddr + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	// config_reloads_total has no series yet, but the metric names we
	// expect as part of the /metrics exposition ship with the binary via
	// the internal/metrics registrations; scrape body is non-empty.
	if len(body) == 0 {
		t.Fatal("empty /metrics body")
	}
}

// TestSIGHUPReloadsConfig starts the binary, rewrites the config to
// replace the inbound listener's port, sends SIGHUP, and verifies that
// the old port is no longer accepting connections while the new one is.
func TestSIGHUPReloadsConfig(t *testing.T) {
	oldPort := allocPort(t)
	newPort := allocPort(t)
	metricsPort := allocPort(t)

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
	cfgPath := writeConfigFile(t, initial)
	metricsAddr := fmt.Sprintf("127.0.0.1:%d", metricsPort)

	rp := startVsockd(t, cfgPath, metricsAddr)

	oldAddr := fmt.Sprintf("127.0.0.1:%d", oldPort)
	if err := waitForListen(oldAddr, 3*time.Second); err != nil {
		t.Fatalf("initial listener never bound: %v\nstderr: %s",
			err, rp.stderr.String())
	}

	updated := fmt.Sprintf(`
inbound:
  - bind: 127.0.0.1
    port: %d
    mode: http-host
    routes:
      - hostname: api.example.com
        cid: 16
        vsock_port: 8080
shutdown_grace: 2s
`, newPort)
	if err := os.WriteFile(cfgPath, []byte(updated), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	if err := rp.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("SIGHUP: %v", err)
	}

	if !waitForLog(rp.stderr, "config reloaded", 3*time.Second) {
		t.Fatalf(`"config reloaded" log not seen; stderr:\n%s`,
			rp.stderr.String())
	}

	newAddr := fmt.Sprintf("127.0.0.1:%d", newPort)
	if err := waitForListen(newAddr, 2*time.Second); err != nil {
		t.Fatalf("new listener never bound: %v", err)
	}

	// The old port should be closed; a dial must fail.
	if c, err := net.DialTimeout("tcp", oldAddr, 200*time.Millisecond); err == nil {
		_ = c.Close()
		t.Fatalf("old listener still accepting after SIGHUP")
	}
}

// TestSIGTERMShutsDownWithinGrace sends SIGTERM and asserts the process
// exits cleanly within the configured grace window.
func TestSIGTERMShutsDownWithinGrace(t *testing.T) {
	inPort := allocPort(t)
	metricsPort := allocPort(t)
	cfg := fmt.Sprintf(`
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
	cfgPath := writeConfigFile(t, cfg)
	metricsAddr := fmt.Sprintf("127.0.0.1:%d", metricsPort)

	rp := startVsockd(t, cfgPath, metricsAddr)

	addr := fmt.Sprintf("127.0.0.1:%d", inPort)
	if err := waitForListen(addr, 3*time.Second); err != nil {
		t.Fatalf("listener never bound: %v\nstderr: %s",
			err, rp.stderr.String())
	}

	start := time.Now()
	if err := rp.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case <-rp.exited:
		elapsed := time.Since(start)
		if err := rp.waitErr(); err != nil {
			t.Fatalf("process exited with error: %v\nstderr: %s",
				err, rp.stderr.String())
		}
		// Should return well within 2x the shutdown grace even with jitter.
		if elapsed > 3*time.Second {
			t.Errorf("shutdown took %s, want <3s", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("process did not exit after SIGTERM within 5s\nstderr: %s",
			rp.stderr.String())
	}
}
