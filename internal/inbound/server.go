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

// modeTCPToVsock is the internal mode tag for listeners constructed from
// TCPToVsockListener config. Kept distinct from user-visible YAML mode
// values so the listener type never collides with http-host/tls-sni.
const modeTCPToVsock = "tcp_to_vsock"

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

	// dialCtx is derived from ctx in Start and cancelled by Shutdown when
	// the grace window elapses. handle() watches it while an upstream vsock
	// Dial is outstanding so a stuck dial cannot hold the process past
	// shutdown_grace.
	dialCtx    context.Context
	cancelDial context.CancelFunc

	// activeConn tracks every in-flight connection (both client-side and
	// upstream) so Shutdown can force-close them if the grace deadline
	// elapses before the proxy goroutines finish.
	connMu     sync.Mutex
	activeConn map[net.Conn]struct{}

	wg sync.WaitGroup
}

// NewServer wires a Server from the configured HTTP-aware inbound listeners
// and the raw tcp_to_vsock listeners. The sockets are not bound yet; Start
// performs the bind.
func NewServer(
	inbound []config.InboundListener,
	tcpToVsock []config.TCPToVsockListener,
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
	for i := range inbound {
		ln, err := newHTTPListener(inbound[i], s)
		if err != nil {
			return nil, fmt.Errorf("inbound[%d]: %w", i, err)
		}
		s.listeners = append(s.listeners, ln)
	}
	for i := range tcpToVsock {
		ln, err := newTCPToVsockListener(tcpToVsock[i], s)
		if err != nil {
			return nil, fmt.Errorf("tcp_to_vsock[%d]: %w", i, err)
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
	s.dialCtx, s.cancelDial = context.WithCancel(ctx)
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

// ApplyPlan holds the pending diff computed by PrepareApply. It carries the
// listeners that still need to be bound (phase 1 already bound them), the
// route-table swaps to apply to kept listeners, the new listener order, and
// the set of existing listeners that must be closed on commit. Callers must
// call either CommitApply or AbortApply exactly once per plan.
type ApplyPlan struct {
	server   *Server
	next     []*listener
	newBinds []*listener
	swaps    []applySwap
	existing map[string]*listener
	kept     map[string]bool
	// committed guards against double-commit / commit-after-abort. The
	// server mutex is not held between Prepare and Commit, so a stray call
	// would otherwise silently mutate the running state.
	committed bool
	aborted   bool
}

type applySwap struct {
	listener *listener
	routes   map[string]config.Route
}

// PrepareApply validates the new configuration, binds any listeners that
// need to appear, and records the diff without touching any running state.
// The returned plan must be either committed via CommitApply (to publish
// the diff) or rolled back via AbortApply (to release the newly bound
// sockets). On error no listener is bound and nothing is mutated.
//
// Splitting the work this way lets app.Reload stage both inbound and
// outbound changes before committing either, so a partial reload cannot
// leave the daemon in a half-new state when one subsystem's bind fails.
func (s *Server) PrepareApply(
	inboundCfgs []config.InboundListener,
	tcpCfgs []config.TCPToVsockListener,
) (*ApplyPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ctx == nil {
		return nil, errors.New("inbound: PrepareApply called before Start")
	}

	existing := make(map[string]*listener, len(s.listeners))
	// existingByAddr lets us detect mode changes on an already-bound
	// bind:port — a case the Apply path cannot handle atomically because
	// the old listener still owns the TCP socket when Phase 1 tries to
	// bind the new one.
	existingByAddr := make(map[string]*listener, len(s.listeners))
	for _, ln := range s.listeners {
		existing[ln.key()] = ln
		existingByAddr[ln.addr()] = ln
	}

	total := len(inboundCfgs) + len(tcpCfgs)
	kept := make(map[string]bool, total)
	next := make([]*listener, 0, total)
	var swaps []applySwap
	var newBinds []*listener
	cleanup := func() {
		for _, ln := range newBinds {
			ln.close()
		}
	}

	apply := func(section string, i int, newLn *listener) error {
		k := newLn.key()
		if cur, ok := existing[k]; ok {
			// For tcp_to_vsock listeners there is no in-place swap of the
			// vsock target today: handleTCP reads cur.targetCID/targetPort,
			// which CommitApply never updates. Silently ignoring a
			// vsock_cid / vsock_port edit would leave operators believing
			// the reload took effect while traffic keeps flowing to the old
			// enclave, so reject the reload loudly instead — mirroring the
			// mode-change guard below.
			if newLn.mode == modeTCPToVsock {
				if cur.targetCID.Load() != newLn.targetCID.Load() ||
					cur.targetPort.Load() != newLn.targetPort.Load() {
					return fmt.Errorf(
						"%s[%d]: cannot change vsock_cid/vsock_port on %s at runtime; restart required",
						section, i, newLn.addr())
				}
			}
			// Key covers bind, port, and mode — the only fields the
			// handle path reads — so we only need to swap the routes map.
			// Leaving cur.routes for non-TCP listeners untouched avoids a
			// data race with accept-loop readers that don't take the
			// server's mu.
			swaps = append(swaps, applySwap{cur, newLn.routesSnapshot()})
			kept[k] = true
			next = append(next, cur)
			return nil
		}
		// Same bind:port with a different mode: the old listener still
		// owns the TCP socket, so net.Listen would fail with EADDRINUSE.
		// Surface a clear error instead of that confusing system message;
		// the operator can restart the daemon to apply the mode change.
		if cur, addrMatch := existingByAddr[newLn.addr()]; addrMatch {
			return fmt.Errorf(
				"%s[%d]: cannot change mode on %s from %q to %q at runtime; restart required",
				section, i, newLn.addr(), cur.mode, newLn.mode)
		}
		if err := newLn.bind(); err != nil {
			return err
		}
		newBinds = append(newBinds, newLn)
		next = append(next, newLn)
		return nil
	}

	for i := range inboundCfgs {
		newLn, err := newHTTPListener(inboundCfgs[i], s)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("inbound[%d]: %w", i, err)
		}
		if err := apply("inbound", i, newLn); err != nil {
			cleanup()
			return nil, err
		}
	}
	for i := range tcpCfgs {
		newLn, err := newTCPToVsockListener(tcpCfgs[i], s)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("tcp_to_vsock[%d]: %w", i, err)
		}
		if err := apply("tcp_to_vsock", i, newLn); err != nil {
			cleanup()
			return nil, err
		}
	}

	return &ApplyPlan{
		server:   s,
		next:     next,
		newBinds: newBinds,
		swaps:    swaps,
		existing: existing,
		kept:     kept,
	}, nil
}

// CommitApply publishes the diff built by PrepareApply: kept listeners get
// their new routes, newly bound listeners start their accept loops, and
// listeners absent from the new config are closed. This phase cannot fail
// and is idempotent (subsequent calls are no-ops).
func (p *ApplyPlan) CommitApply() {
	if p == nil || p.committed || p.aborted {
		return
	}
	s := p.server
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, sw := range p.swaps {
		sw.listener.replaceRoutes(sw.routes)
	}
	for _, ln := range p.newBinds {
		s.wg.Add(1)
		go func(l *listener) {
			defer s.wg.Done()
			l.run(s.ctx)
		}(ln)
	}
	for k, ln := range p.existing {
		if !p.kept[k] {
			ln.close()
		}
	}
	s.listeners = p.next
	p.committed = true
}

// AbortApply releases any listeners bound by PrepareApply without touching
// the running state. Safe to call on a nil plan and idempotent.
func (p *ApplyPlan) AbortApply() {
	if p == nil || p.committed || p.aborted {
		return
	}
	for _, ln := range p.newBinds {
		ln.close()
	}
	p.aborted = true
}

// Apply is a convenience wrapper around PrepareApply + CommitApply kept for
// tests and any caller that doesn't need to coordinate with other
// subsystems' reloads. Use PrepareApply/CommitApply for cross-subsystem
// atomicity.
func (s *Server) Apply(
	inboundCfgs []config.InboundListener,
	tcpCfgs []config.TCPToVsockListener,
) error {
	plan, err := s.PrepareApply(inboundCfgs, tcpCfgs)
	if err != nil {
		return err
	}
	plan.CommitApply()
	return nil
}

// Shutdown stops every accept loop and waits for in-flight proxy goroutines
// to drain. If ctx expires first, dialCtx is cancelled (unblocking any
// still-pending upstream dials), remaining connections are force-closed,
// and ctx.Err() is returned. Callers can still Wait on the Server to
// observe the forced teardown.
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
		if s.cancelDial != nil {
			s.cancelDial()
		}
		return nil
	case <-ctx.Done():
		if s.cancelDial != nil {
			s.cancelDial()
		}
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

// listener owns a single TCP accept loop for one HTTP-aware or
// tcp_to_vsock listener. The mode field determines dispatch: "http-host"
// and "tls-sni" take the sniff+route path; modeTCPToVsock takes handleTCP.
type listener struct {
	bindAddr string
	port     int
	mode     string
	server   *Server
	tcp      net.Listener

	// routes holds the lowercase-hostname → Route map. Stored as an atomic
	// pointer so Apply can swap the whole table without blocking the hot
	// path, and in-flight connections keep a stable reference to the map
	// they observed at accept time. Unused for tcp_to_vsock listeners.
	routes atomic.Pointer[map[string]config.Route]

	// targetCID / targetPort hold the live vsock target for tcp_to_vsock
	// listeners. Atomic types keep concurrent handleTCP reads race-free
	// and leave room to introduce an in-place target swap the same way
	// the outbound side swaps upstream — today a SIGHUP that changes
	// vsock_cid or vsock_port on an already-bound listener is rejected
	// with a "restart required" error by PrepareApply, so these values
	// never change for the lifetime of the listener.
	// Unused for http-host/tls-sni listeners.
	targetCID  atomic.Uint32
	targetPort atomic.Uint32
}

func newHTTPListener(cfg config.InboundListener, s *Server) (*listener, error) {
	switch cfg.Mode {
	case config.ModeHTTPHost, config.ModeTLSSNI:
	default:
		return nil, fmt.Errorf("unknown mode %q", cfg.Mode)
	}
	l := &listener{
		bindAddr: cfg.Bind,
		port:     cfg.Port,
		mode:     cfg.Mode,
		server:   s,
	}
	rm := buildRouteMap(cfg.Routes)
	l.routes.Store(&rm)
	return l, nil
}

func newTCPToVsockListener(
	cfg config.TCPToVsockListener, s *Server,
) (*listener, error) {
	l := &listener{
		bindAddr: cfg.Bind,
		port:     cfg.Port,
		mode:     modeTCPToVsock,
		server:   s,
	}
	l.targetCID.Store(cfg.VsockCID)
	l.targetPort.Store(cfg.VsockPort)
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
	return fmt.Sprintf("%s|%s", l.addr(), l.mode)
}

func (l *listener) addr() string {
	return net.JoinHostPort(l.bindAddr, strconv.Itoa(l.port))
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
			if l.mode == modeTCPToVsock {
				l.handleTCP(ctx, c)
				return
			}
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
			"addr", l.addr(), "mode", l.mode, "err", err)
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

	upstream, err := l.dialUpstream(route.CID, route.VsockPort)
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

// dialUpstream calls the injected vsock Dialer and makes the call
// cancellable via the server's dialCtx. The Dialer interface is
// intentionally context-free (the Linux vsock syscall is synchronous), so
// we run the dial in a goroutine and race it against dialCtx. If dialCtx
// wins, a cleanup goroutine closes the conn once the dial eventually
// returns — bounded by vsock's own connect timeout, after which the
// worker-goroutine count returns to baseline.
func (l *listener) dialUpstream(cid, port uint32) (net.Conn, error) {
	type result struct {
		c   net.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := l.server.dialer.Dial(cid, port)
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-l.server.dialCtx.Done():
		go func() {
			r := <-ch
			if r.err == nil {
				_ = r.c.Close()
			}
		}()
		return nil, l.server.dialCtx.Err()
	}
}

func (l *listener) sniff(r io.Reader) (string, []byte, error) {
	switch l.mode {
	case config.ModeHTTPHost:
		return SniffHost(r)
	case config.ModeTLSSNI:
		return SniffSNI(r)
	default:
		return "", nil, fmt.Errorf("unknown mode %q", l.mode)
	}
}

// proxy runs the bidirectional copy between client and upstream. When one
// direction ends we only half-close the destination (CloseWrite) so the
// reverse direction can still finish — e.g. a client that finished sending
// but is still expecting response bytes. Full teardown happens via the
// outer defer'd Close once both goroutines have returned.
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
		halfCloseWrite(upstream)
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
		halfCloseWrite(client)
	}()
	wg.Wait()
}

// halfCloseWrite shuts down the write side of c when the underlying type
// supports it (TCP, vsock), otherwise falls back to a full Close. Used by
// the proxy paths to preserve half-close semantics across the tunnel.
func halfCloseWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

func isExpectedCopyErr(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe)
}
