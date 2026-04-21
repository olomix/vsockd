package inbound

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// iterReader hands out the underlying bytes one Read at a time using chunks
// of the given size. It exercises readers that do not deliver the whole
// input in a single call, which is how real TCP connections behave.
type iterReader struct {
	data  []byte
	chunk int
}

func (r *iterReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := r.chunk
	if n <= 0 || n > len(r.data) {
		n = len(r.data)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func TestSniffHost_NormalRequest(t *testing.T) {
	raw := "GET /path HTTP/1.1\r\n" +
		"Host: api.example.com\r\n" +
		"User-Agent: test\r\n" +
		"\r\n"
	got, buffered, err := SniffHost(bytes.NewReader([]byte(raw)))
	if err != nil {
		t.Fatalf("SniffHost: %v", err)
	}
	if got != "api.example.com" {
		t.Errorf("host = %q, want %q", got, "api.example.com")
	}
	if string(buffered) != raw {
		t.Errorf("buffered = %q, want %q", buffered, raw)
	}
}

func TestSniffHost_ChunkedReader(t *testing.T) {
	// Same input, but delivered one byte at a time — exercises the
	// TeeReader + LimitedReader path under realistic network conditions.
	raw := "POST /x HTTP/1.1\r\nHost: svc.example.org\r\n\r\nbody"
	got, buffered, err := SniffHost(&iterReader{data: []byte(raw), chunk: 1})
	if err != nil {
		t.Fatalf("SniffHost: %v", err)
	}
	if got != "svc.example.org" {
		t.Errorf("host = %q", got)
	}
	// Buffered should at minimum cover the header block; it may also include
	// body bytes because bufio pre-fetches. Either way it must be a prefix
	// of the input.
	if !strings.HasPrefix(raw, string(buffered)) {
		t.Errorf("buffered is not a prefix of raw input")
	}
	if !bytes.Contains(buffered, []byte("\r\n\r\n")) {
		t.Errorf("buffered missing end-of-headers")
	}
}

func TestSniffHost_HeaderCaseVariations(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "canonical",
			raw:  "GET / HTTP/1.1\r\nHost: API.Example.com\r\n\r\n",
			want: "api.example.com",
		},
		{
			name: "lowercase header name",
			raw:  "GET / HTTP/1.1\r\nhost: api.example.com\r\n\r\n",
			want: "api.example.com",
		},
		{
			name: "uppercase header name",
			raw:  "GET / HTTP/1.1\r\nHOST: api.example.com\r\n\r\n",
			want: "api.example.com",
		},
		{
			name: "mixed case host value",
			raw:  "GET / HTTP/1.1\r\nHost: ApI.ExAmPlE.CoM\r\n\r\n",
			want: "api.example.com",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := SniffHost(strings.NewReader(tc.raw))
			if err != nil {
				t.Fatalf("SniffHost: %v", err)
			}
			if got != tc.want {
				t.Errorf("host = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSniffHost_ExplicitPortStripped(t *testing.T) {
	cases := []struct {
		hostHeader string
		want       string
	}{
		{"api.example.com:8080", "api.example.com"},
		{"api.example.com:443", "api.example.com"},
		{"[::1]:8080", "::1"},
		{"[fe80::1]:443", "fe80::1"},
		{"api.example.com", "api.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.hostHeader, func(t *testing.T) {
			raw := "GET / HTTP/1.1\r\nHost: " + tc.hostHeader + "\r\n\r\n"
			got, _, err := SniffHost(strings.NewReader(raw))
			if err != nil {
				t.Fatalf("SniffHost: %v", err)
			}
			if got != tc.want {
				t.Errorf("host = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSniffHost_OversizedHeaders(t *testing.T) {
	// Build a header block larger than maxHTTPHeaderBytes by padding a
	// single header value. No terminating CRLFCRLF is written — we want the
	// reader to hit the size limit, not an EOF on a well-formed block.
	var buf bytes.Buffer
	buf.WriteString("GET / HTTP/1.1\r\nHost: api.example.com\r\nX-Pad: ")
	buf.Write(bytes.Repeat([]byte("A"), maxHTTPHeaderBytes))
	buf.WriteString("\r\n\r\n")
	_, _, err := SniffHost(&buf)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "exceed") {
		t.Errorf("error = %v; want size-limit error", err)
	}
}

func TestSniffHost_MissingHostHeader(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nUser-Agent: test\r\n\r\n"
	_, _, err := SniffHost(strings.NewReader(raw))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "missing Host") {
		t.Errorf("error = %v; want missing Host", err)
	}
}

func TestSniffHost_EmptyHostValue(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nHost: \r\n\r\n"
	_, _, err := SniffHost(strings.NewReader(raw))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Host") {
		t.Errorf("error = %v; want Host-related error", err)
	}
}

func TestSniffHost_MalformedRequestLine(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "two tokens",
			raw:  "GET /\r\nHost: x\r\n\r\n",
		},
		{
			name: "no HTTP version prefix",
			raw:  "GET / FOO/1.1\r\nHost: x\r\n\r\n",
		},
		{
			name: "empty request line",
			raw:  "\r\nHost: x\r\n\r\n",
		},
		{
			name: "garbage (binary TLS-ish)",
			raw:  "\x16\x03\x01\x00\x10 garbage\r\n\r\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := SniffHost(strings.NewReader(tc.raw))
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), "request line") {
				t.Errorf("error = %v; want request-line error", err)
			}
		})
	}
}

func TestSniffHost_TruncatedBeforeBlankLine(t *testing.T) {
	// Valid request line and Host header, but no blank line terminator
	// before EOF. Must surface as a read error, not a success.
	raw := "GET / HTTP/1.1\r\nHost: api.example.com\r\n"
	_, _, err := SniffHost(strings.NewReader(raw))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "headers") {
		t.Errorf("error = %v; want headers-read error", err)
	}
}

func TestSniffHost_ConnectRequest(t *testing.T) {
	// CONNECT uses authority-form in the request-target; the Host header
	// should still be honoured.
	raw := "CONNECT api.example.com:443 HTTP/1.1\r\n" +
		"Host: api.example.com:443\r\n\r\n"
	got, _, err := SniffHost(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("SniffHost: %v", err)
	}
	if got != "api.example.com" {
		t.Errorf("host = %q, want %q", got, "api.example.com")
	}
}
