package inbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/olomix/vsockd/internal/config"
	"github.com/olomix/vsockd/internal/metrics"
	"github.com/olomix/vsockd/internal/vsockconn"
)

// routeLabelUnknown labels metrics emitted before a route has been resolved
// (accept errors, sniff failures, route misses). Keeping the label value fixed
// stops errant hostnames from inflating the metric cardinality.
const routeLabelUnknown = "unknown"

// sniffReadTimeout bounds how long a silent client can hold a slot before we
// give up on extracting the Host header or SNI. The real upstream copy runs
// without a deadline once the sniff completes.
const sniffReadTimeout = 10 * time.Second

// Server runs one TCP accept loop per configured inbound listener. Each
// accepted connection is sniffed (HTTP Host header or TLS SNI), matched to a
// route, and proxied to the mapped vsock endpoint via the injected Dialer.
type Server struct {
	dialer vsockconn.Dialer
	metric *metrics.Metrics
	logger *slog.Logger

	// mu guards the listeners slice plus the accept-loop context. Accept
	// loops themselves hold no lock, so the mutex is only contended during
	// Start / Apply / Shutdown — never on the hot path.
	mu        sync.Mutex
	listeners []*listener
	ctx       context.Context

	// activeConn tracks every in-flight connection (both client-side and
	// upstream) so Shutdown can force-close them if the grace deadline
	// elapses before the proxy goroutines finish.
	connMu     sync.Mutex
	activeConn map[net.Conn]struct{}

	wg sync.WaitGroup
}

// NewServer wires a Server from the configured inbound listeners. The
// listeners are not bound yet; Start performs the bind.
func NewServer(
	listeners []config.InboundListener,
	dialer vsockconn.Dialer,
	m *metrics.Metrics,
	logger *slog.Logger,
) (*Server, error) {
	if dialer == nil {
		return nil, errors.New("inbound: dialer required")
	}
	if m == nil {
		return nil, errors.New("inbound: metrics required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		dialer:     dialer,
		metric:     m,
		logger:     logger,
		activeConn: map[net.Conn]struct{}{},
	}
	for i := range listeners {
		ln, err := newListener(listeners[i], s)
		if err != nil {
			return nil, fmt.Errorf("inbound[%d]: %w", i, err)
		}
		s.listeners = append(s.listeners, ln)
	}
	return s, nil
}

// Addr returns the bound TCP address of the i-th configured listener, or nil
// if the index is out of range or the listener has not been bound yet. Useful
// for tests that need the ephemeral port.
func (s *Server) Addr(i int) net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.listeners) {
		return nil
	}
	if s.listeners[i].tcp == nil {
		return nil
	}
	return s.listeners[i].tcp.Addr()
}

// ListenerKeys returns the (bind:port) strings of currently active
// listeners, in their configured order. Exposed for tests and for the
// reload path to verify a diff was applied.
func (s *Server) ListenerKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.listeners))
	for _, ln := range s.listeners {
		out = append(out, ln.addr())
	}
	return out
}

// Start binds every listener and launches an accept loop per listener. It
// does not block — callers wait on the context to know when to call
// Shutdown. If any bind fails, listeners already bound are closed and the
// error is returned.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ctx = ctx
	for _, ln := range s.listeners {
		if err := ln.bind(); err != nil {
			for _, other := range s.listeners {
				other.close()
			}
			return err
		}
	}
	for _, ln := range s.listeners {
		s.wg.Add(1)
		go func(l *listener) {
			defer s.wg.Done()
			l.run(ctx)
		}(ln)
	}
	return nil
}

