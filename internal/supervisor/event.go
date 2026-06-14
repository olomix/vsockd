package supervisor

import (
	"encoding/json"
	"time"
)

// Record type values carried in the "type" field of every NDJSON envelope.
const (
	TypeLog   = "log"
	TypeStart = "start"
	TypeExit  = "exit"
	TypeDrop  = "drop"
)

// Stream values for the "stream" field. Child stdout/stderr use StreamStdout /
// StreamStderr; lifecycle records (start/exit/drop) and the supervisor's own
// log records carry StreamNone since they are not tied to a captured stream.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
	StreamNone   = "-"
)

// SrcSupervisor is the "src" value for records the supervisor emits about
// itself (its own logs and drop accounting), as opposed to child output.
const SrcSupervisor = "supervisor"

// tsLayout formats the "ts" field. RFC3339 with nanosecond precision; the
// host-side delivery agent does the authoritative ingest-timestamping.
const tsLayout = time.RFC3339Nano

// envelope is the on-wire NDJSON record. All record types share this struct;
// type-specific fields are pointers so they are emitted only when set. "tags"
// is omitted when nil (no source-side tags configured); host-only fields
// ("cid"/"host") are intentionally absent — log_relay adds those on the host.
type envelope struct {
	Ts     string            `json:"ts"`
	Src    string            `json:"src"`
	PID    int               `json:"pid"`
	Stream string            `json:"stream"`
	Tags   map[string]string `json:"tags,omitzero"`
	Type   string            `json:"type"`
	Msg    *string           `json:"msg,omitempty"`
	Code   *int              `json:"code,omitempty"`
	Signal *string           `json:"signal,omitempty"`
	Count  *int              `json:"count,omitempty"`
	// Truncated marks a log line that exceeded the per-line cap; the "msg"
	// carries only the retained prefix. Omitted (false) for normal lines.
	Truncated bool `json:"truncated,omitempty"`
}

// Framer builds NDJSON frames tagged with the source-side identity. The clock
// is injected so timestamps are deterministic in tests; tags are the
// configured source-side identity emitted under each record's "tags" key.
type Framer struct {
	now  func() time.Time
	tags map[string]string
}

// NewFramer returns a Framer that stamps records with now() and the given tags.
func NewFramer(now func() time.Time, tags map[string]string) *Framer {
	return &Framer{now: now, tags: tags}
}

// frame stamps the timestamp and tags, marshals the envelope, and appends the
// NDJSON line terminator. json.Marshal escapes embedded newlines within
// strings, so a captured line containing '\n' still serialises to one line.
func (f *Framer) frame(e envelope) []byte {
	e.Ts = f.now().Format(tsLayout)
	e.Tags = f.tags
	// Marshalling cannot fail: the envelope holds only JSON-safe scalars and a
	// string map. The repo ignores this error elsewhere (logrelay.go) too.
	b, _ := json.Marshal(e)
	return append(b, '\n')
}

// Log frames a captured output line from a child or the supervisor itself.
func (f *Framer) Log(src string, pid int, stream, msg string) []byte {
	return f.logLine(src, pid, stream, msg, false)
}

// logLine frames a captured output line, flagging it truncated when the line
// exceeded the per-line cap and msg holds only the retained prefix.
func (f *Framer) logLine(src string, pid int, stream, msg string, truncated bool) []byte {
	return f.frame(envelope{
		Src: src, PID: pid, Stream: stream, Type: TypeLog, Msg: new(msg),
		Truncated: truncated,
	})
}

// Start frames a process-start lifecycle event.
func (f *Framer) Start(src string, pid int) []byte {
	return f.frame(envelope{
		Src: src, PID: pid, Stream: StreamNone, Type: TypeStart,
	})
}

// Exit frames a process-exit lifecycle event. signal is the terminating
// signal name (e.g. "SIGKILL") or "" when the process exited normally, in
// which case the "signal" field is omitted.
func (f *Framer) Exit(src string, pid, code int, signal string) []byte {
	e := envelope{
		Src: src, PID: pid, Stream: StreamNone, Type: TypeExit, Code: new(code),
	}
	if signal != "" {
		e.Signal = new(signal)
	}
	return f.frame(e)
}

// Drop frames a drop-accounting record reporting count frames lost to buffer
// overflow. It is attributed to the supervisor; pid is the supervisor's pid.
func (f *Framer) Drop(pid, count int) []byte {
	return f.frame(envelope{
		Src: SrcSupervisor, PID: pid, Stream: StreamNone,
		Type: TypeDrop, Count: new(count),
	})
}
