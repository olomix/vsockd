// Package allowlist matches outbound destinations against a list of patterns.
//
// Pattern grammar (matches what internal/config.validatePattern accepts):
//
//   - "*"                    universal: any host, any port
//   - "*:<port>"             any host on an exact port
//   - "host:<port>"          exact host (case-insensitive) on an exact port
//   - "*.suffix:<port>"      any strict sub-domain of suffix on an exact port
//     (does NOT match the suffix itself, mirroring
//     TLS wildcard-certificate semantics)
//
// Host comparisons are lower-cased. Port must match exactly unless the
// pattern is the universal "*".
package allowlist

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Matcher is an immutable compiled set of allowlist patterns.
type Matcher struct {
	patterns []pattern
}

type patternKind int

const (
	kindUniversal patternKind = iota // "*"
	kindAnyHost                      // "*:port"
	kindHostSuffix                   // "*.example.com:port"
	kindExact                        // "host:port"
)

type pattern struct {
	kind patternKind
	// suffix includes the leading '.' for kindHostSuffix, e.g. ".example.com".
	// host is the normalised lowercase host for kindExact.
	// port is 0 only for kindUniversal.
	suffix string
	host   string
	port   int
}

// New parses and validates every pattern. An empty slice is rejected because
// a zero-pattern matcher denies everything and is almost always a mistake;
// callers that truly want deny-all should pass no matcher at all.
func New(patterns []string) (*Matcher, error) {
	if len(patterns) == 0 {
		return nil, errors.New("at least one pattern required")
	}
	m := &Matcher{patterns: make([]pattern, 0, len(patterns))}
	for i, raw := range patterns {
		p, err := parsePattern(raw)
		if err != nil {
			return nil, fmt.Errorf("patterns[%d]: %w", i, err)
		}
		m.patterns = append(m.patterns, p)
	}
	return m, nil
}

// Allow reports whether (host, port) is permitted by any pattern. A nil
// Matcher denies everything.
func (m *Matcher) Allow(host string, port int) bool {
	if m == nil {
		return false
	}
	if port < 1 || port > 65535 {
		return false
	}
	h := strings.ToLower(host)
	for _, p := range m.patterns {
		if p.match(h, port) {
			return true
		}
	}
	return false
}

func (p pattern) match(host string, port int) bool {
	switch p.kind {
	case kindUniversal:
		return true
	case kindAnyHost:
		return port == p.port
	case kindExact:
		return port == p.port && host == p.host
	case kindHostSuffix:
		return port == p.port && strings.HasSuffix(host, p.suffix)
	}
	return false
}

func parsePattern(raw string) (pattern, error) {
	if raw == "" {
		return pattern{}, errors.New("empty pattern")
	}
	if raw == "*" {
		return pattern{kind: kindUniversal}, nil
	}

	host, portStr, err := net.SplitHostPort(raw)
	if err != nil {
		return pattern{}, fmt.Errorf("pattern %q not host:port or *", raw)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return pattern{}, fmt.Errorf("pattern %q: invalid port %q", raw, portStr)
	}

	host = strings.ToLower(host)
	switch {
	case host == "":
		return pattern{}, fmt.Errorf("pattern %q: host must not be empty", raw)
	case host == "*":
		return pattern{kind: kindAnyHost, port: port}, nil
	case strings.HasPrefix(host, "*."):
		rest := host[2:]
		if rest == "" || strings.Contains(rest, "*") {
			return pattern{}, fmt.Errorf(
				"pattern %q: wildcard must be '*.' followed by a host", raw)
		}
		return pattern{kind: kindHostSuffix, suffix: "." + rest, port: port}, nil
	case strings.Contains(host, "*"):
		return pattern{}, fmt.Errorf(
			"pattern %q: '*' only allowed as leading '*.'", raw)
	default:
		return pattern{kind: kindExact, host: host, port: port}, nil
	}
}
