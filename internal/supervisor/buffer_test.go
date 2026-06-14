package supervisor_test

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/olomix/vsockd/internal/supervisor"
)

// frame builds a single NDJSON frame (newline-terminated) for buffer tests.
func frame(s string) []byte { return []byte(s + "\n") }

func TestRingBufferDrainFIFO(t *testing.T) {
	b := supervisor.NewRingBuffer(0, 10)
	b.Enqueue(frame("a"))
	b.Enqueue(frame("b"))
	b.Enqueue(frame("c"))

	var buf bytes.Buffer
	n, err := b.DrainTo(&buf)
	if err != nil {
		t.Fatalf("DrainTo: %v", err)
	}
	if n != 3 {
		t.Fatalf("drained %d frames, want 3", n)
	}
	if got := buf.String(); got != "a\nb\nc\n" {
		t.Fatalf("FIFO order broken: %q", got)
	}

	// A second drain finds the buffer empty.
	buf.Reset()
	n, err = b.DrainTo(&buf)
	if err != nil {
		t.Fatalf("second DrainTo: %v", err)
	}
	if n != 0 || buf.Len() != 0 {
		t.Fatalf("buffer not emptied: n=%d buf=%q", n, buf.String())
	}
}

func TestRingBufferRecordBudgetDropsOldest(t *testing.T) {
	b := supervisor.NewRingBuffer(0, 2)
	b.Enqueue(frame("a"))
	b.Enqueue(frame("b"))
	b.Enqueue(frame("c")) // exceeds 2 records: oldest ("a") is evicted

	if d := b.TakeDrops(); d != 1 {
		t.Fatalf("drops = %d, want 1", d)
	}

	var buf bytes.Buffer
	if _, err := b.DrainTo(&buf); err != nil {
		t.Fatalf("DrainTo: %v", err)
	}
	if got := buf.String(); got != "b\nc\n" {
		t.Fatalf("kept wrong frames: %q", got)
	}
}

func TestRingBufferByteBudgetEvictsWholeFrames(t *testing.T) {
	// Each frame "xx\n" is 3 bytes; a 6-byte budget holds exactly two.
	b := supervisor.NewRingBuffer(6, 0)
	b.Enqueue(frame("aa"))
	b.Enqueue(frame("bb"))
	b.Enqueue(frame("cc")) // would be 9 bytes: evict "aa" back to 6

	if d := b.TakeDrops(); d != 1 {
		t.Fatalf("drops = %d, want 1", d)
	}

	var buf bytes.Buffer
	if _, err := b.DrainTo(&buf); err != nil {
		t.Fatalf("DrainTo: %v", err)
	}
	got := buf.String()
	if got != "bb\ncc\n" {
		t.Fatalf("kept wrong frames: %q", got)
	}
	// Eviction must never leave a partial frame: every output segment is a
	// complete line, so splitting on '\n' yields only whole frames.
	for line := range strings.SplitSeq(got, "\n") {
		if len(line) != 0 && len(line) != 2 {
			t.Fatalf("partial frame in output: %q", line)
		}
	}
}

func TestRingBufferOversizedFrameDropped(t *testing.T) {
	// A frame larger than the whole byte budget cannot be stored; it is
	// dropped (counted) rather than retained in violation of the budget.
	b := supervisor.NewRingBuffer(2, 0)
	b.Enqueue(frame("toolong"))

	var buf bytes.Buffer
	n, err := b.DrainTo(&buf)
	if err != nil {
		t.Fatalf("DrainTo: %v", err)
	}
	if n != 0 || buf.Len() != 0 {
		t.Fatalf("oversized frame retained: n=%d buf=%q", n, buf.String())
	}
	if d := b.TakeDrops(); d != 1 {
		t.Fatalf("drops = %d, want 1", d)
	}
}

func TestRingBufferTakeDropsOnce(t *testing.T) {
	b := supervisor.NewRingBuffer(0, 1)
	b.Enqueue(frame("a"))
	b.Enqueue(frame("b")) // drop a
	b.Enqueue(frame("c")) // drop b

	if d := b.TakeDrops(); d != 2 {
		t.Fatalf("first TakeDrops = %d, want 2", d)
	}
	if d := b.TakeDrops(); d != 0 {
		t.Fatalf("second TakeDrops = %d, want 0 (count reported once)", d)
	}
}

func TestRingBufferEnqueueNeverBlocks(t *testing.T) {
	b := supervisor.NewRingBuffer(0, 1)
	const n = 1000
	for range n {
		b.Enqueue(frame("x")) // returns immediately even when full
	}
	if d := b.TakeDrops(); d != n-1 {
		t.Fatalf("drops = %d, want %d", d, n-1)
	}
	var buf bytes.Buffer
	if got, err := b.DrainTo(&buf); err != nil || got != 1 {
		t.Fatalf("DrainTo: got=%d err=%v, want 1 frame retained", got, err)
	}
}

// errWriter fails after acceptN successful frame writes, to exercise the
// re-queue path on a partial drain.
type errWriter struct {
	acceptN int
	calls   int
}

func (w *errWriter) Write(p []byte) (int, error) {
	if w.calls >= w.acceptN {
		return 0, errors.New("write failed")
	}
	w.calls++
	return len(p), nil
}

func TestRingBufferDrainErrorRequeuesUnwritten(t *testing.T) {
	b := supervisor.NewRingBuffer(0, 10)
	b.Enqueue(frame("a"))
	b.Enqueue(frame("b"))
	b.Enqueue(frame("c"))

	w := &errWriter{acceptN: 1}
	n, err := b.DrainTo(w)
	if err == nil {
		t.Fatal("expected write error")
	}
	if n != 1 {
		t.Fatalf("written = %d, want 1 before failure", n)
	}

	// The unwritten frames ("b","c") remain queued in order for the next drain.
	var buf bytes.Buffer
	if _, err := b.DrainTo(&buf); err != nil {
		t.Fatalf("retry DrainTo: %v", err)
	}
	if got := buf.String(); got != "b\nc\n" {
		t.Fatalf("unwritten frames not re-queued in order: %q", got)
	}
}

func TestRingBufferConcurrentEnqueueDrain(t *testing.T) {
	b := supervisor.NewRingBuffer(1<<20, 0)
	var wg sync.WaitGroup

	for range 4 {
		wg.Go(func() {
			for range 1000 {
				b.Enqueue(frame("data"))
			}
		})
	}

	done := make(chan struct{})
	go func() {
		var buf bytes.Buffer
		for {
			select {
			case <-done:
				_, _ = b.DrainTo(&buf)
				return
			default:
				_, _ = b.DrainTo(&buf)
				buf.Reset()
				_ = b.TakeDrops()
			}
		}
	}()

	wg.Wait()
	close(done)
}
