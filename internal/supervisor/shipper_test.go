package supervisor_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/olomix/vsockd/internal/supervisor"
	"github.com/olomix/vsockd/internal/vsockconn"
)

const (
	testLogCID    uint32 = 3
	testLogPort   uint32 = 5140
	testSourceCID uint32 = 4
)

// fastShipperCfg returns a shipper config with sub-millisecond timing so
// reconnect/backoff paths run quickly under test.
func fastShipperCfg() supervisor.ShipperConfig {
	return supervisor.ShipperConfig{
		CID:           testLogCID,
		Port:          testLogPort,
		PID:           1,
		MinBackoff:    2 * time.Millisecond,
		MaxBackoff:    20 * time.Millisecond,
		DrainInterval: 5 * time.Millisecond,
	}
}

func shipperFramer() *supervisor.Framer {
	ts := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	return supervisor.NewFramer(func() time.Time { return ts }, nil)
}

// decodeLine parses one NDJSON frame into a map.
func decodeLine(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("unmarshal %q: %v", line, err)
	}
	return m
}

// readLines reads n newline-terminated frames from c, decoding each. A read
// deadline keeps a stalled test from hanging the suite.
func readLines(t *testing.T, c net.Conn, n int) []map[string]any {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	r := bufio.NewReader(c)
	out := make([]map[string]any, 0, n)
	for i := range n {
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read frame %d/%d: %v", i+1, n, err)
		}
		out = append(out, decodeLine(t, line))
	}
	return out
}

// runShipper starts s.Run on a fresh context and returns a stop func that
// cancels it and waits for Run to return.
func runShipper(s *supervisor.Shipper) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	return ctx, func() {
		cancel()
		<-done
	}
}

func TestShipperDeliversFIFO(t *testing.T) {
	reg := vsockconn.NewRegistry()
	ln, err := vsockconn.ListenLoopback(reg, testLogCID, testLogPort)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	dialer := vsockconn.NewLoopbackDialer(reg, testSourceCID)
	buf := supervisor.NewRingBuffer(0, 100)
	s := supervisor.NewShipper(buf, shipperFramer(), dialer, nil, fastShipperCfg())
	_, stop := runShipper(s)
	defer stop()

	for i := range 5 {
		s.Enqueue([]byte(`{"type":"log","msg":"` + string(rune('a'+i)) + "\"}\n"))
	}

	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer conn.Close()

	frames := readLines(t, conn, 5)
	for i, m := range frames {
		want := string(rune('a' + i))
		if m["msg"] != want {
			t.Fatalf("frame %d msg = %v, want %q", i, m["msg"], want)
		}
	}
}

func TestShipperBuffersWhileDownThenFlushesWithDrop(t *testing.T) {
	reg := vsockconn.NewRegistry()
	dialer := vsockconn.NewLoopbackDialer(reg, testSourceCID)
	// Capacity 3: enqueuing 5 frames while disconnected evicts the 2 oldest.
	buf := supervisor.NewRingBuffer(0, 3)
	s := supervisor.NewShipper(buf, shipperFramer(), dialer, nil, fastShipperCfg())
	_, stop := runShipper(s)
	defer stop()

	for i := range 5 {
		s.Enqueue([]byte(`{"type":"log","msg":"` + string(rune('a'+i)) + "\"}\n"))
	}

	ln, err := vsockconn.ListenLoopback(reg, testLogCID, testLogPort)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer conn.Close()

	// One drop record (count=2) followed by the 3 surviving frames (c,d,e).
	frames := readLines(t, conn, 4)
	drop := frames[0]
	if drop["type"] != supervisor.TypeDrop {
		t.Fatalf("first frame type = %v, want drop", drop["type"])
	}
	if drop["count"].(float64) != 2 {
		t.Fatalf("drop count = %v, want 2", drop["count"])
	}
	if drop["src"] != supervisor.SrcSupervisor {
		t.Fatalf("drop src = %v, want supervisor", drop["src"])
	}
	for i, want := range []string{"c", "d", "e"} {
		if frames[i+1]["msg"] != want {
			t.Fatalf("survivor %d msg = %v, want %q", i, frames[i+1]["msg"], want)
		}
	}
}

