// Command supervisor is PID 1's child inside an AWS Nitro Enclave (tini -g
// stays PID 1). It spawns and supervises the enclave's processes (the
// application task(s) plus a vsockd sidecar) under a role/restart policy,
// captures each process's stdout/stderr and its own operational logs, frames
// every line as NDJSON, and ships the combined stream over its OWN vsock
// connection to the parent (CID 3:<log_port>) where the host-side log_relay
// receives it. See internal/supervisor for the component details.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/olomix/vsockd/internal/supervisor"
	"github.com/olomix/vsockd/internal/vsockconn"
)

const version = "0.1.0-dev"

// defaultConfigPath is where the supervisor looks for its YAML config when
// -config is not given.
const defaultConfigPath = "/etc/supervisor/supervisor.yaml"

// defaultFlushGrace bounds the best-effort buffer flush on shutdown (decision
// 9): drain what we can to log_relay, then force-exit when it elapses.
const defaultFlushGrace = 5 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("supervisor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath,
		"path to YAML config file")
	debug := fs.Bool("debug", false, "enable debug logging")
	showVersion := fs.Bool("version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *showVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}

	// Handle the supervisor's OWN SIGTERM/SIGINT to begin graceful shutdown
	// (decision 2: tini -g delivers external signals to the children directly;
	// we do not forward them). Installed before config load so a signal arriving
	// during startup cancels cleanly rather than killing the process.
	ctx, stop := signal.NotifyContext(
		context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	cfg, err := supervisor.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "supervisor: %v\n", err)
		return 1
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}

	return supervise(ctx, superviseOptions{
		cfg:      cfg,
		dialer:   selectDialer(),
		now:      time.Now,
		stderr:   stderr,
		logLevel: level,
	})
}

// superviseOptions carries the wired dependencies for supervise. Tests inject a
// loopback dialer, a deterministic clock, and the helper-process environment;
// production fills these from run.
type superviseOptions struct {
	cfg        *supervisor.Config
	dialer     vsockconn.Dialer
	now        func() time.Time
	stderr     io.Writer
	logLevel   slog.Leveler
	flushGrace time.Duration
	extraEnv   []string
}

// supervise wires config → buffer → shipper → slog handler → manager, runs the
// manager until shutdown, best-effort flushes the buffer within the grace
// window, and returns the resolved supervisor exit code.
func supervise(ctx context.Context, opts superviseOptions) int {
	cfg := opts.cfg
	now := opts.now
	if now == nil {
		now = time.Now
	}
	stderr := opts.stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	grace := opts.flushGrace
	if grace <= 0 {
		grace = defaultFlushGrace
	}
	pid := os.Getpid()

	framer := supervisor.NewFramer(now, cfg.Tags)
	buf := supervisor.NewRingBuffer(cfg.Buffer.MaxBytes, cfg.Buffer.MaxRecords)

	// The shipper is referenced through a closure so the slog handler can enqueue
	// (and wake the drain loop) before the shipper itself is constructed. No log
	// is emitted between here and the assignment below, so the nil deref window
	// is never entered.
	var shipper *supervisor.Shipper
	sink := func(b []byte) { shipper.Enqueue(b) }

	handler := supervisor.NewLogHandler(framer, sink, stderr, pid, opts.logLevel)
	logger := slog.New(handler)

	shipper = supervisor.NewShipper(buf, framer, opts.dialer, logger,
		supervisor.ShipperConfig{CID: cfg.LogCID, Port: cfg.LogPort, PID: pid})

	// The shipper outlives the manager so it can flush buffered frames after the
	// children are gone; it is stopped only once the flush window closes.
	shipCtx, shipCancel := context.WithCancel(context.Background())
	defer shipCancel()
	go shipper.Run(shipCtx)

	mgr := supervisor.NewManager(supervisor.ManagerConfig{
		Processes: cfg.Processes,
		Framer:    framer,
		Sink:      sink,
		Logger:    logger,
		Now:       now,
		ExtraEnv:  opts.extraEnv,
	})

	logger.Info("supervisor starting",
		"version", version, "pid", pid,
		"log_cid", cfg.LogCID, "log_port", cfg.LogPort,
		"processes", len(cfg.Processes))

	code := mgr.Run(ctx)
	logger.Info("supervisor stopping", "exit_code", code)

	flushCtx, cancel := context.WithTimeout(context.Background(), grace)
	shipper.Flush(flushCtx)
	cancel()
	return code
}

// selectDialer returns the vsock dialer for the active backend. The loopback
// backend is strictly for tests and local dev (its registry has no registered
// log listener in the production binary); real deployments dial AF_VSOCK.
func selectDialer() vsockconn.Dialer {
	if vsockconn.UseLoopback() {
		return vsockconn.NewLoopbackDialer(vsockconn.NewRegistry(), 0)
	}
	return vsockconn.NewVsockDialer()
}
