package outbound

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"github.com/olomix/vsockd/internal/metrics"
	"github.com/olomix/vsockd/internal/vsockconn"
)

// shuttleDrainTimeout bounds how long shuttleTCP waits for the reverse
// direction to finish after the first direction has closed cleanly. On
// error-driven close the reverse side is force-closed immediately; this
// timer is only needed when one side half-closed and the peer never
// reacted. 30s matches conventional TCP keepalive/FIN idle windows and
// keeps a wedged peer from leaking a goroutine indefinitely.
//
// Package-level so tests can shrink it to exercise the force-close path
// without stalling the suite.
var shuttleDrainTimeout = 30 * time.Second

// handleTCP proxies bytes between a vsock peer and a fixed TCP upstream.
// Unlike the HTTP forward-proxy path there is no application-layer parse,
// no allowlist, and no per-peer authorization: TCP-mode listeners trust
// every connection the vsock port accepts.
//
// upstreamAddr is captured by the accept loop immediately after
// Accept returns, before the handler goroutine starts, so a reload
// that lands after the capture never redirects this connection.
// The converse window — a reload landing in the narrow handful of
// instructions between Accept returning and the capture — is not
// closed; doing so would require serialising reloads against a
// blocking Accept.
//
// Byte counting follows the enclave's perspective:
//   - up is bytes leaving the enclave (vsock client → TCP upstream)
//   - down is bytes arriving at the enclave (TCP upstream → vsock client)
func (l *listener) handleTCP(ctx context.Context, c vsockconn.Conn, upstreamAddr string) {
	defer c.Close()
	l.server.trackConn(c)
	defer l.server.untrackConn(c)

	peerCID := c.PeerCID()
	peerPort := c.PeerPort()
	listenPort := l.port

	l.server.metric.VsockToTCPConnections.Inc()
	l.server.logger.Debug("inbound vsock connection",
		"cid", peerCID,
		"port", peerPort,
		"listen_port", listenPort)

	// Derive from dialCtx so Shutdown's grace window can cancel a stuck
	// upstream dial instead of waiting it out, matching the HTTP paths.
	dctx, cancel := context.WithTimeout(l.server.dialCtx, upstreamDialTimeout)
	defer cancel()
	upstream, err := l.server.dialer.DialContext(dctx, "tcp", upstreamAddr)
	if err != nil {
		// A dial cancelled by Shutdown (dialCtx done) is an expected teardown
		// signal, not an upstream failure: skip the dial_fail metric and the
		// warn-level log so shutdown does not look like an incident.
		shutdownCancel := l.server.dialCtx.Err() != nil
		if !shutdownCancel {
			l.server.metric.VsockToTCPErrors.
				WithLabelValues(metrics.TCPErrorDial).Inc()
			l.server.logger.Warn("outbound tcp dial failed",
				"cid", peerCID,
				"port", peerPort,
				"listen_port", listenPort,
				"upstream", upstreamAddr,
				"err", err)
		}
		l.server.logger.Debug("vsock connection closed",
			"cid", peerCID,
			"port", peerPort,
			"listen_port", listenPort,
			"total_bytes", int64(0))
		return
	}
	defer upstream.Close()
	l.server.trackConn(upstream)
	defer l.server.untrackConn(upstream)

	upBytes, downBytes, copyErrored := shuttleTCP(c, upstream)

	l.server.metric.VsockToTCPBytes.
		WithLabelValues(metrics.DirectionUp).Add(float64(upBytes))
	l.server.metric.VsockToTCPBytes.
		WithLabelValues(metrics.DirectionDown).Add(float64(downBytes))
	if copyErrored {
		l.server.metric.VsockToTCPErrors.
			WithLabelValues(metrics.TCPErrorCopy).Inc()
	}

	l.server.logger.Debug("vsock connection closed",
		"cid", peerCID,
		"port", peerPort,
		"listen_port", listenPort,
		"total_bytes", upBytes+downBytes)
}

// shuttleTCP runs a bidirectional io.Copy between client (vsock side) and
// upstream (TCP side). Returns (up, down, copyErrored) where up is bytes
// copied from client→upstream, down is bytes copied from upstream→client,
// and copyErrored reports whether either direction ended with an error
// beyond the expected EOF / local-close set.
//
// Close semantics:
//   - First direction returns with nil error (clean EOF / half-close
//     from the source): CloseWrite the sink so the peer observes an
//     orderly EOF on its own read half while the reverse direction
//     keeps flowing. Bounded by shuttleDrainTimeout so a silent peer
//     cannot park the handler indefinitely.
//   - First direction returns with non-nil error (reset, local Close,
//     context cancel): full Close on both ends so the reverse
//     direction unblocks immediately rather than waiting on a peer
//     that may never react.
//
// The outer handler's defer'd Close tears the connections down once
// both directions have returned; calling Close() here first is
// idempotent.
func shuttleTCP(client net.Conn, upstream net.Conn) (int64, int64, bool) {
	type result struct {
		which int
		err   error
	}
	var (
		up int64
		dn int64
	)
	done := make(chan result, 2)

	go func() {
		n, err := io.Copy(upstream, client)
		up = n
		done <- result{which: 1, err: err}
	}()
	go func() {
		n, err := io.Copy(client, upstream)
		dn = n
		done <- result{which: 2, err: err}
	}()

	first := <-done
	if first.err == nil {
		switch first.which {
		case 1:
			halfCloseWrite(upstream)
		case 2:
			halfCloseWrite(client)
		}
	} else {
		_ = client.Close()
		_ = upstream.Close()
	}

	var second result
	select {
	case second = <-done:
	case <-time.After(shuttleDrainTimeout):
		_ = client.Close()
		_ = upstream.Close()
		second = <-done
	}
	copyErrored := isCopyError(first.err) || isCopyError(second.err)
	return up, dn, copyErrored
}

// isCopyError reports whether err from io.Copy represents something the
// operator would want to count as a failure, as opposed to the normal
// stream-end signals (EOF, a Close we initiated ourselves, closed pipe).
func isCopyError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe) {
		return false
	}
	return true
}
