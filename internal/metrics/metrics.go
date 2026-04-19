// Package metrics owns the Prometheus instrumentation for vsockd.
//
// All collectors are registered against a per-instance registry so tests and
// multiple daemons in the same process do not share global state.
package metrics

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Label value constants kept in one place so call sites do not invent new
// values and inflate cardinality.
const (
	DirectionIn  = "in"
	DirectionOut = "out"

	OutboundResultAllowed = "allowed"
	OutboundResultDenied  = "denied"
	OutboundResultError   = "error"

	ReloadResultSuccess = "success"
	ReloadResultFailure = "failure"

	InboundErrorSniff   = "sniff"
	InboundErrorRoute   = "route"
	InboundErrorDial    = "dial"
	InboundErrorCopy    = "copy"
	InboundErrorAccept  = "accept"
)

// Metrics groups every Prometheus collector exposed by vsockd. Construct via
// New and share the pointer between subsystems.
type Metrics struct {
	reg *prometheus.Registry

	InboundConnections *prometheus.CounterVec
	InboundBytes       *prometheus.CounterVec
	InboundErrors      *prometheus.CounterVec

	OutboundConnections *prometheus.CounterVec
	OutboundBytes       *prometheus.CounterVec

	ConfigReloads *prometheus.CounterVec
}

// New builds a Metrics bundle backed by a fresh, isolated registry.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		InboundConnections: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "inbound_connections_total",
				Help: "Total TCP connections accepted on inbound listeners, by route.",
			},
			[]string{"route"},
		),
		InboundBytes: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "inbound_bytes_total",
				Help: "Bytes proxied on inbound connections, by route and direction.",
			},
			[]string{"route", "direction"},
		),
		InboundErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "inbound_errors_total",
				Help: "Inbound errors, by route and error kind.",
			},
			[]string{"route", "kind"},
		),
		OutboundConnections: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "outbound_connections_total",
				Help: "vsock connections accepted on outbound listeners, by CID and policy result.",
			},
			[]string{"cid", "result"},
		),
		OutboundBytes: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "outbound_bytes_total",
				Help: "Bytes proxied on outbound connections, by CID and direction.",
			},
			[]string{"cid", "direction"},
		),
		ConfigReloads: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "config_reloads_total",
				Help: "Configuration reload attempts, by result.",
			},
			[]string{"result"},
		),
	}

	reg.MustRegister(
		m.InboundConnections,
		m.InboundBytes,
		m.InboundErrors,
		m.OutboundConnections,
		m.OutboundBytes,
		m.ConfigReloads,
	)
	return m
}

// Registry returns the underlying registry so tests and Gatherers can poke
// at the raw collectors.
func (m *Metrics) Registry() *prometheus.Registry {
	return m.reg
}

// Handler returns an http.Handler that serves this instance's metrics in the
// Prometheus text exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
		Registry: m.reg,
	})
}

// FormatCID renders a CID as a short decimal string for use as a metric label.
// Centralised so every caller labels the same way.
func FormatCID(cid uint32) string {
	return strconv.FormatUint(uint64(cid), 10)
}

// ServeMetrics starts an HTTP server exposing /metrics on addr and blocks
// until ctx is cancelled or the listener fails. The listener is closed on
// ctx.Done(). http.ErrServerClosed is treated as a clean shutdown.
func (m *Metrics) ServeMetrics(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		<-errCh
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
