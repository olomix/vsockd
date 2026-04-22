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

// Log level values accepted in YAML / env var. Empty means info.
const (
	LogLevelDebug = "debug"
	LogLevelInfo  = "info"
)

// VSOCK reserves CIDs 0..2 (hypervisor/local/host). Enclave CIDs start at 3.
const minCID uint32 = 3

// vsockPortAny is the reserved "any port" sentinel (vsock.PortAny). Config
// values equal to this are rejected; any other uint32 is a valid vsock port.
const vsockPortAny uint32 = 0xFFFFFFFF

// Config is the top-level vsockd configuration.
type Config struct {
	Inbound       []InboundListener    `yaml:"inbound"`
	Outbound      []OutboundListener   `yaml:"outbound"`
	TCPToVsock    []TCPToVsockListener `yaml:"tcp_to_vsock"`
	VsockToTCP    []VsockToTCPListener `yaml:"vsock_to_tcp"`
	Metrics       MetricsConfig        `yaml:"metrics"`
	ShutdownGrace Duration             `yaml:"shutdown_grace"`
	LogFormat     string               `yaml:"log_format"`
	LogLevel      string               `yaml:"log_level"`
}

// InboundListener terminates TCP on the host and routes to enclave vsocks
// using either the HTTP Host header (mode: http-host) or TLS SNI
// (mode: tls-sni). Raw-TCP passthrough moved to TCPToVsockListener.
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

// OutboundListener accepts vsock connections on a single port and runs the
// HTTP forward-proxy handler: every connection is authorized by peer CID,
// then exactly one HTTP request (CONNECT or absolute-URI GET/POST) is
// matched against that CID's egress allowlist. Raw vsock→TCP passthrough
// moved to VsockToTCPListener.
type OutboundListener struct {
	Port uint32        `yaml:"port"`
	CIDs []OutboundCID `yaml:"cids"`
}

// OutboundCID is the per-CID egress policy scoped to one outbound listener.
type OutboundCID struct {
	CID          uint32   `yaml:"cid"`
	AllowedHosts []string `yaml:"allowed_hosts"`
}

// TCPToVsockListener terminates TCP on a host or enclave port and forwards
// the raw byte stream to a fixed vsock endpoint.
type TCPToVsockListener struct {
	Bind      string `yaml:"bind"`
	Port      int    `yaml:"port"`
	VsockCID  uint32 `yaml:"vsock_cid"`
	VsockPort uint32 `yaml:"vsock_port"`
}

// VsockToTCPListener accepts vsock connections on the given vsock port and
// forwards the raw byte stream to a fixed TCP upstream.
type VsockToTCPListener struct {
	Port     uint32 `yaml:"port"`
	Upstream string `yaml:"upstream"`
}

