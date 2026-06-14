package supervisor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/olomix/vsockd/internal/supervisor"
)

// TestHelperProcess is re-exec'd as a supervised child by the manager tests.
// It is inert during a normal `go test` run (the env guard returns immediately)
// and only acts when the manager spawns it with GO_WANT_HELPER_PROCESS=1 and a
// mini-command after the "--" separator. See helperProc for the command grammar.
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
	case "sleep-emit": // sleep-emit <stdout|-> <ms> <code>
		if args[1] != "-" {
			fmt.Fprintln(os.Stdout, args[1])
		}
		time.Sleep(time.Duration(atoiHelper(args[2])) * time.Millisecond)
		os.Exit(atoiHelper(args[3])) //nolint:gocritic // exits the helper
	case "serve": // serve: announce readiness then block until a signal kills us
		fmt.Fprintln(os.Stdout, "ready")
		time.Sleep(time.Hour) // a real timer keeps the deadlock detector quiet
		os.Exit(0)
	}
	os.Exit(0)
}

// helperArgs returns the program arguments after the "--" separator.
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
// mini-command. role/restart/on_failure must be set explicitly since the
// manager consumes already-validated configs (it does not apply defaults).
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

// collector is a concurrency-safe sink that records every framed NDJSON line.
type collector struct {
	mu     sync.Mutex
	frames [][]byte
}

func (c *collector) sink(b []byte) {
	c.mu.Lock()
	c.frames = append(c.frames, append([]byte(nil), b...))
	c.mu.Unlock()
}

func (c *collector) records(t *testing.T) []map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]map[string]any, 0, len(c.frames))
	for _, f := range c.frames {
		var m map[string]any
		if err := json.Unmarshal(f, &m); err != nil {
			t.Fatalf("unmarshal %q: %v", f, err)
		}
		out = append(out, m)
	}
	return out
}

// count returns how many records match the given type and (optional) src.
func count(recs []map[string]any, typ, src string) int {
	n := 0
	for _, r := range recs {
		if r["type"] != typ {
			continue
		}
		if src != "" && r["src"] != src {
			continue
		}
		n++
	}
	return n
}

// runManager builds a manager over procs and runs it to completion (or until
// the returned cancel is invoked), returning the resolved exit code.
func newManager(c *collector, procs ...supervisor.Process) *supervisor.Manager {
	framer := supervisor.NewFramer(
		func() time.Time { return time.Unix(0, 0).UTC() }, nil)
	return supervisor.NewManager(supervisor.ManagerConfig{
		Processes: procs,
		Framer:    framer,
		Sink:      c.sink,
		TermGrace: 2 * time.Second,
		ExtraEnv:  []string{"GO_WANT_HELPER_PROCESS=1"},
	})
}

func TestManagerCapturesOutputAndLifecycle(t *testing.T) {
	c := &collector{}
	m := newManager(c, helperProc(
		"app", supervisor.RoleTask, supervisor.RestartNo,
		supervisor.OnFailureTerminate, 0, 0,
		"emit", "hello-out", "hello-err", "0"))

	if code := m.Run(t.Context()); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	recs := c.records(t)
	if n := count(recs, supervisor.TypeStart, "app"); n != 1 {
		t.Fatalf("start records = %d, want 1", n)
	}
	if n := count(recs, supervisor.TypeExit, "app"); n != 1 {
		t.Fatalf("exit records = %d, want 1", n)
	}

	var sawOut, sawErr bool
	var startPID, logPID float64
	for _, r := range recs {
		switch {
		case r["type"] == supervisor.TypeStart:
			startPID = r["pid"].(float64)
		case r["type"] == supervisor.TypeLog && r["src"] == "app":
			logPID = r["pid"].(float64)
			if r["stream"] == supervisor.StreamStdout && r["msg"] == "hello-out" {
				sawOut = true
			}
			if r["stream"] == supervisor.StreamStderr && r["msg"] == "hello-err" {
				sawErr = true
			}
		case r["type"] == supervisor.TypeExit:
			if r["code"].(float64) != 0 {
				t.Fatalf("exit code field = %v, want 0", r["code"])
			}
		}
	}
	if !sawOut {
		t.Fatalf("missing stdout log record; got %v", recs)
	}
	if !sawErr {
		t.Fatalf("missing stderr log record; got %v", recs)
	}
	if startPID == 0 || startPID != logPID {
		t.Fatalf("pid mismatch: start=%v log=%v", startPID, logPID)
	}
}

