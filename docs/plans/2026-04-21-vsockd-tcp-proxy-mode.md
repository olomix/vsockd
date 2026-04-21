# vsockd TCP Passthrough Proxy Mode

## Overview

Add a raw-TCP proxy mode to vsockd that forwards bytes in both directions
without any application-layer parsing (no HTTP Host, no TLS SNI, no
allowlist).

Two directions, one config surface:

- **Outbound TCP (vsock → TCP)**: accept vsock connections from enclaves
  on a configured vsock port, dial a fixed upstream `ip:port`, shuttle
  bytes both ways.
- **Inbound TCP (TCP → vsock)**: accept TCP connections on a configured
  `bind:port`, dial a fixed enclave vsock endpoint `cid:port`, shuttle
  bytes both ways.

Also add a debug log level that emits per-connection open/close events
for both directions. Messages on the vsock-facing side must include the
peer CID and vsock port; messages on the TCP-facing side include the
peer IP and TCP port. Close messages include total bytes transferred.

**Why**: Callers need to expose non-HTTP workloads (databases, gRPC,
arbitrary protocols) through the enclave boundary. The current HTTP
inbound and HTTP-forward-proxy outbound cannot carry these — a pure
passthrough is the missing primitive.

**How it integrates**: The existing `inbound:` and `outbound:` YAML
sections already carry a `mode:` discriminator for inbound. TCP proxy
extends both by adding `mode: tcp` as a new variant. Existing
`http-host` / `tls-sni` inbound listeners and the legacy
(no-mode) HTTP-forward-proxy outbound listeners are untouched and
backward-compatible.

## Context (from discovery)

**Files/components involved**:

- `internal/config/config.go` — add `mode: tcp` variant and new fields
  (inbound `target_cid`/`target_port`, outbound `mode`/`upstream`)
- `internal/config/config_test.go` — add validation cases
- `internal/inbound/server.go` — dispatch listener to new TCP handler
  when `mode: tcp`
- `internal/inbound/tcp.go` (new) — TCP-to-vsock handler
- `internal/inbound/tcp_test.go` (new)
- `internal/outbound/server.go` — dispatch listener to new TCP handler
  when `mode: tcp`
- `internal/outbound/tcp.go` (new) — vsock-to-TCP handler
- `internal/outbound/tcp_test.go` (new)
- `cmd/vsockd/main.go` — add `-debug` flag, read `VSOCKD_LOG_LEVEL` env
  var, parse `log_level` YAML, build slog handler with dynamic level
- `internal/metrics/metrics.go` — add TCP mode counters (follow existing
  fixed-cardinality labelling)
- `test/e2e/e2e_test.go` — extend with a TCP passthrough scenario
- `examples/vsockd.yaml` — show TCP mode config for both directions
- `README.md` — document TCP mode and debug logging

**Related patterns found**:

- Bidirectional copy: `internal/inbound/server.go` already uses two
  `io.Copy` goroutines after sniff-and-replay. Same pattern applies,
  minus the sniff step.
- Listener lifecycle: `app.Start` / `app.Reload` / `app.Shutdown` plus
  the inbound/outbound servers' `PrepareApply`/`CommitApply` diff-based
  listener update. TCP listeners slot into the same pipeline.
- Tests use loopback vsock (`VSOCKD_BACKEND=loopback`) and the in-package
  `vsockconn` registry. TCP handler tests can use the same setup.
- Logging is `log/slog` via a handler configured once in `main.go` with
  `LevelInfo`. Switching to a `LevelVar` lets `-debug` flip the level at
  startup without rewriting sites.

**Dependencies identified**:

- No new third-party libraries required (`mdlayher/vsock` already
  present; TCP uses stdlib `net`).

## Development Approach

- **Testing approach**: Regular (code first, then tests within the same
  task).
- Complete each task fully before moving to the next.
- Make small, focused changes; keep existing HTTP inbound and HTTP
  forward-proxy outbound behaviour untouched.
- **CRITICAL: every task MUST include new/updated tests** for code
  changes in that task.
  - write unit tests for new functions/methods
  - write unit tests for modified functions/methods
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** — no
  exceptions. Run `go test ./...` with the race detector enabled where
  feasible.
- **CRITICAL: update this plan file when scope changes during
  implementation**.
- Run `go vet ./...` and `staticcheck ./...` at the end of each task
  that touches Go code.
- Maintain strict YAML backward-compatibility: existing config files
  must continue to load and run unchanged.

## Testing Strategy

