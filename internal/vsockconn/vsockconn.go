// Package vsockconn abstracts AF_VSOCK dial and accept operations so the
// rest of vsockd can be written against a single interface regardless of
// backend. Two backends are provided:
//
//   - The real vsock backend (Linux only at runtime), built on
//     github.com/mdlayher/vsock.
//   - A loopback backend that proxies connections through a registry-backed
//     127.0.0.1 TCP listener. Used by unit and integration tests on
//     platforms without AF_VSOCK support.
//
// Backend selection is a wiring-time decision made by the caller. A small
// helper, UseLoopback, reads VSOCKD_BACKEND so tests can flip the default
// without threading a flag through every constructor.
package vsockconn

import (
	"net"
	"os"
)

// Conn is a net.Conn whose peer is identified by a vsock context ID.
// The outbound listener uses PeerCID to authorize the connection before
// reading any bytes.
type Conn interface {
	net.Conn
	// PeerCID returns the remote endpoint's vsock context ID. For the real
	// vsock backend this is the kernel-reported peer CID (trusted — set by
	// the Nitro hypervisor). For the loopback backend it is the value the
	// dialing side passed to NewLoopbackDialer.
	PeerCID() uint32
	// PeerPort returns the remote endpoint's vsock port. For the real
	// vsock backend this is the kernel-assigned source port. For the
	// loopback backend it is 0 (no meaningful vsock port is carried over
	// a TCP emulation of vsock).
	PeerPort() uint32
}

// Dialer opens a connection to a (cid, port) pair. The inbound server uses
// it to reach enclaves after sniffing the target hostname.
type Dialer interface {
	Dial(cid, port uint32) (net.Conn, error)
}

// Listener accepts inbound vsock-style connections and exposes the peer CID
// on each accepted connection. The outbound server uses it on its single
// vsock port per listener.
type Listener interface {
	Accept() (Conn, error)
	Close() error
	Addr() net.Addr
}

// BackendLoopback is the VSOCKD_BACKEND value that selects the loopback
// backend. Any other value (including the empty string) leaves vsock as the
// default. The env var is intended strictly for tests and local development.
const BackendLoopback = "loopback"

// BackendEnv is the environment variable inspected by UseLoopback.
const BackendEnv = "VSOCKD_BACKEND"

// UseLoopback reports whether the loopback backend has been requested via
// the VSOCKD_BACKEND environment variable.
func UseLoopback() bool {
	return os.Getenv(BackendEnv) == BackendLoopback
}
