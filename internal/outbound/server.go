// Package outbound implements the vsock-facing forward HTTP proxy used by
// enclaves to reach external destinations.
//
// Each configured outbound port listens on vsock. Every accepted connection
// is authorized by peer CID before any bytes are read; unauthorized peers
// are dropped and counted as "denied". Authorized peers get exactly one
// HTTP request parsed — CONNECT (HTTPS tunnel) or absolute-URI GET/POST
// (plain HTTP) — whose destination is matched against that CID's egress
// allowlist. Allow → dial TCP, proxy the request/tunnel. Deny → 403 and
// close.
package outbound

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/olomix/vsockd/internal/allowlist"
	"github.com/olomix/vsockd/internal/config"
	"github.com/olomix/vsockd/internal/metrics"
	"github.com/olomix/vsockd/internal/vsockconn"
)

// Timeouts are deliberately generous; the enclave is trusted by CID, so the
// goal is to prevent indefinite hangs rather than to police peer behaviour.
const (
	headerReadTimeout   = 30 * time.Second
	upstreamDialTimeout = 30 * time.Second
	statusWriteTimeout  = 5 * time.Second
)

// ListenFunc opens a vsock-style Listener on the given port. Production
// code passes vsockconn.ListenVsock; tests inject a loopback-backed
// variant to run without AF_VSOCK.
type ListenFunc func(port uint32) (vsockconn.Listener, error)

// Server accepts vsock connections from enclaves and proxies their
// HTTP/HTTPS forward-proxy requests to allowed TCP destinations.
type Server struct {
	listenFunc ListenFunc
	dialer     *net.Dialer
	metric     *metrics.Metrics
	logger     *slog.Logger

	// mu guards the listeners slice plus the accept-loop context.
	mu        sync.Mutex
	listeners []*listener
	ctx       context.Context

	// dialCtx is derived from ctx in Start and cancelled by Shutdown when
	// the grace window elapses. In-flight upstream DialContext calls watch
	// it so a stuck dial cannot hold the process past shutdown_grace.
	dialCtx    context.Context
	cancelDial context.CancelFunc

	// activeConn tracks every connection (vsock accept side plus any
	// upstream TCP dial) so Shutdown can force-close them when its grace
	// deadline expires.
	connMu     sync.Mutex
	activeConn map[net.Conn]struct{}

	wg sync.WaitGroup
}

// NewServer wires a Server from the outbound config. Listeners are not
// bound yet; Start performs the bind. Returns an error if any per-listener
// configuration is inconsistent (for example, a duplicate CID on the same
// port — cross-port duplicates are caught earlier in config.Validate).
func NewServer(
	cfgs []config.OutboundListener,
	listenFn ListenFunc,
	m *metrics.Metrics,
	logger *slog.Logger,
) (*Server, error) {
	if listenFn == nil {
		return nil, errors.New("outbound: listen func required")
	}
	if m == nil {
		return nil, errors.New("outbound: metrics required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		listenFunc: listenFn,
		dialer:     &net.Dialer{Timeout: upstreamDialTimeout},
		metric:     m,
		logger:     logger,
		activeConn: map[net.Conn]struct{}{},
	}
	for i := range cfgs {
		ln, err := newListener(cfgs[i], s)
		if err != nil {
			return nil, fmt.Errorf("outbound[%d]: %w", i, err)
		}
		s.listeners = append(s.listeners, ln)
	}
	return s, nil
}

// Addr returns the bound address of the i-th configured listener, or nil
// if the index is out of range or Start has not yet been called.
func (s *Server) Addr(i int) net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.listeners) || s.listeners[i].ln == nil {
		return nil
	}
	return s.listeners[i].ln.Addr()
}

// ListenerPorts returns the set of currently-active outbound vsock ports in
// their configured order. Exposed for tests and reload sanity checks.
func (s *Server) ListenerPorts() []uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uint32, 0, len(s.listeners))
	for _, ln := range s.listeners {
		out = append(out, ln.cfg.Port)
	}
	return out
}