func TestShipperNoDropRecordWithoutOverflow(t *testing.T) {
	reg := vsockconn.NewRegistry()
	dialer := vsockconn.NewLoopbackDialer(reg, testSourceCID)
	buf := supervisor.NewRingBuffer(0, 100)
	s := supervisor.NewShipper(buf, shipperFramer(), dialer, nil, fastShipperCfg())
	_, stop := runShipper(s)
	defer stop()

	for i := range 3 {
		s.Enqueue([]byte(`{"type":"log","msg":"` + string(rune('a'+i)) + "\"}\n"))
	}

	ln, err := vsockconn.ListenLoopback(reg, testLogCID, testLogPort)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer conn.Close()

	frames := readLines(t, conn, 3)
	for i, want := range []string{"a", "b", "c"} {
		if frames[i]["type"] == supervisor.TypeDrop {
			t.Fatalf("unexpected drop record at %d", i)
		}
		if frames[i]["msg"] != want {
			t.Fatalf("frame %d msg = %v, want %q", i, frames[i]["msg"], want)
		}
	}
}

// trackingDialer wraps an inner dialer and records each connection it hands
// out so a test can close the shipper's own end of a live connection,
// deterministically breaking it (unlike closing the accepted end, where the
// first write into a FIN'd socket may still succeed).
type trackingDialer struct {
	inner vsockconn.Dialer
	mu    sync.Mutex
	conns []net.Conn
}

func (d *trackingDialer) Dial(cid, port uint32) (net.Conn, error) {
	c, err := d.inner.Dial(cid, port)
	if err == nil {
		d.mu.Lock()
		d.conns = append(d.conns, c)
		d.mu.Unlock()
	}
	return c, err
}

func (d *trackingDialer) closeLast() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if n := len(d.conns); n > 0 {
		_ = d.conns[n-1].Close()
	}
}

func TestShipperReconnectsOnBrokenConnection(t *testing.T) {
	reg := vsockconn.NewRegistry()
	ln, err := vsockconn.ListenLoopback(reg, testLogCID, testLogPort)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	dialer := &trackingDialer{inner: vsockconn.NewLoopbackDialer(reg, testSourceCID)}
	buf := supervisor.NewRingBuffer(0, 100)
	s := supervisor.NewShipper(buf, shipperFramer(), dialer, nil, fastShipperCfg())
	_, stop := runShipper(s)
	defer stop()

	s.Enqueue([]byte(`{"type":"log","msg":"first"}` + "\n"))
	conn1, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept 1: %v", err)
	}
	if got := readLines(t, conn1, 1); got[0]["msg"] != "first" {
		t.Fatalf("frame 1 msg = %v", got[0]["msg"])
	}

	// Break the shipper's own end; the next drain must fail and trigger a
	// reconnect. The unsent frame is requeued, not lost.
	dialer.closeLast()
	_ = conn1.Close()
	s.Enqueue([]byte(`{"type":"log","msg":"second"}` + "\n"))

	conn2, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept 2: %v", err)
	}
	defer conn2.Close()
	if got := readLines(t, conn2, 1); got[0]["msg"] != "second" {
		t.Fatalf("frame 2 msg = %v", got[0]["msg"])
	}
}

// failWriteConn is a net.Conn whose Write always fails, simulating a peer that
// dies after the loopback handshake (which Dial already completed) but before
// any frame is delivered.
type failWriteConn struct{ net.Conn }

func (failWriteConn) Write([]byte) (int, error) {
	return 0, errors.New("simulated write failure")
}

// failFirstWriteDialer wraps a dialer so the first successfully-dialed
// connection fails on every write; later connections behave normally. Dials
// that fail at the inner dialer (e.g. no listener yet) are not counted.
type failFirstWriteDialer struct {
	inner vsockconn.Dialer
	mu    sync.Mutex
	dials int
}

