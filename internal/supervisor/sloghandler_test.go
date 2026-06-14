package supervisor_test

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olomix/vsockd/internal/supervisor"
)

// sink collects frames enqueued by the handler. It is concurrency-safe so the
// never-block test can hammer it from one goroutine without data races under
// -race while still exposing the captured frames.
type sink struct {
	mu     sync.Mutex
	frames [][]byte
}

func (s *sink) enqueue(f []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Copy: slog/handler may reuse the backing array across calls.
	s.frames = append(s.frames, bytes.Clone(f))
}

func (s *sink) all() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.frames))
	copy(out, s.frames)
	return out
}

func newHandlerLogger(
	sk func([]byte), mirror *bytes.Buffer, level slog.Leveler,
) *slog.Logger {
	f := newFramer() // src tags: service=abc, version=1.1.2
	h := supervisor.NewLogHandler(f, sk, mirror, 1, level)
	return slog.New(h)
}

func TestLogHandlerEmitsSupervisorLogRecord(t *testing.T) {
	var sk sink
	var mirror bytes.Buffer
	log := newHandlerLogger(sk.enqueue, &mirror, slog.LevelInfo)

	log.Info("restarting vsockd", "attempt", 2, "max", 5)

	frames := sk.all()
	if len(frames) != 1 {
		t.Fatalf("want exactly one frame, got %d", len(frames))
	}
	m := decode(t, frames[0])

	if m["type"] != supervisor.TypeLog {
		t.Fatalf("type = %v, want %q", m["type"], supervisor.TypeLog)
	}
	if m["src"] != supervisor.SrcSupervisor {
		t.Fatalf("src = %v, want %q", m["src"], supervisor.SrcSupervisor)
	}
	if m["pid"].(float64) != 1 {
		t.Fatalf("pid = %v, want 1", m["pid"])
	}
	if m["stream"] != supervisor.StreamNone {
		t.Fatalf("stream = %v, want %q", m["stream"], supervisor.StreamNone)
	}
	msg, _ := m["msg"].(string)
	for _, want := range []string{"INFO", "restarting vsockd", "attempt=2", "max=5"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("msg %q missing %q", msg, want)
		}
	}
	tags, ok := m["tags"].(map[string]any)
	if !ok || tags["service"] != "abc" || tags["version"] != "1.1.2" {
		t.Fatalf("tags = %v, want configured source tags", m["tags"])
	}
	for _, k := range []string{"cid", "host", "code", "signal", "count"} {
		if _, ok := m[k]; ok {
			t.Fatalf("supervisor log record must not carry %q: %v", k, m)
		}
	}
}

func TestLogHandlerMirrorsSameLineToStderr(t *testing.T) {
	var sk sink
	var mirror bytes.Buffer
	log := newHandlerLogger(sk.enqueue, &mirror, slog.LevelInfo)

	log.Info("hello world")

	frames := sk.all()
	if len(frames) != 1 {
		t.Fatalf("want one frame, got %d", len(frames))
	}
	if !bytes.Equal(mirror.Bytes(), frames[0]) {
		t.Fatalf("mirror %q != enqueued frame %q", mirror.Bytes(), frames[0])
	}
}

func TestLogHandlerRespectsLevel(t *testing.T) {
	var sk sink
	var mirror bytes.Buffer
	log := newHandlerLogger(sk.enqueue, &mirror, slog.LevelInfo)

	log.Debug("suppressed")
	if n := len(sk.all()); n != 0 {
		t.Fatalf("debug below threshold must be dropped, got %d frames", n)
	}
	log.Warn("kept")
	if n := len(sk.all()); n != 1 {
		t.Fatalf("warn must be emitted, got %d frames", n)
	}
}

func TestLogHandlerWithAttrsAndGroup(t *testing.T) {
	var sk sink
	var mirror bytes.Buffer
	base := newHandlerLogger(sk.enqueue, &mirror, slog.LevelInfo)

	log := base.With("svc", "vsockd").WithGroup("net")
	log.Info("dial", "cid", 3)

	frames := sk.all()
	if len(frames) != 1 {
		t.Fatalf("want one frame, got %d", len(frames))
	}
	m := decode(t, frames[0])
	msg, _ := m["msg"].(string)
	for _, want := range []string{"dial", "svc=vsockd", "net.cid=3"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("msg %q missing %q", msg, want)
		}
	}
}

func TestLogHandlerNeverBlocksWhenBufferFull(t *testing.T) {
	// A tiny ring buffer forces overflow; the handler must keep accepting log
	// calls without blocking, relying on the buffer's drop-oldest policy.
	buf := supervisor.NewRingBuffer(0, 4)
	var mirror bytes.Buffer
	log := newHandlerLogger(buf.Enqueue, &mirror, slog.LevelInfo)

	done := make(chan struct{})
	go func() {
		for i := range 1000 {
			log.Info("spam", "i", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("logging blocked when buffer was full")
	}

	if got := buf.Len(); got > 4 {
		t.Fatalf("buffer exceeded cap: len = %d, want <= 4", got)
	}
	if buf.TakeDrops() == 0 {
		t.Fatal("expected drops to be counted after overflow")
	}
}