// Start binds every listener and launches an accept loop per listener. If
// any bind fails, listeners already bound are closed and the error is
// returned to the caller.
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

// ApplyPlan is the pending outbound reload diff produced by PrepareApply.
// Callers must resolve it via CommitApply or AbortApply exactly once. The
// split mirrors inbound.ApplyPlan so app.Reload can stage both subsystems
// before committing either.
type ApplyPlan struct {
	server   *Server
	next     []*listener
	newBinds []*listener
	swaps    []applySwap
	existing map[uint32]*listener
	kept     map[uint32]bool

	committed bool
	aborted   bool
}

type applySwap struct {
	listener *listener
	matchers map[uint32]*allowlist.Matcher
}

// PrepareApply validates the new outbound configuration, binds any newly
// added listeners, and records the diff without disturbing the running
// state. Returns an ApplyPlan that the caller must resolve via CommitApply
// or AbortApply. Cross-port CID uniqueness is assumed to have been checked
// by config.Validate before this is called.
func (s *Server) PrepareApply(cfgs []config.OutboundListener) (*ApplyPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ctx == nil {
		return nil, errors.New("outbound: PrepareApply called before Start")
	}

	existing := make(map[uint32]*listener, len(s.listeners))
	for _, ln := range s.listeners {
		existing[ln.cfg.Port] = ln
	}

	kept := make(map[uint32]bool, len(cfgs))
	next := make([]*listener, 0, len(cfgs))
	var swaps []applySwap
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
			return nil, fmt.Errorf("outbound[%d]: %w", i, err)
		}
		if cur, ok := existing[cfg.Port]; ok {
			// Port alone keys the listener; updating cur.cfg would race
			// with the accept-loop reader. Swap only the matcher table.
			swaps = append(swaps, applySwap{cur, newLn.matchersSnapshot()})
			kept[cfg.Port] = true
			next = append(next, cur)
			continue
		}
		if err := newLn.bind(); err != nil {
			cleanup()
			return nil, err
		}
		newBinds = append(newBinds, newLn)
		next = append(next, newLn)
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

// CommitApply publishes the diff built by PrepareApply. This phase cannot
// fail and is idempotent.
func (p *ApplyPlan) CommitApply() {
	if p == nil || p.committed || p.aborted {
		return
	}
	s := p.server
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, sw := range p.swaps {
		sw.listener.replaceMatchers(sw.matchers)
	}
	for _, ln := range p.newBinds {
		s.wg.Add(1)
		go func(l *listener) {
			defer s.wg.Done()
			l.run(s.ctx)
		}(ln)
	}
	for port, ln := range p.existing {
		if !p.kept[port] {
			ln.close()
		}
	}
	s.listeners = p.next
	p.committed = true
}

// AbortApply releases any listeners bound by PrepareApply without touching
// running state. Safe to call on a nil plan and idempotent.
func (p *ApplyPlan) AbortApply() {
	if p == nil || p.committed || p.aborted {
		return
	}
	for _, ln := range p.newBinds {
		ln.close()
	}
	p.aborted = true
}

// Apply is a convenience wrapper that prepares and commits in a single
// call. Use PrepareApply/CommitApply when coordinating with other
// subsystems to keep the reload atomic across them.
func (s *Server) Apply(cfgs []config.OutboundListener) error {
	plan, err := s.PrepareApply(cfgs)
	if err != nil {
		return err
	}
	plan.CommitApply()
	return nil
}

// Shutdown stops every accept loop and waits for in-flight proxy
// goroutines to drain. If ctx expires first, remaining tracked connections
// are force-closed, any still-pending upstream DialContext calls are
// cancelled via dialCtx, and ctx.Err() is returned.
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

