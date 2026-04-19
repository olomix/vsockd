// Package inbound implements the host-side TCP listeners that sniff a
// hostname out of the first bytes of a connection and forward the raw stream
// (including those first bytes) to an enclave's vsock endpoint.
package inbound

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// TLS record and handshake constants from RFC 5246 §6.2 and RFC 8446 §5.
const (
	tlsRecordHeaderLen      = 5
	tlsContentTypeHandshake = 0x16
	tlsHandshakeClientHello = 0x01

	// RFC 5246 caps TLSPlaintext.fragment at 2^14 bytes. Anything larger is
	// rejected before we allocate a buffer for it.
	maxTLSRecordFragment = 1 << 14

	// server_name extension, RFC 6066 §3.
	tlsExtServerName   uint16 = 0x0000
	tlsSNINameTypeHost byte   = 0x00
)

// SniffSNI reads a single TLS ClientHello record from r and returns the
// server_name from the SNI extension (lower-cased — SNI is case-insensitive
// per RFC 6066 §3). The raw bytes actually consumed are returned in
// buffered so the caller can replay them to the upstream vsock connection;
// vsockd does transparent TLS passthrough and never terminates TLS.
//
// SniffSNI reads at most 5 + 16384 bytes before returning. A record that
// declares a larger fragment is rejected without allocation.
func SniffSNI(r io.Reader) (host string, buffered []byte, err error) {
	var header [tlsRecordHeaderLen]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return "", nil, fmt.Errorf("read TLS record header: %w", err)
	}
	if header[0] != tlsContentTypeHandshake {
		return "", nil, fmt.Errorf(
			"not a TLS handshake record: content type 0x%02x", header[0])
	}

	fragLen := int(binary.BigEndian.Uint16(header[3:5]))
	if fragLen == 0 {
		return "", nil, errors.New("empty TLS record fragment")
	}
	if fragLen > maxTLSRecordFragment {
		return "", nil, fmt.Errorf(
			"TLS record fragment %d exceeds max %d",
			fragLen, maxTLSRecordFragment)
	}

	buf := make([]byte, tlsRecordHeaderLen+fragLen)
	copy(buf, header[:])
	if _, err := io.ReadFull(r, buf[tlsRecordHeaderLen:]); err != nil {
		return "", nil, fmt.Errorf("read TLS record fragment: %w", err)
	}

	host, err = parseClientHelloSNI(buf[tlsRecordHeaderLen:])
	if err != nil {
		return "", nil, err
	}
	return host, buf, nil
}

// parseClientHelloSNI walks a ClientHello handshake message and returns the
// first host_name entry from its server_name extension. data begins at the
// handshake header (HandshakeType + 24-bit length).
func parseClientHelloSNI(data []byte) (string, error) {
	if len(data) < 4 {
		return "", errors.New("handshake header truncated")
	}
	if data[0] != tlsHandshakeClientHello {
		return "", fmt.Errorf(
			"expected ClientHello (type %d), got type %d",
			tlsHandshakeClientHello, data[0])
	}
	bodyLen := int(data[1])<<16 | int(data[2])<<8 | int(data[3])
	body := data[4:]
	if bodyLen > len(body) {
		return "", errors.New("ClientHello body truncated")
	}
	body = body[:bodyLen]

	// ClientHello layout (RFC 8446 §4.1.2):
	//   legacy_version (2) | random (32) | legacy_session_id<0..32>
	//   | cipher_suites<2..2^16-2> | legacy_compression_methods<1..2^8-1>
	//   | extensions<8..2^16-1>
	if len(body) < 2+32 {
		return "", errors.New("ClientHello truncated before session_id")
	}
	body = body[2+32:]

	sid, rest, err := readVec8(body)
	if err != nil {
		return "", fmt.Errorf("session_id: %w", err)
	}
	_ = sid
	body = rest

	cs, rest, err := readVec16(body)
	if err != nil {
		return "", fmt.Errorf("cipher_suites: %w", err)
	}
	_ = cs
	body = rest

	cm, rest, err := readVec8(body)
	if err != nil {
		return "", fmt.Errorf("compression_methods: %w", err)
	}
	_ = cm
	body = rest

	exts, rest, err := readVec16(body)
	if err != nil {
		return "", fmt.Errorf("extensions: %w", err)
	}
	_ = rest

	for len(exts) >= 4 {
		extType := binary.BigEndian.Uint16(exts[:2])
		extLen := int(binary.BigEndian.Uint16(exts[2:4]))
		exts = exts[4:]
		if extLen > len(exts) {
			return "", errors.New("extension truncated")
		}
		extData := exts[:extLen]
		exts = exts[extLen:]

		if extType != tlsExtServerName {
			continue
		}
		return parseSNIExtension(extData)
	}

	return "", errors.New("server_name extension not present")
}

// parseSNIExtension returns the first host_name from a ServerNameList
// (RFC 6066 §3). Non-host_name entries are skipped.
func parseSNIExtension(data []byte) (string, error) {
	if len(data) < 2 {
		return "", errors.New("server_name extension too short")
	}
	listLen := int(binary.BigEndian.Uint16(data[:2]))
	list := data[2:]
	if listLen > len(list) {
		return "", errors.New("ServerNameList truncated")
	}
	list = list[:listLen]

	for len(list) >= 3 {
		nameType := list[0]
		nameLen := int(binary.BigEndian.Uint16(list[1:3]))
		list = list[3:]
		if nameLen > len(list) {
			return "", errors.New("server_name entry truncated")
		}
		name := list[:nameLen]
		list = list[nameLen:]

		if nameType != tlsSNINameTypeHost {
			continue
		}
		if nameLen == 0 {
			return "", errors.New("empty host_name")
		}
		return strings.ToLower(string(name)), nil
	}

	return "", errors.New("no host_name in server_name extension")
}

// readVec8 reads a TLS-style vector prefixed with a single length byte.
func readVec8(b []byte) (vec, rest []byte, err error) {
	if len(b) < 1 {
		return nil, nil, errors.New("missing 1-byte length")
	}
	n := int(b[0])
	if n > len(b)-1 {
		return nil, nil, errors.New("vector truncated")
	}
	return b[1 : 1+n], b[1+n:], nil
}

// readVec16 reads a TLS-style vector prefixed with a 2-byte big-endian length.
func readVec16(b []byte) (vec, rest []byte, err error) {
	if len(b) < 2 {
		return nil, nil, errors.New("missing 2-byte length")
	}
	n := int(binary.BigEndian.Uint16(b[:2]))
	if n > len(b)-2 {
		return nil, nil, errors.New("vector truncated")
	}
	return b[2 : 2+n], b[2+n:], nil
}
