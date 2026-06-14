package outbound

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"

	"github.com/olomix/vsockd/internal/config"
	"github.com/olomix/vsockd/internal/metrics"
	"github.com/olomix/vsockd/internal/vsockconn"
)

// defaultLogRelayMaxLine bounds a single relayed NDJSON line when the
// listener config did not set one. Mirrors config.defaultMaxLineBytes; kept
// here too so a listener built directly (in tests) still gets a sane bound.
const defaultLogRelayMaxLine = 1 << 20 // 1 MiB

// logRelayDefaultHostKey is the JSON key under which host tags are emitted
// when enrich.host_key is omitted. config.Validate already applies this
// default; repeated here so a directly-constructed listener is also safe.
const logRelayDefaultHostKey = "host"

// sink is the destination a log_relay listener writes enriched NDJSON to.
// File sinks are closed on listener teardown; the stdout sink's Close is a
// no-op so the shared process stdout is never closed.
type sink interface {
	io.Writer
	Close() error
}

// refSink reference-counts a sink so a SIGHUP reload can swap a listener's
// live sink while an in-flight relay keeps writing to the old one (plan
// decision 9). The listener holds one reference; each in-flight handler
// acquires its own for the lifetime of the connection. The underlying sink
// is closed exactly once, when the last reference is released — so the old
// file fd is released only after the in-flight relay using it finishes,
// without ever closing it out from under that relay. stdout's Close is a
// no-op, so refcounting a stdout sink is harmless.
type refSink struct {
	s  sink
	mu sync.Mutex
	n  int
}

func newRefSink(s sink) *refSink { return &refSink{s: s, n: 1} }

// acquire takes an additional reference and returns the underlying sink.
func (r *refSink) acquire() sink {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	return r.s
}

// release drops one reference, closing the underlying sink when the last
// reference goes away.
func (r *refSink) release() {
	r.mu.Lock()
	r.n--
	last := r.n == 0
	r.mu.Unlock()
	if last {
		_ = r.s.Close()
	}
}

// stdoutSinkWriter is where output:stdout listeners write. Package-level so
// tests can redirect it (mirroring shuttleDrainTimeout); production leaves
// it at os.Stdout.
var stdoutSinkWriter io.Writer = os.Stdout

// stdoutSink wraps the process stdout (or a test writer). Its Close is a
// no-op: a listener must never close stdout out from under the daemon.
type stdoutSink struct{ w io.Writer }

func (s stdoutSink) Write(p []byte) (int, error) { return s.w.Write(p) }
func (stdoutSink) Close() error                  { return nil }

