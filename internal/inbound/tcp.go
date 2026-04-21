package inbound

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/olomix/vsockd/internal/metrics"
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

// handleTCP proxies bytes between a TCP peer and a fixed vsock target.
// Unlike the http-host / tls-sni inbound paths there is no sniff step,
// no route table, and no per-hostname policy: mode=tcp listeners trust
// every TCP connection their bound address accepts and always forward
// to the configured (target_cid, target_port).
//
// Byte counting is symmetric: the two returned totals are summed for the
// debug close log's total_bytes attr — the proxy has no directional
// semantics to preserve here.
func (l *listener) handleTCP(ctx context.Context, c net.Conn) {
	defer c.Close()
	l.server.trackConn(c)
	defer l.server.untrackConn(c)

	remoteAddr := c.RemoteAddr().String()
	// Use the actually-bound address rather than l.addr(): port may have
	// been configured as 0 (ephemeral) and tests/operators want to see
	// the real endpoint in the log.
	listenAddr := l.tcp.Addr().String()
	targetCID := l.targetCID.Load()
	targetPort := l.targetPort.Load()

	l.server.metric.TCPInboundConnections.Inc()
	l.server.logger.Debug("inbound tcp connection",
		"remote", remoteAddr,
		"listen", listenAddr)

	upstream, err := l.dialUpstream(targetCID, targetPort)
	if err != nil {
		l.server.metric.TCPInboundErrors.
			WithLabelValues(metrics.TCPErrorDial).Inc()
		l.server.logger.Warn("inbound tcp dial failed",
			"remote", remoteAddr,
			"listen", listenAddr,
			"target_cid", targetCID,
			"target_port", targetPort,
			"err", err)
		l.server.logger.Debug("tcp connection closed",
			"remote", remoteAddr,
			"listen", listenAddr,
			"total_bytes", int64(0))
		return
	}
	defer upstream.Close()
	l.server.trackConn(upstream)
	defer l.server.untrackConn(upstream)

	upBytes, downBytes, copyErrored := shuttleTCP(c, upstream)

	l.server.metric.TCPInboundBytes.
		WithLabelValues(metrics.DirectionUp).Add(float64(upBytes))
	l.server.metric.TCPInboundBytes.
		WithLabelValues(metrics.DirectionDown).Add(float64(downBytes))
	if copyErrored {
		l.server.metric.TCPInboundErrors.
			WithLabelValues(metrics.TCPErrorCopy).Inc()
	}

	l.server.logger.Debug("tcp connection closed",
		"remote", remoteAddr,
		"listen", listenAddr,
		"total_bytes", upBytes+downBytes)
}

// shuttleTCP runs a bidirectional io.Copy between client (TCP side) and
// upstream (vsock side). Returns (up, down, copyErrored) where up is
// bytes copied from client→upstream, down is bytes copied from
// upstream→client, and copyErrored reports whether either direction
// ended with an error beyond the expected EOF / local-close set.
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
	copyErrored := isUnexpectedCopyErr(first.err) ||
		isUnexpectedCopyErr(second.err)
	return up, dn, copyErrored
}

// isUnexpectedCopyErr inverts the shared isExpectedCopyErr helper so the
// TCP handler can read naturally: "did this direction end with an error
// worth counting?". EOF and locally-initiated close are filtered out.
func isUnexpectedCopyErr(err error) bool {
	return err != nil && !isExpectedCopyErr(err)
}
