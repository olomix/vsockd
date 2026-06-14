package supervisor

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/olomix/vsockd/internal/vsockconn"
)

// Default shipper timing applied when a ShipperConfig field is left zero.
// Backoff is bounded exponential (decision: 100ms → 5s) so a missing listener
// is retried promptly at first and then patiently.
const (
	defaultMinBackoff    = 100 * time.Millisecond
	defaultMaxBackoff    = 5 * time.Second
	defaultDrainInterval = 100 * time.Millisecond
)

// flushPollInterval is how often Flush re-checks the drain state. It is short
// so a graceful shutdown's grace window is spent flushing, not polling.
const flushPollInterval = 5 * time.Millisecond

// ShipperConfig parameterises a Shipper. Timing fields default (see the
// default* constants) when left zero; CID/Port/PID are required.
type ShipperConfig struct {
	CID, Port     uint32
	PID           int
	MinBackoff    time.Duration
	MaxBackoff    time.Duration
	DrainInterval time.Duration
}

// Shipper drains the ring buffer to the parent over its own vsock connection
// (decision 1: a direct dial, never through vsockd, so vsockd's own crash
// output is still shipped). It reconnects with bounded backoff, never blocks
// producers (they only touch the buffer), and after any overflow emits a drop
// record on the next successful send so loss is observable downstream
// (decision 8).
type Shipper struct {
	buf    *RingBuffer
	framer *Framer
	dialer vsockconn.Dialer
	logger *slog.Logger
	cfg    ShipperConfig

	// wake nudges the drain loop when a frame is enqueued or a flush is
	// requested, so frames ship without waiting for the drain ticker. Buffered
	// size 1: a pending wake already covers any number of enqueues.
	wake chan struct{}

	// pendingDrops carries a drop count whose drop record could not be written
	// (the connection broke mid-emit) so it is retried, not lost. Written by the
	// Run goroutine and read by Flush via idle(), both under mu.
	pendingDrops int

	// draining guards the in-flight window of a drain cycle so Flush waits for
	// frames already taken from the buffer (and being written) before
	// reporting the buffer fully drained. mu also guards pendingDrops.
	mu       sync.Mutex
	draining bool
}

// NewShipper builds a Shipper. A nil logger discards the shipper's own logs.
func NewShipper(
	buf *RingBuffer, framer *Framer, dialer vsockconn.Dialer,
	logger *slog.Logger, cfg ShipperConfig,
) *Shipper {
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = defaultMinBackoff
	}
	if cfg.MaxBackoff < cfg.MinBackoff {
		cfg.MaxBackoff = defaultMaxBackoff
	}
	if cfg.MaxBackoff < cfg.MinBackoff {
		cfg.MaxBackoff = cfg.MinBackoff
	}
	if cfg.DrainInterval <= 0 {
		cfg.DrainInterval = defaultDrainInterval
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Shipper{
		buf:    buf,
		framer: framer,
		dialer: dialer,
		logger: logger,
		cfg:    cfg,
		wake:   make(chan struct{}, 1),
	}
}

// Enqueue queues a frame for shipping and wakes the drain loop. It never
// blocks: overflow is absorbed by the ring buffer's drop-oldest policy.
func (s *Shipper) Enqueue(frame []byte) {
	s.buf.Enqueue(frame)
	s.notify()
}

// notify performs a non-blocking wake of the drain loop.
func (s *Shipper) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run dials the parent and ships frames until ctx is cancelled. It is meant to
// run in its own goroutine for the supervisor's lifetime.
func (s *Shipper) Run(ctx context.Context) {
	var backoff time.Duration
	for {
		if ctx.Err() != nil {
			return
		}
		if backoff > 0 && !s.sleep(ctx, backoff) {
			return
		}
		conn, err := s.dialer.Dial(s.cfg.CID, s.cfg.Port)
		if err != nil {
			s.logger.Warn("log shipper dial failed",
				"cid", s.cfg.CID, "port", s.cfg.Port, "err", err)
			backoff = s.nextBackoff(backoff)
			continue
		}
		// A reconnect after a connected session waits at least MinBackoff so a
		// listener that accepts then immediately drops cannot spin a hot loop.
		backoff = s.cfg.MinBackoff
		s.serve(ctx, conn)
	}
}

// nextBackoff doubles the current backoff, clamped to [MinBackoff, MaxBackoff].
func (s *Shipper) nextBackoff(cur time.Duration) time.Duration {
	if cur <= 0 {
		return s.cfg.MinBackoff
	}
	n := cur * 2
	if n > s.cfg.MaxBackoff {
		return s.cfg.MaxBackoff
	}
	return n
}

// sleep waits for d or ctx cancellation, reporting whether d fully elapsed.
func (s *Shipper) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// serve drains the buffer to conn until a write fails or ctx is cancelled. It
// returns so Run can reconnect (on error) or stop (on cancel).
func (s *Shipper) serve(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	ticker := time.NewTicker(s.cfg.DrainInterval)
	defer ticker.Stop()
	for {
		if err := s.drainOnce(conn); err != nil {
			s.logger.Warn("log shipper write failed, reconnecting", "err", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-ticker.C:
		}
	}
}

// drainOnce writes any pending drop record, then drains buffered frames to
// conn in FIFO order. The drop record precedes the surviving frames so the gap
// is marked exactly where it occurred in the stream.
func (s *Shipper) drainOnce(conn net.Conn) error {
	s.setDraining(true)
	defer s.setDraining(false)

	s.mu.Lock()
	s.pendingDrops += s.buf.TakeDrops()
	pending := s.pendingDrops
	s.mu.Unlock()
	if pending > 0 {
		if _, err := conn.Write(s.framer.Drop(s.cfg.PID, pending)); err != nil {
			return err
		}
		s.mu.Lock()
		s.pendingDrops = 0
		s.mu.Unlock()
	}
	if _, err := s.buf.DrainTo(conn); err != nil {
		return err
	}
	return nil
}

func (s *Shipper) setDraining(v bool) {
	s.mu.Lock()
	s.draining = v
	s.mu.Unlock()
}

// idle reports that no drain is in flight, the buffer is empty, and no drop
// count is still awaiting a drop record. Unreported drops keep Flush waiting so
// shutdown does not return before loss is made observable downstream
// (decision 8): oversized frames can be dropped to an empty buffer, so an
// empty buffer alone does not mean every drop has been emitted.
func (s *Shipper) idle() bool {
	s.mu.Lock()
	busy := s.draining || s.pendingDrops > 0
	s.mu.Unlock()
	return !busy && s.buf.Len() == 0 && s.buf.Drops() == 0
}

// Flush best-effort drains the buffer within ctx's deadline, returning once the
// buffer is empty or the deadline passes (decision 9). It only nudges the Run
// goroutine; it never writes to the connection itself.
func (s *Shipper) Flush(ctx context.Context) {
	ticker := time.NewTicker(flushPollInterval)
	defer ticker.Stop()
	for {
		s.notify()
		if s.idle() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
