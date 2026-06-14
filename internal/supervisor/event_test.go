package supervisor_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/olomix/vsockd/internal/supervisor"
)

// fixedClock returns a clock function pinned to a known instant so the "ts"
// field is deterministic.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// decode parses a single framed line into a generic map and asserts the frame
// is exactly one '\n'-terminated NDJSON line.
func decode(t *testing.T, frame []byte) map[string]any {
	t.Helper()
	if n := bytes.Count(frame, []byte("\n")); n != 1 {
		t.Fatalf("frame must contain exactly one newline, got %d: %q", n, frame)
	}
	if len(frame) == 0 || frame[len(frame)-1] != '\n' {
		t.Fatalf("frame must be newline-terminated: %q", frame)
	}
	var m map[string]any
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatalf("unmarshal %q: %v", frame, err)
	}
	return m
}

func newFramer() *supervisor.Framer {
	ts := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	tags := map[string]string{"service": "abc", "version": "1.1.2"}
	return supervisor.NewFramer(fixedClock(ts), tags)
}

func TestLogRecord(t *testing.T) {
	f := newFramer()
	m := decode(t, f.Log("app", 42, supervisor.StreamStdout, "hello"))

	if m["type"] != supervisor.TypeLog {
		t.Fatalf("type = %v, want %q", m["type"], supervisor.TypeLog)
	}
	if m["src"] != "app" {
		t.Fatalf("src = %v", m["src"])
	}
	if m["pid"].(float64) != 42 {
		t.Fatalf("pid = %v", m["pid"])
	}
	if m["stream"] != supervisor.StreamStdout {
		t.Fatalf("stream = %v", m["stream"])
	}
	if m["msg"] != "hello" {
		t.Fatalf("msg = %v", m["msg"])
	}
	for _, k := range []string{"code", "signal", "count"} {
		if _, ok := m[k]; ok {
			t.Fatalf("log record must not carry %q: %v", k, m)
		}
	}
}

func TestStartRecord(t *testing.T) {
	f := newFramer()
	m := decode(t, f.Start("app", 42))

	if m["type"] != supervisor.TypeStart {
		t.Fatalf("type = %v, want %q", m["type"], supervisor.TypeStart)
	}
	if m["stream"] != supervisor.StreamNone {
		t.Fatalf("stream = %v, want %q", m["stream"], supervisor.StreamNone)
	}
	for _, k := range []string{"msg", "code", "signal", "count"} {
		if _, ok := m[k]; ok {
			t.Fatalf("start record must not carry %q: %v", k, m)
		}
	}
}

func TestExitRecordNoSignal(t *testing.T) {
	f := newFramer()
	m := decode(t, f.Exit("vsockd", 12, 1, ""))

	if m["type"] != supervisor.TypeExit {
		t.Fatalf("type = %v, want %q", m["type"], supervisor.TypeExit)
	}
	if m["code"].(float64) != 1 {
		t.Fatalf("code = %v, want 1", m["code"])
	}
	// signal is optional: absent when the process was not killed by a signal.
	if _, ok := m["signal"]; ok {
		t.Fatalf("exit without signal must not carry signal: %v", m)
	}
}

func TestExitRecordWithSignal(t *testing.T) {
	f := newFramer()
	m := decode(t, f.Exit("app", 42, -1, "SIGKILL"))

	if m["code"].(float64) != -1 {
		t.Fatalf("code = %v, want -1", m["code"])
	}
	if m["signal"] != "SIGKILL" {
		t.Fatalf("signal = %v, want SIGKILL", m["signal"])
	}
}

func TestDropRecord(t *testing.T) {
	f := newFramer()
	m := decode(t, f.Drop(1, 128))

	if m["type"] != supervisor.TypeDrop {
		t.Fatalf("type = %v, want %q", m["type"], supervisor.TypeDrop)
	}
	if m["src"] != supervisor.SrcSupervisor {
		t.Fatalf("src = %v, want %q", m["src"], supervisor.SrcSupervisor)
	}
	if m["count"].(float64) != 128 {
		t.Fatalf("count = %v, want 128", m["count"])
	}
	if m["stream"] != supervisor.StreamNone {
		t.Fatalf("stream = %v", m["stream"])
	}
}

func TestTimestampFromClock(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 600000000, time.UTC)
	f := supervisor.NewFramer(fixedClock(ts), nil)
	m := decode(t, f.Start("app", 1))

	want := ts.Format(time.RFC3339Nano)
	if m["ts"] != want {
		t.Fatalf("ts = %v, want %v", m["ts"], want)
	}
}

func TestEmbeddedNewlineDoesNotBreakFraming(t *testing.T) {
	f := newFramer()
	// A captured line containing a newline must still serialise to a single
	// NDJSON frame (json escapes the newline inside the string).
	msg := "line one\nline two"
	frame := f.Log("app", 42, supervisor.StreamStderr, msg)

	m := decode(t, frame) // decode asserts exactly one trailing newline
	if m["msg"] != msg {
		t.Fatalf("msg = %q, want %q", m["msg"], msg)
	}
}

func TestTagsOnEveryRecordAndNoHostFields(t *testing.T) {
	f := newFramer()
	frames := map[string][]byte{
		"log":   f.Log("app", 42, supervisor.StreamStdout, "x"),
		"start": f.Start("app", 42),
		"exit":  f.Exit("app", 42, 0, ""),
		"drop":  f.Drop(1, 3),
	}
	for name, frame := range frames {
		m := decode(t, frame)
		tags, ok := m["tags"].(map[string]any)
		if !ok {
			t.Fatalf("%s: tags missing or wrong type: %v", name, m["tags"])
		}
		if tags["service"] != "abc" || tags["version"] != "1.1.2" {
			t.Fatalf("%s: tags = %v", name, tags)
		}
		// The supervisor owns "tags" only; host-side fields are added later
		// on the host by log_relay and must never appear here.
		for _, k := range []string{"cid", "host"} {
			if _, ok := m[k]; ok {
				t.Fatalf("%s: record must not carry %q: %v", name, k, m)
			}
		}
	}
}

func TestNilTagsOmitted(t *testing.T) {
	f := supervisor.NewFramer(fixedClock(time.Unix(0, 0).UTC()), nil)
	m := decode(t, f.Log("app", 1, supervisor.StreamStdout, "x"))
	if _, ok := m["tags"]; ok {
		t.Fatalf("nil tags must be omitted: %v", m)
	}
}

func TestEmptyMsgIsPresent(t *testing.T) {
	f := newFramer()
	m := decode(t, f.Log("app", 1, supervisor.StreamStdout, ""))
	v, ok := m["msg"]
	if !ok {
		t.Fatalf("empty msg must still be present: %v", m)
	}
	if v != "" {
		t.Fatalf("msg = %v, want empty string", v)
	}
}
