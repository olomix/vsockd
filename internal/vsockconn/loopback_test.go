package vsockconn

import (
	"errors"
	"io"
	"testing"
)

func TestLoopbackRoundtrip(t *testing.T) {
	reg := NewRegistry()
	ln, err := ListenLoopback(reg, 16, 8080)
	if err != nil {
		t.Fatalf("ListenLoopback: %v", err)
	}
	defer ln.Close()

	type accepted struct {
		peerCID uint32
		payload []byte
		err     error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			ch <- accepted{err: err}
			return
		}
		defer c.Close()
		buf := make([]byte, 5)
		if _, err := io.ReadFull(c, buf); err != nil {
			ch <- accepted{err: err}
			return
		}
		if _, err := c.Write([]byte("pong")); err != nil {
			ch <- accepted{err: err}
			return
		}
		ch <- accepted{peerCID: c.PeerCID(), payload: buf}
	}()

	d := NewLoopbackDialer(reg, 2) // CID 2 == Host in vsock parlance
	conn, err := d.Dial(16, 8080)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	resp := make([]byte, 4)
	if _, err := io.ReadFull(conn, resp); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(resp) != "pong" {
		t.Fatalf("unexpected response %q", resp)
	}

	res := <-ch
	if res.err != nil {
		t.Fatalf("accept side error: %v", res.err)
	}
	if res.peerCID != 2 {
		t.Fatalf("peer CID = %d, want 2", res.peerCID)
	}
	if string(res.payload) != "hello" {
		t.Fatalf("payload = %q, want \"hello\"", res.payload)
	}
}

func TestLoopbackPeerCIDPropagation(t *testing.T) {
	reg := NewRegistry()
	ln, err := ListenLoopback(reg, 42, 9000)
	if err != nil {
		t.Fatalf("ListenLoopback: %v", err)
	}
	defer ln.Close()

	// Two dialers with different source CIDs should produce distinct peer
	// CIDs on Accept.
	const conns = 2
	cids := make(chan uint32, conns)
	errs := make(chan error, conns)
	go func() {
		for i := 0; i < conns; i++ {
			c, err := ln.Accept()
			if err != nil {
				errs <- err
				return
			}
			cids <- c.PeerCID()
			c.Close()
		}
	}()

	for _, src := range []uint32{3, 99} {
		d := NewLoopbackDialer(reg, src)
		conn, err := d.Dial(42, 9000)
		if err != nil {
			t.Fatalf("Dial(src=%d): %v", src, err)
		}
		conn.Close()
	}

	seen := map[uint32]bool{}
	for i := 0; i < conns; i++ {
		select {
		case c := <-cids:
			seen[c] = true
		case err := <-errs:
			t.Fatalf("Accept: %v", err)
		}
	}
	if !seen[3] || !seen[99] {
		t.Fatalf("peer CIDs %v, want {3, 99}", seen)
	}
}

func TestLoopbackDialUnknownTarget(t *testing.T) {
	reg := NewRegistry()
	d := NewLoopbackDialer(reg, 2)
	if _, err := d.Dial(16, 8080); !errors.Is(err, ErrConnRefused) {
		t.Fatalf("Dial unknown: err = %v, want %v", err, ErrConnRefused)
	}
}

func TestLoopbackDuplicateRegistration(t *testing.T) {
	reg := NewRegistry()
	ln, err := ListenLoopback(reg, 16, 8080)
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	defer ln.Close()

	if _, err := ListenLoopback(reg, 16, 8080); err == nil {
		t.Fatal("second listen on same (cid,port) should fail")
	}
}

func TestLoopbackCloseReleasesBinding(t *testing.T) {
	reg := NewRegistry()
	ln, err := ListenLoopback(reg, 16, 8080)
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ln2, err := ListenLoopback(reg, 16, 8080)
	if err != nil {
		t.Fatalf("re-listen after close: %v", err)
	}
	ln2.Close()
}

func TestLoopbackDifferentCIDsIsolate(t *testing.T) {
	reg := NewRegistry()
	ln1, err := ListenLoopback(reg, 16, 8080)
	if err != nil {
		t.Fatalf("listen 16: %v", err)
	}
	defer ln1.Close()
	ln2, err := ListenLoopback(reg, 17, 8080)
	if err != nil {
		t.Fatalf("listen 17: %v", err)
	}
	defer ln2.Close()

	if ln1.Addr().String() == ln2.Addr().String() {
		t.Fatal("distinct (cid,port) should bind distinct TCP addrs")
	}
}

func TestUseLoopback(t *testing.T) {
	t.Setenv(BackendEnv, "")
	if UseLoopback() {
		t.Fatal("UseLoopback true when env unset")
	}
	t.Setenv(BackendEnv, BackendLoopback)
	if !UseLoopback() {
		t.Fatal("UseLoopback false when env=loopback")
	}
	t.Setenv(BackendEnv, "other")
	if UseLoopback() {
		t.Fatal("UseLoopback true for unknown backend value")
	}
}