// Apply reconciles the running listeners with the supplied configuration.
// Listeners whose (bind, port, mode) key matches an existing one keep their
// TCP listener alive but atomically swap their route table so that already
// in-flight connections continue with the rules they started on while new
// connections immediately see the new rules. Listeners that disappeared are
// closed; listeners that appeared are bound and their accept loops spawned.
//
// Apply is atomic: if any validation or bind fails, no running listener is
// mutated and any listener bound during this call is closed before the
// error is returned.
func (s *Server) Apply(cfgs []config.InboundListener) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ctx == nil {
		return errors.New("inbound: Apply called before Start")
	}

	// Index current listeners by key so we can diff in O(n).
	existing := make(map[string]*listener, len(s.listeners))
	for _, ln := range s.listeners {
		existing[ln.key()] = ln
	}

	// Phase 1: construct all listeners, bind new ones, and record the
	// route swaps to apply to kept listeners. Nothing observable changes
	// in this phase; a failure closes any just-bound sockets and leaves
	// the running state unchanged.
	type pendingSwap struct {
		listener *listener
		routes   map[string]config.Route
	}
	kept := make(map[string]bool, len(cfgs))
	next := make([]*listener, 0, len(cfgs))
	var swaps []pendingSwap
	var newBinds []*listener
	cleanup := func() {
		for _, ln := range newBinds {
			ln.close()
		}
	}

	for i := range cfgs {
		cfg := cfgs[i]
		newLn, err := newListener(cfg, s)
		if err != nil {
			cleanup()
			return fmt.Errorf("inbound[%d]: %w", i, err)
		}
		k := newLn.key()
		if cur, ok := existing[k]; ok {
			// Key covers Bind, Port, and Mode — the only cfg fields the
			// handle path reads — so we only need to swap the routes map.
			// Leaving cur.cfg alone avoids a data race with accept-loop
			// readers that don't take the server's mu.
			swaps = append(swaps, pendingSwap{cur, newLn.routesSnapshot()})
			kept[k] = true
			next = append(next, cur)
			continue
		}
		if err := newLn.bind(); err != nil {
			cleanup()
			return err
		}
		newBinds = append(newBinds, newLn)
		next = append(next, newLn)
	}

	// Phase 2: commit. No failure path remains past this point.
	for _, sw := range swaps {
		sw.listener.replaceRoutes(sw.routes)
	}
	for _, ln := range newBinds {
		s.wg.Add(1)
		go func(l *listener) {
			defer s.wg.Done()
			l.run(s.ctx)
		}(ln)
	}
	// Close listeners that disappeared from the config.
	for k, ln := range existing {
		if !kept[k] {
			ln.close()
		}
	}
	s.listeners = next
	return nil
}

// Shutdown stops every accept loop and waits for in-flight proxy goroutines
// to drain. If ctx expires first, remaining connections are force-closed and
// ctx.Err() is returned; callers can still Wait on the Server to observe the
// forced teardown.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	for _, ln := range s.listeners {
		ln.close()
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.connMu.Lock()
		for c := range s.activeConn {
			_ = c.Close()
		}
		s.connMu.Unlock()
		<-done
		return ctx.Err()
	}
}

func (s *Server) trackConn(c net.Conn) {
	s.connMu.Lock()
	s.activeConn[c] = struct{}{}
	s.connMu.Unlock()
}

func (s *Server) untrackConn(c net.Conn) {
	s.connMu.Lock()
	delete(s.activeConn, c)
	s.connMu.Unlock()
}

// listener owns a single TCP accept loop for one InboundListener.
type listener struct {
	cfg    config.InboundListener
	server *Server
	tcp    net.Listener

	// routes holds the lowercase-hostname → Route map. Stored as an atomic
	// pointer so Apply can swap the whole table without blocking the hot
	// path, and in-flight connections keep a stable reference to the map
	// they observed at accept time.
	routes atomic.Pointer[map[string]config.Route]
}

func newListener(cfg config.InboundListener, s *Server) (*listener, error) {
	switch cfg.Mode {
	case config.ModeHTTPHost, config.ModeTLSSNI:
	default:
		return nil, fmt.Errorf("unknown mode %q", cfg.Mode)
	}
	l := &listener{cfg: cfg, server: s}
	rm := buildRouteMap(cfg.Routes)
	l.routes.Store(&rm)
	return l, nil
}

func buildRouteMap(rs []config.Route) map[string]config.Route {
	out := make(map[string]config.Route, len(rs))
	for _, r := range rs {
		out[strings.ToLower(r.Hostname)] = r
	}
	return out
}

func (l *listener) routesSnapshot() map[string]config.Route {
	p := l.routes.Load()
	if p == nil {
		return nil
	}
	return *p
}

func (l *listener) replaceRoutes(rm map[string]config.Route) {
	l.routes.Store(&rm)
}

// key identifies a listener for reload diffing. Mode is part of the key:
// changing the sniff mode on the same (bind, port) semantically creates a
// new listener (different protocol) even though the TCP address is shared.
func (l *listener) key() string {
	return fmt.Sprintf("%s|%s", l.addr(), l.cfg.Mode)
}

func (l *listener) addr() string {
	return net.JoinHostPort(l.cfg.Bind, strconv.Itoa(l.cfg.Port))
}

func (l *listener) bind() error {
	tcp, err := net.Listen("tcp", l.addr())
	if err != nil {
		return fmt.Errorf("listen %s: %w", l.addr(), err)
	}
	l.tcp = tcp
	return nil
}

