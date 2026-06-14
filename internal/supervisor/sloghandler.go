package supervisor

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
)

// LogHandler is a slog.Handler that ships the supervisor's OWN log records over
// the same path as child output (decision 5). Each record is rendered into a
// single text line, framed as an NDJSON "log" record with src:"supervisor",
// and enqueued to the ring buffer; the same frame is mirrored to stderr as a
// debug-console fallback and as the only egress before the buffer/shipper exist
// (e.g. a config-load failure). Inside an enclave this is the only way the
// supervisor's own logs escape.
//
// The wire format (decision 6) has no level/attr fields, so a record's level
// and attributes are folded into the "msg" string as "LEVEL message k=v ...".
type LogHandler struct {
	framer *Framer
	sink   func([]byte)
	mirror io.Writer
	pid    int
	level  slog.Leveler

	// attrs is the preformatted suffix accumulated by WithAttrs (each as a
	// leading-space " key=value"); group is the dotted prefix from WithGroup.
	// Both are immutable after clone, so Handle needs no lock to read them.
	attrs string
	group string

	// mirrorMu serializes mirror writes so concurrent log calls cannot
	// interleave bytes of different frames on stderr. Shared across clones.
	mirrorMu *sync.Mutex
}

// NewLogHandler builds a handler that frames records via framer, enqueues them
// through sink (e.g. the shipper's Enqueue or the buffer's Enqueue), and
// mirrors each frame to mirror. pid is the supervisor's pid, stamped on every
// record. A nil level defaults to Info; a nil sink or mirror is skipped.
func NewLogHandler(
	framer *Framer, sink func([]byte), mirror io.Writer,
	pid int, level slog.Leveler,
) *LogHandler {
	if level == nil {
		level = slog.LevelInfo
	}
	return &LogHandler{
		framer:   framer,
		sink:     sink,
		mirror:   mirror,
		pid:      pid,
		level:    level,
		mirrorMu: &sync.Mutex{},
	}
}

// Enabled reports whether a record at the given level should be handled.
func (h *LogHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

// Handle frames the record and dispatches it to the buffer and stderr mirror.
// It never blocks: the buffer's drop-oldest policy absorbs overflow, so logging
// from the supervisor's hot paths (waitpid loop, restarts) cannot stall.
func (h *LogHandler) Handle(_ context.Context, r slog.Record) error {
	frame := h.framer.Log(SrcSupervisor, h.pid, StreamNone, h.format(r))
	if h.sink != nil {
		h.sink(frame)
	}
	if h.mirror != nil {
		h.mirrorMu.Lock()
		_, _ = h.mirror.Write(frame)
		h.mirrorMu.Unlock()
	}
	return nil
}

// format renders "LEVEL message k=v ..." for the record's level, message,
// the preformatted WithAttrs suffix, and the record's own attributes.
func (h *LogHandler) format(r slog.Record) string {
	var b strings.Builder
	b.WriteString(r.Level.String())
	b.WriteByte(' ')
	b.WriteString(r.Message)
	b.WriteString(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&b, h.group, a)
		return true
	})
	return b.String()
}

// WithAttrs returns a clone with the given attributes preformatted under the
// current group prefix, so they are emitted on every subsequent record.
func (h *LogHandler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	var b strings.Builder
	b.WriteString(h.attrs)
	for _, a := range as {
		appendAttr(&b, h.group, a)
	}
	h2 := *h
	h2.attrs = b.String()
	return &h2
}

// WithGroup returns a clone that dot-prefixes subsequent attribute keys.
func (h *LogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h2 := *h
	h2.group = h.group + name + "."
	return &h2
}

// appendAttr writes " prefixkey=value" for a, recursing into group values so
// nested groups dot-join their keys. Empty attrs and empty groups are skipped,
// matching slog's documented handler behavior.
func appendAttr(b *strings.Builder, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		group := a.Value.Group()
		if len(group) == 0 {
			return
		}
		next := prefix
		if a.Key != "" {
			next = prefix + a.Key + "."
		}
		for _, ga := range group {
			appendAttr(b, next, ga)
		}
		return
	}
	b.WriteByte(' ')
	b.WriteString(prefix)
	b.WriteString(a.Key)
	b.WriteByte('=')
	b.WriteString(a.Value.String())
}