// listener owns one vsock accept loop for a single OutboundListener.
type listener struct {
	cfg    config.OutboundListener
	server *Server

	ln vsockconn.Listener

	// matchers holds the authorized-CID → compiled allowlist table. Stored
	// as an atomic pointer so Apply can swap the whole table at reload
	// time without disturbing in-flight connections. Only populated for
	// legacy HTTP-mode listeners (cfg.Mode == "").
	matchers atomic.Pointer[map[uint32]*allowlist.Matcher]

	// upstream holds the live TCP upstream address for mode=tcp listeners.
	// Atomic so SIGHUP reloads can swap the destination without tearing
	// down the vsock accept loop. Unused for HTTP-mode listeners.
	upstream atomic.Pointer[string]
}

func newListener(cfg config.OutboundListener, s *Server) (*listener, error) {
	l := &listener{cfg: cfg, server: s}
	if cfg.Mode == config.ModeTCP {
		u := cfg.Upstream
		l.upstream.Store(&u)
		return l, nil
	}
	if len(cfg.CIDs) == 0 {
		return nil, fmt.Errorf("port %d: no cids configured", cfg.Port)
	}
	matchers := make(map[uint32]*allowlist.Matcher, len(cfg.CIDs))
	for _, oc := range cfg.CIDs {
		if _, dup := matchers[oc.CID]; dup {
			return nil, fmt.Errorf(
				"port %d: duplicate cid %d", cfg.Port, oc.CID)
		}
		m, err := allowlist.New(oc.AllowedHosts)
		if err != nil {
			return nil, fmt.Errorf(
				"port %d cid %d: %w", cfg.Port, oc.CID, err)
		}
		matchers[oc.CID] = m
	}
	l.matchers.Store(&matchers)
	return l, nil
}

func (l *listener) matchersSnapshot() map[uint32]*allowlist.Matcher {
	p := l.matchers.Load()
	if p == nil {
		return nil
	}
	return *p
}

func (l *listener) replaceMatchers(m map[uint32]*allowlist.Matcher) {
	l.matchers.Store(&m)
}

// upstreamSnapshot returns the currently configured upstream host:port
// for a TCP-mode listener. Safe to call concurrently with reloads: the
// atomic pointer guarantees readers see one self-consistent value.
func (l *listener) upstreamSnapshot() string {
	p := l.upstream.Load()
	if p == nil {
		return ""
	}
	return *p
}

func (l *listener) bind() error {
	ln, err := l.server.listenFunc(l.cfg.Port)
	if err != nil {
		return fmt.Errorf("listen vsock port %d: %w", l.cfg.Port, err)
	}
	l.ln = ln
	return nil
}

func (l *listener) close() {
	if l.ln != nil {
		_ = l.ln.Close()
	}
}

func (l *listener) run(ctx context.Context) {
	// Match net/http.Server.Serve's back-off on temporary Accept errors:
	// under FD exhaustion (EMFILE/ENFILE) the loop would otherwise pin a
	// CPU core and flood the log with warn-level events.
	var tempDelay time.Duration
	for {
		c, err := l.ln.Accept()
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
			l.server.logger.Warn("outbound accept error",
				"port", l.cfg.Port, "err", err, "retry_in", tempDelay)
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
			if l.cfg.Mode == config.ModeTCP {
				l.handleTCP(ctx, c)
				return
			}
			l.handle(ctx, c)
		}()
	}
}