- **Unit tests**: required for every task (see Development Approach).
- **Integration tests**: TCP inbound and outbound handlers each get a
  test that uses the loopback vsock backend plus an in-process TCP
  echo server to exercise the full round-trip.
- **E2E test**: one scenario in `test/e2e/e2e_test.go` exercises both
  directions end-to-end through `app.New` / `app.Start`.
- No UI / browser e2e applicable.

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document issues/blockers with ⚠️ prefix.
- Update plan if implementation deviates from original scope.
- Keep plan in sync with actual work done.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code changes, tests, and
  documentation updates achievable in this repo.
- **Post-Completion** (no checkboxes): anything requiring external
  action (running the binary inside a real Nitro Enclave, smoke tests
  against production upstreams, etc.).

## Implementation Steps

### Task 1: Extend config schema with `mode: tcp` for inbound and outbound

- [x] in `internal/config/config.go`: add constant `ModeTCP = "tcp"`
- [x] extend `InboundListener` with `TargetCID uint32` (yaml
  `target_cid`) and `TargetPort uint32` (yaml `target_port`) fields
- [x] extend `OutboundListener` with `Mode string` (yaml `mode`, empty
  means legacy HTTP forward-proxy) and `Upstream string` (yaml
  `upstream`) fields
- [x] extend `Config` with `LogLevel string` (yaml `log_level`; empty,
  `debug`, or `info`); add `LogLevelDebug` / `LogLevelInfo` constants
- [x] update `InboundListener.validate`: when `Mode == ModeTCP`, require
  `TargetCID >= minCID`, require valid `TargetPort`, forbid `Routes`;
  when `Mode` is `http-host`/`tls-sni`, forbid `TargetCID`/`TargetPort`
- [x] update outbound validation: when `Mode == ModeTCP`, require
  non-empty `Upstream` that parses as `host:port` with a 1..65535 port,
  forbid `CIDs`; when `Mode` is empty (legacy HTTP), forbid `Upstream`
  and keep existing per-CID allowlist checks
- [x] validate `LogLevel` is one of "", "debug", "info"
- [x] add table-driven `config_test.go` cases: valid inbound
  `mode: tcp`, valid outbound `mode: tcp`, missing `target_cid`,
  missing `upstream`, invalid `upstream` host:port, mixing `routes:`
  with `mode: tcp`, mixing `cids:` with `mode: tcp`, invalid
  `log_level`
- [x] run `go test ./internal/config/...` — must pass before next task

### Task 2: Wire dynamic log level (`-debug` flag, `VSOCKD_LOG_LEVEL`, yaml `log_level`)

- [x] in `cmd/vsockd/main.go`: add `-debug` bool flag (default false)
- [x] resolve effective log level with precedence: `-debug` flag →
  `VSOCKD_LOG_LEVEL` env var → YAML `log_level` → default `info`
- [x] build handler using a `slog.LevelVar` so the level is fixed at
  start (no runtime flip needed, but the var pattern keeps all
  handlers consistent)
- [x] log the resolved log level at startup at Info so it is visible in
  production
- [x] add `main_test.go` (or a helper-level test in a new
  `internal/logging` package if extraction is clean) covering the
  precedence chain — pure function, no network
- [x] run `go test ./...` — must pass before next task

### Task 3: Implement TCP outbound handler (vsock → TCP upstream)

- [x] create `internal/outbound/tcp.go` exposing
  `handleTCP(ctx, listener config, conn vsockconn.Conn)` or equivalent
  wired from `server.go` when `Mode == config.ModeTCP`
- [x] dial `cfg.Upstream` using `net.Dialer` with a sane timeout
  (reuse any existing dial-timeout constant; otherwise 10s)
- [x] on accept, if debug enabled, emit
  `slog.Debug("inbound vsock connection", "cid", peerCID,
  "port", peerPort, "listen_port", listenerPort)`
- [x] run bidirectional `io.Copy` in two goroutines; capture the
  byte counts from both directions; wait for both to finish
- [x] on close, if debug enabled, emit
  `slog.Debug("vsock connection closed", "cid", peerCID,
  "port", peerPort, "listen_port", listenerPort,
  "total_bytes", upBytes+downBytes)`
- [x] half-close gracefully: when one side reaches EOF, `CloseWrite`
  the other side so the peer sees EOF (use `net.TCPConn.CloseWrite` and
  the vsock equivalent if available; otherwise fall through to `Close`)
- [x] update `internal/outbound/server.go` so TCP-mode listeners go
  through this handler; HTTP-mode listeners keep their existing path
- [x] make sure TCP listeners register with the same active-conn map
  used for graceful shutdown so `shutdown_grace` applies uniformly