func (l *listener) close() {
	if l.tcp != nil {
		_ = l.tcp.Close()
	}
}

func (l *listener) run(ctx context.Context) {
	// Match net/http.Server.Serve's back-off on temporary Accept errors:
	// under FD exhaustion (EMFILE/ENFILE) the loop would otherwise pin a
	// CPU core and flood the metric + log with warn-level events.
	var tempDelay time.Duration
	for {
		c, err := l.tcp.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			if tempDelay == 0 {
				tempDelay = 5 * time.Millisecond
			} else {
				tempDelay *= 2
			}
			if tempDelay > time.Second {
				tempDelay = time.Second
			}
			l.server.metric.InboundErrors.
				WithLabelValues(routeLabelUnknown, metrics.InboundErrorAccept).Inc()
			l.server.logger.Warn("inbound accept error",
				"addr", l.addr(), "err", err, "retry_in", tempDelay)
			select {
			case <-time.After(tempDelay):
			case <-ctx.Done():
				return
			}
			continue
		}
		tempDelay = 0
		l.server.wg.Add(1)
		go func() {
			defer l.server.wg.Done()
			l.handle(ctx, c)
		}()
	}
}

func (l *listener) handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	l.server.trackConn(c)
	defer l.server.untrackConn(c)

	_ = c.SetReadDeadline(time.Now().Add(sniffReadTimeout))
	host, buffered, err := l.sniff(c)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		l.server.metric.InboundErrors.
			WithLabelValues(routeLabelUnknown, metrics.InboundErrorSniff).Inc()
		l.server.logger.Warn("inbound sniff failed",
			"addr", l.addr(), "mode", l.cfg.Mode, "err", err)
		return
	}

	routes := l.routesSnapshot()
	route, ok := routes[host]
	if !ok {
		l.server.metric.InboundErrors.
			WithLabelValues(routeLabelUnknown, metrics.InboundErrorRoute).Inc()
		l.server.logger.Warn("inbound route miss",
			"addr", l.addr(), "host", host)
		return
	}
	routeLabel := route.Hostname
	l.server.metric.InboundConnections.WithLabelValues(routeLabel).Inc()

	upstream, err := l.server.dialer.Dial(route.CID, route.VsockPort)
	if err != nil {
		l.server.metric.InboundErrors.
			WithLabelValues(routeLabel, metrics.InboundErrorDial).Inc()
		l.server.logger.Warn("inbound dial failed",
			"host", host, "cid", route.CID, "port", route.VsockPort, "err", err)
		return
	}
	defer upstream.Close()
	l.server.trackConn(upstream)
	defer l.server.untrackConn(upstream)

	if _, err := upstream.Write(buffered); err != nil {
		l.server.metric.InboundErrors.
			WithLabelValues(routeLabel, metrics.InboundErrorCopy).Inc()
		l.server.logger.Warn("inbound replay write failed",
			"host", host, "err", err)
		return
	}
	l.server.metric.InboundBytes.
		WithLabelValues(routeLabel, metrics.DirectionIn).
		Add(float64(len(buffered)))

	l.proxy(c, upstream, routeLabel)
}

func (l *listener) sniff(r io.Reader) (string, []byte, error) {
	switch l.cfg.Mode {
	case config.ModeHTTPHost:
		return SniffHost(r)
	case config.ModeTLSSNI:
		return SniffSNI(r)
	default:
		return "", nil, fmt.Errorf("unknown mode %q", l.cfg.Mode)
	}
}

// proxy runs the bidirectional copy between client and upstream. Each
// direction ends by closing the destination so the peer observes EOF.
func (l *listener) proxy(
	client net.Conn,
	upstream net.Conn,
	routeLabel string,
) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		n, err := io.Copy(upstream, client)
		l.server.metric.InboundBytes.
			WithLabelValues(routeLabel, metrics.DirectionIn).Add(float64(n))
		if err != nil && !isExpectedCopyErr(err) {
			l.server.metric.InboundErrors.
				WithLabelValues(routeLabel, metrics.InboundErrorCopy).Inc()
		}
		_ = upstream.Close()
	}()
	go func() {
		defer wg.Done()
		n, err := io.Copy(client, upstream)
		l.server.metric.InboundBytes.
			WithLabelValues(routeLabel, metrics.DirectionOut).Add(float64(n))
		if err != nil && !isExpectedCopyErr(err) {
			l.server.metric.InboundErrors.
				WithLabelValues(routeLabel, metrics.InboundErrorCopy).Inc()
		}
		_ = client.Close()
	}()
	wg.Wait()
}

func isExpectedCopyErr(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe)
}
