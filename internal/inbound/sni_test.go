package inbound

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// recordClientHello drives a crypto/tls client against a net.Pipe so we can
// capture the wire bytes of a real ClientHello. The handshake is torn down
// immediately after the first record, which is all this test needs.
func recordClientHello(t *testing.T, serverName string) []byte {
	t.Helper()

	server, client := net.Pipe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer client.Close()
		tc := tls.Client(client, &tls.Config{
			ServerName:         serverName,
			InsecureSkipVerify: true,
		})
		// Handshake will fail once we close the server side, that is fine —
		// ClientHello has already been flushed by then.
		_ = tc.Handshake()
	}()

	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))

	var header [5]byte
	if _, err := io.ReadFull(server, header[:]); err != nil {
		t.Fatalf("read record header: %v", err)
	}
	fragLen := int(binary.BigEndian.Uint16(header[3:5]))
	fragment := make([]byte, fragLen)
	if _, err := io.ReadFull(server, fragment); err != nil {
		t.Fatalf("read record fragment: %v", err)
	}
	_ = server.Close()
	<-done

	out := make([]byte, 0, len(header)+len(fragment))
	out = append(out, header[:]...)
	out = append(out, fragment...)
	return out
}

func TestSniffSNI_RealClientHello(t *testing.T) {
	cases := []string{
		"example.com",
		"api.example.com",
		"a.b.c.d.example.org",
		"API.Example.COM",
	}
	for _, sn := range cases {
		t.Run(sn, func(t *testing.T) {
			raw := recordClientHello(t, sn)
			got, buffered, err := SniffSNI(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("SniffSNI: %v", err)
			}
			want := strings.ToLower(sn)
			if got != want {
				t.Errorf("host = %q, want %q", got, want)
			}
			if !bytes.Equal(buffered, raw) {
				t.Errorf("buffered bytes not equal to input: len got=%d want=%d",
					len(buffered), len(raw))
			}
		})
	}
}

// buildClientHelloRecord assembles a TLS 1.2 record whose fragment is the
// ClientHello described by extensions. extensions is the raw bytes that go
// inside the extensions<2..2^16-1> vector (each extension already encoded).
// An empty extensions slice means "no extensions".
func buildClientHelloRecord(extensions []byte) []byte {
	body := make([]byte, 0, 64+len(extensions))
	body = append(body, 0x03, 0x03)           // legacy_version: TLS 1.2
	body = append(body, make([]byte, 32)...)  // random
	body = append(body, 0x00)                 // session_id<0..32>: empty
	body = append(body, 0x00, 0x02, 0x00, 0x2F) // cipher_suites: one, TLS_RSA_WITH_AES_128_CBC_SHA
	body = append(body, 0x01, 0x00)           // compression_methods: null
	body = append(body, byte(len(extensions)>>8), byte(len(extensions)))
	body = append(body, extensions...)

	hs := make([]byte, 0, 4+len(body))
	hs = append(hs, 0x01) // HandshakeType: client_hello
	hs = append(hs, byte(len(body)>>16), byte(len(body)>>8), byte(len(body)))
	hs = append(hs, body...)

	rec := make([]byte, 0, 5+len(hs))
	rec = append(rec, 0x16, 0x03, 0x01) // handshake, TLS 1.0 for record layer
	rec = append(rec, byte(len(hs)>>8), byte(len(hs)))
	rec = append(rec, hs...)
	return rec
}

func TestSniffSNI_NoSNIExtension(t *testing.T) {
	raw := buildClientHelloRecord(nil)
	_, _, err := SniffSNI(bytes.NewReader(raw))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "server_name extension") {
		t.Errorf("error = %v; want mention of server_name", err)
	}
}

func TestSniffSNI_WrongContentType(t *testing.T) {
	// Handshake-shaped bytes but content type 0x17 (application_data).
	raw := []byte{0x17, 0x03, 0x03, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00}
	_, _, err := SniffSNI(bytes.NewReader(raw))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not a TLS handshake") {
		t.Errorf("error = %v; want handshake content-type error", err)
	}
}