- [x] in `tcp_test.go`: start a loopback vsock listener, start a TCP
  echo server on `127.0.0.1:<ephemeral>`, run the handler, dial in
  from a loopback vsock client, verify bytes echo both ways
- [x] in `tcp_test.go`: verify byte count is captured accurately
  (transfer N bytes each way, check debug log output via a
  `slog.Handler` captured in the test)
- [x] in `tcp_test.go`: failure-mode tests — upstream dial fails
  (connection refused), client disconnects mid-stream, context cancel
- [x] run `go test -race ./internal/outbound/...` — must pass before
  next task

### Task 4: Implement TCP inbound handler (TCP → vsock target)

- [x] create `internal/inbound/tcp.go` analogous to the outbound TCP
  handler but in the reverse direction
- [x] dial the vsock target with the project's `vsockconn.Dialer` so
  tests can use loopback
- [x] on accept of a TCP peer, if debug enabled, emit
  `slog.Debug("inbound tcp connection", "remote", remoteAddr,
  "listen", listenAddr)`
- [x] run bidirectional `io.Copy` with captured byte counts, same
  half-close handling as Task 3
- [x] on close, if debug enabled, emit
  `slog.Debug("tcp connection closed", "remote", remoteAddr,
  "listen", listenAddr, "total_bytes", upBytes+downBytes)`
- [x] update `internal/inbound/server.go` so `mode: tcp` listeners
  bypass sniff/route and dispatch to this handler
- [x] register with the active-conn map for graceful shutdown
- [x] in `tcp_test.go`: start a loopback vsock target (echo), start the
  TCP inbound handler on `127.0.0.1:<ephemeral>`, connect a TCP client,
  verify bytes round-trip both ways
- [x] verify byte counting and debug log emission via a captured
  `slog.Handler`
- [x] failure-mode tests: vsock dial fails, client disconnects
  mid-stream, context cancel
- [x] run `go test -race ./internal/inbound/...` — must pass before
  next task

### Task 5: Add metrics for TCP-mode listeners

- [x] in `internal/metrics/metrics.go`: add counters following the
  existing fixed-cardinality pattern — `tcp_inbound_connections_total`,
  `tcp_inbound_bytes_total{direction=up|down}`,
  `tcp_outbound_connections_total`,
  `tcp_outbound_bytes_total{direction=up|down}`, and
  `tcp_*_errors_total{reason=dial_fail|copy_error}`
- [x] call the new counters from the TCP handlers in Tasks 3 and 4
- [x] add tests in `metrics_test.go` verifying the counters exist with
  the expected label sets (no cardinality explosion) and increment
  correctly when the handlers are exercised from the TCP test helpers
- [x] run `go test ./internal/metrics/...` and
  `go test -race ./internal/{inbound,outbound}/...` — must pass before
  next task

### Task 6: E2E coverage for TCP passthrough in both directions

- [x] extend `test/e2e/e2e_test.go` with a scenario:
  - spin up a fake enclave TCP-echo server on loopback vsock
  - spin up a local TCP echo on `127.0.0.1`
  - write a YAML config with one `inbound mode: tcp` listener and one
    `outbound mode: tcp` listener
  - `app.New` + `app.Start`
  - verify a TCP client can round-trip bytes through the inbound
    listener to the enclave echo
  - verify an enclave vsock client can round-trip bytes through the
    outbound listener to the TCP echo
- [x] enable debug log level in this test and capture the handler
  output; assert one `inbound vsock connection` / `vsock connection
  closed` pair per outbound connection, and one `inbound tcp
  connection` / `tcp connection closed` pair per inbound connection,
  with correct byte totals
- [x] add a SIGHUP reload sub-case: reload config that changes the
  TCP upstream address; verify new connections use the new upstream
  while existing ones drain (required extending outbound `ApplyPlan` to
  also swap the atomic upstream pointer on kept-by-port mode=tcp
  listeners)
- [x] run `go test -race ./test/e2e/...` — must pass before next task

### Task 7: Verify acceptance criteria

- [x] verify all requirements from Overview are implemented: TCP
  passthrough both directions, debug log messages on vsock side
  (CID:port, total bytes), debug log messages on TCP side (IP:port,
  total bytes)
- [x] verify debug toggle works via all three sources (`-debug` flag,
  `VSOCKD_LOG_LEVEL=debug`, yaml `log_level: debug`) with correct
  precedence
- [x] verify existing HTTP inbound and HTTP-forward-proxy outbound
  behaviour is unchanged (regression-test the existing e2e scenarios)