func (l *listener) handle(ctx context.Context, c vsockconn.Conn) {
	defer c.Close()
	l.server.trackConn(c)
	defer l.server.untrackConn(c)

	cid := c.PeerCID()
	matchers := l.matchersSnapshot()
	matcher, ok := matchers[cid]
	if !ok {
		// Arbitrary peer CIDs must never become metric labels: unauthorized
		// sources would otherwise let the cardinality grow without bound.
		l.server.metric.OutboundConnections.
			WithLabelValues(metrics.CIDLabelUnauthorized, metrics.OutboundResultDenied).Inc()
		l.server.logger.Warn("outbound peer cid not authorized for port",
			"port", l.cfg.Port, "cid", cid)
		return
	}
	cidLabel := metrics.FormatCID(cid)

	_ = c.SetReadDeadline(time.Now().Add(headerReadTimeout))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		writeStatusLine(c, http.StatusBadRequest, "Bad Request")
		l.server.metric.OutboundConnections.
			WithLabelValues(cidLabel, metrics.OutboundResultError).Inc()
		l.server.logger.Warn("outbound bad request",
			"port", l.cfg.Port, "cid", cid, "err", err)
		return
	}

	switch {
	case req.Method == http.MethodConnect:
		l.handleConnect(c, br, req, matcher, cid, cidLabel)
	case req.URL != nil && req.URL.IsAbs() && req.URL.Host != "":
		l.handleProxy(c, req, matcher, cid, cidLabel)
	default:
		writeStatusLine(c, http.StatusBadRequest, "Bad Request")
		l.server.metric.OutboundConnections.
			WithLabelValues(cidLabel, metrics.OutboundResultError).Inc()
		l.server.logger.Warn("outbound non-proxy request",
			"port", l.cfg.Port, "cid", cid,
			"method", req.Method, "requestURI", req.RequestURI)
	}
}