func TestSniffSNI_WrongHandshakeType(t *testing.T) {
	// Record says handshake but inner handshake type is ServerHello (0x02).
	body := []byte{0x02, 0x00, 0x00, 0x00}
	rec := []byte{0x16, 0x03, 0x03, byte(len(body) >> 8), byte(len(body))}
	rec = append(rec, body...)
	_, _, err := SniffSNI(bytes.NewReader(rec))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "ClientHello") {
		t.Errorf("error = %v; want ClientHello-type error", err)
	}
}

func TestSniffSNI_TruncatedHeader(t *testing.T) {
	raw := []byte{0x16, 0x03} // only 2 of 5 header bytes
	_, _, err := SniffSNI(bytes.NewReader(raw))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("error = %v; want wraps io.ErrUnexpectedEOF", err)
	}
}

func TestSniffSNI_TruncatedFragment(t *testing.T) {
	// Header says 100-byte fragment, supply 3 bytes only.
	raw := []byte{0x16, 0x03, 0x03, 0x00, 0x64, 0xaa, 0xbb, 0xcc}
	_, _, err := SniffSNI(bytes.NewReader(raw))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("error = %v; want wraps io.ErrUnexpectedEOF", err)
	}
}

func TestSniffSNI_OversizedRecord(t *testing.T) {
	// Declare a 16 KiB + 1 fragment; no body bytes need follow because the
	// length check trips before any further read.
	raw := []byte{0x16, 0x03, 0x03, 0x40, 0x01}
	_, _, err := SniffSNI(bytes.NewReader(raw))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds max") {
		t.Errorf("error = %v; want size-limit error", err)
	}
}

func TestSniffSNI_EmptyFragment(t *testing.T) {
	raw := []byte{0x16, 0x03, 0x03, 0x00, 0x00}
	_, _, err := SniffSNI(bytes.NewReader(raw))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error = %v; want empty-fragment error", err)
	}
}

func TestSniffSNI_SkipsNonHostNameEntry(t *testing.T) {
	// ServerNameList with one unknown-type entry followed by a host_name.
	var nameList bytes.Buffer
	// Unknown type 0x99, length 3, three payload bytes.
	nameList.WriteByte(0x99)
	nameList.Write([]byte{0x00, 0x03, 0xaa, 0xbb, 0xcc})
	// host_name "x.test"
	host := []byte("x.test")
	nameList.WriteByte(tlsSNINameTypeHost)
	nameList.Write([]byte{byte(len(host) >> 8), byte(len(host))})
	nameList.Write(host)

	// server_name extension = uint16 list length + nameList bytes.
	var extData bytes.Buffer
	extData.Write([]byte{byte(nameList.Len() >> 8), byte(nameList.Len())})
	extData.Write(nameList.Bytes())

	// Wrap in extension header: uint16 type + uint16 length + data.
	var exts bytes.Buffer
	exts.Write([]byte{0x00, 0x00}) // server_name type
	exts.Write([]byte{byte(extData.Len() >> 8), byte(extData.Len())})
	exts.Write(extData.Bytes())

	raw := buildClientHelloRecord(exts.Bytes())
	got, buffered, err := SniffSNI(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("SniffSNI: %v", err)
	}
	if got != "x.test" {
		t.Errorf("host = %q, want %q", got, "x.test")
	}
	if !bytes.Equal(buffered, raw) {
		t.Errorf("buffered mismatch")
	}
}

func TestSniffSNI_EmptyHostName(t *testing.T) {
	// server_name extension with a single host_name of zero length.
	var nameList bytes.Buffer
	nameList.WriteByte(tlsSNINameTypeHost)
	nameList.Write([]byte{0x00, 0x00})

	var extData bytes.Buffer
	extData.Write([]byte{byte(nameList.Len() >> 8), byte(nameList.Len())})
	extData.Write(nameList.Bytes())

	var exts bytes.Buffer
	exts.Write([]byte{0x00, 0x00})
	exts.Write([]byte{byte(extData.Len() >> 8), byte(extData.Len())})
	exts.Write(extData.Bytes())

	raw := buildClientHelloRecord(exts.Bytes())
	_, _, err := SniffSNI(bytes.NewReader(raw))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "empty host_name") {
		t.Errorf("error = %v; want empty host_name", err)
	}
}
