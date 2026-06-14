package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// defaultTermGrace is the per-child wait between SIGTERM and SIGKILL during
// shutdown: a child gets this long to exit on its own before being killed.
const defaultTermGrace = 5 * time.Second

// signalExitBase is the shell convention for "killed by signal N": the exit
// code is reported as 128+N so a signalled child still yields a numeric code.
const signalExitBase = 128

// startFailureCode is the synthetic exit code for a process that could not be
// started at all (exec failure), mirroring the shell's "command not found".
const startFailureCode = 127

// procState is a unit's lifecycle state in the manager's bookkeeping.
type procState int

const (
	stateRunning   procState = iota // a live process (or one about to restart)
	stateDone                       // a task that exited 0 (success, terminal)
	stateAbandoned                  // a task given up via on_failure=continue
	stateStopped                    // sidecar stopped/tolerated, or any shutdown exit
)

// restartBudget implements windowed crash-loop counting (decision 4a). Each
// permitted restart is timestamped; attempts older than window are pruned, so a
// process that crashes occasionally but then runs healthily past the window
// gets a fresh budget, while a genuine hot loop exhausts it.
type restartBudget struct {
	max     int
	window  time.Duration
	history []time.Time
}

// allow prunes attempts older than the window and reports whether another
// restart is within budget, recording now as an attempt when it returns true.
func (b *restartBudget) allow(now time.Time) bool {
	if b.window > 0 {
		cutoff := now.Add(-b.window)
		kept := b.history[:0]
		for _, t := range b.history {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		b.history = kept
	}
	if len(b.history) >= b.max {
		return false
	}
	b.history = append(b.history, now)
	return true
}

// unit is a supervised process plus its restart bookkeeping.
type unit struct {
	cfg       Process
	cmd       *exec.Cmd
	pid       int
	state     procState
	budget    restartBudget
	killTimer *time.Timer
}

// exitEvent reports a child exit to the manager's run loop. real distinguishes
// a genuine process exit (emit start/exit records) from a start failure.
type exitEvent struct {
	u      *unit
	pid    int
	code   int
	signal string
	real   bool
}

// ManagerConfig parameterises a Manager. Framer and Sink are required; the
// rest default (Now→time.Now, Logger→discard, TermGrace→defaultTermGrace).
// ExtraEnv is appended to each child's environment (used by tests to flag the
// re-exec'd helper); nil leaves children with the supervisor's environment.
type ManagerConfig struct {
	Processes []Process
	Framer    *Framer
	Sink      func([]byte)
	Logger    *slog.Logger
	Now       func() time.Time
	TermGrace time.Duration
	ExtraEnv  []string
}

// Manager spawns and supervises the configured processes, capturing their
// stdout/stderr into the ring buffer and enforcing the role/restart/on_failure
// policy (decision 4). Its run loop is single-threaded: every state mutation
// happens while handling an exit event or a shutdown trigger, so the unit
// bookkeeping needs no locking.
type Manager struct {
	units     []*unit
	framer    *Framer
	sink      func([]byte)
	logger    *slog.Logger
	now       func() time.Time
	termGrace time.Duration
	extraEnv  []string

	exits        chan exitEvent
	shuttingDown bool
	exitCode     int
	hasTasks     bool
}

// NewManager builds a Manager from cfg, applying defaults for the optional
// fields.
func NewManager(cfg ManagerConfig) *Manager {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	grace := cfg.TermGrace
	if grace <= 0 {
		grace = defaultTermGrace
	}
	sink := cfg.Sink
	if sink == nil {
		sink = func([]byte) {}
	}
	m := &Manager{
		framer:    cfg.Framer,
		sink:      sink,
		logger:    logger,
		now:       now,
		termGrace: grace,
		extraEnv:  cfg.ExtraEnv,
		exits:     make(chan exitEvent, len(cfg.Processes)+1),
	}
	for _, p := range cfg.Processes {
		m.units = append(m.units, &unit{
			cfg: p,
			budget: restartBudget{
				max:    p.MaxRestarts,
				window: p.RestartWindow.Duration(),
			},
		})
		if p.Role == RoleTask {
			m.hasTasks = true
		}
	}
	return m
}

// Run spawns every process and supervises them until all tasks reach a terminal
// state (then sidecars are shut down), an on_failure=terminate give-up occurs,
// or ctx is cancelled (an external shutdown signal). It returns the resolved
// supervisor exit code: 0 when every task succeeded and shutdown was clean,
// otherwise the propagated failing child's code (128+signal for a signalled
// child). Run blocks for the supervisor's lifetime and must be called once.
func (m *Manager) Run(ctx context.Context) int {
	for _, u := range m.units {
		m.spawn(u)
	}

	// Once consumed, the cancel channel is nilled so the always-ready closed
	// channel does not busy-spin the select while children drain.
	shutdownCh := ctx.Done()
	for {
		if !m.shuttingDown && m.hasTasks && m.allTasksTerminal() {
			m.beginShutdown("all tasks complete")
		}
		if m.shuttingDown && m.runningCount() == 0 {
			return m.exitCode
		}
		select {
		case <-shutdownCh:
			shutdownCh = nil
			if !m.shuttingDown {
				m.logger.Info("shutdown signal received")
				m.beginShutdown("signal")
			}
		case ev := <-m.exits:
			m.handleExit(ev)
		}
	}
}

// spawn starts u's process, wires its pipes into the ring buffer, and launches
// the reap goroutine that reports the eventual exit. A start failure is fed
// back through the same exit path as a synthetic non-zero exit.
func (m *Manager) spawn(u *unit) {
	u.state = stateRunning
	cmd := exec.Command(u.cfg.Command, u.cfg.Args...)
	if m.extraEnv != nil {
		cmd.Env = append(os.Environ(), m.extraEnv...)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.startFailed(u, err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		m.startFailed(u, err)
		return
	}
	if err := cmd.Start(); err != nil {
		m.startFailed(u, err)
		return
	}
	u.cmd = cmd
	u.pid = cmd.Process.Pid
	m.sink(m.framer.Start(u.cfg.Name, u.pid))
	m.logger.Info("process started",
		"name", u.cfg.Name, "pid", u.pid, "role", u.cfg.Role)

	// Per StdoutPipe's contract, the pipes must be fully read before Wait. The
	// pump goroutines hit EOF when the child exits (decision 9: drain to EOF,
	// then waitpid), so waiting on them before Wait both honours that contract
	// and orders the exit record after the child's final output.
	var wg sync.WaitGroup
	wg.Go(func() { m.pump(u.cfg.Name, u.pid, StreamStdout, stdout) })
	wg.Go(func() { m.pump(u.cfg.Name, u.pid, StreamStderr, stderr) })
	go func() {
		wg.Wait()
		code, sig := classify(cmd.Wait())
		m.exits <- exitEvent{u: u, pid: u.pid, code: code, signal: sig, real: true}
	}()
}

// startFailed reports a process that never started as a synthetic exit so the
// restart/on_failure policy applies uniformly. The send is async to avoid
// re-entering the run loop (spawn is called from it).
func (m *Manager) startFailed(u *unit, err error) {
	m.logger.Error("failed to start process", "name", u.cfg.Name, "err", err)
	go func() {
		m.exits <- exitEvent{u: u, code: startFailureCode}
	}()
}

// maxChildLine bounds a single captured line from a child's stdout/stderr.
// A line longer than this is emitted truncated and the remainder is drained.
// Mirrors log_relay's defaultLogRelayMaxLine so both ends cap a line the same.
const maxChildLine = 1 << 20 // 1 MiB

// pump reads r line by line and frames each line as a log record tagged with
// the process name (src), pid, and stream. ReadSlice over a fixed buffer caps
// per-line memory: a child that writes a huge line or never emits '\n' would
// make an unbounded ReadBytes grow until OOM — long before the ring buffer's
// byte budget (decision 8) could shed the frame. On overflow the capped prefix
// is emitted flagged truncated and the rest of the line is drained so the next
// record starts at a real line boundary. A trailing partial line at EOF is
// still emitted.
func (m *Manager) pump(name string, pid int, stream string, r io.Reader) {
	br := bufio.NewReaderSize(r, maxChildLine+1)
	for {
		line, err := br.ReadSlice('\n')
		truncated := errors.Is(err, bufio.ErrBufferFull)
		if len(line) > 0 {
			payload := line
			if truncated {
				if len(payload) > maxChildLine {
					payload = payload[:maxChildLine]
				}
			} else {
				payload = bytes.TrimRight(payload, "\r\n")
			}
			m.sink(m.framer.logLine(name, pid, stream, string(payload), truncated))
		}
		if truncated {
			if derr := discardToNewline(br); derr != nil {
				return // EOF or read error while resyncing; stop.
			}
			continue
		}
		if err != nil {
			return // io.EOF (child gone) or a read error; either way, stop.
		}
	}
}

// discardToNewline reads and discards bytes until the next newline (or stream
// end), letting the reader resync after an over-long line.
func discardToNewline(br *bufio.Reader) error {
	for {
		_, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return err
	}
}

// handleExit applies decision 4 to one child exit. While shutting down, exits
// are recorded but never restarted (decision 3) — the single most important
// correctness rule.
func (m *Manager) handleExit(ev exitEvent) {
	u := ev.u
	if u.killTimer != nil {
		u.killTimer.Stop()
		u.killTimer = nil
	}
	if ev.real {
		m.sink(m.framer.Exit(u.cfg.Name, ev.pid, ev.code, ev.signal))
	}
	m.logger.Info("process exited",
		"name", u.cfg.Name, "pid", ev.pid, "code", ev.code, "signal", ev.signal)

	if m.shuttingDown {
		u.state = stateStopped
		return
	}

	if warrantsRestart(u.cfg.Restart, ev.code) {
		if u.budget.allow(m.now()) {
			m.logger.Info("restarting process",
				"name", u.cfg.Name,
				"attempts", len(u.budget.history), "max", u.cfg.MaxRestarts)
			m.spawn(u)
			return
		}
		m.logger.Warn("restart budget exhausted, giving up", "name", u.cfg.Name)
		m.applyOnFailure(u, ev.code)
		return
	}

	if ev.code == 0 {
		if u.cfg.Role == RoleTask {
			u.state = stateDone
			m.logger.Info("task complete", "name", u.cfg.Name)
		} else {
			u.state = stateStopped
			m.logger.Info("sidecar exited, tolerated", "name", u.cfg.Name)
		}
		return
	}
	// Non-zero exit that does not warrant a restart (restart=no): terminal
	// failure → apply on_failure.
	m.applyOnFailure(u, ev.code)
}

// applyOnFailure resolves a give-up per the unit's on_failure policy: terminate
// shuts the whole supervisor down and propagates code; continue abandons just
// this process (a task abandonment still makes the final exit non-zero).
func (m *Manager) applyOnFailure(u *unit, code int) {
	switch u.cfg.OnFailure {
	case OnFailureContinue:
		if u.cfg.Role == RoleTask {
			u.state = stateAbandoned
			m.setExitCode(code)
		} else {
			u.state = stateStopped
		}
		m.logger.Warn("process abandoned (on_failure=continue)",
			"name", u.cfg.Name, "code", code)
	default: // OnFailureTerminate
		u.state = stateStopped
		m.setExitCode(code)
		m.logger.Error("process failed (on_failure=terminate), shutting down",
			"name", u.cfg.Name, "code", code)
		m.beginShutdown("process failed: " + u.cfg.Name)
	}
}

// beginShutdown suspends restarts (decision 3) and SIGTERMs every running
// child, arming a per-child SIGKILL escalation after termGrace. It is
// idempotent.
func (m *Manager) beginShutdown(reason string) {
	if m.shuttingDown {
		return
	}
	m.shuttingDown = true
	m.logger.Info("beginning shutdown", "reason", reason)
	for _, u := range m.units {
		if u.state != stateRunning || u.cmd == nil || u.cmd.Process == nil {
			continue
		}
		_ = u.cmd.Process.Signal(syscall.SIGTERM)
		proc := u.cmd.Process
		u.killTimer = time.AfterFunc(m.termGrace, func() {
			_ = proc.Signal(syscall.SIGKILL)
		})
	}
}

// setExitCode records the first non-zero exit code; later failures do not
// overwrite it.
func (m *Manager) setExitCode(code int) {
	if m.exitCode == 0 && code != 0 {
		m.exitCode = code
	}
}

// allTasksTerminal reports whether every task has reached a terminal state
// (done or abandoned). Sidecars are not considered.
func (m *Manager) allTasksTerminal() bool {
	for _, u := range m.units {
		if u.cfg.Role == RoleTask && u.state == stateRunning {
			return false
		}
	}
	return true
}

// runningCount returns the number of units with a live process.
func (m *Manager) runningCount() int {
	n := 0
	for _, u := range m.units {
		if u.state == stateRunning {
			n++
		}
	}
	return n
}

// warrantsRestart reports whether an exit with the given code warrants a
// restart under policy: always→any exit, on-failure→non-zero, no→never.
func warrantsRestart(policy string, code int) bool {
	switch policy {
	case RestartAlways:
		return true
	case RestartOnFailure:
		return code != 0
	default: // RestartNo
		return false
	}
}

// classify maps an os/exec Wait error to a (code, signal-name) pair. A normal
// exit yields its status code and an empty signal; a signalled exit yields
// 128+signum and the signal name; any other error is reported as a generic
// non-zero exit.
func classify(err error) (code int, signal string) {
	if err == nil {
		return 0, ""
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			sig := ws.Signal()
			return signalExitBase + int(sig), sig.String()
		}
		return ee.ExitCode(), ""
	}
	return 1, ""
}
