package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olomix/vsockd/internal/config"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "vsockd.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	yamlDoc := `
inbound:
  - bind: 0.0.0.0
    port: 443
    mode: tls-sni
    routes:
      - hostname: api.example.com
        cid: 16
        vsock_port: 8443
      - hostname: admin.example.com
        cid: 20
        vsock_port: 8443
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes:
      - hostname: api.example.com
        cid: 16
        vsock_port: 8080

outbound:
  - port: 8080
    cids:
      - cid: 16
        allowed_hosts: ["api.stripe.com:443", "*.s3.amazonaws.com:443"]
      - cid: 20
        allowed_hosts: ["*.internal.example.com:443"]
  - port: 8082
    cids:
      - cid: 42
        allowed_hosts: ["*"]

metrics:
  bind: 0.0.0.0:9090

shutdown_grace: 30s
log_format: json
`
	cfg, err := config.Load(writeConfig(t, yamlDoc))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Inbound) != 2 {
		t.Fatalf("want 2 inbound listeners, got %d", len(cfg.Inbound))
	}
	if cfg.Inbound[0].Mode != config.ModeTLSSNI {
		t.Fatalf("inbound[0].Mode = %q, want %q",
			cfg.Inbound[0].Mode, config.ModeTLSSNI)
	}
	if cfg.Inbound[1].Mode != config.ModeHTTPHost {
		t.Fatalf("inbound[1].Mode = %q, want %q",
			cfg.Inbound[1].Mode, config.ModeHTTPHost)
	}
	if got := cfg.Inbound[0].Routes[0].CID; got != 16 {
		t.Fatalf("inbound[0].routes[0].cid = %d, want 16", got)
	}
	if len(cfg.Outbound) != 2 {
		t.Fatalf("want 2 outbound listeners, got %d", len(cfg.Outbound))
	}
	if got := cfg.Outbound[0].CIDs[1].CID; got != 20 {
		t.Fatalf("outbound[0].cids[1].cid = %d, want 20", got)
	}
	if got := cfg.Outbound[1].CIDs[0].AllowedHosts[0]; got != "*" {
		t.Fatalf("universal pattern: got %q", got)
	}
	if cfg.ShutdownGrace.Duration() != 30*time.Second {
		t.Fatalf("shutdown_grace = %v, want 30s",
			cfg.ShutdownGrace.Duration())
	}
	if cfg.LogFormat != config.LogFormatJSON {
		t.Fatalf("log_format = %q", cfg.LogFormat)
	}
	if cfg.Metrics.Bind != "0.0.0.0:9090" {
		t.Fatalf("metrics.bind = %q", cfg.Metrics.Bind)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "duplicate CID across outbound ports",
			yaml: `
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 8080}
outbound:
  - port: 8080
    cids:
      - {cid: 16, allowed_hosts: ["*"]}
  - port: 8082
    cids:
      - {cid: 16, allowed_hosts: ["*"]}
`,
			want: "already listed under outbound port 8080",
		},
		{
			name: "duplicate inbound (bind, port)",
			yaml: `
inbound:
  - bind: 0.0.0.0
    port: 443
    mode: tls-sni
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 8443}
  - bind: 0.0.0.0
    port: 443
    mode: http-host
    routes:
      - {hostname: b.example.com, cid: 4, vsock_port: 8080}
`,
			want: "duplicate bind 0.0.0.0:443",
		},
		{
			name: "duplicate hostname in inbound listener (case-insensitive)",
			yaml: `
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 8080}
      - {hostname: A.example.com, cid: 4, vsock_port: 8081}
`,
			want: "duplicate hostname",
		},
		{
			name: "unknown inbound mode",
			yaml: `
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-bogus
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 8080}
`,
			want: `mode "http-bogus"`,
		},
		{
			name: "malformed allowlist pattern (no port)",
			yaml: `
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 8080}
outbound:
  - port: 8080
    cids:
      - cid: 16
        allowed_hosts: ["api.example.com"]
`,
			want: "not host:port",
		},
		{
			name: "wildcard in middle of host",
			yaml: `
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 8080}
outbound:
  - port: 8080
    cids:
      - cid: 16
        allowed_hosts: ["a.*.example.com:443"]
`,
			want: "'*' only allowed as leading",
		},
		{
			name: "unknown YAML field (strict parser)",
			yaml: `
unexpected: true
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 8080}
`,
			want: "field unexpected not found",
		},
		{
			name: "invalid shutdown_grace string",
			yaml: `
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 8080}
shutdown_grace: not-a-duration
`,
			want: "invalid duration",
		},
		{
			name: "cid below minimum",
			yaml: `
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes:
      - {hostname: a.example.com, cid: 2, vsock_port: 8080}
`,
			want: "cid 2 must be >= 3",
		},
		{
			name: "inbound vsock_port zero",
			yaml: `
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 0}
`,
			want: "vsock_port 0",
		},
		{
			name: "inbound TCP port out of range",
			yaml: `
inbound:
  - bind: 0.0.0.0
    port: 70000
    mode: http-host
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 8080}
`,
			want: "port 70000 out of range",
		},
		{
			name: "inbound hostname empty",
			yaml: `
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes:
      - {hostname: "", cid: 3, vsock_port: 8080}
`,
			want: "hostname must not be empty",
		},
		{
			name: "inbound bind empty",
			yaml: `
inbound:
  - bind: ""
    port: 80
    mode: http-host
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 8080}
`,
			want: "bind must not be empty",
		},
		{
			name: "inbound no routes",
			yaml: `
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes: []
`,
			want: "at least one route required",
		},
		{
			name: "duplicate outbound port",
			yaml: `
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 8080}
outbound:
  - port: 8080
    cids:
      - {cid: 16, allowed_hosts: ["*"]}
  - port: 8080
    cids:
      - {cid: 17, allowed_hosts: ["*"]}
`,
			want: "duplicate port 8080",
		},
		{
			name: "outbound cid below minimum",
			yaml: `
outbound:
  - port: 8080
    cids:
      - {cid: 2, allowed_hosts: ["*"]}
`,
			want: "cid 2 must be >= 3",
		},
		{
			name: "outbound allowed_hosts empty",
			yaml: `
outbound:
  - port: 8080
    cids:
      - {cid: 16, allowed_hosts: []}
`,
			want: "allowed_hosts must not be empty",
		},
		{
			name: "empty document",
			yaml: "",
			want: "no listeners configured",
		},
		{
			name: "unknown log_format",
			yaml: `
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 8080}
log_format: xml
`,
			want: `log_format "xml"`,
		},
		{
			name: "malformed metrics bind",
			yaml: `
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 8080}
metrics:
  bind: "not-host-port"
`,
			want: "metrics.bind",
		},
		{
			name: "old-style inbound mode: tcp rejected by strict YAML",
			yaml: `
inbound:
  - bind: 0.0.0.0
    port: 5432
    mode: tcp
    target_cid: 3
    target_port: 8080
`,
			want: "field target_cid not found",
		},
		{
			name: "old-style outbound mode: tcp rejected by strict YAML",
			yaml: `
outbound:
  - port: 8080
    mode: tcp
    upstream: "10.0.0.5:5432"
`,
			want: "field mode not found",
		},
		{
			name: "outbound cids empty",
			yaml: `
outbound:
  - port: 8080
    cids: []
`,
			want: "at least one cid required",
		},
		{
			name: "tcp_to_vsock missing bind",
			yaml: `
tcp_to_vsock:
  - port: 5432
    vsock_cid: 16
    vsock_port: 5432
`,
			want: "bind must not be empty",
		},
		{
			name: "tcp_to_vsock bad port",
			yaml: `
tcp_to_vsock:
  - bind: 0.0.0.0
    port: 70000
    vsock_cid: 16
    vsock_port: 5432
`,
			want: "port 70000 out of range",
		},
		{
			name: "tcp_to_vsock vsock_cid below minimum",
			yaml: `
tcp_to_vsock:
  - bind: 0.0.0.0
    port: 5432
    vsock_cid: 2
    vsock_port: 5432
`,
			want: "vsock_cid 2 must be >= 3",
		},
		{
			name: "tcp_to_vsock vsock_port zero",
			yaml: `
tcp_to_vsock:
  - bind: 0.0.0.0
    port: 5432
    vsock_cid: 16
    vsock_port: 0
`,
			want: "vsock_port 0 out of range",
		},
		{
			name: "vsock_to_tcp port zero",
			yaml: `
vsock_to_tcp:
  - port: 0
    upstream: "10.0.0.5:5432"
`,
			want: "port 0 out of range",
		},
		{
			name: "vsock_to_tcp upstream empty",
			yaml: `
vsock_to_tcp:
  - port: 9000
    upstream: ""
`,
			want: "upstream must not be empty",
		},
		{
			name: "vsock_to_tcp upstream missing port",
			yaml: `
vsock_to_tcp:
  - port: 9000
    upstream: "10.0.0.5"
`,
			want: "not host:port",
		},
		{
			name: "vsock_to_tcp upstream wildcard",
			yaml: `
vsock_to_tcp:
  - port: 9000
    upstream: "*.example.com:5432"
`,
			want: "wildcards not allowed",
		},
		{
			name: "cross-section collision inbound vs tcp_to_vsock",
			yaml: `
inbound:
  - bind: 0.0.0.0
    port: 443
    mode: tls-sni
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 8443}
tcp_to_vsock:
  - bind: 0.0.0.0
    port: 443
    vsock_cid: 16
    vsock_port: 5432
`,
			want: "duplicate bind 0.0.0.0:443 already declared in inbound[0]",
		},
		{
			name: "cross-section collision outbound vs vsock_to_tcp",
			yaml: `
outbound:
  - port: 9000
    cids:
      - {cid: 16, allowed_hosts: ["*"]}
vsock_to_tcp:
  - port: 9000
    upstream: "10.0.0.5:5432"
`,
			want: "duplicate port 9000 already declared in outbound[0]",
		},
		{
			name: "metrics bind and vsock_port both set",
			yaml: `
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 8080}
metrics:
  bind: "0.0.0.0:9090"
  vsock_port: 9090
`,
			want: "metrics.bind and metrics.vsock_port are mutually exclusive",
		},
		{
			name: "metrics vsock_port out of range (PortAny)",
			yaml: `
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 8080}
metrics:
  vsock_port: 4294967295
`,
			want: "metrics.vsock_port 4294967295 out of range",
		},
		{
			name: "metrics vsock_port collides with outbound",
			yaml: `
outbound:
  - port: 9090
    cids:
      - {cid: 16, allowed_hosts: ["*"]}
metrics:
  vsock_port: 9090
`,
			want: "metrics.vsock_port 9090 already declared in outbound[0]",
		},
		{
			name: "metrics vsock_port collides with vsock_to_tcp",
			yaml: `
vsock_to_tcp:
  - port: 9090
    upstream: "10.0.0.5:5432"
metrics:
  vsock_port: 9090
`,
			want: "metrics.vsock_port 9090 already declared in vsock_to_tcp[0]",
		},
		{
			name: "log_relay missing output",
			yaml: `
log_relay:
  - port: 5140
    path: /var/log/app.ndjson
`,
			want: `output must be "file" or "stdout"`,
		},
		{
			name: "log_relay file output without path",
			yaml: `
log_relay:
  - port: 5140
    output: file
`,
			want: `output "file" requires a non-empty path`,
		},
		{
			name: "log_relay stdout output with path",
			yaml: `
log_relay:
  - port: 5140
    output: stdout
    path: /var/log/app.ndjson
`,
			want: `output "stdout" must not set a path`,
		},
		{
			name: "log_relay bad output",
			yaml: `
log_relay:
  - port: 5140
    output: syslog
`,
			want: `output "syslog" must be`,
		},
		{
			name: "log_relay port out of range",
			yaml: `
log_relay:
  - port: 4294967295
    output: stdout
`,
			want: "port 4294967295 out of range",
		},
		{
			name: "log_relay negative max_line_bytes",
			yaml: `
log_relay:
  - port: 5140
    output: stdout
    max_line_bytes: -1
`,
			want: "max_line_bytes -1 must be >= 0",
		},
		{
			name: "log_relay sub-floor max_line_bytes",
			yaml: `
log_relay:
  - port: 5140
    output: stdout
    max_line_bytes: 8
`,
			want: "max_line_bytes 8 must be 0 (default) or >= 16",
		},
		{
			name: "log_relay empty tag value",
			yaml: `
log_relay:
  - port: 5140
    output: stdout
    enrich:
      tags:
        region: ""
`,
			want: `enrich.tags["region"] has an empty value`,
		},
		{
			name: "log_relay empty tag key",
			yaml: `
log_relay:
  - port: 5140
    output: stdout
    enrich:
      tags:
        "": us-east-1
`,
			want: "enrich.tags has an empty key",
		},
		{
			name: "log_relay host_key collides with raw wrapper",
			yaml: `
log_relay:
  - port: 5140
    output: stdout
    enrich:
      host_key: msg
      tags:
        region: us-east-1
`,
			want: `enrich.host_key "msg" is reserved by the relay`,
		},
		{
			name: "log_relay host_key cid collides with enrich.cid",
			yaml: `
log_relay:
  - port: 5140
    output: stdout
    enrich:
      cid: true
      host_key: cid
      tags:
        region: us-east-1
`,
			want: `enrich.host_key "cid" collides with enrich.cid`,
		},
		{
			name: "log_relay port collides with outbound",
			yaml: `
outbound:
  - port: 5140
    cids:
      - {cid: 16, allowed_hosts: ["*"]}
log_relay:
  - port: 5140
    output: stdout
`,
			want: "duplicate port 5140 already declared in outbound[0]",
		},
		{
			name: "log_relay port collides with vsock_to_tcp",
			yaml: `
vsock_to_tcp:
  - port: 5140
    upstream: "10.0.0.5:5432"
log_relay:
  - port: 5140
    output: stdout
`,
			want: "duplicate port 5140 already declared in vsock_to_tcp[0]",
		},
		{
			name: "metrics vsock_port collides with log_relay",
			yaml: `
log_relay:
  - port: 9090
    output: stdout
metrics:
  vsock_port: 9090
`,
			want: "metrics.vsock_port 9090 already declared in log_relay[0]",
		},
		{
			name: "invalid log_level",
			yaml: `
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 8080}
log_level: trace
`,
			want: `log_level "trace"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(writeConfig(t, tc.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestLoadTCPToVsock verifies the new top-level tcp_to_vsock section
// round-trips through the schema.
func TestLoadTCPToVsock(t *testing.T) {
	yamlDoc := `
log_level: debug

tcp_to_vsock:
  - bind: 0.0.0.0
    port: 5432
    vsock_cid: 16
    vsock_port: 5432
  - bind: 127.0.0.1
    port: 6379
    vsock_cid: 17
    vsock_port: 6379
`
	cfg, err := config.Load(writeConfig(t, yamlDoc))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != config.LogLevelDebug {
		t.Fatalf("log_level = %q, want %q",
			cfg.LogLevel, config.LogLevelDebug)
	}
	if len(cfg.TCPToVsock) != 2 {
		t.Fatalf("want 2 tcp_to_vsock listeners, got %d",
			len(cfg.TCPToVsock))
	}
	if got := cfg.TCPToVsock[0].VsockCID; got != 16 {
		t.Fatalf("tcp_to_vsock[0].VsockCID = %d, want 16", got)
	}
	if got := cfg.TCPToVsock[0].VsockPort; got != 5432 {
		t.Fatalf("tcp_to_vsock[0].VsockPort = %d, want 5432", got)
	}
	if got := cfg.TCPToVsock[1].Bind; got != "127.0.0.1" {
		t.Fatalf("tcp_to_vsock[1].Bind = %q, want 127.0.0.1", got)
	}
}

// TestLoadVsockToTCP verifies the new top-level vsock_to_tcp section
// round-trips through the schema.
func TestLoadVsockToTCP(t *testing.T) {
	yamlDoc := `
vsock_to_tcp:
  - port: 9000
    upstream: "10.0.0.5:5432"
  - port: 9001
    upstream: "db.internal:3306"
`
	cfg, err := config.Load(writeConfig(t, yamlDoc))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.VsockToTCP) != 2 {
		t.Fatalf("want 2 vsock_to_tcp listeners, got %d",
			len(cfg.VsockToTCP))
	}
	if got := cfg.VsockToTCP[0].Port; got != 9000 {
		t.Fatalf("vsock_to_tcp[0].Port = %d, want 9000", got)
	}
	if got := cfg.VsockToTCP[0].Upstream; got != "10.0.0.5:5432" {
		t.Fatalf("vsock_to_tcp[0].Upstream = %q", got)
	}
	if got := cfg.VsockToTCP[1].Upstream; got != "db.internal:3306" {
		t.Fatalf("vsock_to_tcp[1].Upstream = %q", got)
	}
}

// TestLoadLogRelay verifies the log_relay section round-trips through the
// schema and that defaults (max_line_bytes, enrich.host_key) are applied.
func TestLoadLogRelay(t *testing.T) {
	yamlDoc := `
log_relay:
  - port: 5140
    output: file
    path: /var/log/enclave/app.ndjson
    max_line_bytes: 2048
    enrich:
      cid: true
      host_key: meta
      tags:
        region: us-east-1
        instance: i-0abc123
  - port: 5141
    output: stdout
  - port: 5142
    output: file
    path: /var/log/enclave/other.ndjson
    enrich:
      cid: true
`
	cfg, err := config.Load(writeConfig(t, yamlDoc))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.LogRelay) != 3 {
		t.Fatalf("want 3 log_relay listeners, got %d", len(cfg.LogRelay))
	}

	first := cfg.LogRelay[0]
	if first.Port != 5140 {
		t.Fatalf("log_relay[0].Port = %d, want 5140", first.Port)
	}
	if first.Output != config.LogRelayOutputFile {
		t.Fatalf("log_relay[0].Output = %q, want file", first.Output)
	}
	if first.Path != "/var/log/enclave/app.ndjson" {
		t.Fatalf("log_relay[0].Path = %q", first.Path)
	}
	if first.MaxLineBytes != 2048 {
		t.Fatalf("log_relay[0].MaxLineBytes = %d, want 2048",
			first.MaxLineBytes)
	}
	if first.Enrich == nil {
		t.Fatalf("log_relay[0].Enrich = nil, want non-nil")
	}
	if !first.Enrich.CID {
		t.Fatalf("log_relay[0].Enrich.CID = false, want true")
	}
	if first.Enrich.HostKey != "meta" {
		t.Fatalf("log_relay[0].Enrich.HostKey = %q, want meta",
			first.Enrich.HostKey)
	}
	if got := first.Enrich.Tags["region"]; got != "us-east-1" {
		t.Fatalf("log_relay[0].Enrich.Tags[region] = %q", got)
	}

	second := cfg.LogRelay[1]
	if second.Output != config.LogRelayOutputStdout {
		t.Fatalf("log_relay[1].Output = %q, want stdout", second.Output)
	}
	if second.Path != "" {
		t.Fatalf("log_relay[1].Path = %q, want empty", second.Path)
	}
	if second.Enrich != nil {
		t.Fatalf("log_relay[1].Enrich = %+v, want nil", second.Enrich)
	}
	// max_line_bytes omitted -> default applied.
	if second.MaxLineBytes != 1<<20 {
		t.Fatalf("log_relay[1].MaxLineBytes = %d, want default 1 MiB",
			second.MaxLineBytes)
	}

	// host_key omitted -> defaults to "host".
	third := cfg.LogRelay[2]
	if third.Enrich == nil || third.Enrich.HostKey != "host" {
		t.Fatalf("log_relay[2] host_key default not applied: %+v",
			third.Enrich)
	}
}

// TestLoadMetricsVsockPort verifies metrics.vsock_port round-trips through
// the schema without triggering the mutual-exclusion rule.
func TestLoadMetricsVsockPort(t *testing.T) {
	yamlDoc := `
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 8080}
metrics:
  vsock_port: 9090