func (l *listener) handleConnect(
	c vsockconn.Conn,
	br *bufio.Reader,
	req *http.Request,
	matcher *allowlist.Matcher,
	cid uint32,
	cidLabel string,
) {
	host, portStr, err := net.SplitHostPort(req.URL.Host)
	if err != nil {
		writeStatusLine(c, http.StatusBadRequest, "Bad Request")
		l.server.metric.OutboundConnections.
			WithLabelValues(cidLabel, metrics.OutboundResultError).Inc()
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		writeStatusLine(c, http.StatusBadRequest, "Bad Request")
		l.server.metric.OutboundConnections.
			WithLabelValues(cidLabel, metrics.OutboundResultError).Inc()
		return
	}

	if !matcher.Allow(host, port) {
		writeStatusLine(c, http.StatusForbidden, "Forbidden")
		l.server.metric.OutboundConnections.
			WithLabelValues(cidLabel, metrics.OutboundResultDenied).Inc()
		l.server.logger.Warn("outbound CONNECT denied",
			"cid", cid, "host", host, "port", port)
		return
	}

	// Derive from dialCtx, not the accept-loop ctx, so Shutdown's grace
	// window can cancel a stuck TCP dial instead of waiting it out.
	dctx, cancel := context.WithTimeout(l.server.dialCtx, upstreamDialTimeout)
	defer cancel()
	upstream, err := l.server.dialer.DialContext(
		dctx, "tcp", net.JoinHostPort(host, portStr))
	if err != nil {
		writeStatusLine(c, http.StatusBadGateway, "Bad Gateway")
		l.server.metric.OutboundConnections.
			WithLabelValues(cidLabel, metrics.OutboundResultError).Inc()
		l.server.logger.Warn("outbound CONNECT dial failed",
			"cid", cid, "host", host, "port", port, "err", err)
		return
	}
	defer upstream.Close()
	l.server.trackConn(upstream)
	defer l.server.untrackConn(upstream)

	if _, err := c.Write([]byte(
		"HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		l.server.metric.OutboundConnections.
			WithLabelValues(cidLabel, metrics.OutboundResultError).Inc()
		return
	}

	l.server.metric.OutboundConnections.
		WithLabelValues(cidLabel, metrics.OutboundResultAllowed).Inc()

	// bufio may have already buffered bytes the enclave pipelined after
	// its CONNECT line (typically the tunneled TLS ClientHello). Drain
	// them before switching to raw copy or those bytes would be lost.
	clientReader := mergeBuffered(br, c)
	l.tunnel(clientReader, c, upstream, cidLabel)
}

func (l *listener) handleProxy(
	c vsockconn.Conn,
	req *http.Request,
	matcher *allowlist.Matcher,
	cid uint32,
	cidLabel string,
) {
	defer req.Body.Close()

	if req.URL.Scheme != "http" {
		// Plain-HTTP forward proxying only. HTTPS uses CONNECT; an
		// "https://" absolute-URI request on this proxy is a client bug.
		writeStatusLine(c, http.StatusBadRequest, "Bad Request")
		l.server.metric.OutboundConnections.
			WithLabelValues(cidLabel, metrics.OutboundResultError).Inc()
		return
	}

	host := req.URL.Hostname()
	portStr := req.URL.Port()
	if portStr == "" {
		portStr = "80"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		writeStatusLine(c, http.StatusBadRequest, "Bad Request")
		l.server.metric.OutboundConnections.
			WithLabelValues(cidLabel, metrics.OutboundResultError).Inc()
		return
	}

	if !matcher.Allow(host, port) {
		writeStatusLine(c, http.StatusForbidden, "Forbidden")
		l.server.metric.OutboundConnections.
			WithLabelValues(cidLabel, metrics.OutboundResultDenied).Inc()
		l.server.logger.Warn("outbound proxy denied",
			"cid", cid, "host", host, "port", port)
		return
	}

	// Same rationale as handleConnect: derive from dialCtx so Shutdown can
	// abort a stuck upstream dial when its grace window elapses.
	dctx, cancel := context.WithTimeout(l.server.dialCtx, upstreamDialTimeout)
	defer cancel()
	upstream, err := l.server.dialer.DialContext(
		dctx, "tcp", net.JoinHostPort(host, portStr))
	if err != nil {
		writeStatusLine(c, http.StatusBadGateway, "Bad Gateway")
		l.server.metric.OutboundConnections.
			WithLabelValues(cidLabel, metrics.OutboundResultError).Inc()
		l.server.logger.Warn("outbound proxy dial failed",
			"cid", cid, "host", host, "port", portStr, "err", err)
		return
	}
	defer upstream.Close()
	l.server.trackConn(upstream)
	defer l.server.untrackConn(upstream)

	// Rewrite to origin form: strip Scheme/Host from URL so req.Write
	// emits a relative request-URI. Replace req.Host with req.URL.Host
	// unconditionally: RFC 7230 §5.4 requires a proxy handling an
	// absolute-form request-target to ignore any received Host header and
	// substitute the host from the request-target. This also keeps the
	// upstream Host header aligned with the allowlisted hostname instead
	// of letting the enclave target a different vhost on the same IP.
	req.Host = req.URL.Host
	req.URL = &url.URL{
		Path:     req.URL.Path,
		RawPath:  req.URL.RawPath,
		RawQuery: req.URL.RawQuery,
	}
	req.RequestURI = ""
	stripHopByHop(req.Header)
	// One request per vsock connection; forcing close simplifies response
	// framing (no need to re-read after the first response body).
	req.Close = true

	// Each connection contributes exactly one outbound_connections_total
	// event: "allowed" on full success, "error" on any mid-request failure.
	// The counter is incremented at the sole success return below to match
	// the CONNECT path's semantics.
	upCount := &countingWriter{W: upstream}
	if err := req.Write(upCount); err != nil {
		l.server.metric.OutboundConnections.
			WithLabelValues(cidLabel, metrics.OutboundResultError).Inc()
		l.server.metric.OutboundBytes.
			WithLabelValues(cidLabel, metrics.DirectionOut).
			Add(float64(upCount.N))
		return
	}
	l.server.metric.OutboundBytes.
		WithLabelValues(cidLabel, metrics.DirectionOut).
		Add(float64(upCount.N))

	respBR := bufio.NewReader(upstream)
	resp, err := http.ReadResponse(respBR, req)
	if err != nil {
		l.server.metric.OutboundConnections.
			WithLabelValues(cidLabel, metrics.OutboundResultError).Inc()
		return
	}
	defer resp.Body.Close()
	stripHopByHop(resp.Header)
	resp.Close = true

	downCount := &countingWriter{W: c}
	werr := resp.Write(downCount)
	l.server.metric.OutboundBytes.
		WithLabelValues(cidLabel, metrics.DirectionIn).
		Add(float64(downCount.N))
	if werr != nil {
		l.server.metric.OutboundConnections.
			WithLabelValues(cidLabel, metrics.OutboundResultError).Inc()
		return
	}
	l.server.metric.OutboundConnections.
		WithLabelValues(cidLabel, metrics.OutboundResultAllowed).Inc()
}

// tunnel runs the bidirectional copy for a CONNECT tunnel. When one
// direction reaches EOF we only half-close the destination (CloseWrite),
// letting the reverse direction finish — a full Close here would truncate
// response bytes still in flight on the other side of the tunnel. The
// outer defer'd Close tears everything down after both directions return.
//
// Byte directions are recorded from the enclave's perspective:
//   - DirectionOut: bytes leaving the enclave (client → upstream)
//   - DirectionIn:  bytes arriving at the enclave (upstream → client)
func (l *listener) tunnel(
	clientReader io.Reader,
	client net.Conn,
	upstream net.Conn,
	cidLabel string,
) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, _ := io.Copy(upstream, clientReader)
		l.server.metric.OutboundBytes.
			WithLabelValues(cidLabel, metrics.DirectionOut).
			Add(float64(n))
		halfCloseWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		n, _ := io.Copy(client, upstream)
		l.server.metric.OutboundBytes.
			WithLabelValues(cidLabel, metrics.DirectionIn).
			Add(float64(n))
		halfCloseWrite(client)
	}()
	wg.Wait()
}