- [x] run `go test -race ./...` — all tests must pass (fixed a race in
  `TestTCP_Passthrough_ContextCancelViaShutdown`: the test now gates on
  upstream Accept so Shutdown only runs after handleTCP is parked in
  shuttleTCP)
- [x] run `go vet ./...` — must be clean
- [x] run `staticcheck ./...` — must be clean
- [x] verify coverage: `go test -cover ./internal/...` on `config`,
  `inbound`, `outbound`, `metrics` — config 95.4% (target 92%+),
  allowlist 97.7% (unchanged), metrics 82.6%, inbound 79.1%
  (up from 76.2% baseline), outbound 69.0% (up from 62.7% baseline)

### Task 8: [Final] Update documentation

- [ ] update `examples/vsockd.yaml` with commented examples for
  `inbound mode: tcp` and `outbound mode: tcp`, and `log_level: debug`
- [ ] update `README.md` with a short "TCP passthrough mode" section:
  config example, the debug log line format, and the three ways to
  toggle debug
- [ ] confirm no inbound/outbound schema notes elsewhere (in
  `docs/` or package doc comments) have gone stale

*Note: ralphex automatically moves completed plans to
`docs/plans/completed/`*

## Technical Details

### YAML schema (new/changed fields only)

```yaml
log_level: debug   # or "info" (default). Overridden by -debug and
                   # VSOCKD_LOG_LEVEL (precedence: flag > env > yaml).

inbound:
  # NEW: TCP passthrough variant
  - bind: 0.0.0.0
    port: 5432
    mode: tcp
    target_cid: 3
    target_port: 8080
  # Existing variants continue to work unchanged:
  - bind: 0.0.0.0
    port: 443
    mode: tls-sni
    routes:
      - hostname: app.example.com
        cid: 3
        vsock_port: 8443

outbound:
  # NEW: TCP passthrough variant
  - port: 8080
    mode: tcp
    upstream: 10.0.0.5:5432
  # Existing HTTP forward-proxy variant (mode omitted) continues to
  # work unchanged:
  - port: 9443
    cids:
      - cid: 3
        allowed_hosts:
          - api.example.com:443
```

### Debug log line shapes

All fields are emitted as slog key/value attrs so JSON and text
handlers both stay structured.

**Vsock-side (outbound mode: tcp)**:

```
DEBUG inbound vsock connection cid=3 port=1234 listen_port=8080
DEBUG vsock connection closed cid=3 port=1234 listen_port=8080 total_bytes=12345
```

**TCP-side (inbound mode: tcp)**:

```
DEBUG inbound tcp connection remote=192.168.1.10:54321 listen=0.0.0.0:5432
DEBUG tcp connection closed remote=192.168.1.10:54321 listen=0.0.0.0:5432 total_bytes=12345
```

### Byte counting

Each direction's goroutine captures the `int64` returned by `io.Copy`.
The outer handler waits for both goroutines (via `sync.WaitGroup` or
channel) and sums the two counts for the `total_bytes` log attr. This
matches what the existing inbound HTTP path already does for its own
connection lifetime tracking.

### Log level resolution order

```
1. -debug flag set            → Debug
2. VSOCKD_LOG_LEVEL=debug     → Debug
3. YAML log_level: debug      → Debug
4. otherwise                  → Info
```

Only `debug` and `info` are accepted from the env var and yaml; any
other value is a fatal config error at startup.

### Backward compatibility

- Inbound: existing listeners with `mode: http-host` / `mode: tls-sni`
  continue to validate and run without change.
- Outbound: existing listeners without a `mode` field default to the
  legacy HTTP forward-proxy behaviour with the per-CID allowlist.
- `log_level` is optional; omitting it keeps the current `info`
  default.
- `-debug` flag is new; all other existing flags keep their current
  semantics.

## Post-Completion

*Items requiring manual intervention or external systems — no
checkboxes, informational only.*

**Manual verification**:

- Smoke-test the new binary inside a real EC2 Nitro Enclave with one
  `inbound mode: tcp` and one `outbound mode: tcp` listener against a
  real non-HTTP workload (e.g. Postgres or a gRPC service) to confirm
  the passthrough is transparent end-to-end.
- Confirm on a production-like load that debug logging does not
  dominate throughput; if it does, plan a follow-up to sample or rate
  limit the debug events.

**External system updates**:

- None required for consumers of the binary beyond updating their YAML
  config files to adopt `mode: tcp` where appropriate.
- Deployment charts / systemd units that hardcode CLI flags may want to
  expose `-debug` as a togglable parameter.
