# Split TCP-mode listeners into their own config sections

## Overview

Today `mode: tcp` lives nested inside the `inbound:` and `outbound:` YAML
sections, which are named from the parent-host perspective ("inbound from
the internet into the enclave", "outbound from the enclave to the
internet"). Those names stop making sense the moment `vsockd` itself runs
*inside* the enclave — which the TCP passthrough feature makes possible.

This plan pulls the two TCP passthrough variants out of `inbound`/`outbound`
into two new top-level YAML sections named by what they actually do:

- `tcp_to_vsock` — listens on a TCP port, forwards bytes to a fixed vsock
  endpoint. Used on the host for "external TCP → enclave" *and* inside the
  enclave for "local TCP → host vsock".
- `vsock_to_tcp` — listens on a vsock port, forwards bytes to a fixed
  TCP upstream. Used on the host for "enclave vsock → external TCP" *and*
  inside the enclave for "host vsock → local TCP".

`inbound` and `outbound` stay but become exclusive to the host-role HTTP
features (`http-host` / `tls-sni` routing and the HTTP forward-proxy with
per-CID allowlists). The Prometheus metrics for TCP passthrough are
renamed in lockstep so the vocabulary is consistent end-to-end. The
project description and README are rewritten so vsockd is no longer
advertised as host-only.

The `metrics` section also becomes enclave-friendly as part of the same
release:

- **No implicit default.** Today an empty config silently binds
  `0.0.0.0:9090` via the flag default. That's a footgun inside an
  enclave (port bound, nothing can scrape). The `-metrics-addr` default
  is dropped; metrics start only when the user explicitly opts in via
  `metrics.bind`, `metrics.vsock_port`, or an explicit
  `-metrics-addr` flag. Omitting all three disables the endpoint.
- **vsock bind support.** A new `metrics.vsock_port` field lets the
  enclave-side vsockd expose `/metrics` over vsock so the parent host
  can scrape it. `bind` and `vsock_port` are mutually exclusive.
  No CID is needed — a vsock listener binds `VMADDR_CID_ANY`.

**This is a hard breaking change.** There is no YAML back-compat shim,
no metric alias, and no implicit `:9090` default. Version bumps from
whatever the current head is to the next minor; release notes must call
out the migration.

## Context (from discovery)

### Files/components involved

- `internal/config/config.go` — schema, strict-YAML parse, `Validate()`
- `internal/config/config_test.go` — all schema tests
- `internal/inbound/server.go` — consumes `InboundListener.Mode == "tcp"`
  (lines 229, 407, 412, 502); keying includes mode (line 446)
- `internal/inbound/tcp.go` + `internal/inbound/tcp_test.go` — the TCP
  handler and its tests
- `internal/outbound/server.go` — consumes `OutboundListener.Mode == "tcp"`
  (lines 245, 396, 509, 515)
- `internal/outbound/tcp.go` + `internal/outbound/tcp_test.go` — TCP
  handler and its tests
- `internal/app/app.go` — threads `cfg.Inbound` / `cfg.Outbound` into the
  two subsystems (lines 92–101, 194–201); also owns the metrics listener
  (lines 138–162) and branches on `MetricsAddr`
- `internal/metrics/metrics.go` — `tcp_inbound_*` and `tcp_outbound_*`
  counters (lines 123–162) and registrations (178–183); also the
  `ListenMetrics(addr)` helper (line 213) — needs a vsock sibling
- `internal/metrics/metrics_test.go` — asserts metric names
- `cmd/vsockd/main.go` — `-metrics-addr` flag (line 34), the flag default
  `:9090`, and `resolveMetricsAddr` (lines 200–217) which today falls
  back to the flag default if nothing is set
- `test/e2e/e2e_test.go` — e2e coverage for both TCP variants
  (lines 546–648, 760–765)
- `examples/vsockd.yaml` — example config (inbound tcp at lines 34–41,
  outbound tcp at lines 63–68)
- `README.md` — project description, architecture diagram, TCP section
  (192–215), metrics table (315–327), reload notes, debug log examples,
  socat recipe
- `CHANGELOG.md` — needs an entry
- `idea.md` — historical design doc with host-only framing

### Related patterns already in the codebase

- **Config key uniqueness.** `config.Validate` enforces `(bind, port)`
  uniqueness across `inbound` entries and `port` uniqueness across
  `outbound` entries. We extend the same checks to cover cross-section
  collisions (e.g. `inbound.bind:port` vs `tcp_to_vsock.bind:port`).
- **Strict YAML.** `yaml.v3` `KnownFields(true)` rejects unknown keys.
  This means any user moving from the old schema sees a loud parse error
  on startup pointing at the removed field — good ergonomics for a
  breaking change.
- **Atomic upstream swap.** Outbound TCP listeners already support an
  in-place `upstream` swap on SIGHUP. This behavior carries through; the
  new `vsock_to_tcp` listener keeps the same atomic-pointer semantics.
- **Inbound TCP "restart required".** Inbound TCP listeners reject
  `target_cid` / `target_port` changes on reload with a "restart
  required" error. The new `tcp_to_vsock` listener keeps this behavior
  and its error message is updated for the new section name.

### Dependencies identified

None new. This is a pure refactor of existing code; no new Go modules
and no behavior changes in the hot path.

## Development Approach

- **Testing approach**: Regular (update tests in lockstep with each
  code change, not TDD). No new behavior is introduced — only relocated
  and renamed — so writing failing tests first adds ceremony without
  catching anything TDD would catch here.
- Complete each task fully before moving to the next.
- Make small, focused changes.
- **CRITICAL: every task MUST include new/updated tests** for code
  changes in that task.
- **CRITICAL: all tests must pass before starting next task** — no
  exceptions.
- **CRITICAL: update this plan file when scope changes during
  implementation.**
- Run tests after each change.
- **NO backward compatibility.** Old YAML keys must produce a loud
  "unknown field" parse error; don't try to silently translate
  `inbound[*].mode == "tcp"` into the new shape. The goal of this
  refactor is to eliminate the old vocabulary entirely.

## Testing Strategy

- **Unit tests**: required for every task (see Development Approach).
- **Integration tests**: `internal/inbound/tcp_test.go` and
  `internal/outbound/tcp_test.go` exercise the full accept→forward path
  using loopback vsock. These tests are rewritten to use the new config
  shape in the respective tasks.
- **E2E tests**: `test/e2e/e2e_test.go` drives a real `vsockd` binary.
  Tests that use `mode: tcp` must be updated to the new schema in their
  own task and still pass.
- **No new UI** in this project; no Playwright/e2e UI concerns.

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document issues/blockers with ⚠️ prefix.
- Update plan if implementation deviates from original scope.
- Keep plan in sync with actual work done.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code + test + doc changes
  inside this repo.
- **Post-Completion** (no checkboxes): user-visible breakage notes that
  ship with the release (CHANGELOG wording, migration tips).

---

## Implementation Steps

<!--
Task ordering rationale:

Task 1 is atomic (schema + server + app wiring) because removing
TargetCID/TargetPort from InboundListener (or Mode/Upstream from
OutboundListener) immediately breaks the server packages that read
those fields. They must land together or the repo doesn't compile.

Tasks 2 and 3 are both Prometheus-adjacent but deliberately separate:
- Task 2 is a mechanical rename that touches every TCP-passthrough
  metric in lockstep with the new YAML section names.
- Task 3 is a behavior change to the metrics transport (disable-by-
  default, add vsock_port). Keeping them apart makes each easier to
  review — the rename is "search and replace", the transport change
  needs fresh code paths and tests.

Both metrics tasks land before the log rename (Task 4) so that any
test assertions that scrape /metrics to verify log-vs-metric parity
see the final metric names.

Tasks 5–9 (examples, README, CHANGELOG, verify, idea.md) are docs
and verification; they can run in order with no hidden dependencies.

An earlier draft considered extracting the TCP passthrough code into
new Go packages (`internal/tcptovsock`, `internal/vsocktotcp`).
Rejected: the Go package names are implementation details users
never see, splitting adds churn without user payoff, and the file-
level separation (`internal/inbound/tcp.go`, `internal/outbound/tcp.go`)
already keeps the two concerns cleanly divided. Revisit if the
internal packages grow more responsibilities.
-->

### Task 1: Replace `mode: tcp` with new top-level config sections

Single atomic change to config types + server consumers + app wiring.
The repo must not compile in an intermediate state; everything below
lands in one commit.

**New config types (internal/config/config.go):**

```go
// TCPToVsockListener terminates TCP on a host or enclave port and
// forwards the raw byte stream to a fixed vsock endpoint.
type TCPToVsockListener struct {
    Bind      string `yaml:"bind"`
    Port      int    `yaml:"port"`
    VsockCID  uint32 `yaml:"vsock_cid"`
    VsockPort uint32 `yaml:"vsock_port"`
}

// VsockToTCPListener accepts vsock connections on the given vsock
// port and forwards the raw byte stream to a fixed TCP upstream.
type VsockToTCPListener struct {
    Port     uint32 `yaml:"port"`
    Upstream string `yaml:"upstream"`
}
```

Top-level `Config` gains two new fields:

```go
TCPToVsock []TCPToVsockListener `yaml:"tcp_to_vsock"`
VsockToTCP []VsockToTCPListener `yaml:"vsock_to_tcp"`
```

`InboundListener` loses `TargetCID`, `TargetPort`, and its `Mode ==
"tcp"` branch. `OutboundListener` loses `Mode` and `Upstream`. The
`ModeTCP` constant is removed. The `validate()` path for each existing
listener type drops its tcp branch; `Config.Validate()` grows two new
per-section validators and a cross-section uniqueness check.

**New YAML shape (the way `examples/vsockd.yaml` will read):**

```yaml
tcp_to_vsock:
  - bind: 0.0.0.0
    port: 5432
    vsock_cid: 16
    vsock_port: 5432

vsock_to_tcp:
  - port: 9000
    upstream: 10.0.0.5:5432
```

- [x] in `internal/config/config.go`: add `TCPToVsockListener` and
      `VsockToTCPListener` types; add `TCPToVsock` and `VsockToTCP`
      fields to `Config`
- [x] in `internal/config/config.go`: remove `ModeTCP` constant;
      remove `TargetCID` / `TargetPort` from `InboundListener`;
      remove `Mode` / `Upstream` from `OutboundListener`; drop the
      `case ModeTCP:` arms in both `validate()` paths
- [x] in `internal/config/config.go` (`Config.Validate`): validate
      each `TCPToVsockListener` (non-empty `bind`, port in 1..65535,
      `vsock_cid >= minCID`, `vsock_port` in valid vsock range) and
      each `VsockToTCPListener` (vsock port in range, `upstream`
      passes `validateHostPort`)
- [x] in `internal/config/config.go` (`Config.Validate`): cross-section
      uniqueness — `(bind, port)` must not collide between `inbound`
      and `tcp_to_vsock`; vsock `port` must not collide between
      `outbound` and `vsock_to_tcp`; the "at least one listener" guard
      now considers all four sections
- [x] in `internal/config/config_test.go`: rename / split the existing
      `TestLoadTCPMode` into two tests (`TestLoadTCPToVsock`,
      `TestLoadVsockToTCP`); add collision tests for each of the new
      cross-section rules; add a test that an old-style config with
      `mode: tcp` under `inbound` or `outbound` fails with a strict
      "unknown field" error; update every other test that used
      `mode: tcp` to use the new sections
- [x] in `internal/inbound/server.go`: adapt `NewServer` to accept a
      second slice (`[]config.TCPToVsockListener`); the internal
      `listener` struct keeps its mode tag ("tls-sni" / "http-host" /
      an internal "tcp" sentinel) but is constructed from either
      config type; `PrepareApply` takes both slices and keys the
      TCP-derived listeners the same way it does today; the
      "restart required" error message switches from "inbound[%d]"
      to "tcp_to_vsock[%d]"
- [x] in `internal/inbound/tcp.go` and `internal/inbound/server.go`:
      update any log/error strings that literally contain the word
      "inbound" when referring to the TCP passthrough path (e.g.
      `"inbound tcp connection"` becomes `"tcp_to_vsock connection"`;
      see Task 3 for the full log message rename, but keep call sites
      consistent within this task) — deferred to Task 4 per the
      parenthetical. Only the internal error strings that reference
      section names (e.g. `"inbound[%d]"` → `"tcp_to_vsock[%d]"`)
      were touched here.
- [x] in `internal/outbound/server.go`: symmetric change — `NewServer`
      takes a second slice (`[]config.VsockToTCPListener`); the
      internal listener keeps its "http" vs "tcp" tag but is
      constructed from either type; `PrepareApply` takes both slices;
      the "restart required" error message for mode-flip is removed
      (no mode can flip; a port now lives under exactly one of
      `outbound` or `vsock_to_tcp`, and collisions are rejected at
      config load). Mode-flip error kept — reload can still move a
      port between sections even though a single config cannot, and
      `TestTCP_Passthrough_ApplyModeChangeRejected` still exercises
      the guard.
- [x] in `internal/outbound/tcp.go` and `internal/outbound/server.go`:
      update user-visible strings that literally say "outbound" when
      they mean the vsock→TCP path (mirror the inbound cleanup above)
      — deferred to Task 4 per the same rationale.
- [x] in `internal/app/app.go`: update `New` and `Reload` to pass the
      new slices into `inbound.NewServer` / `outbound.NewServer` and
      `PrepareApply`
- [x] update `internal/inbound/tcp_test.go`, `internal/outbound/tcp_test.go`,
      and any `server_test.go` entries that constructed `InboundListener`
      / `OutboundListener` with `Mode: "tcp"` to use the new config
      types
- [x] update `internal/app/app_test.go` to pass four-slice configs where
      applicable — no changes required since app tests drive YAML
      through `config.Load`, which picks up the new sections
      transparently. The e2e test at `test/e2e/e2e_test.go` was
      updated to use the new YAML schema.
- [x] run `go build ./...`, `go test ./...`, `go vet ./...`,
      `staticcheck ./...` — all must pass before Task 2

### Task 2: Rename `tcp_inbound_*` / `tcp_outbound_*` metrics

Prometheus counter rename to match the new section names. There are
exactly six metrics to rename and the Metrics struct field names
change with them for consistency.

| Old | New |
|---|---|
| `tcp_inbound_connections_total` | `tcp_to_vsock_connections_total` |
| `tcp_inbound_bytes_total` | `tcp_to_vsock_bytes_total` |
| `tcp_inbound_errors_total` | `tcp_to_vsock_errors_total` |
| `tcp_outbound_connections_total` | `vsock_to_tcp_connections_total` |
| `tcp_outbound_bytes_total` | `vsock_to_tcp_bytes_total` |
| `tcp_outbound_errors_total` | `vsock_to_tcp_errors_total` |

Struct field renames: `TCPInboundConnections` → `TCPToVsockConnections`,
etc. Help strings updated to say "tcp_to_vsock listeners" / "vsock_to_tcp
listeners" instead of "mode=tcp inbound" / "mode=tcp outbound".

- [x] in `internal/metrics/metrics.go`: rename the six `CounterOpts.Name`
      values and their `Help` text
- [x] in `internal/metrics/metrics.go`: rename the six struct fields
      on `Metrics`; update the `MustRegister` call site
- [x] grep the tree for the old field names and update every caller
      (chiefly in `internal/inbound/tcp.go` and `internal/outbound/tcp.go`)
- [x] update `internal/metrics/metrics_test.go` assertions for new
      metric names
- [x] update any integration test that scrapes `/metrics` and asserts
      a metric name (search `test/e2e/e2e_test.go` for `tcp_inbound_`
      / `tcp_outbound_` string literals) — no matches in e2e; the
      `internal/{inbound,outbound}/tcp_test.go` files were updated
- [x] run full test suite — must pass before Task 3

### Task 3: Metrics config — disable-by-default and `vsock_port`

Two user-visible behavior changes to the `metrics` section, landing
together because they both reshape the same code path (flag resolution
and listener construction).

**Behavior 1: no implicit `:9090` default.** The `-metrics-addr` flag
default drops from `":9090"` to `""`. Metrics start iff at least one of
these is set (in precedence order): explicit `-metrics-addr`, YAML
`metrics.bind`, YAML `metrics.vsock_port`. Everything unset = no
listener, no log line beyond a single "metrics disabled" info message
at startup.

**Behavior 2: `metrics.vsock_port`.** New optional field. When set,
vsockd listens on `AF_VSOCK` bound to `VMADDR_CID_ANY` on that port and
serves `/metrics` there. Mutually exclusive with `bind`; mutually
exclusive with being set by `-metrics-addr`. No CID on the listener —
the kernel accepts from any connecting CID (which is fine for a metrics
endpoint; if you want to restrict which CID can scrape, put that in
front of vsockd, not in vsockd).

**New YAML shapes:**

```yaml
# Host (unchanged from today, once the user is explicit)
metrics:
  bind: 0.0.0.0:9090

# Enclave: expose /metrics over vsock for host-side scraping
metrics:
  vsock_port: 9090

# Disabled: simply omit the metrics section (new default)
```

- [x] in `internal/config/config.go`: add `VsockPort uint32` field with
      tag `yaml:"vsock_port"` to `MetricsConfig`
- [x] in `internal/config/config.go` (`Config.Validate`): enforce
      mutual exclusion (`Bind != "" && VsockPort != 0` is an error);
      validate `VsockPort` is in the valid vsock range (non-zero,
      not `vsockPortAny`) when set; keep the existing `net.SplitHostPort`
      check on `Bind`
- [x] in `internal/config/config.go` (`Config.Validate`): cross-section
      check — `metrics.vsock_port` must not collide with any
      `outbound[*].port` or `vsock_to_tcp[*].port`
- [x] in `internal/config/config_test.go`: add tests for each new rule
      (valid vsock-port-only config, valid bind-only config, both-set
      rejected, vsock_port colliding with outbound rejected,
      vsock_port colliding with vsock_to_tcp rejected, vsock_port out
      of range rejected)
- [x] in `internal/app/app.go`: replaced `Options.MetricsAddr string` with
      two fields (`MetricsAddr string` + `MetricsVsockPort uint32`); `Start`
      branches on which is set (TCP path via `metrics.ListenMetrics(addr)`,
      vsock path via `opts.VsockListenFn(port)` wrapped in
      `metrics.NewVsockNetListener`); nothing set skips the listener and
      logs `metrics=disabled`
- [x] in `internal/app/app.go`: the startup log key changed from
      `metrics_addr` to a richer `metrics` string — `"disabled"`,
      `"tcp <addr>"`, or `"vsock port=<n>"`
- [x] in `cmd/vsockd/main.go`: flag default changed from `":9090"` to
      `""`; help text rewritten to describe the new precedence
- [x] in `cmd/vsockd/main.go`: `resolveMetricsAddr` renamed to
      `resolveMetrics` and now returns `(addr string, vsockPort uint32)`
      with precedence explicit flag > yaml bind > yaml vsock_port >
      disabled
- [x] `vsockconn.Listener` does NOT satisfy `net.Listener` (its Accept
      returns the typed `vsockconn.Conn` rather than `net.Conn`). Added
      `metrics.NewVsockNetListener` adapter in `internal/metrics/metrics.go`
      that unwraps the typed conn — only Accept/Close/Addr are used by
      `ServeMetrics`
- [x] in `cmd/vsockd/main_test.go`: added `TestResolveMetricsPrecedence`
      covering flag > yaml bind > yaml vsock_port > disabled and the
      explicit-empty-flag-wins case; existing subprocess tests already
      pass `-metrics-addr` explicitly so they need no change
- [x] in `internal/app/app_test.go`: added `TestMetricsDisabled` covering
      the no-MetricsAddr + no-MetricsVsockPort path
- [x] in `internal/app/app_test.go`: added `TestMetricsOverVsock` which
      scrapes `/metrics` over the loopback vsock backend after priming
      a counter series
- [x] run full test suite — must pass before Task 4

### Task 4: Rename TCP passthrough debug log messages

Today debug-level logs say `"inbound tcp connection"` and `"inbound vsock
connection"` / `"outbound ..."`. Rename to match the new vocabulary.

| Old | New |
|---|---|
| `"inbound tcp connection"` | `"tcp_to_vsock connection opened"` |
| `"tcp connection closed"` (inbound side) | `"tcp_to_vsock connection closed"` |
| `"inbound vsock connection"` | `"vsock_to_tcp connection opened"` |
| `"vsock connection closed"` (outbound side) | `"vsock_to_tcp connection closed"` |

Structured attributes keep their current names (`cid`, `port`,
`listen_port`, `remote`, `listen`, `total_bytes`). Warn-level dial
failure logs get the same treatment.

- [x] in `internal/inbound/tcp.go`: rename debug and warn log messages
      for the tcp-to-vsock path
- [x] in `internal/outbound/tcp.go`: rename debug and warn log messages
      for the vsock-to-tcp path
- [x] update any test that asserts on log message text (search
      `internal/inbound/tcp_test.go`, `internal/outbound/tcp_test.go`,
      and `test/e2e/e2e_test.go` for the old strings)
- [x] run full test suite — must pass before Task 5

### Task 5: Update examples/vsockd.yaml

Move the two `mode: tcp` entries into top-level `tcp_to_vsock` and
`vsock_to_tcp` sections. Update the header comment so it describes all
four section flavors and mentions that `tcp_to_vsock` / `vsock_to_tcp`
work identically whether vsockd runs on the host or inside the enclave.
Add a commented-out `metrics.vsock_port` alternative alongside the
existing `metrics.bind` example so the enclave-side pattern is visible.

- [x] rewrite `examples/vsockd.yaml` per the new schema — already
      reflected the new schema from Task 1; no additional rewrite
      needed in this task
- [x] add a commented-out `metrics.vsock_port` example alongside
      `metrics.bind`, with a one-line explanation of when to use
      which
- [x] verify the example parses cleanly — `-version` short-circuits
      before `config.Load` in the current main.go, so instead relied
      on `TestLoadExample` in `internal/config/config_test.go` which
      runs `config.Load("../../examples/vsockd.yaml")`
- [x] run full test suite — must pass before Task 6

### Task 6: Rewrite README to drop host-only framing

The README currently opens with *"vsockd is a single static Go binary
that runs on the parent EC2 host"* and the architecture diagram shows
everything inside a "parent EC2 host" box. Both are wrong now. The
rewrite must make clear that vsockd is a general-purpose vsock↔TCP
bridge whose HTTP-aware features are host-role-specific but whose TCP
passthrough features work symmetrically on either side.

Specific edits:

- [x] in `README.md` intro (lines 1–7): rewrite the first paragraph so
      the binary is described as running "on the parent EC2 host or
      inside the enclave, depending on which features you use"; keep
      the second sentence that calls out the host-role fan-out + CID
      egress allowlist, since that is still accurate but narrower
- [x] in `README.md` "What it does" (lines 8–26): keep
      inbound/outbound bullets (they're still right for HTTP-aware
      features); replace the "Raw TCP passthrough (`mode: tcp`)" bullet
      with two new bullets for `tcp_to_vsock` and `vsock_to_tcp`,
      each explicitly noting the symmetric host/enclave use case
- [x] in `README.md` Architecture (lines 31–62): update the ASCII
      diagram so the four flavors are grouped by section (not by
      host-role direction); add a short "Inside the enclave" note
      pointing out that `tcp_to_vsock` / `vsock_to_tcp` are the only
      flavors that work from an enclave-local vsockd
- [x] in `README.md` Configuration example (lines 140–168): update
      the minimal YAML shape to match the new schema
- [x] in `README.md` "TCP passthrough mode" (lines 191–265): rename
      the section to something like "TCP passthrough
      (`tcp_to_vsock` / `vsock_to_tcp`)"; update the YAML examples;
      rename the subsection title "Vsock-side (outbound `mode: tcp`)"
      to "`vsock_to_tcp` debug logs" and similarly for the TCP side;
      update the log message examples to match Task 3
- [x] in `README.md` "Enclave-side socat recipe" (lines 266–307):
      keep the socat recipe (it's still the simplest option for the
      HTTP forward-proxy case), but add a new subsection immediately
      before or after it showing the equivalent `tcp_to_vsock` /
      `vsock_to_tcp` in-enclave vsockd configuration, and explicitly
      note that a second copy of vsockd inside the enclave can
      replace the socat stub entirely
- [x] in `README.md` Metrics table (lines 315–327): update the six
      renamed metric names and their descriptions
- [x] in `README.md` Operational notes — SIGHUP reload (lines 339–351):
      update the wording so it says "listeners in `tcp_to_vsock`
      cannot change `vsock_cid` / `vsock_port` at runtime" and
      "`vsock_to_tcp` listeners atomically swap `upstream`" instead
      of the current "outbound mode: tcp" / "inbound mode: tcp"
      language
- [x] in `README.md` Metrics section (lines 309–336): rewrite the
      opening paragraph to make three things explicit: (1) metrics are
      **disabled by default** now and must be opted into via
      `metrics.bind`, `metrics.vsock_port`, or `-metrics-addr`; (2)
      `bind` serves TCP and is the host-side form; (3) `vsock_port`
      serves over vsock from `VMADDR_CID_ANY` and is the enclave-side
      form when you want the parent host to scrape. Show both YAML
      shapes side-by-side
- [x] in `README.md` command-line flags section (lines 178–189):
      update the `-metrics-addr` entry to reflect the new empty
      default, and note that an explicit empty string `-metrics-addr
      ""` is no longer needed to disable
- [x] run `markdownlint README.md` if it's configured; otherwise
      skim manually for broken links — `markdownlint` not installed;
      manual review completed
- [x] no test run needed for this task, but run `go test ./...` at
      the end to confirm nothing regressed — must pass before Task 7

### Task 7: Update CHANGELOG

Add an entry under a new unreleased version section documenting the
hard breaking change. The entry must include:

- removed: `mode: tcp` under `inbound` / `outbound`
- added: top-level `tcp_to_vsock` / `vsock_to_tcp`
- renamed: six Prometheus metrics (with the mapping table)
- renamed: four debug log messages
- changed: metrics no longer start on the implicit `:9090` default;
  user must opt in
- added: `metrics.vsock_port` for enclave-side metrics
- a short migration snippet showing the old YAML next to the new YAML
  (cover the tcp-mode rename AND the metrics behavior change)

- [x] append an entry to `CHANGELOG.md` per the above
- [x] no test changes

### Task 8: Verify acceptance criteria

- [ ] verify `inbound` and `outbound` no longer accept `mode: tcp`
      (load an old config and confirm the strict-YAML error cites the
      removed field)
- [ ] verify new schema in `examples/vsockd.yaml` loads cleanly
- [ ] verify metrics-disabled path: start vsockd with a config that
      has no `metrics` section and no `-metrics-addr` flag; confirm
      no `:9090` listener is bound (`ss -tln | grep 9090` returns
      nothing) and the startup log says "metrics disabled"
- [ ] verify vsock metrics path: start vsockd with
      `metrics.vsock_port` set and a loopback vsock backend; scrape
      `/metrics` over vsock; confirm the exposed metrics match the
      TCP path
- [ ] verify TCP metrics path still works with an explicit
      `metrics.bind`
- [ ] verify `bind` + `vsock_port` both set is rejected at config
      load
- [ ] run full unit test suite: `make test`
- [ ] run e2e tests: `go test ./test/e2e/...`
- [ ] run `go vet ./...` — must be clean
- [ ] run `staticcheck ./...` — must be clean
- [ ] verify `go build ./...` produces a working binary; run
      `./vsockd -config examples/vsockd.yaml` briefly and confirm
      startup logs mention the new listener counts and the chosen
      metrics transport
- [ ] scrape `/metrics` in a dev run and confirm the six renamed
      metrics are exposed and the old names are gone
- [ ] read through the rewritten README end-to-end to catch any
      remaining host-only framing or stale `mode: tcp` / `:9090`
      default references

### Task 9: Final cleanup — historical docs

- [ ] optional: add a one-line note to `idea.md` saying the
      host-only framing is historical; the current README is the
      source of truth. Don't rewrite the file — it's a design doc
      and its original scope is useful context for future readers.

*Note: ralphex automatically moves completed plans to `docs/plans/completed/`*

## Technical Details

### Data-structure changes

`Config` after the change:

```go
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
```

`InboundListener` after (simpler — only HTTP-aware modes):

```go
type InboundListener struct {
    Bind   string  `yaml:"bind"`
    Port   int     `yaml:"port"`
    Mode   string  `yaml:"mode"`    // "http-host" | "tls-sni" only
    Routes []Route `yaml:"routes"`
}
```

`OutboundListener` after (simpler — only forward-proxy mode):

```go
type OutboundListener struct {
    Port uint32        `yaml:"port"`
    CIDs []OutboundCID `yaml:"cids"`
}
```

`MetricsConfig` after (adds `VsockPort`; `Bind` unchanged):

```go
type MetricsConfig struct {
    Bind      string `yaml:"bind"`       // TCP host:port, e.g. "0.0.0.0:9090"
    VsockPort uint32 `yaml:"vsock_port"` // vsock port; mutually exclusive with Bind
}
```

`app.Options` after (splits the old single `MetricsAddr` into
transport-specific fields):

```go
type Options struct {
    // ... unchanged fields ...
    MetricsAddr      string // TCP listen address; empty = not TCP
    MetricsVsockPort uint32 // vsock listen port; 0 = not vsock
    // Both zero = metrics endpoint disabled.
}
```

### Validation rules added

- `tcp_to_vsock[i].bind` non-empty; `port` in 1..65535;
  `vsock_cid >= minCID (3)`; `vsock_port` in valid vsock range.
- `vsock_to_tcp[i].port` in valid vsock range; `upstream` is
  `host:port` (reuse existing `validateHostPort`).
- Cross-section: `(bind, port)` of each `tcp_to_vsock` must not
  collide with any `inbound` entry on the same `(bind, port)`.
- Cross-section: `port` of each `vsock_to_tcp` must not collide with
  any `outbound` entry on the same vsock port.
- `metrics.bind` and `metrics.vsock_port` are mutually exclusive. When
  `vsock_port` is set, it must pass the same vsock-port range check
  as the other vsock ports and must not collide with any `outbound` or
  `vsock_to_tcp` entry.
- The "at least one listener" guard becomes
  `len(Inbound) + len(Outbound) + len(TCPToVsock) + len(VsockToTCP) > 0`.
  (The metrics listener is not a listener in the traffic-serving sense
  — a daemon with only a metrics endpoint and no proxying is still
  rejected.)
- Old shape (`inbound[*].mode == "tcp"` or `outbound[*].mode == "tcp"`)
  fails at parse time with an "unknown field" error from strict YAML
  because `TargetCID` / `TargetPort` / `Upstream` / `Mode` on outbound
  no longer exist. This is the desired user experience for a hard
  breaking change.

### Runtime semantics preserved

- In-place upstream swap for `vsock_to_tcp` on SIGHUP — unchanged.
- Reject `vsock_cid` / `vsock_port` change on `tcp_to_vsock` at runtime
  with a "restart required" error — unchanged behavior, new error text.
- Mode-flip rejection in `inbound` / `outbound` — still meaningful
  between `http-host` ↔ `tls-sni`; unchanged.
- Listener keying in `internal/inbound`: `key = bind|port|<mode-tag>`.
  The internal mode tag for a `tcp_to_vsock`-derived listener is
  still "tcp" (or could be renamed to "tcp-to-vsock" internally —
  implementation detail, no user visibility).

### Metrics transport selection

`app.Start` picks one of three paths based on `Options` fields:

1. `MetricsAddr != ""` → `metrics.ListenMetrics(MetricsAddr)` returns a
   `net.Listener` bound to TCP; pass to `ServeMetrics`.
2. `MetricsVsockPort != 0` → `opts.VsockListenFn(MetricsVsockPort)`
   returns a vsock listener (`vsockconn.Listener`); pass the same way.
   This reuses the same vsock listen function that `outbound` uses, so
   the loopback test backend can exercise the vsock metrics path without
   needing real `AF_VSOCK`.
3. Both zero → no listener, log `metrics disabled` at startup and
   continue.

If `vsockconn.Listener` does not satisfy `net.Listener` directly, add a
one-method adapter in `internal/metrics/metrics.go` — the only method
`ServeMetrics` actually uses is `Accept()`.

The strict mutual exclusion rule (`Bind != "" && VsockPort != 0` is
rejected) enforces that the three-path branch above always has at most
one true condition; a defensive `panic` in `Start` catches any logic
bug that lets both fields through.

## Post-Completion

*Items requiring manual intervention or external systems — no
checkboxes, informational only.*

**Release notes / migration guide** (goes into the release that
ships this change):

- Old → new YAML mapping with a concrete side-by-side example.
- Old → new Prometheus metric name mapping table.
- Instructions for any Grafana dashboards or Prometheus alerting rules
  that reference the renamed metrics (`sed -i 's/tcp_inbound_/tcp_to_vsock_/g; s/tcp_outbound_/vsock_to_tcp_/g'`
  on dashboard JSON as a starting point).
- Instructions for any log-based alerting that greps for
  `"inbound tcp connection"` / `"inbound vsock connection"` — these
  strings no longer appear.
- **Metrics default changed.** Call out explicitly that hosts which
  previously relied on the implicit `:9090` default now need an
  explicit `metrics.bind: 0.0.0.0:9090` in their config (or an
  explicit `-metrics-addr :9090` flag). Without either, the endpoint
  is silently disabled — monitoring will stop receiving data.
  Recommend a pre-upgrade check: `grep -q 'metrics:' vsockd.yaml ||
  echo "add metrics.bind before upgrading"`.
- **New metrics.vsock_port field** for enclave-side deployments;
  include a one-paragraph recipe.

**External verification** (cannot be automated in this repo):

- Run one release candidate on a real Nitro Enclaves setup to confirm
  that host-side `tcp_to_vsock` and `vsock_to_tcp` still reach the
  enclave. The loopback test backend in this repo does not exercise
  real vsock sockets.
- Test the new "vsockd inside the enclave" use case: build the binary
  for the enclave image, run it with a `tcp_to_vsock` / `vsock_to_tcp`
  config pointing at the parent host's CID (3), and confirm an
  otherwise vsock-unaware app in the enclave can reach / be reached
  from the host through it.
- Test enclave-side metrics over vsock end-to-end: run vsockd inside
  the enclave with `metrics.vsock_port: 9090`, scrape from the parent
  host via `socat VSOCK-CONNECT:<enclave-cid>:9090 -` (or equivalent
  prometheus configuration with a vsock service discovery shim), and
  confirm the usual counters show up.