// MetricsConfig controls the Prometheus /metrics endpoint. Exactly one of
// Bind or VsockPort may be set. Leaving both empty disables the endpoint;
// the CLI flag can still opt back in.
type MetricsConfig struct {
	// Bind is a TCP host:port (e.g. "0.0.0.0:9090") for host-side deployments.
	Bind string `yaml:"bind"`
	// VsockPort exposes /metrics over vsock from VMADDR_CID_ANY. Intended
	// for enclave-side vsockd so the parent host can scrape over vsock.
	VsockPort uint32 `yaml:"vsock_port"`
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
	if len(c.Inbound) == 0 && len(c.Outbound) == 0 &&
		len(c.TCPToVsock) == 0 && len(c.VsockToTCP) == 0 {
		return errors.New("config has no listeners configured")
	}

	// Two TCP listeners sharing the same (bind, port) would collide at bind
	// time on first Start, and silently collapse during SIGHUP reloads where
	// the Apply keyed-existing-listener fast path treats duplicate keys as
	// the same listener. The map is shared across inbound and tcp_to_vsock
	// so a port declared in one section cannot also appear in the other.
	seenBind := make(map[string]string)
	for i := range c.Inbound {
		if err := c.Inbound[i].validate(); err != nil {
			return fmt.Errorf("inbound[%d]: %w", i, err)
		}
		key := fmt.Sprintf("%s|%d", c.Inbound[i].Bind, c.Inbound[i].Port)
		if prev, ok := seenBind[key]; ok {
			return fmt.Errorf(
				"inbound[%d]: duplicate bind %s:%d already declared in %s",
				i, c.Inbound[i].Bind, c.Inbound[i].Port, prev)
		}
		seenBind[key] = fmt.Sprintf("inbound[%d]", i)
	}
	for i := range c.TCPToVsock {
		if err := c.TCPToVsock[i].validate(); err != nil {
			return fmt.Errorf("tcp_to_vsock[%d]: %w", i, err)
		}
		key := fmt.Sprintf("%s|%d", c.TCPToVsock[i].Bind, c.TCPToVsock[i].Port)
		if prev, ok := seenBind[key]; ok {
			return fmt.Errorf(
				"tcp_to_vsock[%d]: duplicate bind %s:%d already declared in %s",
				i, c.TCPToVsock[i].Bind, c.TCPToVsock[i].Port, prev)
		}
		seenBind[key] = fmt.Sprintf("tcp_to_vsock[%d]", i)
	}

	// Cross-port uniqueness: a CID may appear under at most one outbound
	// port. Remember the owning port so the error message points to the
	// conflict. Vsock ports are unique across outbound and vsock_to_tcp.
	cidOwner := make(map[uint32]uint32)
	seenPort := make(map[uint32]string)

	for i := range c.Outbound {
		ob := &c.Outbound[i]
		if ob.Port == 0 || ob.Port >= vsockPortAny {
			return fmt.Errorf("outbound[%d]: port %d out of range", i, ob.Port)
		}
		if prev, ok := seenPort[ob.Port]; ok {
			return fmt.Errorf(
				"outbound[%d]: duplicate port %d already declared in %s",
				i, ob.Port, prev)
		}
		seenPort[ob.Port] = fmt.Sprintf("outbound[%d]", i)

		if len(ob.CIDs) == 0 {
			return fmt.Errorf("outbound[%d]: at least one cid required", i)
		}
		for j := range ob.CIDs {
			oc := &ob.CIDs[j]
			if oc.CID < minCID {
				return fmt.Errorf(
					"outbound[%d].cids[%d]: cid %d must be >= %d",
					i, j, oc.CID, minCID)
			}
			if prev, ok := cidOwner[oc.CID]; ok {
				return fmt.Errorf(
					"outbound[%d].cids[%d]: cid %d already listed under outbound port %d",
					i, j, oc.CID, prev)
			}
			cidOwner[oc.CID] = ob.Port
			if len(oc.AllowedHosts) == 0 {
				return fmt.Errorf(
					"outbound[%d].cids[%d]: allowed_hosts must not be empty",
					i, j)
			}
			for k, pat := range oc.AllowedHosts {
				if err := validatePattern(pat); err != nil {
					return fmt.Errorf(
						"outbound[%d].cids[%d].allowed_hosts[%d]: %w",
						i, j, k, err)
				}
			}
		}
	}

	for i := range c.VsockToTCP {
		if err := c.VsockToTCP[i].validate(); err != nil {
			return fmt.Errorf("vsock_to_tcp[%d]: %w", i, err)
		}
		p := c.VsockToTCP[i].Port
		if prev, ok := seenPort[p]; ok {
			return fmt.Errorf(
				"vsock_to_tcp[%d]: duplicate port %d already declared in %s",
				i, p, prev)
		}
		seenPort[p] = fmt.Sprintf("vsock_to_tcp[%d]", i)
	}

	switch c.LogFormat {
	case "", LogFormatJSON, LogFormatText, LogFormatAuto:
	default:
		return fmt.Errorf("log_format %q must be one of %q, %q, %q",
			c.LogFormat, LogFormatJSON, LogFormatText, LogFormatAuto)
	}

	switch c.LogLevel {
	case "", LogLevelDebug, LogLevelInfo:
	default:
		return fmt.Errorf("log_level %q must be %q or %q",
			c.LogLevel, LogLevelDebug, LogLevelInfo)
	}

	if c.ShutdownGrace < 0 {
		return fmt.Errorf("shutdown_grace must be non-negative, got %s",
			c.ShutdownGrace.Duration())
	}

	if c.Metrics.Bind != "" && c.Metrics.VsockPort != 0 {
		return errors.New(
			"metrics.bind and metrics.vsock_port are mutually exclusive")
	}
	if c.Metrics.Bind != "" {
		if _, _, err := net.SplitHostPort(c.Metrics.Bind); err != nil {
			return fmt.Errorf("metrics.bind %q: %w", c.Metrics.Bind, err)
		}
	}
	if c.Metrics.VsockPort != 0 {
		if c.Metrics.VsockPort >= vsockPortAny {
			return fmt.Errorf("metrics.vsock_port %d out of range",
				c.Metrics.VsockPort)
		}
		if prev, ok := seenPort[c.Metrics.VsockPort]; ok {
			return fmt.Errorf(
				"metrics.vsock_port %d already declared in %s",
				c.Metrics.VsockPort, prev)
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
			return fmt.Errorf(
				"routes[%d]: duplicate hostname %q", i, r.Hostname)
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

func (t *TCPToVsockListener) validate() error {
	if t.Bind == "" {
		return errors.New("bind must not be empty")
	}
	if t.Port <= 0 || t.Port > 65535 {
		return fmt.Errorf("port %d out of range (1..65535)", t.Port)
	}
	if t.VsockCID < minCID {
		return fmt.Errorf("vsock_cid %d must be >= %d", t.VsockCID, minCID)
	}
	if t.VsockPort == 0 || t.VsockPort >= vsockPortAny {
		return fmt.Errorf("vsock_port %d out of range", t.VsockPort)
	}
	return nil
}

func (v *VsockToTCPListener) validate() error {
	if v.Port == 0 || v.Port >= vsockPortAny {
		return fmt.Errorf("port %d out of range", v.Port)
	}
	if v.Upstream == "" {
		return errors.New("upstream must not be empty")
	}
	if err := validateHostPort(v.Upstream); err != nil {
		return fmt.Errorf("upstream %w", err)
	}
	return nil
}

// validateHostPort checks a "host:port" string for the outbound TCP upstream.
// host must be non-empty; port must be in 1..65535. Wildcards are not allowed
// here (unlike the allowlist patterns).
func validateHostPort(s string) error {
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return fmt.Errorf("%q not host:port: %w", s, err)
	}
	if host == "" {
		return fmt.Errorf("%q: host must not be empty", s)
	}
	if strings.Contains(host, "*") {
		return fmt.Errorf("%q: wildcards not allowed", s)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("%q: invalid port %q", s, portStr)
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
