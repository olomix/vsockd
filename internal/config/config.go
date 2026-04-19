// Package config loads and validates the vsockd YAML configuration.
//
// The schema mirrors examples/vsockd.yaml. Load applies strict YAML parsing
// (unknown fields rejected) followed by Validate. All constraints called out
// in the implementation plan live here.
package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Inbound mode values; the YAML mode field must be one of these.
const (
	ModeHTTPHost = "http-host"
	ModeTLSSNI   = "tls-sni"
)

// Log format values; empty means the caller picks a default.
const (
	LogFormatJSON = "json"
	LogFormatText = "text"
	LogFormatAuto = "auto"
)

// VSOCK reserves CIDs 0..2 (hypervisor/local/host). Enclave CIDs start at 3.
const minCID uint32 = 3

// vsockPortAny is the reserved "any port" sentinel (vsock.PortAny). Config
// values equal to this are rejected; any other uint32 is a valid vsock port.
const vsockPortAny uint32 = 0xFFFFFFFF

// Config is the top-level vsockd configuration.
type Config struct {
	Inbound       []InboundListener  `yaml:"inbound"`
	Outbound      []OutboundListener `yaml:"outbound"`
	Metrics       MetricsConfig      `yaml:"metrics"`
	ShutdownGrace Duration           `yaml:"shutdown_grace"`
	LogFormat     string             `yaml:"log_format"`
}

// InboundListener terminates TCP on the host and routes to enclave vsocks.
type InboundListener struct {
	Bind   string  `yaml:"bind"`
	Port   int     `yaml:"port"`
	Mode   string  `yaml:"mode"`
	Routes []Route `yaml:"routes"`
}

// Route binds a hostname (HTTP Host header or TLS SNI) to an enclave endpoint.
type Route struct {
	Hostname  string `yaml:"hostname"`
	CID       uint32 `yaml:"cid"`
	VsockPort uint32 `yaml:"vsock_port"`
}

// OutboundListener accepts vsock connections on a single port; each allowed
// peer CID carries its own egress allowlist.
type OutboundListener struct {
	Port uint32        `yaml:"port"`
	CIDs []OutboundCID `yaml:"cids"`
}

// OutboundCID is the per-CID egress policy scoped to one outbound listener.
type OutboundCID struct {
	CID          uint32   `yaml:"cid"`
	AllowedHosts []string `yaml:"allowed_hosts"`
}

// MetricsConfig controls the Prometheus HTTP endpoint.
type MetricsConfig struct {
	Bind string `yaml:"bind"`
}

// Duration is a time.Duration that unmarshals from a human string like "30s".
type Duration time.Duration

// UnmarshalYAML parses a duration string; yaml.v3's default behaviour would
// take an integer nanosecond count, which is not what users expect here.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string: %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Duration returns the value as a standard time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// Load reads, strict-parses, and validates a YAML config file.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return &cfg, nil
}