`
	cfg, err := config.Load(writeConfig(t, yamlDoc))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Metrics.VsockPort; got != 9090 {
		t.Fatalf("metrics.vsock_port = %d, want 9090", got)
	}
	if cfg.Metrics.Bind != "" {
		t.Fatalf("metrics.bind = %q, want empty", cfg.Metrics.Bind)
	}
}

// TestLoadMetricsEmpty verifies that omitting the metrics section leaves
// both Bind and VsockPort zero-valued — this is the new disable-by-default
// default.
func TestLoadMetricsEmpty(t *testing.T) {
	yamlDoc := `
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 8080}
`
	cfg, err := config.Load(writeConfig(t, yamlDoc))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Metrics.Bind != "" || cfg.Metrics.VsockPort != 0 {
		t.Fatalf("metrics should be empty by default, got %+v", cfg.Metrics)
	}
}

// TestLoadLogLevelInfo verifies the explicit "info" value round-trips.
func TestLoadLogLevelInfo(t *testing.T) {
	yamlDoc := `
log_level: info
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: 8080}
`
	cfg, err := config.Load(writeConfig(t, yamlDoc))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != config.LogLevelInfo {
		t.Fatalf("log_level = %q, want %q",
			cfg.LogLevel, config.LogLevelInfo)
	}
}

// TestLoadExample guards against drift between the schema and the shipped
// example; the example is also a reader-facing source of truth.
func TestLoadExample(t *testing.T) {
	cfg, err := config.Load("../../examples/vsockd.yaml")
	if err != nil {
		t.Fatalf("Load examples/vsockd.yaml: %v", err)
	}
	if len(cfg.Inbound) < 2 {
		t.Fatalf("example should have >=2 inbound listeners, got %d",
			len(cfg.Inbound))
	}
	if len(cfg.Outbound) == 0 {
		t.Fatalf("example should have at least one outbound listener")
	}
	if len(cfg.TCPToVsock) == 0 {
		t.Fatalf("example should have at least one tcp_to_vsock listener")
	}
	if len(cfg.VsockToTCP) == 0 {
		t.Fatalf("example should have at least one vsock_to_tcp listener")
	}
	if len(cfg.LogRelay) == 0 {
		t.Fatalf("example should have at least one log_relay listener")
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := config.Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatalf("expected error for missing file")
	}
}

// TestVsockPortBoundary documents the boundary between accepted vsock port
// values and the reserved PortAny sentinel: 0xFFFFFFFE (4294967294) is a
// valid user port, while 0xFFFFFFFF (4294967295 = vsock.PortAny) must be
// rejected.
func TestVsockPortBoundary(t *testing.T) {
	tmpl := `
