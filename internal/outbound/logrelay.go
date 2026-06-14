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

// relayState bundles a log_relay listener's live sink and enrichment so a
// reload swaps them as one atomic unit. Holding them in two separate
// atomic.Pointers let a connection accepted mid-swap pair a new sink with old
// enrichment (or vice versa); a single pointer makes the pair indivisible
// (plan decision 9).
type relayState struct {
	sink   *refSink
	enrich *enrichConfig
	// maxLineBytes lives here (not on the listener) so a same-port reload that
	// only changes max_line_bytes takes effect: the swap installs a new
	// relayState and new connections read the limit from the snapshot.
	maxLineBytes int
}

// acquire takes an additional reference and returns the underlying sink. It
// reports false if the refSink was already released to zero (a concurrent
// reload closed the underlying fd); the caller must then re-Load the listener's
// current refSink rather than resurrect this dead one and write to a closed fd.
func (r *refSink) acquire() (sink, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == 0 {
		return nil, false
	}
	r.n++
	return r.s, true
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
// key order) survive intact. reserved holds the top-level key names this
// config adds ("cid" and/or the host key) so emit can detect a record that
// already carries one of them (see hasReservedTopLevelKey).
type enrichConfig struct {
	cid         bool
	hostKeyJSON []byte
	hostObjJSON []byte
	reserved    map[string]struct{}
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
	reserved := make(map[string]struct{})
	if e.CID {
		reserved["cid"] = struct{}{}
	}
	if len(e.Tags) > 0 {
		// json.Marshal sorts map keys, giving deterministic output bytes.
		b, err := json.Marshal(e.Tags)
		if err != nil {
			return nil, fmt.Errorf("enrich.tags: %w", err)
		}
		ec.hostObjJSON = b
		reserved[hostKey] = struct{}{}
	}
	if len(reserved) > 0 {
		ec.reserved = reserved
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
// reserved is the set of top-level keys the prefix adds, used to detect a
// record that would collide with them.
type enricher struct {
	active   bool
	prefix   []byte
	reserved map[string]struct{}
}

func newEnricher(ec *enrichConfig, cid uint32) enricher {
	if ec == nil {
		return enricher{active: false}
	}
	return enricher{
		active:   true,
		prefix:   ec.buildPrefix(cid),
		reserved: ec.reserved,
	}
}

// emit returns the enriched output line (without trailing newline). When
// enrichment is active a valid JSON object gets the host prefix spliced after
// its opening brace, preserving every original byte, and a non-object line is
// wrapped as a raw record, so every emitted line is well-formed NDJSON. With
// no enrichment a non-truncated line passes through verbatim (so NDJSON
// well-formedness then depends on the producer). A truncated over-long line is
// always wrapped as a raw record carrying the truncated marker, regardless of
// enrichment, so truncation is never silent.
func (e enricher) emit(line []byte, truncated bool) []byte {
	if truncated {
		return wrapRaw(e.prefix, line, true)
	}
	if !e.active {
		return line
	}
	if isJSONObject(line) {
		// A record that already declares a top-level key the host adds (its
		// own "cid", or the host key) would yield duplicate top-level keys
		// after splicing. Many JSON parsers keep the last value, so the
		// enclave could shadow the host's authoritative "cid" and defeat the
		// un-spoofable guarantee. Wrap such records as raw instead: the host
		// fields stay at the top level un-shadowed and the original bytes are
		// preserved verbatim inside "msg".
		if len(e.reserved) > 0 && hasReservedTopLevelKey(line, e.reserved) {
			return wrapRaw(e.prefix, line, false)
		}
		return spliceObject(e.prefix, line)
	}
	return wrapRaw(e.prefix, line, false)
}

// hasReservedTopLevelKey reports whether the already-valid JSON object in line
// declares any top-level key in reserved. Only top-level keys count; an
// identically named key nested inside a value object is harmless. The scan
// tracks brace/bracket depth so nested keys are skipped.
func hasReservedTopLevelKey(line []byte, reserved map[string]struct{}) bool {
	dec := json.NewDecoder(bytes.NewReader(line))
	// Decode numbers as json.Number, not float64: a syntactically valid but
	// float64-overflowing number (e.g. 1e1000) would otherwise make Token()
	// fail mid-scan, aborting before a later reserved key is seen and letting
	// a spoofed key slip through into spliceObject.
	dec.UseNumber()
	// Consume the opening brace of the (already-validated) object.
	if _, err := dec.Token(); err != nil {
		return false
	}
	depth := 1
	expectKey := true
	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
				expectKey = false
			case '}', ']':
				depth--
				if depth == 0 {
					return false
				}
				expectKey = depth == 1
			}
			continue
		}
		if depth != 1 {
			continue
		}
		if expectKey {
			if s, ok := tok.(string); ok {
				if _, hit := reserved[s]; hit {
					return true
				}
			}
			expectKey = false
		} else {
			expectKey = true
		}
	}
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
	maxLine := cfg.MaxLineBytes
	if maxLine <= 0 {
		maxLine = defaultLogRelayMaxLine
	}
	ec, err := newEnrichConfig(cfg.Enrich)
	if err != nil {
		return nil, err
	}
	sk, err := openSink(cfg)
	if err != nil {
		s.metric.LogRelayErrors.
			WithLabelValues(metrics.LogRelayErrorSink).Inc()
		return nil, err
	}
	l.relay.Store(&relayState{
		sink: newRefSink(sk), enrich: ec, maxLineBytes: maxLine,
	})
	return l, nil
}

// closeSink releases the listener's reference to its sink if one is set.
// Safe to call on non-log_relay listeners (no sink stored). Swap(nil) makes
// it idempotent: only the first caller gets the ref and releases it, and an
// in-flight relay holding its own reference keeps the underlying sink open
// until it finishes.
func (l *listener) closeSink() {
	if st := l.relay.Swap(nil); st != nil {
		st.sink.release()
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

	// Snapshot sink and enrichment as one atomic pair and acquire a reference
	// for this connection's lifetime, so a concurrent reload that swaps (or
	// removes) them does not close the fd this relay is still writing to and
	// cannot split the pair (new sink with old enrichment, or vice versa) —
	// plan decision 9. The matching release runs after the loop returns.
	var st *relayState
	var sk sink
	for {
		st = l.relay.Load()
		if st == nil {
			return
		}
		var ok bool
		if sk, ok = st.sink.acquire(); ok {
			break
		}
		// A concurrent reload released this sink to zero between Load and
		// acquire; re-Load the now-current state and retry.
	}
	defer st.sink.release()
	enr := newEnricher(st.enrich, cid)
	maxLine := st.maxLineBytes
	if maxLine <= 0 {
		maxLine = defaultLogRelayMaxLine
	}

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
			} else if len(payload) > maxLine {
				// ReadSlice fills the maxLine+1 buffer before flagging
				// ErrBufferFull, so the returned slice can carry one byte past
				// the limit. Trim it so the emitted record honors maxLine.
				payload = payload[:maxLine]
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
