package vsockconn

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// ErrConnRefused is returned by the loopback dialer when the requested
// (cid, port) has no listener registered. It mirrors the behaviour of a
// real vsock connect against an unbound port.
var ErrConnRefused = errors.New("vsockconn: loopback target not registered")

// peerCIDHeaderLen is the byte size of the per-connection handshake the
// loopback dialer writes so that the listener's Accept can report the
// dialing side's CID. The real vsock backend gets this from the kernel.
const peerCIDHeaderLen = 4

// Registry maps (cid, port) to the TCP address of a backing loopback
// listener. It lets a single test process host multiple fake enclaves and
// host endpoints without colliding on real vsock ports.
type Registry struct {
	mu       sync.RWMutex
	bindings map[registryKey]net.Addr
}

type registryKey struct{ cid, port uint32 }

// NewRegistry returns an empty, ready-to-use Registry.
func NewRegistry() *Registry {
	return &Registry{bindings: make(map[registryKey]net.Addr)}
}

func (r *Registry) register(cid, port uint32, addr net.Addr) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := registryKey{cid: cid, port: port}
	if _, ok := r.bindings[k]; ok {
		return fmt.Errorf(
			"vsockconn: (cid=%d,port=%d) already registered", cid, port)
	}
	r.bindings[k] = addr
	return nil
}

func (r *Registry) unregister(cid, port uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.bindings, registryKey{cid: cid, port: port})
}

func (r *Registry) lookup(cid, port uint32) (net.Addr, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.bindings[registryKey{cid: cid, port: port}]
	return a, ok
}

// loopbackConn wraps an accepted TCP connection and carries the peer CID
// parsed out of the loopback handshake header.
type loopbackConn struct {
	net.Conn
	peerCID uint32
}

func (c *loopbackConn) PeerCID() uint32 { return c.peerCID }

type loopbackDialer struct {
	registry  *Registry
	sourceCID uint32
}

// NewLoopbackDialer returns a Dialer that reaches listeners registered in
// reg. Each Dial writes a 4-byte big-endian sourceCID header to the
// accepted TCP connection so the peer's Accept can return PeerCID.
func NewLoopbackDialer(reg *Registry, sourceCID uint32) Dialer {
	return &loopbackDialer{registry: reg, sourceCID: sourceCID}
}

func (d *loopbackDialer) Dial(cid, port uint32) (net.Conn, error) {
	addr, ok := d.registry.lookup(cid, port)
	if !ok {
		return nil, fmt.Errorf(
			"vsockconn: dial cid=%d port=%d: %w", cid, port, ErrConnRefused)
	}
	c, err := net.Dial(addr.Network(), addr.String())
	if err != nil {
		return nil, err
	}
	var hdr [peerCIDHeaderLen]byte
	binary.BigEndian.PutUint32(hdr[:], d.sourceCID)
	if _, err := c.Write(hdr[:]); err != nil {
		c.Close()
		return nil, fmt.Errorf("vsockconn: loopback handshake: %w", err)
	}
	return c, nil
}

type loopbackListener struct {
	cid, port uint32
	tcp       net.Listener
	registry  *Registry
}

// ListenLoopback binds a TCP listener on 127.0.0.1 and registers it under
// (cid, port) in reg. The listener's Accept reads the handshake header so
// callers get the peer CID without any extra plumbing.
func ListenLoopback(reg *Registry, cid, port uint32) (Listener, error) {
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	if err := reg.register(cid, port, tcp.Addr()); err != nil {
		tcp.Close()
		return nil, err
	}
	return &loopbackListener{cid: cid, port: port, tcp: tcp, registry: reg}, nil
}

func (l *loopbackListener) Accept() (Conn, error) {
	c, err := l.tcp.Accept()
	if err != nil {
		return nil, err
	}
	var hdr [peerCIDHeaderLen]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		c.Close()
		return nil, fmt.Errorf("vsockconn: loopback handshake read: %w", err)
	}
	return &loopbackConn{
		Conn:    c,
		peerCID: binary.BigEndian.Uint32(hdr[:]),
	}, nil
}

func (l *loopbackListener) Close() error {
	l.registry.unregister(l.cid, l.port)
	return l.tcp.Close()
}

func (l *loopbackListener) Addr() net.Addr { return l.tcp.Addr() }
