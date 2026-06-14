package supervisor

import (
	"io"
	"sync"
)

// RingBuffer is a bounded, frame-granular queue of NDJSON frames feeding the
// vsock shipper. It implements the supervisor's loss policy (decision 8):
// producers never block on the network, and overflow drops the OLDEST whole
// frames — never a partial frame, which would corrupt the NDJSON stream.
// Dropped frames are counted so loss is observable downstream via a drop
// record. A zero budget on a dimension means that dimension is unbounded; the
// config layer guarantees at least one positive budget.
type RingBuffer struct {
	mu         sync.Mutex
	frames     [][]byte
	bytes      int
	maxBytes   int
	maxRecords int
	drops      int
}

// NewRingBuffer returns a buffer bounded by maxBytes and/or maxRecords. A
// non-positive bound disables that dimension.
func NewRingBuffer(maxBytes, maxRecords int) *RingBuffer {
	return &RingBuffer{maxBytes: maxBytes, maxRecords: maxRecords}
}

// Enqueue appends a frame and evicts oldest whole frames to honor the budget.
// It never blocks: overflow is resolved by dropping, not by waiting.
func (b *RingBuffer) Enqueue(f []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.frames = append(b.frames, f)
	b.bytes += len(f)
	b.evict()
}

// evict drops oldest whole frames until the buffer is within budget. A single
// frame larger than the byte budget cannot be stored, so the loop also evicts
// it (down to an empty buffer) rather than retaining an over-budget frame.
// Caller holds b.mu.
func (b *RingBuffer) evict() {
	for len(b.frames) > 0 && b.overBudget() {
		old := b.frames[0]
		b.frames[0] = nil // release reference for GC
		b.frames = b.frames[1:]
		b.bytes -= len(old)
		b.drops++
	}
}

// overBudget reports whether either configured bound is exceeded. Caller holds
// b.mu.
func (b *RingBuffer) overBudget() bool {
	if b.maxRecords > 0 && len(b.frames) > b.maxRecords {
		return true
	}
	if b.maxBytes > 0 && b.bytes > b.maxBytes {
		return true
	}
	return false
}

// DrainTo writes buffered frames to w in FIFO order and returns the number
// written. It takes ownership of the queued frames under the lock and writes
// without holding it, so producers are never blocked by a slow or stalled
// network write. On a write error the unwritten frames (including the one that
// failed) are re-queued at the front, preserving FIFO order relative to
// anything enqueued meanwhile.
func (b *RingBuffer) DrainTo(w io.Writer) (int, error) {
	b.mu.Lock()
	frames := b.frames
	b.frames = nil
	b.bytes = 0
	b.mu.Unlock()

	for i, f := range frames {
		if _, err := w.Write(f); err != nil {
			b.requeueFront(frames[i:])
			return i, err
		}
	}
	return len(frames), nil
}

// requeueFront prepends unwritten frames (older than anything enqueued during
// the drain) and re-applies the budget, dropping oldest if the transient
// overage exceeds it.
func (b *RingBuffer) requeueFront(unwritten [][]byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var sz int
	for _, f := range unwritten {
		sz += len(f)
	}
	b.frames = append(unwritten, b.frames...)
	b.bytes += sz
	b.evict()
}

// TakeDrops returns the number of frames dropped since the last call and
// resets the counter, so each dropped frame is reported exactly once. The
// shipper calls this as part of its drain cycle to emit a drop record.
func (b *RingBuffer) TakeDrops() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	d := b.drops
	b.drops = 0
	return d
}

// Drops reports the number of frames dropped but not yet reported, without
// consuming the count. Flush uses it so shutdown does not return before a
// pending drop record has been emitted downstream.
func (b *RingBuffer) Drops() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.drops
}

// Len reports the number of frames currently queued.
func (b *RingBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.frames)
}
