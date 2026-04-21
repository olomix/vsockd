// Command vsockd is the host-side daemon that bridges network traffic
// between EC2 Nitro Enclaves and the outside world over vsock. See
// docs/plans and internal/app for the lifecycle details.
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

	"github.com/olomix/vsockd/internal/app"
	"github.com/olomix/vsockd/internal/config"
	"github.com/olomix/vsockd/internal/outbound"
	"github.com/olomix/vsockd/internal/vsockconn"
)

const version = "0.1.0-dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("vsockd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "/etc/vsockd/vsockd.yaml",
		"path to YAML config file")
	metricsAddr := fs.String("metrics-addr", "",
		"TCP listen address for the Prometheus /metrics endpoint "+
			"(overrides metrics.bind / metrics.vsock_port in the "+
			"config file). Empty and no metrics section = disabled.")
	logFormat := fs.String("log-format", "",
		"log format: json | text | auto (empty uses config.log_format)")
	debug := fs.Bool("debug", false,
		"enable debug logging (overrides VSOCKD_LOG_LEVEL and config.log_level)")
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

	// Install signal handlers immediately so termination signals arriving
	// during config load or listener bind are queued rather than defaulting
	// to process termination.
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "vsockd: %v\n", err)
		return 1
	}

	level, err := resolveLogLevel(*debug,
		os.Getenv("VSOCKD_LOG_LEVEL"), cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(stderr, "vsockd: %v\n", err)
		return 1
	}

	logger := buildLogger(stderr,
		resolveLogFormat(*logFormat, cfg.LogFormat), level)

	dialer, listenFn, backend := selectVsockBackend()
	logger.Info("vsockd starting",
		"version", version, "config", *configPath,
		"vsock_backend", backend, "log_level", level.String())

	metricsAddrRes, metricsVsockPort := resolveMetrics(fs, *metricsAddr, cfg)
	a, err := app.New(app.Options{
		ConfigPath:       *configPath,
		Config:           cfg,
		Logger:           logger,
		MetricsAddr:      metricsAddrRes,
		MetricsVsockPort: metricsVsockPort,
		VsockDialer:      dialer,
		VsockListenFn:    listenFn,
	})
	if err != nil {
		fmt.Fprintf(stderr, "vsockd: %v\n", err)
		return 1
	}

	// serveCtx stays live for the life of the process. We never cancel it
	// directly — Shutdown drives listener teardown and waits for drain.
	serveCtx, serveCancel := context.WithCancel(context.Background())
	defer serveCancel()

	if err := a.Start(serveCtx); err != nil {
		fmt.Fprintf(stderr, "vsockd: %v\n", err)
		return 1
	}

	return runSignalLoop(a, logger, sigCh)
}

// runSignalLoop blocks on OS signals: SIGHUP triggers a config reload
// (failures are logged but do not terminate the process), SIGTERM/SIGINT
// trigger a graceful shutdown bounded by shutdown_grace.
func runSignalLoop(a *app.App, logger *slog.Logger, sigCh <-chan os.Signal) int {
	for sig := range sigCh {
		switch sig {
		case syscall.SIGHUP:
			if err := a.Reload(); err != nil {
				logger.Error("reload failed", "err", err)
			}
		case syscall.SIGTERM, syscall.SIGINT:
			logger.Info("termination signal received", "signal", sig.String())
			if err := a.Shutdown(context.Background()); err != nil {
				logger.Error("shutdown error", "err", err)
				return 1
			}
			return 0
		}
	}
	return 0
}

// resolveLogFormat picks the concrete handler kind. Priority: flag (if
// set) → config.log_format → "auto". "auto" resolves to text when stderr
// is a TTY and json otherwise.
func resolveLogFormat(flagVal, cfgVal string) string {
	f := flagVal
	if f == "" {
		f = cfgVal
	}
	if f == "" || f == config.LogFormatAuto {
		if isTerminal(os.Stderr) {
			return config.LogFormatText
		}
		return config.LogFormatJSON
	}
	return f
}

// isTerminal reports whether f is connected to a character device (TTY)
// rather than a pipe, file, or socket. Avoiding a go-isatty dependency
// keeps the binary's third-party footprint minimal.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func buildLogger(w io.Writer, format string, level slog.Level) *slog.Logger {
	// slog.LevelVar keeps all handlers behind one mutable level. We set it
	// once at startup; future dynamic flipping can reuse the same var.
	var lvl slog.LevelVar
	lvl.Set(level)
	opts := &slog.HandlerOptions{Level: &lvl}
	var h slog.Handler
	switch format {
	case config.LogFormatText:
		h = slog.NewTextHandler(w, opts)
	default:
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}

// resolveLogLevel picks the effective slog level given the -debug flag, the
// VSOCKD_LOG_LEVEL env var, and the yaml log_level value. Precedence:
// flag > env > yaml > info. An invalid env var value is a fatal error
// (yaml values are validated at config-load time).
func resolveLogLevel(debug bool, envVar, cfgLevel string) (slog.Level, error) {
	if debug {
		return slog.LevelDebug, nil
	}
	if envVar != "" {
		switch envVar {
		case config.LogLevelDebug:
			return slog.LevelDebug, nil
		case config.LogLevelInfo:
			return slog.LevelInfo, nil
		default:
			return slog.LevelInfo, fmt.Errorf(
				"VSOCKD_LOG_LEVEL %q must be %q or %q",
				envVar, config.LogLevelDebug, config.LogLevelInfo)
		}
	}
	if cfgLevel == config.LogLevelDebug {
		return slog.LevelDebug, nil
	}
	return slog.LevelInfo, nil
}

// resolveMetrics picks the metrics transport. Precedence: explicit
// -metrics-addr flag > metrics.bind > metrics.vsock_port > disabled. An
// explicit flag always wins and forces TCP even if YAML declared vsock; this
// matches the flag's "operator override" role. config.Validate rejects
// Bind and VsockPort both being non-zero, so the YAML path yields at most
// one transport.
func resolveMetrics(
	fs *flag.FlagSet, flagVal string, cfg *config.Config,
) (addr string, vsockPort uint32) {
	userSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "metrics-addr" {
			userSet = true
		}
	})
	if userSet {
		return flagVal, 0
	}
	if cfg.Metrics.Bind != "" {
		return cfg.Metrics.Bind, 0
	}
	if cfg.Metrics.VsockPort != 0 {
		return "", cfg.Metrics.VsockPort
	}
	return "", 0
}

// selectVsockBackend returns the dialer, listen function, and a short label
// for the active vsock backend. The loopback backend is strictly for tests
// and local dev; real deployments always use AF_VSOCK.
func selectVsockBackend() (vsockconn.Dialer, outbound.ListenFunc, string) {
	if vsockconn.UseLoopback() {
		// Tests construct their own Registry when they need a loopback
		// backend in-process. The production binary deliberately cannot
		// be driven into a working loopback mode via env alone: without a
		// registry, dials have nowhere to go.
		reg := vsockconn.NewRegistry()
		return vsockconn.NewLoopbackDialer(reg, 0),
			func(port uint32) (vsockconn.Listener, error) {
				return vsockconn.ListenLoopback(reg, 0, port)
			},
			"loopback"
	}
	return vsockconn.NewVsockDialer(),
		func(port uint32) (vsockconn.Listener, error) {
			return vsockconn.ListenVsock(port)
		},
		"vsock"
}