// openSink opens the configured sink. The file is opened append-only so
// concurrent or restarted writers never truncate prior log content.
func openSink(cfg config.LogRelayListener) (sink, error) {
	switch cfg.Output {
	case config.LogRelayOutputFile:
		f, err := os.OpenFile(
			cfg.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
		if err != nil {
			return nil, err
		}
		return f, nil
	case config.LogRelayOutputStdout:
		return stdoutSink{w: stdoutSinkWriter}, nil
	default:
		return nil, fmt.Errorf("unknown output %q", cfg.Output)
	}
}

// enrichConfig is the precomputed, per-listener host enrichment. cid toggles
// the top-level "cid" field; hostObjJSON is the marshaled host-tags object
// (nil when there are no tags) emitted under the key whose JSON-quoted form is
// hostKeyJSON. The enclave's own payload is never decoded, so values (int64s,
// key order) survive intact.
type enrichConfig struct {
	cid         bool
	hostKeyJSON []byte
	hostObjJSON []byte
}

// newEnrichConfig builds the per-listener enrichment from config. Returns
// nil (pass-through, no enrichment) when the config has no enrich block.
func newEnrichConfig(e *config.LogRelayEnrich) (*enrichConfig, error) {
	if e == nil {
		return nil, nil
	}
	hostKey := e.HostKey
	if hostKey == "" {
		hostKey = logRelayDefaultHostKey
	}
	// Marshal the key as JSON (not strconv.Quote) so an operator-supplied
	// host_key with control bytes still yields well-formed NDJSON.
	hostKeyJSON, err := json.Marshal(hostKey)
	if err != nil {
		return nil, fmt.Errorf("enrich.host_key: %w", err)
	}
	ec := &enrichConfig{cid: e.CID, hostKeyJSON: hostKeyJSON}
	if len(e.Tags) > 0 {
		// json.Marshal sorts map keys, giving deterministic output bytes.
		b, err := json.Marshal(e.Tags)
		if err != nil {
			return nil, fmt.Errorf("enrich.tags: %w", err)
		}
		ec.hostObjJSON = b
	}
	return ec, nil
}

// buildPrefix returns the host fields to splice in after a record's opening
// brace, e.g. `"cid":16,"host":{"region":"us-east-1"},`. The trailing comma
// lets it sit directly before the record's first original field; the
// empty-object splice path drops it. Returns empty when nothing is enabled.
func (ec *enrichConfig) buildPrefix(cid uint32) []byte {
	var b []byte
	if ec.cid {
		b = append(b, `"cid":`...)
		b = strconv.AppendUint(b, uint64(cid), 10)
		b = append(b, ',')
	}
	if len(ec.hostObjJSON) > 0 {
		b = append(b, ec.hostKeyJSON...)
		b = append(b, ':')
		b = append(b, ec.hostObjJSON...)
		b = append(b, ',')
	}
	return b
}

// enricher applies one connection's host enrichment to each input line.
// active distinguishes a configured-but-empty enrichment (which still wraps
// non-object lines as raw records) from no enrichment at all (pass-through).
type enricher struct {
	active bool
	prefix []byte
}

func (l *listener) newEnricher(cid uint32) enricher {
	ec := l.enrich.Load()
	if ec == nil {
		return enricher{active: false}
	}
	return enricher{active: true, prefix: ec.buildPrefix(cid)}
}

// emit returns the enriched output line (without trailing newline). A valid
// JSON object gets the host prefix spliced after its opening brace,
// preserving every original byte; anything else (non-object, or a truncated
// over-long line) is wrapped as a raw record so output is always
// well-formed NDJSON. With no enrichment and no truncation the line passes
// through unmodified.
func (e enricher) emit(line []byte, truncated bool) []byte {
	if truncated {
		return wrapRaw(e.prefix, line, true)
	}
	if !e.active {
		return line
	}
	if isJSONObject(line) {
		return spliceObject(e.prefix, line)
	}
	return wrapRaw(e.prefix, line, false)
}

// isJSONObject reports whether line is a JSON object (leading '{' and valid
// JSON as a whole). Arrays, scalars, and malformed input are not objects.
func isJSONObject(line []byte) bool {
	t := bytes.TrimLeft(line, " \t\r\n")
	if len(t) == 0 || t[0] != '{' {
		return false
	}
	return json.Valid(line)
}

// spliceObject inserts prefix immediately after the object's opening brace,
// copying the rest of the record verbatim. For an empty object ({}) the
// trailing comma in prefix is dropped so the result stays valid.
func spliceObject(prefix, line []byte) []byte {
	if len(prefix) == 0 {
		return line
	}
	open := bytes.IndexByte(line, '{')
	rest := line[open+1:]
	out := make([]byte, 0, len(line)+len(prefix))
	out = append(out, line[:open+1]...)
	if trimmed := bytes.TrimLeft(rest, " \t\r\n"); len(trimmed) > 0 &&
		trimmed[0] == '}' {
		// Empty object: drop the prefix's trailing comma.
		out = append(out, prefix[:len(prefix)-1]...)
	} else {
		out = append(out, prefix...)
	}
	out = append(out, rest...)
	return out
}

// wrapRaw produces a raw record carrying the original (or truncated) text in
// "msg". prefix, when non-empty, already ends with a comma.
func wrapRaw(prefix, line []byte, truncated bool) []byte {
	msg, _ := json.Marshal(string(line))
	var b []byte
	b = append(b, '{')
	b = append(b, prefix...)
	b = append(b, `"type":"raw","msg":`...)
	b = append(b, msg...)
	if truncated {
		b = append(b, `,"truncated":true`...)
	}
	b = append(b, '}')
	return b
}

func newLogRelayListener(
	cfg config.LogRelayListener, s *Server,
) (*listener, error) {
	l := &listener{
		port:   cfg.Port,
		mode:   modeLogRelay,
		server: s,
		done:   make(chan struct{}),
	}
	l.maxLineBytes = cfg.MaxLineBytes
	if l.maxLineBytes <= 0 {
		l.maxLineBytes = defaultLogRelayMaxLine
	}
	ec, err := newEnrichConfig(cfg.Enrich)
	if err != nil {
		return nil, err
	}
	l.enrich.Store(ec)
	sk, err := openSink(cfg)
	if err != nil {
		s.metric.LogRelayErrors.
			WithLabelValues(metrics.LogRelayErrorSink).Inc()
		return nil, err
	}
	l.sink.Store(newRefSink(sk))
	return l, nil
}

// closeSink releases the listener's reference to its sink if one is set.
// Safe to call on non-log_relay listeners (no sink stored). Swap(nil) makes
// it idempotent: only the first caller gets the ref and releases it, and an
// in-flight relay holding its own reference keeps the underlying sink open
// until it finishes.
func (l *listener) closeSink() {
	if rp := l.sink.Swap(nil); rp != nil {
		rp.release()
	}
}

// handleLogRelay reads the connection's NDJSON stream line-by-line, enriches
// each record with the peer CID and host tags, and writes the result to the
// listener's sink. There is no upstream dial, no allowlist, and no per-CID
// auth: like vsock_to_tcp, the listener trusts every peer the port accepts.
//
// The sink and enrichment are snapshotted once for the lifetime of this
// connection so a concurrent reload swap only affects later connections
// (plan decision 9).
func (l *listener) handleLogRelay(ctx context.Context, c vsockconn.Conn) {
	_ = ctx
	defer c.Close()
	l.server.trackConn(c)
	defer l.server.untrackConn(c)
	l.server.metric.LogRelayConnections.Inc()

	cid := c.PeerCID()
	enr := l.newEnricher(cid)
	maxLine := l.maxLineBytes
	if maxLine <= 0 {
		maxLine = defaultLogRelayMaxLine
	}

	// Acquire a reference for this connection's lifetime so a concurrent
	// reload that swaps (or removes) the listener's sink does not close the
	// fd this relay is still writing to (plan decision 9). The matching
	// release runs after the loop returns.
	rp := l.sink.Load()
	if rp == nil {
		return
	}
	sk := rp.acquire()
	defer rp.release()

	// +1 so ReadSlice can hold a full max-length line *and* its '\n'
	// delimiter: a line whose content is exactly maxLine bytes must be
	// accepted, not flagged truncated.
	br := bufio.NewReaderSize(c, maxLine+1)
	var out bytes.Buffer
	for {
		line, err := br.ReadSlice('\n')
		// ErrBufferFull means the line exceeded maxLine before a newline was
		// seen: emit what we have, flagged truncated, then drain the rest of
		// the line so the next record is read cleanly (never wedge or drop).
		truncated := errors.Is(err, bufio.ErrBufferFull)

		if len(line) > 0 {
			payload := line
			if !truncated {
				payload = dropNewline(line)
			}
			out.Reset()
			out.Write(enr.emit(payload, truncated))
			out.WriteByte('\n')
			n, werr := sk.Write(out.Bytes())
			l.server.metric.LogRelayBytes.Add(float64(n))
			if werr != nil {
				l.server.logger.Warn("log_relay sink write failed",
					"port", l.port, "cid", cid, "err", werr)
				return
			}
			l.server.metric.LogRelayLines.Inc()
		}

		if truncated {
			l.server.metric.LogRelayErrors.
				WithLabelValues(metrics.LogRelayErrorLineTooLong).Inc()
			if derr := discardToNewline(br); derr != nil {
				if !errors.Is(derr, io.EOF) {
					l.server.metric.LogRelayErrors.
						WithLabelValues(metrics.LogRelayErrorRead).Inc()
					l.server.logger.Warn("log_relay read error",
						"port", l.port, "cid", cid, "err", derr)
				}
				return
			}
			continue
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				l.server.metric.LogRelayErrors.
					WithLabelValues(metrics.LogRelayErrorRead).Inc()
				l.server.logger.Warn("log_relay read error",
					"port", l.port, "cid", cid, "err", err)
			}
			return
		}
	}
}

// dropNewline trims a trailing "\n" (and an accompanying "\r") so the
// enriched record carries the line content without its delimiter.
func dropNewline(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
		if n = len(b); n > 0 && b[n-1] == '\r' {
			b = b[:n-1]
		}
	}
	return b
}

// discardToNewline reads and discards bytes until the next newline (or
// stream end), letting the reader resync after an over-long line.
func discardToNewline(br *bufio.Reader) error {
	for {
		_, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return err
	}
}
