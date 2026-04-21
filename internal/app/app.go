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
	"net"
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
	metricsLn   net.Listener

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
		opts.Config.Inbound, opts.Config.TCPToVsock,
		opts.VsockDialer, m, opts.Logger)
	if err != nil {
		return nil, fmt.Errorf("inbound: %w", err)
	}
	out, err := outbound.NewServer(
		opts.Config.Outbound, opts.Config.VsockToTCP,
		opts.VsockListenFn, m, opts.Logger)
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
		// Bind synchronously so EADDRINUSE / permission errors abort Start
		// instead of vanishing into an async log line that leaves the daemon
		// running without a /metrics endpoint.
		ln, err := metrics.ListenMetrics(a.opts.MetricsAddr)
		if err != nil {
			_ = a.inbound.Shutdown(context.Background())
			_ = a.out.Shutdown(context.Background())
			return fmt.Errorf(
				"metrics listen %s: %w", a.opts.MetricsAddr, err)
		}
		a.metricsLn = ln
		a.metricsCtx, a.metricsStop = context.WithCancel(context.Background())
		a.metricsDone = make(chan struct{})
		grace := a.grace()
		go func() {
			defer close(a.metricsDone)
			if err := a.metric.ServeMetrics(
				a.metricsCtx, ln, grace,
			); err != nil {
				a.opts.Logger.Error("metrics server exited",
					"addr", a.opts.MetricsAddr, "err", err)
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
//
// The diff is staged through both subsystems before anything is committed:
// we bind every new listener first (phase 1 on inbound, then outbound),
// and only commit the swap once both subsystems have validated and bound
// successfully. If outbound's phase 1 fails, inbound's pending plan is
// aborted (its newly bound sockets are released) before returning. This
// keeps the daemon's observable state atomic across the two subsystems.
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
	inPlan, err := a.inbound.PrepareApply(cfg.Inbound, cfg.TCPToVsock)
	if err != nil {
		a.metric.ConfigReloads.
			WithLabelValues(metrics.ReloadResultFailure).Inc()
		a.opts.Logger.Error("inbound reload failed", "err", err)
		return err
	}
	outPlan, err := a.out.PrepareApply(cfg.Outbound, cfg.VsockToTCP)
	if err != nil {
		inPlan.AbortApply()
		a.metric.ConfigReloads.
			WithLabelValues(metrics.ReloadResultFailure).Inc()
		a.opts.Logger.Error("outbound reload failed", "err", err)
		return err
	}
	inPlan.CommitApply()
	outPlan.CommitApply()
	a.currentCfg = cfg
	a.metric.ConfigReloads.
		WithLabelValues(metrics.ReloadResultSuccess).Inc()
	a.opts.Logger.Info("config reloaded",
		"path", a.opts.ConfigPath,
		"inbound_listeners", len(a.inbound.ListenerKeys()),
		"outbound_ports", len(a.out.ListenerPorts()))
	return nil
}

// Shutdown stops every subsystem. Inbound, outbound, and the metrics HTTP
// server each get the full grace window (they run concurrently, not in
// series) after which any remaining connections are force-closed.
func (a *App) Shutdown(ctx context.Context) error {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()

	grace := a.grace()
	shutdownCtx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()

	a.opts.Logger.Info("vsockd shutting down", "grace", grace)

	// Run the three shutdowns concurrently so a slow inbound drain does not
	// starve outbound or metrics of their share of the grace window.
	var (
		wg                  sync.WaitGroup
		errMu               sync.Mutex
		inErr, outErr, mErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := a.inbound.Shutdown(shutdownCtx); err != nil {
			errMu.Lock()
			inErr = err
			errMu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		if err := a.out.Shutdown(shutdownCtx); err != nil {
			errMu.Lock()
			outErr = err
			errMu.Unlock()
		}
	}()
	if a.metricsStop != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.metricsStop()
			// If shutdownCtx expires while metrics is still draining
			// in-flight scrapes, force the listener closed so srv.Serve
			// returns and the goroutine exits instead of blocking the
			// process past the grace window (e.g. systemd's
			// TimeoutStopSec).
			select {
			case <-a.metricsDone:
			case <-shutdownCtx.Done():
				_ = a.metricsLn.Close()
				<-a.metricsDone
				errMu.Lock()
				mErr = shutdownCtx.Err()
				errMu.Unlock()
			}
		}()
	}
	wg.Wait()

	switch {
	case inErr != nil:
		return fmt.Errorf("inbound shutdown: %w", inErr)
	case outErr != nil:
		return fmt.Errorf("outbound shutdown: %w", outErr)
	case mErr != nil:
		return fmt.Errorf("metrics shutdown: %w", mErr)
	}
	return nil
}

func (a *App) grace() time.Duration {
	g := a.currentCfg.ShutdownGrace.Duration()
	if g <= 0 {
		return DefaultShutdownGrace
	}
	return g
}