func TestManagerOnFailureRestartsThenGivesUp(t *testing.T) {
	c := &collector{}
	// restart=on-failure, max_restarts=2: a child that always exits non-zero is
	// spawned 1 + 2 times, then the budget is exhausted and terminate fires.
	m := newManager(c, helperProc(
		"app", supervisor.RoleTask, supervisor.RestartOnFailure,
		supervisor.OnFailureTerminate, 2, time.Minute,
		"emit", "-", "-", "3"))

	if code := m.Run(t.Context()); code != 3 {
		t.Fatalf("exit code = %d, want 3 (propagated child code)", code)
	}
	if n := count(c.records(t), supervisor.TypeStart, "app"); n != 3 {
		t.Fatalf("start records = %d, want 3 (initial + 2 restarts)", n)
	}
}

func TestManagerOnFailureDoesNotRestartZeroExit(t *testing.T) {
	c := &collector{}
	m := newManager(c, helperProc(
		"app", supervisor.RoleTask, supervisor.RestartOnFailure,
		supervisor.OnFailureTerminate, 3, time.Minute,
		"emit", "-", "-", "0"))

	if code := m.Run(t.Context()); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if n := count(c.records(t), supervisor.TypeStart, "app"); n != 1 {
		t.Fatalf("start records = %d, want 1 (exit 0 not restarted)", n)
	}
}

func TestManagerAlwaysRestartsZeroExit(t *testing.T) {
	c := &collector{}
	// restart=always restarts even a clean exit; the budget bounds the loop.
	m := newManager(c, helperProc(
		"app", supervisor.RoleTask, supervisor.RestartAlways,
		supervisor.OnFailureTerminate, 2, time.Minute,
		"emit", "-", "-", "0"))

	m.Run(t.Context())
	if n := count(c.records(t), supervisor.TypeStart, "app"); n != 3 {
		t.Fatalf("start records = %d, want 3 (always restarts exit 0)", n)
	}
}

func TestManagerRestartNoNeverRestarts(t *testing.T) {
	c := &collector{}
	m := newManager(c, helperProc(
		"app", supervisor.RoleTask, supervisor.RestartNo,
		supervisor.OnFailureTerminate, 0, 0,
		"emit", "-", "-", "5"))

	if code := m.Run(t.Context()); code != 5 {
		t.Fatalf("exit code = %d, want 5", code)
	}
	if n := count(c.records(t), supervisor.TypeStart, "app"); n != 1 {
		t.Fatalf("start records = %d, want 1 (restart=no)", n)
	}
}

func TestManagerOnFailureContinueAbandonsButOthersRun(t *testing.T) {
	c := &collector{}
	// task1 fails and is abandoned (continue); task2 must still run to
	// completion, proving its failure did not tear the whole supervisor down.
	m := newManager(c,
		helperProc("fail", supervisor.RoleTask, supervisor.RestartNo,
			supervisor.OnFailureContinue, 0, 0,
			"emit", "-", "-", "7"),
		helperProc("ok", supervisor.RoleTask, supervisor.RestartNo,
			supervisor.OnFailureTerminate, 0, 0,
			"sleep-emit", "done2", "100", "0"),
	)

	if code := m.Run(t.Context()); code != 7 {
		t.Fatalf("exit code = %d, want 7 (abandoned task is non-zero)", code)
	}

	recs := c.records(t)
	var sawDone2 bool
	for _, r := range recs {
		if r["src"] == "ok" && r["type"] == supervisor.TypeLog &&
			r["msg"] == "done2" {
			sawDone2 = true
		}
	}
	if !sawDone2 {
		t.Fatalf("task 'ok' did not run to completion; records: %v", recs)
	}
	if n := count(recs, supervisor.TypeExit, "ok"); n != 1 {
		t.Fatalf("task 'ok' exit records = %d, want 1", n)
	}
}

func TestManagerOnFailureTerminateShutsDownSidecar(t *testing.T) {
	c := &collector{}
	m := newManager(c,
		helperProc("app", supervisor.RoleTask, supervisor.RestartNo,
			supervisor.OnFailureTerminate, 0, 0,
			"emit", "-", "-", "9"),
		helperProc("sidecar", supervisor.RoleSidecar, supervisor.RestartAlways,
			supervisor.OnFailureTerminate, 5, time.Minute, "serve"),
	)

	if code := m.Run(t.Context()); code != 9 {
		t.Fatalf("exit code = %d, want 9", code)
	}
	// The sidecar was SIGTERM'd as part of the shutdown and not restarted.
	recs := c.records(t)
	if n := count(recs, supervisor.TypeStart, "sidecar"); n != 1 {
		t.Fatalf("sidecar start records = %d, want 1 (no restart at shutdown)", n)
	}
	if n := count(recs, supervisor.TypeExit, "sidecar"); n != 1 {
		t.Fatalf("sidecar exit records = %d, want 1", n)
	}
}

