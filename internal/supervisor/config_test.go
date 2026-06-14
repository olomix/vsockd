package supervisor_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olomix/vsockd/internal/supervisor"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "supervisor.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	yamlDoc := `
log_port: 5140
log_cid: 3
tags:
  service: abc
  version: "1.1.2"
buffer:
  max_bytes: 8388608
  max_records: 10000
processes:
  - name: app
    role: task
    command: /usr/local/bin/node
    args: ["server.js"]
    restart: on-failure
    max_restarts: 3
    restart_window: 60s
    on_failure: terminate
  - name: worker
    role: task
    command: /usr/local/bin/worker
    restart: always
    max_restarts: 5
    restart_window: 30s
  - name: vsockd
    role: sidecar
    command: /usr/local/bin/vsockd
    args: ["-config", "/etc/vsockd/vsockd.yaml"]
    restart: always
    max_restarts: 5
    restart_window: 60s
    on_failure: terminate
`
	cfg, err := supervisor.Load(writeConfig(t, yamlDoc))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogPort != 5140 {
		t.Fatalf("LogPort = %d, want 5140", cfg.LogPort)
	}
	if cfg.LogCID != 3 {
		t.Fatalf("LogCID = %d, want 3", cfg.LogCID)
	}
	if cfg.Tags["service"] != "abc" || cfg.Tags["version"] != "1.1.2" {
		t.Fatalf("Tags = %v", cfg.Tags)
	}
	if cfg.Buffer.MaxBytes != 8388608 || cfg.Buffer.MaxRecords != 10000 {
		t.Fatalf("Buffer = %+v", cfg.Buffer)
	}
	if len(cfg.Processes) != 3 {
		t.Fatalf("want 3 processes, got %d", len(cfg.Processes))
	}
	if cfg.Processes[0].Role != supervisor.RoleTask {
		t.Fatalf("processes[0].Role = %q", cfg.Processes[0].Role)
	}
	if cfg.Processes[0].Args[0] != "server.js" {
		t.Fatalf("processes[0].Args = %v", cfg.Processes[0].Args)
	}
	if cfg.Processes[2].Role != supervisor.RoleSidecar {
		t.Fatalf("processes[2].Role = %q", cfg.Processes[2].Role)
	}
	if cfg.Processes[0].RestartWindow.Duration() != 60*time.Second {
		t.Fatalf("processes[0].RestartWindow = %s",
			cfg.Processes[0].RestartWindow.Duration())
	}
}

func TestDefaults(t *testing.T) {
	// Minimal config: only log_port and one task with the required fields.
	// restart, on_failure, log_cid, and the buffer budget all default.
	yamlDoc := `
log_port: 5140
processes:
  - name: app
    role: task
    command: /bin/true
`
	cfg, err := supervisor.Load(writeConfig(t, yamlDoc))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogCID != supervisor.DefaultLogCID {
		t.Fatalf("LogCID = %d, want %d", cfg.LogCID, supervisor.DefaultLogCID)
	}
	if cfg.Processes[0].Restart != supervisor.RestartOnFailure {
		t.Fatalf("Restart = %q, want %q",
			cfg.Processes[0].Restart, supervisor.RestartOnFailure)
	}
	if cfg.Processes[0].OnFailure != supervisor.OnFailureTerminate {
		t.Fatalf("OnFailure = %q, want %q",
			cfg.Processes[0].OnFailure, supervisor.OnFailureTerminate)
	}
	if cfg.Buffer.MaxBytes <= 0 || cfg.Buffer.MaxRecords <= 0 {
		t.Fatalf("Buffer defaults not applied: %+v", cfg.Buffer)
	}
}

func TestZeroTasksDaemonMode(t *testing.T) {
	// Zero processes is valid (daemon mode that runs until an external signal).
	yamlDoc := `
log_port: 5140
`
	if _, err := supervisor.Load(writeConfig(t, yamlDoc)); err != nil {
		t.Fatalf("zero-process config should be valid: %v", err)
	}
}