func (d *failFirstWriteDialer) Dial(cid, port uint32) (net.Conn, error) {
	c, err := d.inner.Dial(cid, port)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.dials++
	first := d.dials == 1
	d.mu.Unlock()
	if first {
		return failWriteConn{Conn: c}, nil
	}
	return c, nil
}

// TestShipperRedeliversDropRecordAcrossBrokenConnection covers the hardest case
// of decision 8 (loss observable exactly once): the connection breaks while the
// drop record itself is being emitted. The count must survive to the next
// connection rather than being lost or double-counted.
func TestShipperRedeliversDropRecordAcrossBrokenConnection(t *testing.T) {
	reg := vsockconn.NewRegistry()
	dialer := &failFirstWriteDialer{inner: vsockconn.NewLoopbackDialer(reg, testSourceCID)}
	// Capacity 3: with no listener yet, enqueuing 5 frames drops the 2 oldest.
	buf := supervisor.NewRingBuffer(0, 3)
	s := supervisor.NewShipper(buf, shipperFramer(), dialer, nil, fastShipperCfg())
	_, stop := runShipper(s)
	defer stop()

	for i := range 5 {
		s.Enqueue([]byte(`{"type":"log","msg":"` + string(rune('a'+i)) + "\"}\n"))
	}

	ln, err := vsockconn.ListenLoopback(reg, testLogCID, testLogPort)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// First connection: the drop-record write fails, so the peer sees no frame
	// before the shipper drops the connection and reconnects.
	conn1, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept 1: %v", err)
	}
	_ = conn1.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := bufio.NewReader(conn1).ReadBytes('\n'); err == nil {
		t.Fatal("expected no frame on the broken connection")
	}
	_ = conn1.Close()

	// Second connection: the drop record (count=2) is redelivered exactly once,
	// ahead of the 3 surviving frames.
	conn2, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept 2: %v", err)
	}
	defer conn2.Close()

	frames := readLines(t, conn2, 4)
	drop := frames[0]
	if drop["type"] != supervisor.TypeDrop {
		t.Fatalf("first frame type = %v, want drop", drop["type"])
	}
	if drop["count"].(float64) != 2 {
		t.Fatalf("drop count = %v, want 2", drop["count"])
	}
	for i, want := range []string{"c", "d", "e"} {
		if frames[i+1]["msg"] != want {
			t.Fatalf("survivor %d msg = %v, want %q", i, frames[i+1]["msg"], want)
		}
	}
}

func TestShipperFlushDrainsThenReturns(t *testing.T) {
	reg := vsockconn.NewRegistry()
	ln, err := vsockconn.ListenLoopback(reg, testLogCID, testLogPort)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	dialer := vsockconn.NewLoopbackDialer(reg, testSourceCID)
	buf := supervisor.NewRingBuffer(0, 100)
	s := supervisor.NewShipper(buf, shipperFramer(), dialer, nil, fastShipperCfg())
	_, stop := runShipper(s)
	defer stop()

	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer conn.Close()

	for range 4 {
		s.Enqueue([]byte(`{"type":"log","msg":"x"}` + "\n"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.Flush(ctx)

	if n := buf.Len(); n != 0 {
		t.Fatalf("buffer not drained after flush: %d frames left", n)
	}
	readLines(t, conn, 4) // all four frames must have arrived
}

func TestShipperFlushReturnsAtDeadlineWhenDown(t *testing.T) {
	reg := vsockconn.NewRegistry()
	dialer := vsockconn.NewLoopbackDialer(reg, testSourceCID)
	buf := supervisor.NewRingBuffer(0, 100)
	s := supervisor.NewShipper(buf, shipperFramer(), dialer, nil, fastShipperCfg())
	_, stop := runShipper(s)
	defer stop()

	s.Enqueue([]byte(`{"type":"log","msg":"stuck"}` + "\n"))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.Flush(ctx)
	elapsed := time.Since(start)

	if elapsed < 40*time.Millisecond {
		t.Fatalf("flush returned too early (%v); should block until deadline", elapsed)
	}
	if n := buf.Len(); n == 0 {
		t.Fatalf("buffer drained though no listener exists")
	}
}
