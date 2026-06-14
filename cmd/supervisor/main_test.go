package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/olomix/vsockd/internal/supervisor"
	"github.com/olomix/vsockd/internal/vsockconn"
)

// TestMain lets the test binary re-exec itself as the real supervisor entry
// point when SUPERVISOR_TEST_SUBPROCESS=1 (used by the CLI smoke tests). The
// GO_WANT_HELPER_PROCESS path is handled separately by TestHelperProcess, which
// the supervisor spawns as a supervised child.
func TestMain(m *testing.M) {
	if os.Getenv("SUPERVISOR_TEST_SUBPROCESS") == "1" {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// TestHelperProcess is re-exec'd as a supervised child by the end-to-end tests.
// It is inert during a normal run (the env guard returns immediately) and only
// acts when spawned with GO_WANT_HELPER_PROCESS=1 and a mini-command after "--".
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := helperArgs()
	if len(args) == 0 {
		os.Exit(0)
	}
	switch args[0] {
	case "emit": // emit <stdout|-> <stderr|-> <code>
		if args[1] != "-" {
			fmt.Fprintln(os.Stdout, args[1])
		}
		if args[2] != "-" {
			fmt.Fprintln(os.Stderr, args[2])
		}
		os.Exit(atoiHelper(args[3]))
	case "serve": // announce readiness then block until a signal kills us
		fmt.Fprintln(os.Stdout, "ready")
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	os.Exit(0)
}

func helperArgs() []string {
	for i, a := range os.Args {
		if a == "--" {
			return os.Args[i+1:]
		}
	}
	return nil
}

func atoiHelper(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// helperProc builds a Process that re-execs TestHelperProcess with the given
// mini-command. role/restart/on_failure are set explicitly since supervise
// consumes an already-validated config.
func helperProc(
	name, role, restart, onFailure string,
	maxRestarts int, window time.Duration, cmd ...string,
) supervisor.Process {
	args := append([]string{"-test.run=^TestHelperProcess$", "--"}, cmd...)
	return supervisor.Process{
		Name:          name,
		Command:       os.Args[0],
		Args:          args,
		Role:          role,
		Restart:       restart,
		MaxRestarts:   maxRestarts,
		RestartWindow: supervisor.Duration(window),
		OnFailure:     onFailure,
	}
}

const (
	testLogCID    uint32 = 3
	testLogPort   uint32 = 5140
	testSourceCID uint32 = 4
)

// fixedNow is a deterministic clock so record timestamps are stable in tests.
func fixedNow() time.Time { return time.Unix(0, 0).UTC() }

// drainConn reads every newline-framed NDJSON record from c until EOF, decoding
// each into a map. A read deadline keeps a stalled test from hanging the suite.
func drainConn(t *testing.T, c net.Conn) []map[string]any {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(8 * time.Second))
	r := bufio.NewReader(c)
	var out []map[string]any
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var m map[string]any
			if json.Unmarshal(line, &m) == nil {
				out = append(out, m)
			}
		}
		if err != nil {
			return out
		}
	}
}

// extraEnv is the environment the supervised helper children need to act.
var extraEnv = []string{"GO_WANT_HELPER_PROCESS=1"}

func TestSuperviseStreamsFullNDJSON(t *testing.T) {
	reg := vsockconn.NewRegistry()
	ln, err := vsockconn.ListenLoopback(reg, testLogCID, testLogPort)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	recvCh := make(chan []map[string]any, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			recvCh <- nil
			return
		}
		defer conn.Close()
		recvCh <- drainConn(t, conn)
	}()

	cfg := &supervisor.Config{
		LogCID:  testLogCID,
		LogPort: testLogPort,
		Tags:    map[string]string{"service": "test"},
		Buffer:  &supervisor.Buffer{MaxRecords: 1000},
		Processes: []supervisor.Process{
			helperProc("app", supervisor.RoleTask, supervisor.RestartNo,
				supervisor.OnFailureTerminate, 0, 0,
				"emit", "hello-out", "hello-err", "0"),
			helperProc("vsockd", supervisor.RoleSidecar, supervisor.RestartAlways,
				supervisor.OnFailureTerminate, 5, time.Minute, "serve"),
		},
	}

	code := supervise(t.Context(), superviseOptions{
		cfg:      cfg,
		dialer:   vsockconn.NewLoopbackDialer(reg, testSourceCID),
		now:      fixedNow,
		stderr:   io.Discard,
		extraEnv: extraEnv,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (all tasks done)", code)
	}

	var recs []map[string]any
	select {
	case recs = <-recvCh:
	case <-time.After(8 * time.Second):
		t.Fatal("no frames received from loopback listener")
	}

	// The task's stdout and stderr lines must arrive as log records tagged with
	// the right src/stream, with the configured source-side tags and no host
	// enrichment (cid/host are added on the host, not by the supervisor).
	var sawOut, sawErr, sawStart, sawExit, sawSupervisor bool
	for _, r := range recs {
		if tags, ok := r["tags"].(map[string]any); ok {
			if tags["service"] != "test" {
				t.Fatalf("record missing source tag: %v", r)
			}
		}
		if _, ok := r["cid"]; ok {
			t.Fatalf("supervisor must not set cid: %v", r)
		}
		if _, ok := r["host"]; ok {
			t.Fatalf("supervisor must not set host: %v", r)
		}
		switch {
		case r["type"] == supervisor.TypeStart && r["src"] == "app":
			sawStart = true
		case r["type"] == supervisor.TypeExit && r["src"] == "app":
			sawExit = true
			if r["code"].(float64) != 0 {
				t.Fatalf("app exit code = %v, want 0", r["code"])
			}
		case r["type"] == supervisor.TypeLog && r["src"] == "app":
			if r["stream"] == supervisor.StreamStdout && r["msg"] == "hello-out" {
				sawOut = true
			}
			if r["stream"] == supervisor.StreamStderr && r["msg"] == "hello-err" {
				sawErr = true
			}
		case r["type"] == supervisor.TypeLog && r["src"] == supervisor.SrcSupervisor:
			sawSupervisor = true
		}
	}
	if !sawStart || !sawExit {
		t.Fatalf("missing app lifecycle records; got %v", recs)
	}
	if !sawOut || !sawErr {
		t.Fatalf("missing app stdout/stderr log records; got %v", recs)
	}
	if !sawSupervisor {
		t.Fatalf("supervisor's own logs were not shipped; got %v", recs)
	}
}