// halfCloseWrite shuts down the write side of c when the underlying type
// supports it (TCP, vsock), otherwise falls back to a full Close. Used by
// the tunnel to preserve half-close semantics across a CONNECT passthrough.
func halfCloseWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// mergeBuffered returns a reader that first yields any bytes already
// consumed from c into br's internal buffer, then falls through to c
// directly. Needed after http.ReadRequest in the CONNECT path: the
// enclave may pipeline the TLS ClientHello right after its CONNECT
// request line, and those bytes now sit inside br — not c.
func mergeBuffered(br *bufio.Reader, c net.Conn) io.Reader {
	n := br.Buffered()
	if n == 0 {
		return c
	}
	buf := make([]byte, n)
	_, _ = io.ReadFull(br, buf)
	return io.MultiReader(bytes.NewReader(buf), c)
}

// hop-by-hop headers listed in RFC 7230 §6.1. Forward proxies must not
// propagate these to either side of the hop.
var hopByHop = []string{
	"Proxy-Connection",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Keep-Alive",
	"TE",
	"Trailer",
	"Upgrade",
}

// stripHopByHop removes per-hop headers from h and also honours Connection:
// the token list in Connection names additional hop-scoped headers.
func stripHopByHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, f := range strings.Split(v, ",") {
			if name := strings.TrimSpace(f); name != "" {
				h.Del(name)
			}
		}
	}
	h.Del("Connection")
	for _, name := range hopByHop {
		h.Del(name)
	}
}

// writeStatusLine writes a minimal HTTP/1.1 status response. Used for 400,
// 403, 502 responses; any write error is ignored because we are about to
// close the connection anyway.
func writeStatusLine(c net.Conn, code int, text string) {
	_ = c.SetWriteDeadline(time.Now().Add(statusWriteTimeout))
	line := fmt.Sprintf(
		"HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Length: 0\r\n\r\n",
		code, text)
	_, _ = c.Write([]byte(line))
	_ = c.SetWriteDeadline(time.Time{})
}

// countingWriter wraps an io.Writer and accumulates the total number of
// bytes successfully written, so the proxy can emit byte-count metrics
// without re-buffering the payload.
type countingWriter struct {
	W io.Writer
	N int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.W.Write(p)
	w.N += int64(n)
	return n, err
}