// TestLoadExample guards against drift between the schema and the shipped
// example; the example is also a reader-facing source of truth.
func TestLoadExample(t *testing.T) {
	cfg, err := supervisor.Load("../../examples/supervisor.yaml")
	if err != nil {
		t.Fatalf("Load examples/supervisor.yaml: %v", err)
	}
	var tasks, sidecars int
	for _, p := range cfg.Processes {
		switch p.Role {
		case supervisor.RoleTask:
			tasks++
		case supervisor.RoleSidecar:
			sidecars++
		}
	}
	if tasks == 0 {
		t.Fatalf("example should have at least one task")
	}
	if sidecars == 0 {
		t.Fatalf("example should have at least one sidecar")
	}
	// The log_port must match the host-side log_relay example so the two
	// halves of the documented log path line up.
	if cfg.LogPort != 5140 {
		t.Fatalf("example log_port = %d, want 5140 (log_relay example)",
			cfg.LogPort)
	}
}

func TestLoadInvalid(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "missing log_port",
			yaml:    "processes: []\n",
			wantErr: "log_port",
		},
		{
			name:    "log_port out of range",
			yaml:    "log_port: 4294967295\n",
			wantErr: "log_port",
		},
		{
			name: "log_cid out of range",
			yaml: `
log_port: 5140
log_cid: 2
`,
			wantErr: "log_cid",
		},
		{
			name: "missing command",
			yaml: `
log_port: 5140
processes:
  - name: app
    role: task
`,
			wantErr: "command",
		},
		{
			name: "missing role",
			yaml: `
log_port: 5140
processes:
  - name: app
    command: /bin/true
`,
			wantErr: "role",
		},
		{
			name: "bad role",
			yaml: `
log_port: 5140
processes:
  - name: app
    role: bogus
    command: /bin/true
`,
			wantErr: "role",
		},
		{
			name: "bad restart",
			yaml: `
log_port: 5140
processes:
  - name: app
    role: task
    command: /bin/true
    restart: sometimes
`,
			wantErr: "restart",
		},
		{
			name: "bad on_failure",
			yaml: `
log_port: 5140
processes:
  - name: app
    role: task
    command: /bin/true
    on_failure: explode
`,
			wantErr: "on_failure",
		},
		{
			name: "duplicate process name",
			yaml: `
log_port: 5140
processes:
  - name: app
    role: task
    command: /bin/true
  - name: app
    role: sidecar
    command: /bin/false
`,
			wantErr: "duplicate",
		},
		{
			name: "restart_window required when restarts can occur",
			yaml: `
log_port: 5140
processes:
  - name: app
    role: task
    command: /bin/true
    restart: always
    max_restarts: 3
`,
			wantErr: "restart_window",
		},
		{
			name: "negative max_restarts",
			yaml: `
log_port: 5140
processes:
  - name: app
    role: task
    command: /bin/true
    restart: always
    max_restarts: -1
    restart_window: 30s
`,
			wantErr: "max_restarts",
		},
		{
			name: "negative buffer size",
			yaml: `
log_port: 5140
buffer:
  max_bytes: -1
`,
			wantErr: "max_bytes",
		},
		{
			name: "explicit empty buffer",
			yaml: `
log_port: 5140
buffer:
  max_bytes: 0
  max_records: 0
`,
			wantErr: "buffer",
		},
		{
			name: "empty tag key",
			yaml: `
log_port: 5140
tags:
  "": abc
`,
			wantErr: "tags",
		},
		{
			name: "empty tag value",
			yaml: `
log_port: 5140
tags:
  service: ""
`,
			wantErr: "tags",
		},
		{
			name: "unknown field rejected",
			yaml: `
log_port: 5140
bogus_field: 1
`,
			wantErr: "bogus_field",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := supervisor.Load(writeConfig(t, tc.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}
