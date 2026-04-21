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

	// Direction labels for TCP passthrough counters. `up` and `down` are
	// symmetrical (the proxy does not attribute enclave-vs-host semantics
	// to a raw TCP stream); their only purpose is to expose per-direction
	// throughput so operators can spot half-duplex traffic patterns.
	DirectionUp   = "up"
	DirectionDown = "down"

	OutboundResultAllowed = "allowed"
	OutboundResultDenied  = "denied"
	OutboundResultError   = "error"

	ReloadResultSuccess = "success"
	ReloadResultFailure = "failure"

	InboundErrorSniff  = "sniff"
	InboundErrorRoute  = "route"
	InboundErrorDial   = "dial"
	InboundErrorCopy   = "copy"
	InboundErrorAccept = "accept"

	// TCP passthrough error reasons. Fixed values keep the `reason` label
	// bounded so a misbehaving upstream cannot inflate cardinality.
	TCPErrorDial = "dial_fail"
	TCPErrorCopy = "copy_error"

	// CIDLabelUnauthorized is emitted on outbound_connections_total in place
	// of the raw peer CID when a connection is rejected because the CID is
	// not configured on that port. Using a fixed value prevents arbitrary
	// source CIDs from inflating metric cardinality.
	CIDLabelUnauthorized = "unauthorized"
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

	// TCP passthrough counters. Labels are kept minimal and bounded:
	// direction ∈ {up, down}, reason ∈ {dial_fail, copy_error}. No
	// per-connection identifiers leak into label values, so cardinality is
	// constant regardless of traffic patterns.
	TCPToVsockConnections prometheus.Counter
	TCPToVsockBytes       *prometheus.CounterVec
	TCPToVsockErrors      *prometheus.CounterVec
	VsockToTCPConnections prometheus.Counter
	VsockToTCPBytes       *prometheus.CounterVec
	VsockToTCPErrors      *prometheus.CounterVec

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
		TCPToVsockConnections: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "tcp_to_vsock_connections_total",
				Help: "TCP connections accepted on tcp_to_vsock listeners.",
			},
		),
		TCPToVsockBytes: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "tcp_to_vsock_bytes_total",
				Help: "Bytes proxied on tcp_to_vsock connections, by direction.",
			},
			[]string{"direction"},
		),
		TCPToVsockErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "tcp_to_vsock_errors_total",
				Help: "Errors on tcp_to_vsock connections, by reason.",
			},
			[]string{"reason"},
		),
		VsockToTCPConnections: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "vsock_to_tcp_connections_total",
				Help: "vsock connections accepted on vsock_to_tcp listeners.",
			},
		),
		VsockToTCPBytes: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "vsock_to_tcp_bytes_total",
				Help: "Bytes proxied on vsock_to_tcp connections, by direction.",
			},
			[]string{"direction"},
		),
		VsockToTCPErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "vsock_to_tcp_errors_total",
				Help: "Errors on vsock_to_tcp connections, by reason.",
			},
			[]string{"reason"},
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
		m.TCPToVsockConnections,
		m.TCPToVsockBytes,
		m.TCPToVsockErrors,
		m.VsockToTCPConnections,
		m.VsockToTCPBytes,
		m.VsockToTCPErrors,
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

// ListenMetrics binds a TCP listener for the /metrics HTTP endpoint. Split
// from ServeMetrics so callers can surface bind errors (EADDRINUSE, permission
// denied) synchronously from their Start paths rather than having them vanish
// into an async log line.
func ListenMetrics(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}

// ServeMetrics runs the /metrics HTTP server on ln and blocks until ctx is
// cancelled or the server fails. On cancellation, srv.Shutdown is given up
// to shutdownGrace to drain in-flight scrapes so the caller's overall grace
// window is honoured; http.ErrServerClosed is treated as a clean shutdown.
func (m *Metrics) ServeMetrics(
	ctx context.Context, ln net.Listener, shutdownGrace time.Duration,
) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), shutdownGrace)
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
