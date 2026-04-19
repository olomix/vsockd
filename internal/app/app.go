// Package app wires vsockd's subsystems (metrics HTTP, inbound and
// outbound proxies) into a single lifecycle with Start, Reload, and
// Shutdown methods. cmd/vsockd/main.go is a thin flag-parsing layer on
// top; putting the lifecycle here keeps signal handling and reload logic
// testable without spawning a subprocess.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/olomix/vsockd/internal/config"
	"github.com/olomix/vsockd/internal/inbound"
	"github.com/olomix/vsockd/internal/metrics"
	"github.com/olomix/vsockd/internal/outbound"
	"github.com/olomix/vsockd/internal/vsockconn"
)

// DefaultShutdownGrace is used when the config's shutdown_grace is zero.
const DefaultShutdownGrace = 30 * time.Second

// Options bundles everything New needs to build an App. A caller typically
// parses flags plus the initial config file, then fills this struct.
type Options struct {
	// ConfigPath is the on-disk location of the YAML config. Reload reads
	// from this path; it must stay stable for the lifetime of the App.
	ConfigPath string

	// Config is the initially loaded config. Subsequent reloads read fresh
	// content from ConfigPath.
	Config *config.Config

	// Logger receives structured logs. Required.
	Logger *slog.Logger

	// MetricsAddr is the listen address for the /metrics HTTP endpoint.
	// Ignored when empty (no metrics endpoint is started).
	MetricsAddr string

	// VsockDialer reaches enclaves from inbound. Required.
	VsockDialer vsockconn.Dialer

	// VsockListenFn opens outbound vsock listeners. Required.
	VsockListenFn outbound.ListenFunc
}

// App is the composed runtime for vsockd.
type App struct {
	opts    Options
	metric  *metrics.Metrics
	inbound *inbound.Server
	out     *outbound.Server

	// reloadMu serializes reloads so concurrent SIGHUPs do not race on the
	// diff. Start, Reload, and Shutdown all take it.
	reloadMu sync.Mutex

	// metricsDone is closed once the metrics HTTP server goroutine exits.
	// Shutdown waits on it so the process does not leak the listener.
	metricsDone chan struct{}
	metricsCtx  context.Context
	metricsStop context.CancelFunc

	// currentCfg is the most recently loaded+applied config. Reload
	// replaces it atomically while holding reloadMu.
	currentCfg *config.Config
}

// New constructs an App from opts. It does not start anything; call Start.
func New(opts Options) (*App, error) {
	if opts.Config == nil {
		return nil, errors.New("app: Config is required")
	}
	if opts.Logger == nil {
		return nil, errors.New("app: Logger is required")
	}
	if opts.VsockDialer == nil {
		return nil, errors.New("app: VsockDialer is required")
	}
	if opts.VsockListenFn == nil {
		return nil, errors.New("app: VsockListenFn is required")
	}

	m := metrics.New()

	in, err := inbound.NewServer(
		opts.Config.Inbound, opts.VsockDialer, m, opts.Logger)
	if err != nil {
		return nil, fmt.Errorf("inbound: %w", err)
	}
	out, err := outbound.NewServer(
		opts.Config.Outbound, opts.VsockListenFn, m, opts.Logger)
	if err != nil {
		return nil, fmt.Errorf("outbound: %w", err)
	}

	return &App{
		opts:       opts,
		metric:     m,
		inbound:    in,
		out:        out,
		currentCfg: opts.Config,
	}, nil
}

// Metrics returns the Metrics bundle. Useful for tests that want to poke
// at counters without scraping the HTTP endpoint.
func (a *App) Metrics() *metrics.Metrics { return a.metric }

// Inbound returns the inbound server. Used by tests.
func (a *App) Inbound() *inbound.Server { return a.inbound }

// Outbound returns the outbound server. Used by tests.
func (a *App) Outbound() *outbound.Server { return a.out }

// Start binds all inbound and outbound listeners and launches the metrics
// HTTP endpoint if configured. ctx is retained by the servers for the life
// of their accept loops; cancelling ctx does NOT trigger a graceful
// shutdown — call Shutdown for that.
func (a *App) Start(ctx context.Context) error {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()

	if err := a.inbound.Start(ctx); err != nil {
		return fmt.Errorf("inbound start: %w", err)
	}
	if err := a.out.Start(ctx); err != nil {
		_ = a.inbound.Shutdown(context.Background())
		return fmt.Errorf("outbound start: %w", err)
	}

	if a.opts.MetricsAddr != "" {
		a.metricsCtx, a.metricsStop = context.WithCancel(context.Background())
		a.metricsDone = make(chan struct{})
		addr := a.opts.MetricsAddr
		go func() {
			defer close(a.metricsDone)
			if err := a.metric.ServeMetrics(a.metricsCtx, addr); err != nil {
				a.opts.Logger.Error("metrics server exited",
					"addr", addr, "err", err)
			}
		}()
	}
	a.opts.Logger.Info("vsockd started",
		"inbound_listeners", len(a.inbound.ListenerKeys()),
		"outbound_ports", len(a.out.ListenerPorts()),
		"metrics_addr", a.opts.MetricsAddr)
	return nil
}

// Reload re-reads the config file from disk, validates it, and applies the
// diff to the inbound and outbound servers. A failed reload leaves the
// running configuration unchanged and returns the error; a successful
// reload replaces currentCfg atomically. Either way, the config_reloads
// counter is incremented.
func (a *App) Reload() error {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()

	cfg, err := config.Load(a.opts.ConfigPath)
	if err != nil {
		a.metric.ConfigReloads.
			WithLabelValues(metrics.ReloadResultFailure).Inc()
		a.opts.Logger.Error("config reload failed",
			"path", a.opts.ConfigPath, "err", err)
		return err
	}
	if err := a.inbound.Apply(cfg.Inbound); err != nil {
		a.metric.ConfigReloads.
			WithLabelValues(metrics.ReloadResultFailure).Inc()
		a.opts.Logger.Error("inbound reload failed", "err", err)
		return err
	}
	if err := a.out.Apply(cfg.Outbound); err != nil {
		a.metric.ConfigReloads.
			WithLabelValues(metrics.ReloadResultFailure).Inc()
		a.opts.Logger.Error("outbound reload failed", "err", err)
		return err
	}
	a.currentCfg = cfg
	a.metric.ConfigReloads.
		WithLabelValues(metrics.ReloadResultSuccess).Inc()
	a.opts.Logger.Info("config reloaded",
		"path", a.opts.ConfigPath,
		"inbound_listeners", len(a.inbound.ListenerKeys()),
		"outbound_ports", len(a.out.ListenerPorts()))
	return nil
}

// Shutdown stops every subsystem. It enforces the grace window by sharing
// a deadline across inbound, outbound, and the metrics HTTP server. After
// the grace period elapses, remaining connections are force-closed.
func (a *App) Shutdown(ctx context.Context) error {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()

	grace := a.grace()
	shutdownCtx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()

	a.opts.Logger.Info("vsockd shutting down", "grace", grace)

	var firstErr error
	if err := a.inbound.Shutdown(shutdownCtx); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("inbound shutdown: %w", err)
	}
	if err := a.out.Shutdown(shutdownCtx); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("outbound shutdown: %w", err)
	}
	if a.metricsStop != nil {
		a.metricsStop()
		<-a.metricsDone
	}
	return firstErr
}

func (a *App) grace() time.Duration {
	g := a.currentCfg.ShutdownGrace.Duration()
	if g <= 0 {
		return DefaultShutdownGrace
	}
	return g
}
