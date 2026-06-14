// Package supervisor implements the in-enclave process supervisor: it spawns
// and supervises the application task(s) and a vsockd sidecar, captures their
// stdout/stderr plus its own operational logs, frames every line as NDJSON,
// and ships the combined stream over its own vsock connection to the parent.
//
// This file loads and validates the supervisor YAML configuration. The schema
// and validation mirror internal/config (strict parsing via KnownFields(true)
// followed by Validate). All constraints from the implementation plan live
// here: the log destination (log_port/log_cid), source-side tags, the bounded
// ring-buffer budget, and the supervised process list with its
// role/restart/on_failure policy.
package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Process role values; role is required and must be one of these.
const (
	RoleTask    = "task"
	RoleSidecar = "sidecar"
)

// Restart policy values; restart defaults to RestartOnFailure.
const (
	RestartNo        = "no"
	RestartOnFailure = "on-failure"
	RestartAlways    = "always"
)

// on_failure values; on_failure defaults to OnFailureTerminate.
const (
	OnFailureTerminate = "terminate"
	OnFailureContinue  = "continue"
)

// DefaultLogCID is the parent CID the supervisor ships logs to when log_cid is
// omitted. VSOCK reserves CIDs 0..2 (hypervisor/local/host); from inside the
// enclave the parent is CID 3.
const DefaultLogCID uint32 = 3

// minCID is the lowest assignable vsock CID; 0..2 are reserved.
const minCID uint32 = 3

// vsockPortAny is the reserved "any port" sentinel (vsock.PortAny). A log_port
// equal to this is rejected; any other non-zero uint32 is a valid vsock port.
const vsockPortAny uint32 = 0xFFFFFFFF

// Default ring-buffer budget applied when the buffer section is omitted.
// Placeholders per the plan's Open Items; tune once measured.
const (
	defaultBufferMaxBytes   = 8 << 20 // 8 MiB
	defaultBufferMaxRecords = 10000
)

// Config is the top-level supervisor configuration.
type Config struct {
	LogPort   uint32            `yaml:"log_port"`
	LogCID    uint32            `yaml:"log_cid"`
	Tags      map[string]string `yaml:"tags"`
	Buffer    *Buffer           `yaml:"buffer"`
	Processes []Process         `yaml:"processes"`
}

// Buffer bounds the ring buffer that decouples log producers from the network.
// At least one of MaxBytes / MaxRecords must be positive; the other may be 0
// to leave that dimension unbounded.
type Buffer struct {
	MaxBytes   int `yaml:"max_bytes"`
	MaxRecords int `yaml:"max_records"`
}

// Process declares one supervised child and its restart/failure policy.
type Process struct {
	Name          string   `yaml:"name"`
	Command       string   `yaml:"command"`
	Args          []string `yaml:"args"`
	Role          string   `yaml:"role"`
	Restart       string   `yaml:"restart"`
	MaxRestarts   int      `yaml:"max_restarts"`
	RestartWindow Duration `yaml:"restart_window"`
	OnFailure     string   `yaml:"on_failure"`
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

// Validate enforces the semantic constraints from the implementation plan and
// applies defaults in place (log_cid, restart, on_failure, buffer budget).
func (c *Config) Validate() error {
	if c.LogPort == 0 || c.LogPort >= vsockPortAny {
		return fmt.Errorf("log_port %d out of range", c.LogPort)
	}
	if c.LogCID == 0 {
		c.LogCID = DefaultLogCID
	} else if c.LogCID < minCID {
		return fmt.Errorf("log_cid %d must be >= %d", c.LogCID, minCID)
	}

	for k, v := range c.Tags {
		if k == "" {
			return errors.New("tags has an empty key")
		}
		if v == "" {
			return fmt.Errorf("tags[%q] has an empty value", k)
		}
	}

	if err := c.validateBuffer(); err != nil {
		return err
	}

	seenName := make(map[string]bool)
	for i := range c.Processes {
		p := &c.Processes[i]
		if p.Name == "" {
			return fmt.Errorf("processes[%d]: name must not be empty", i)
		}
		if seenName[p.Name] {
			return fmt.Errorf("processes[%d]: duplicate name %q", i, p.Name)
		}
		seenName[p.Name] = true
		if err := p.validate(); err != nil {
			return fmt.Errorf("processes[%d] (%s): %w", i, p.Name, err)
		}
	}

	return nil
}

// validateBuffer applies the default budget when buffer is omitted, rejects
// negative dimensions, and requires an explicit buffer to bound at least one
// dimension (an all-zero buffer would be unbounded, defeating the loss policy).
func (c *Config) validateBuffer() error {
	if c.Buffer == nil {
		c.Buffer = &Buffer{
			MaxBytes:   defaultBufferMaxBytes,
			MaxRecords: defaultBufferMaxRecords,
		}
		return nil
	}
	if c.Buffer.MaxBytes < 0 {
		return fmt.Errorf("buffer.max_bytes %d must be >= 0", c.Buffer.MaxBytes)
	}
	if c.Buffer.MaxRecords < 0 {
		return fmt.Errorf("buffer.max_records %d must be >= 0",
			c.Buffer.MaxRecords)
	}
	if c.Buffer.MaxBytes == 0 && c.Buffer.MaxRecords == 0 {
		return errors.New(
			"buffer must bound at least one of max_bytes or max_records")
	}
	return nil
}

// validate checks one process and applies restart/on_failure defaults in place.
func (p *Process) validate() error {
	if p.Command == "" {
		return errors.New("command must not be empty")
	}
	switch p.Role {
	case RoleTask, RoleSidecar:
	case "":
		return fmt.Errorf("role must be %q or %q", RoleTask, RoleSidecar)
	default:
		return fmt.Errorf("role %q must be %q or %q",
			p.Role, RoleTask, RoleSidecar)
	}

	if p.Restart == "" {
		p.Restart = RestartOnFailure
	}
	switch p.Restart {
	case RestartNo, RestartOnFailure, RestartAlways:
	default:
		return fmt.Errorf("restart %q must be %q, %q or %q",
			p.Restart, RestartNo, RestartOnFailure, RestartAlways)
	}

	if p.OnFailure == "" {
		p.OnFailure = OnFailureTerminate
	}
	switch p.OnFailure {
	case OnFailureTerminate, OnFailureContinue:
	default:
		return fmt.Errorf("on_failure %q must be %q or %q",
			p.OnFailure, OnFailureTerminate, OnFailureContinue)
	}

	if p.MaxRestarts < 0 {
		return fmt.Errorf("max_restarts %d must be >= 0", p.MaxRestarts)
	}
	// A restart window is only meaningful when restarts can actually happen.
	// Require it then so the windowed crash-loop cap has a real interval.
	if p.Restart != RestartNo && p.MaxRestarts > 0 && p.RestartWindow <= 0 {
		return errors.New(
			"restart_window must be > 0 when restart != no and max_restarts > 0")
	}
	return nil
}