func TestSuperviseTerminateGiveUpExitsNonZero(t *testing.T) {
	reg := vsockconn.NewRegistry()
	// No listener is registered: the buffer absorbs frames and the short flush
	// grace bounds the best-effort drain on shutdown.
	cfg := &supervisor.Config{
		LogCID:  testLogCID,
		LogPort: testLogPort,
		Buffer:  &supervisor.Buffer{MaxRecords: 1000},
		Processes: []supervisor.Process{
			helperProc("app", supervisor.RoleTask, supervisor.RestartNo,
				supervisor.OnFailureTerminate, 0, 0,
				"emit", "-", "-", "7"),
		},
	}

	code := supervise(t.Context(), superviseOptions{
		cfg:        cfg,
		dialer:     vsockconn.NewLoopbackDialer(reg, testSourceCID),
		now:        time.Now,
		stderr:     io.Discard,
		flushGrace: 100 * time.Millisecond,
		extraEnv:   extraEnv,
	})
	if code != 7 {
		t.Fatalf("exit code = %d, want 7 (terminate give-up propagates code)", code)
	}
}

func TestSuperviseShutsDownOnContextCancel(t *testing.T) {
	reg := vsockconn.NewRegistry()
	ln, err := vsockconn.ListenLoopback(reg, testLogCID, testLogPort)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	started := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		var once sync.Once
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				var m map[string]any
				if json.Unmarshal(line, &m) == nil &&
					m["type"] == supervisor.TypeStart {
					once.Do(func() { close(started) })
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Daemon mode: a lone sidecar runs until the context is cancelled (the
	// equivalent of the supervisor receiving SIGTERM), which drives a graceful
	// shutdown returning exit 0.
	cfg := &supervisor.Config{
		LogCID:  testLogCID,
		LogPort: testLogPort,
		Buffer:  &supervisor.Buffer{MaxRecords: 1000},
		Processes: []supervisor.Process{
			helperProc("sidecar", supervisor.RoleSidecar, supervisor.RestartAlways,
				supervisor.OnFailureTerminate, 5, time.Minute, "serve"),
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- supervise(ctx, superviseOptions{
			cfg:      cfg,
			dialer:   vsockconn.NewLoopbackDialer(reg, testSourceCID),
			now:      time.Now,
			stderr:   io.Discard,
			extraEnv: extraEnv,
		})
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("sidecar never started")
	}
	cancel()

	select {
	case code := <-codeCh:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (clean shutdown)", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor did not shut down after context cancel")
	}
}

// --- CLI smoke tests (re-exec the real entry point) ---

func runSubprocess(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "SUPERVISOR_TEST_SUBPROCESS=1")
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code = 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("unexpected exec error: %v", err)
		}
	}
	return outBuf.String(), errBuf.String(), code
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

func TestHelpExitsZero(t *testing.T) {
	_, _, code := runSubprocess(t, "-help")
	if code != 0 {
		t.Fatalf("-help exit code = %d, want 0", code)
	}
}

func TestMissingConfigExitsOne(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	_, stderr, code := runSubprocess(t, "-config", missing)
	if code != 1 {
		t.Fatalf("missing config exit code = %d, want 1", code)
	}
	if stderr == "" {
		t.Fatalf("expected an error on stderr for a missing config")
	}
}
