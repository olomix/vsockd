package vsockconn

import (
	"fmt"
	"net"

	"github.com/mdlayher/vsock"
)

// NewVsockDialer returns a Dialer backed by AF_VSOCK. On non-Linux
// platforms the underlying library returns an "not implemented" error from
// every Dial, so this backend is effectively Linux-only at runtime.
func NewVsockDialer() Dialer { return vsockDialer{} }

type vsockDialer struct{}

func (vsockDialer) Dial(cid, port uint32) (net.Conn, error) {
	return vsock.Dial(cid, port, nil)
}

// ListenVsock binds the real vsock listener on the given port for every
// CID assigned to this machine. ContextID of the listener is inferred by
// the library from the running host.
func ListenVsock(port uint32) (Listener, error) {
	l, err := vsock.Listen(port, nil)
	if err != nil {
		return nil, err
	}
	return &vsockListener{l: l}, nil
}

type vsockListener struct{ l *vsock.Listener }

func (v *vsockListener) Accept() (Conn, error) {
	c, err := v.l.Accept()
	if err != nil {
		return nil, err
	}
	addr, ok := c.RemoteAddr().(*vsock.Addr)
	if !ok {
		c.Close()
		return nil, fmt.Errorf(
			"vsockconn: unexpected remote addr type %T", c.RemoteAddr())
	}
	return &vsockConn{Conn: c, peerCID: addr.ContextID}, nil
}

func (v *vsockListener) Close() error   { return v.l.Close() }
func (v *vsockListener) Addr() net.Addr { return v.l.Addr() }

type vsockConn struct {
	net.Conn
	peerCID uint32
}

func (v *vsockConn) PeerCID() uint32 { return v.peerCID }

// CloseWrite shuts down the write side of the underlying *vsock.Conn so
// the peer observes EOF while still being able to send remaining bytes.
// Required by the proxy paths' half-close semantics.
func (v *vsockConn) CloseWrite() error {
	if cw, ok := v.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return v.Conn.Close()
}