func TestManagerTaskCompletionShutsDownSidecar(t *testing.T) {
	c := &collector{}
	m := newManager(c,
		helperProc("app", supervisor.RoleTask, supervisor.RestartNo,
			supervisor.OnFailureTerminate, 0, 0,
			"emit", "-", "-", "0"),
		helperProc("sidecar", supervisor.RoleSidecar, supervisor.RestartAlways,
			supervisor.OnFailureTerminate, 5, time.Minute, "serve"),
	)

	if code := m.Run(t.Context()); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	recs := c.records(t)
	if n := count(recs, supervisor.TypeStart, "sidecar"); n != 1 {
		t.Fatalf("sidecar start records = %d, want 1 (suspended at shutdown)", n)
	}
	if n := count(recs, supervisor.TypeExit, "sidecar"); n != 1 {
		t.Fatalf("sidecar exit records = %d, want 1", n)
	}
}

func TestManagerSidecarZeroExitTolerated(t *testing.T) {
	c := &collector{}
	// A sidecar that exits 0 under on-failure is tolerated (no restart, no
	// shutdown); the task then completes and brings the supervisor down.
	m := newManager(c,
		helperProc("sidecar", supervisor.RoleSidecar, supervisor.RestartOnFailure,
			supervisor.OnFailureTerminate, 3, time.Minute,
			"emit", "-", "-", "0"),
		helperProc("app", supervisor.RoleTask, supervisor.RestartNo,
			supervisor.OnFailureTerminate, 0, 0,
			"sleep-emit", "appdone", "50", "0"),
	)

	if code := m.Run(t.Context()); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if n := count(c.records(t), supervisor.TypeStart, "sidecar"); n != 1 {
		t.Fatalf("sidecar start records = %d, want 1 (exit 0 tolerated)", n)
	}
}

func TestManagerStartFailureGivesUp(t *testing.T) {
	c := &collector{}
	// A command that cannot be exec'd is reported as a synthetic non-zero exit
	// (127), so the restart/on_failure policy applies just as for a real exit.
	bad := supervisor.Process{
		Name:      "missing",
		Command:   "/nonexistent/definitely-not-a-real-binary",
		Role:      supervisor.RoleTask,
		Restart:   supervisor.RestartNo,
		OnFailure: supervisor.OnFailureTerminate,
	}
	framer := supervisor.NewFramer(
		func() time.Time { return time.Unix(0, 0).UTC() }, nil)
	m := supervisor.NewManager(supervisor.ManagerConfig{
		Processes: []supervisor.Process{bad},
		Framer:    framer,
		Sink:      c.sink,
		TermGrace: time.Second,
	})

	if code := m.Run(t.Context()); code != 127 {
		t.Fatalf("exit code = %d, want 127 (start failure)", code)
	}
	// No process ran, so neither a start nor an exit record is emitted.
	if n := count(c.records(t), supervisor.TypeStart, "missing"); n != 0 {
		t.Fatalf("start records = %d, want 0 (process never started)", n)
	}
}

func TestManagerSignalCancelTriggersShutdown(t *testing.T) {
	c := &collector{}
	// Daemon mode: a lone sidecar runs until the context is cancelled, which
	// drives a graceful shutdown that SIGTERMs the child and records its exit.
	m := newManager(c, helperProc(
		"sidecar", supervisor.RoleSidecar, supervisor.RestartAlways,
		supervisor.OnFailureTerminate, 5, time.Minute, "serve"))

	ctx, cancel := context.WithCancel(t.Context())
	codeCh := make(chan int, 1)
	go func() { codeCh <- m.Run(ctx) }()

	if !waitForRecord(c, supervisor.TypeStart, "sidecar") {
		t.Fatal("sidecar never started")
	}
	cancel()

	select {
	case code := <-codeCh:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (clean shutdown)", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("manager did not shut down after cancel")
	}
	if n := count(c.records(t), supervisor.TypeExit, "sidecar"); n != 1 {
		t.Fatalf("sidecar exit records = %d, want 1", n)
	}
}

// waitForRecord polls the collector until a record of the given type/src
// appears or a short deadline passes.
func waitForRecord(c *collector, typ, src string) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		frames := c.frames
		c.mu.Unlock()
		for _, f := range frames {
			var m map[string]any
			if json.Unmarshal(f, &m) == nil &&
				m["type"] == typ && m["src"] == src {
				return true
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}