// Validate enforces the semantic constraints from the implementation plan.
func (c *Config) Validate() error {
	if len(c.Inbound) == 0 && len(c.Outbound) == 0 {
		return errors.New("config has no inbound or outbound listeners")
	}

	// Two listeners sharing the same (bind, port) would collide at bind time
	// on first Start, and silently collapse during SIGHUP reloads where the
	// Apply keyed-existing-listener fast path treats duplicate keys as the
	// same listener.
	seenBind := make(map[string]int)
	for i := range c.Inbound {
		if err := c.Inbound[i].validate(); err != nil {
			return fmt.Errorf("inbound[%d]: %w", i, err)
		}
		key := fmt.Sprintf("%s|%d", c.Inbound[i].Bind, c.Inbound[i].Port)
		if prev, ok := seenBind[key]; ok {
			return fmt.Errorf(
				"inbound[%d]: duplicate bind %s:%d already declared in inbound[%d]",
				i, c.Inbound[i].Bind, c.Inbound[i].Port, prev)
		}
		seenBind[key] = i
	}

	// Cross-port uniqueness: a CID may appear under at most one outbound port.
	// Remember the owning port so the error message points to the conflict.
	cidOwner := make(map[uint32]uint32)
	seenPort := make(map[uint32]bool)

	for i := range c.Outbound {
		ob := &c.Outbound[i]
		if ob.Port == 0 || ob.Port >= vsockPortAny {
			return fmt.Errorf("outbound[%d]: port %d out of range", i, ob.Port)
		}
		if seenPort[ob.Port] {
			return fmt.Errorf("outbound[%d]: duplicate port %d", i, ob.Port)
		}
		seenPort[ob.Port] = true

		if len(ob.CIDs) == 0 {
			return fmt.Errorf("outbound[%d]: at least one cid required", i)
		}
		for j := range ob.CIDs {
			oc := &ob.CIDs[j]
			if oc.CID < minCID {
				return fmt.Errorf("outbound[%d].cids[%d]: cid %d must be >= %d",
					i, j, oc.CID, minCID)
			}
			if prev, ok := cidOwner[oc.CID]; ok {
				return fmt.Errorf(
					"outbound[%d].cids[%d]: cid %d already listed under outbound port %d",
					i, j, oc.CID, prev)
			}
			cidOwner[oc.CID] = ob.Port
			if len(oc.AllowedHosts) == 0 {
				return fmt.Errorf("outbound[%d].cids[%d]: allowed_hosts must not be empty",
					i, j)
			}
			for k, pat := range oc.AllowedHosts {
				if err := validatePattern(pat); err != nil {
					return fmt.Errorf("outbound[%d].cids[%d].allowed_hosts[%d]: %w",
						i, j, k, err)
				}
			}
		}
	}

	switch c.LogFormat {
	case "", LogFormatJSON, LogFormatText, LogFormatAuto:
	default:
		return fmt.Errorf("log_format %q must be one of %q, %q, %q",
			c.LogFormat, LogFormatJSON, LogFormatText, LogFormatAuto)
	}

	if c.ShutdownGrace < 0 {
		return fmt.Errorf("shutdown_grace must be non-negative, got %s",
			c.ShutdownGrace.Duration())
	}

	if c.Metrics.Bind != "" {
		if _, _, err := net.SplitHostPort(c.Metrics.Bind); err != nil {
			return fmt.Errorf("metrics.bind %q: %w", c.Metrics.Bind, err)
		}
	}

	return nil
}

func (il *InboundListener) validate() error {
	if il.Bind == "" {
		return errors.New("bind must not be empty")
	}
	if il.Port <= 0 || il.Port > 65535 {
		return fmt.Errorf("port %d out of range (1..65535)", il.Port)
	}
	switch il.Mode {
	case ModeHTTPHost, ModeTLSSNI:
	default:
		return fmt.Errorf("mode %q must be %q or %q",
			il.Mode, ModeHTTPHost, ModeTLSSNI)
	}
	if len(il.Routes) == 0 {
		return errors.New("at least one route required")
	}

	seenHost := make(map[string]bool)
	for i := range il.Routes {
		r := &il.Routes[i]
		if r.Hostname == "" {
			return fmt.Errorf("routes[%d]: hostname must not be empty", i)
		}
		h := strings.ToLower(r.Hostname)
		if seenHost[h] {
			return fmt.Errorf("routes[%d]: duplicate hostname %q", i, r.Hostname)
		}
		seenHost[h] = true
		if r.CID < minCID {
			return fmt.Errorf("routes[%d]: cid %d must be >= %d",
				i, r.CID, minCID)
		}
		if r.VsockPort == 0 || r.VsockPort >= vsockPortAny {
			return fmt.Errorf("routes[%d]: vsock_port %d out of range",
				i, r.VsockPort)
		}
	}
	return nil
}

// validatePattern accepts "*" (any host, any port), exact "host:port", or a
// leading suffix wildcard like "*.example.com:443". Task 3 implements the
// actual matching; here we only check syntax.
func validatePattern(pat string) error {
	if pat == "*" {
		return nil
	}
	host, portStr, err := net.SplitHostPort(pat)
	if err != nil {
		return fmt.Errorf("pattern %q not host:port or *", pat)
	}
	switch {
	case host == "":
		return fmt.Errorf("pattern %q: host must not be empty", pat)
	case host == "*":
		// Any-host wildcard with a specific port is allowed.
	case strings.HasPrefix(host, "*."):
		rest := host[2:]
		if rest == "" || strings.Contains(rest, "*") {
			return fmt.Errorf(
				"pattern %q: wildcard must be '*.' followed by a host", pat)
		}
	case strings.Contains(host, "*"):
		return fmt.Errorf(
			"pattern %q: '*' only allowed as leading '*.'", pat)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("pattern %q: invalid port %q", pat, portStr)
	}
	return nil
}
