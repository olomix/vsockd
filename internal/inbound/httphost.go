package inbound

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"strings"
)

// maxHTTPHeaderBytes caps how many bytes SniffHost will consume from a
// connection while looking for the end of the HTTP request headers. Go's
// net/http uses 1 MiB by default; 8 KiB is plenty for a Host-header sniff and
// keeps us far below the point where a misbehaving client can exhaust memory.
const maxHTTPHeaderBytes = 8 * 1024

// SniffHost reads an HTTP/1.x request line and header block from r, returns
// the value of the Host header (lower-cased, with any ":port" suffix
// stripped), and the raw bytes consumed from r so the caller can replay them
// verbatim to the upstream vsock connection.
//
// At most maxHTTPHeaderBytes bytes are read. A connection whose header block
// is larger than that, or whose request line/headers are malformed, or whose
// Host header is missing or empty, returns an error. In the error case the
// bytes already read are still returned in buffered so callers can log or
// forward them to a 400 handler if desired.
func SniffHost(r io.Reader) (host string, buffered []byte, err error) {
	var buf bytes.Buffer
	// LimitedReader lets us distinguish "oversized headers" (N hit 0) from a
	// short upstream EOF.
	lr := &io.LimitedReader{R: r, N: maxHTTPHeaderBytes}
	tee := io.TeeReader(lr, &buf)
	br := bufio.NewReader(tee)
	tp := textproto.NewReader(br)

	requestLine, err := tp.ReadLine()
	if err != nil {
		return "", buf.Bytes(), wrapReadErr("read request line", err, lr)
	}
	if err := validateRequestLine(requestLine); err != nil {
		return "", buf.Bytes(), err
	}

	hdr, err := tp.ReadMIMEHeader()
	if err != nil {
		return "", buf.Bytes(), wrapReadErr("read headers", err, lr)
	}

	host = hdr.Get("Host")
	if host == "" {
		return "", buf.Bytes(), errors.New("missing Host header")
	}
	host = stripPort(strings.TrimSpace(host))
	if host == "" {
		return "", buf.Bytes(), errors.New("empty Host header value")
	}
	return strings.ToLower(host), buf.Bytes(), nil
}

func wrapReadErr(op string, err error, lr *io.LimitedReader) error {
	if lr.N == 0 {
		return fmt.Errorf(
			"HTTP headers exceed %d bytes", maxHTTPHeaderBytes)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// validateRequestLine rejects anything that is not a plausible HTTP/1.x
// request line: "METHOD SP request-target SP HTTP/X.Y". We do not parse the
// method or target further — the point is to fail fast on garbage (e.g. a
// mis-routed TLS ClientHello hitting an http-host listener).
func validateRequestLine(line string) error {
	parts := strings.Split(line, " ")
	if len(parts) != 3 {
		return fmt.Errorf("malformed request line: %q", line)
	}
	if parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("malformed request line: %q", line)
	}
	if !strings.HasPrefix(parts[2], "HTTP/") {
		return fmt.Errorf(
			"malformed request line: bad HTTP version %q", parts[2])
	}
	return nil
}

// stripPort removes a trailing ":port" from a Host header value. IPv6
// literals are bracketed per RFC 7230 §5.4 ("[::1]:8080"); we strip the
// brackets as well so downstream comparisons match the unbracketed form.
func stripPort(host string) string {
	if strings.HasPrefix(host, "[") {
		if end := strings.IndexByte(host, ']'); end >= 0 {
			return host[1:end]
		}
		return host
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		return host[:i]
	}
	return host
}