inbound:
  - bind: 0.0.0.0
    port: 80
    mode: http-host
    routes:
      - {hostname: a.example.com, cid: 3, vsock_port: %d}
outbound:
  - port: %d
    cids:
      - {cid: 16, allowed_hosts: ["*"]}
`
	t.Run("max valid port 0xFFFFFFFE accepted", func(t *testing.T) {
		const maxPort = 0xFFFFFFFE
		yamlDoc := fmt.Sprintf(tmpl, maxPort, maxPort)
		cfg, err := config.Load(writeConfig(t, yamlDoc))
		if err != nil {
			t.Fatalf("Load rejected valid port 0xFFFFFFFE: %v", err)
		}
		if got := cfg.Inbound[0].Routes[0].VsockPort; got != maxPort {
			t.Fatalf("inbound route vsock_port = %d, want %d", got, maxPort)
		}
		if got := cfg.Outbound[0].Port; got != maxPort {
			t.Fatalf("outbound port = %d, want %d", got, maxPort)
		}
	})

	t.Run("PortAny 0xFFFFFFFF rejected (inbound route)", func(t *testing.T) {
		yamlDoc := fmt.Sprintf(tmpl, uint32(0xFFFFFFFF), uint32(8080))
		_, err := config.Load(writeConfig(t, yamlDoc))
		if err == nil {
			t.Fatalf("expected PortAny (0xFFFFFFFF) to be rejected on route")
		}
		if !strings.Contains(err.Error(), "out of range") {
			t.Fatalf("error %q does not mention out of range", err.Error())
		}
	})

	t.Run("PortAny 0xFFFFFFFF rejected (outbound port)", func(t *testing.T) {
		yamlDoc := fmt.Sprintf(tmpl, uint32(8080), uint32(0xFFFFFFFF))
		_, err := config.Load(writeConfig(t, yamlDoc))
		if err == nil {
			t.Fatalf("expected PortAny (0xFFFFFFFF) to be rejected on port")
		}
		if !strings.Contains(err.Error(), "out of range") {
			t.Fatalf("error %q does not mention out of range", err.Error())
		}
	})
}
