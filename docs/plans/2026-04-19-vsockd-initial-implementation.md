# vsockd — initial implementation

## Overview

Build `vsockd`, a single static Go binary that runs on the parent EC2 host of
AWS Nitro Enclaves and bridges network traffic in both directions over vsock:

- **Inbound**: terminate TCP on the host (e.g. 80/443), route each connection
  to the correct enclave's vsock endpoint based on the HTTP `Host` header
  (plain HTTP) or TLS SNI (HTTPS, transparent passthrough — TLS is not
  terminated; the enclave owns the cert).
- **Outbound**: accept vsock connections from enclaves, speak the standard
  HTTP/HTTPS forward-proxy protocol (`CONNECT` for HTTPS, absolute-URI
  `GET`/`POST` for HTTP), and enforce a per-(port, CID) egress allowlist
  before dialing the real destination.

Driven by a single declarative YAML config, reloadable on `SIGHUP` without
dropping existing connections, with structured logging (`log/slog`) and
Prometheus metrics. Multiple enclaves may share the same inbound host port
under distinct hostnames.

Module path: `github.com/olomix/vsockd`. Binary: `vsockd`.

## Context (from discovery)

- Project directory is a greenfield Go project — only `idea.md` exists.
- Spec pins: Go 1.26+, `github.com/mdlayher/vsock`, `log/slog`,
  Prometheus `/metrics`, single static binary, minimal dependencies.
- Planned package layout (from idea.md): `config`, `inbound`, `outbound`,
  `vsockconn`, `allowlist`, `metrics`, plus `cmd/vsockd/main.go`.
- Deliverables include example YAML config, README (host install +
  enclave-side `socat` recipe), Dockerfile.
- No existing code to refactor or be backwards-compatible with.

### Design decisions taken during planning

- **Outbound listener model**: multi-port config where each port declares the
  set of CIDs allowed to connect to it and each CID carries its own egress
  allowlist. Supports both "single shared port" and "port-per-CID" shapes
  under one schema. Enforcement at accept time checks the peer CID against
  the port's CID list; mismatch → close.
- **CID uniqueness**: same CID may NOT appear under more than one outbound
  port. Reject at config-load time (ambiguous allowlist otherwise).
- **Peer CID is trusted identity**: set by the Nitro hypervisor, enclaves
  cannot spoof. No additional auth beyond CID (explicit v1 scope).
- **No TLS termination on host**: passthrough only for HTTPS inbound; the
  daemon only sniffs SNI and forwards raw bytes.
- **Testing approach**: regular (implementation first, tests in the same
  task). Unit tests required for every task (see Development Approach).

## Development Approach

- **Testing approach**: Regular (code first, then tests) — but every task
  still ends with test coverage before moving on.
- Complete each task fully before moving to the next.
- Make small, focused changes.
- **CRITICAL: every task MUST include new/updated tests** for code changes
  in that task:
  - tests are not optional — they are a required part of the checklist
  - unit tests for new functions/methods
  - unit tests for modified functions/methods
  - cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** — no exceptions.
- **CRITICAL: update this plan file when scope changes during implementation.**
- Run tests after each change.
- Maintain backward compatibility (N/A for v1 greenfield, but keep exported
  APIs stable once declared).

## Testing Strategy

- **Unit tests**: required for every task.
- **Integration smoke tests**: exercise the full inbound and outbound paths
  using a loopback-TCP stand-in for vsock where CI lacks vsock support.
  Idea.md explicitly calls this out.
- No UI; no e2e framework needed.
- Target 80%+ coverage on `config`, `allowlist`, and the parsers (SNI,
  HTTP Host). Wiring code (listeners, main) covered via integration tests.
- Run `go test ./...` and `go vet ./...` after each task.

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document issues/blockers with ⚠️ prefix.
- Update plan if implementation deviates from original scope.
- Keep plan in sync with actual work done.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, unit tests, integration
  tests, example config, Dockerfile, README — all automatable in this repo.
- **Post-Completion** (no checkboxes): anything requiring real Nitro
  hardware, manual load/security testing, or deployment wiring.

## Implementation Steps

### Task 1: Project skeleton and tooling

- [x] `go mod init github.com/olomix/vsockd` with Go 1.26 directive
- [x] create directories: `cmd/vsockd/`, `internal/config/`,
      `internal/allowlist/`, `internal/inbound/`, `internal/outbound/`,
      `internal/vsockconn/`, `internal/metrics/`, `examples/`
- [x] add `cmd/vsockd/main.go` stub that parses `-config` flag, prints
      version, exits 0 — just enough to compile
- [x] add `Makefile` with `build`, `test`, `vet`, `lint`, `docker` targets
- [x] add `.gitignore` (Go defaults + `/vsockd` binary + `/dist/`)
- [x] add `examples/vsockd.yaml` placeholder (filled in Task 2)
- [x] write a smoke test in `cmd/vsockd/main_test.go` that runs the binary
      with `-help` and asserts exit code 0
- [x] run `go build ./...` and `go test ./...` — both must pass

### Task 2: Config package

- [x] define types in `internal/config/config.go`:
  - `Config` (top-level)
  - `InboundListener` { Bind, Port, Mode (`http-host`|`tls-sni`), Routes }
  - `Route` { Hostname, CID, VsockPort }
  - `OutboundListener` { Port, CIDs []OutboundCID }
  - `OutboundCID` { CID, AllowedHosts []string }
- [x] implement `Load(path string) (*Config, error)` using
      `gopkg.in/yaml.v3` with `KnownFields(true)` (strict)
- [x] implement `Validate()`:
  - inbound mode must be one of the two known values
  - every inbound route hostname non-empty, CID ≥ 3, vsock port in range
  - **same CID must not appear under more than one outbound port** (reject)
  - allowlist patterns must be well-formed `host:port` or `*`
  - reject duplicate hostnames within a single inbound listener
- [x] write `examples/vsockd.yaml` covering: two inbound listeners (HTTP +
      TLS), multi-CID outbound port, per-CID allowlist, wildcard entry
- [x] write tests: table-driven load+validate cases (valid config,
      duplicate CID across ports, duplicate hostname, unknown mode,
      malformed allowlist, unknown YAML field)
- [x] run `go test ./internal/config/...` — must pass before next task

### Task 3: Allowlist matcher

- [x] in `internal/allowlist/allowlist.go` define `Matcher` built from
      a list of patterns; supports exact `host:port`, suffix wildcard
      `*.example.com:443`, and universal `*`
- [x] implement `New(patterns []string) (*Matcher, error)` — parse once,
      validate patterns
- [x] implement `Allow(host string, port int) bool`
- [x] write tests: exact match, suffix match (including nested subdomains),
      port mismatch, host casing normalisation, `*` wildcard, rejection
      path, malformed pattern
- [x] run `go test ./internal/allowlist/...` — must pass before next task

### Task 4: Metrics package

- [x] in `internal/metrics/metrics.go` define a struct holding the
      Prometheus collectors:
  - `inbound_connections_total{route}` counter
  - `inbound_bytes_total{route,direction}` counter
  - `inbound_errors_total{route,kind}` counter
  - `outbound_connections_total{cid,result}` counter (result: allowed,
    denied, error)
  - `outbound_bytes_total{cid,direction}` counter
  - `config_reloads_total{result}` counter
- [x] provide `New() *Metrics` and an `http.Handler` (`promhttp.HandlerFor`
      against an isolated registry — no global state)
- [x] expose a `ServeMetrics(addr string)` helper that wires the handler
      onto `/metrics`
- [x] write tests: registering and scraping the handler returns expected
      metric names; label cardinality stays bounded
- [x] run `go test ./internal/metrics/...` — must pass before next task

### Task 5: TLS SNI parser

- [x] in `internal/inbound/sni.go` implement
      `SniffSNI(r io.Reader) (host string, buffered []byte, err error)`:
  - read the TLS record header and ClientHello
  - extract `server_name` from the extensions
  - return the full bytes consumed in `buffered` so the caller can replay
    them to the upstream vsock connection (transparent passthrough)
  - bounded read (reject records larger than a sane max, e.g. 16 KiB)
- [x] write tests using hand-crafted / recorded ClientHello bytes:
  - valid SNI extracted (multiple hostnames)
  - ClientHello with no SNI → error
  - malformed record → error
  - oversized record → error
- [x] run `go test ./internal/inbound/...` — must pass before next task

### Task 6: HTTP Host header sniffer

- [x] in `internal/inbound/httphost.go` implement
      `SniffHost(r io.Reader) (host string, buffered []byte, err error)`:
  - read request line + headers into a buffer (up to max, e.g. 8 KiB)
  - parse `Host:` header; strip port if present
  - return buffered bytes so the caller can replay them
  - return error on malformed request or missing Host
- [x] write tests: normal request, header case variations, explicit port in
      Host, oversized headers, missing Host, malformed request line
- [x] run `go test ./internal/inbound/...` — must pass before next task

### Task 7: vsockconn abstraction

- [x] in `internal/vsockconn/vsockconn.go` define:
  - `Dialer` interface { `Dial(cid, port uint32) (net.Conn, error)` }
  - `Listener` interface — wraps `net.Listener` but `Accept()` also
    returns the peer CID
- [x] real implementation `NewVsockDialer()` / `ListenVsock(port uint32)`
      using `github.com/mdlayher/vsock`
- [x] loopback-TCP fallback for CI/dev: `NewLoopbackDialer(registry)` and
      `ListenLoopback(registry, cid, port)` — a small in-process registry
      maps `(cid, port)` to real `127.0.0.1:<ephemeral>` listeners, and the
      fallback Accept returns the fake peer CID
- [x] pick the backend via build tag or config flag
      (`VSOCKD_BACKEND=loopback` env var used only by tests)
- [x] write tests for the loopback backend: dial/accept roundtrip, peer
      CID reported correctly, unknown CID → ECONNREFUSED-like error
- [x] run `go test ./internal/vsockconn/...` — must pass before next task

### Task 8: Inbound listener

- [x] in `internal/inbound/server.go` implement `Server` with
      `Start(ctx)` / `Shutdown(ctx)`:
  - listens on configured `bind:port` per inbound listener
  - for each accepted conn: sniff host (HTTP or TLS per mode), look up
    route, dial the mapped vsock `(cid, port)` via the `Dialer`
  - write the buffered sniff bytes to the upstream, then bidirectional
    copy (two goroutines, both halves closed on either side's EOF/error)
  - record metrics (connections, bytes, errors, denied-route)
  - graceful shutdown: stop accepting, wait for active copies up to a
    configurable grace period
- [x] write integration tests using the loopback vsock backend:
  - plain HTTP: request with matching Host routes to the right fake
    enclave; non-matching Host is closed with a log/metric
  - TLS: record a tiny fake ClientHello; matching SNI routed, non-matching
    closed
  - upstream failure propagates to client
- [x] run `go test ./internal/inbound/...` — must pass before next task

### Task 9: Outbound listener

- [x] in `internal/outbound/server.go` implement `Server` with
      `Start(ctx)` / `Shutdown(ctx)`:
  - opens one vsock listener per configured outbound port
  - on Accept, take peer CID; if not listed for this port → log, increment
    `outbound_connections_total{result="denied"}`, close
  - read one HTTP request:
    - `CONNECT host:port HTTP/1.1` → check allowlist, on allow dial TCP,
      write `200 Connection Established`, then bidirectional copy; on
      deny write a clean `403 Forbidden` and close
    - absolute-URI `GET`/`POST http://host/...` → check allowlist, dial,
      rewrite request line to origin form, stream request through, stream
      response back; deny → 403
  - sensible timeouts on header read, upstream dial
  - metrics on every decision and byte counts
- [x] write integration tests using loopback vsock:
  - allowed CONNECT to a local echo TCP server passes bytes both ways
  - denied CONNECT (allowlist miss) → 403, no upstream dial
  - allowed absolute-URI GET → response flows back
  - wrong peer CID for the port → connection closed
  - malformed request line → 400 then close
- [x] run `go test ./internal/outbound/...` — must pass before next task

### Task 10: Main wiring, signals, reload, graceful shutdown

- [x] wire `cmd/vsockd/main.go`:
  - flags: `-config`, `-metrics-addr` (default `:9090`)
  - load config, construct `Metrics`, build inbound + outbound servers,
    start all with a shared `context.Context`
- [x] `SIGHUP` handler:
  - reload config from disk, validate
  - diff: close listeners that disappeared, start listeners that appeared,
    update per-route/CID tables under a mutex
  - **existing connections continue to use the rules they started with**
    (simplest correct behaviour; document it)
  - increment `config_reloads_total{result="success"|"failure"}`
- [x] `SIGTERM`/`SIGINT` handler: stop all listeners, cancel context,
      wait for active connections up to `shutdown-grace` (default 30s),
      then force-close remaining
- [x] structured logging everywhere via `slog`; JSON handler by default,
      text handler when stderr is a TTY (use `-log-format` flag)
- [x] write tests:
  - start binary in-process with a minimal config, hit `/metrics`, assert
    200 and expected metric names present
  - SIGHUP reload: change config on disk, send SIGHUP, verify new listener
    appears and a removed one is gone; existing conn survives
  - SIGTERM: verify graceful shutdown within grace window
- [x] run `go test ./...` — must pass before next task

### Task 11: End-to-end integration smoke test

- [ ] in `test/e2e/` (or `internal/integration/`) write a test that uses
      the loopback-vsock backend to run a full scenario:
  - spin up fake "enclave" that listens on loopback vsock + speaks HTTP
  - start `vsockd` in-process with a generated config
  - inbound path: open TCP to the host listener, send HTTP with matching
    Host → fake enclave receives request, responds, client gets response
  - outbound path: fake enclave dials loopback vsock, issues CONNECT to an
    allowed host → succeeds; issues CONNECT to a disallowed host → 403
  - SIGHUP reload adds a new route → new inbound TCP dial works
- [ ] run `go test ./test/e2e/...` — must pass before next task

### Task 12: Dockerfile and example config

- [ ] multi-stage `Dockerfile`:
  - builder stage: `golang:1.26-alpine`, `CGO_ENABLED=0`,
    `go build -trimpath -ldflags "-s -w"`
  - runtime stage: `gcr.io/distroless/static-debian12:nonroot`
  - ENTRYPOINT `["/vsockd", "-config", "/etc/vsockd/vsockd.yaml"]`
- [ ] finalise `examples/vsockd.yaml` with realistic annotated sample
- [ ] add `docker` target to Makefile that builds the image
- [ ] (no new tests — build verification only)
- [ ] run `docker build .` — must succeed before next task

### Task 13: README

- [ ] `README.md` with:
  - what it is / why / architecture diagram (ASCII ok)
  - build + install on host (systemd unit snippet)
  - config reference (link to example)
  - enclave-side `socat` recipe for HTTP_PROXY/HTTPS_PROXY over vsock
  - metrics reference
  - operational notes: SIGHUP, graceful shutdown, log format
  - limitations / out-of-scope for v1

### Task 14: Verify acceptance criteria

- [ ] every responsibility from `idea.md` §Responsibilities is implemented
- [ ] every non-functional requirement from `idea.md` is satisfied
- [ ] `go vet ./...` clean
- [ ] `go test ./...` green (race detector: `go test -race ./...`)
- [ ] coverage ≥ 80% on `config`, `allowlist`, `inbound` parsers
- [ ] `staticcheck ./...` clean (or the equivalent linter agreed on in
      Task 1's Makefile)
- [ ] `idea.md` §"Out of scope for v1" items confirmed absent (no TLS
      termination code path, no rate limiter, no auth beyond CID)

### Task 15: Documentation polish

- [ ] cross-check README against final config shape (names may have drifted)
- [ ] add CHANGELOG.md with a v0.1.0 entry

## Technical Details

### Config shape (YAML)

```yaml
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
        allowed_hosts:
          - "api.stripe.com:443"
          - "*.s3.amazonaws.com:443"
      - cid: 20
        allowed_hosts:
          - "*.internal.example.com:443"
  - port: 8082
    cids:
      - cid: 42
        allowed_hosts: ["*"]   # unrestricted

metrics:
  bind: 0.0.0.0:9090

shutdown_grace: 30s
log_format: json
```

### Processing flow — inbound

1. TCP accept on `(bind, port)`.
2. Read just enough bytes to extract hostname (Host header or SNI),
   keeping them in a buffer.
3. Look up `hostname → (cid, vsock_port)`; miss → close.
4. Dial vsock `(cid, vsock_port)`.
5. Write buffered sniff bytes upstream, then `io.Copy` both ways.

### Processing flow — outbound

1. vsock accept on configured port; record peer CID.
2. If peer CID not in this port's CID set → deny, close.
3. Read one HTTP request (CONNECT or absolute-URI).
4. Extract destination `host:port`.
5. Allowlist check against the CID's patterns.
6. Allow → dial TCP, return `200 Connection Established` (CONNECT) or
   rewrite+forward (absolute-URI), then stream.
7. Deny → `403 Forbidden`, close.

### Reload semantics

- Config validated in full before any listener is touched.
- Listeners whose `(bind, port)` disappeared: close their accept loop;
  in-flight connections continue until they end naturally.
- Listeners that appeared: start fresh accept loop.
- Listeners unchanged: swap their route/CID tables atomically under a
  mutex; new connections see the new rules, active ones keep theirs.

## Post-Completion

*Items requiring manual intervention or external systems — informational only.*

**Manual verification on real Nitro hardware:**

- Deploy to an EC2 Nitro-capable instance with at least one enclave built
  from a simple test image; verify end-to-end inbound and outbound paths
  against the production vsock backend (not loopback).
- Measure throughput and latency overhead vs direct TCP under a small load
  (e.g. `hey` or `wrk`) to confirm the proxy is not a bottleneck.
- Confirm `/metrics` scraping works from the host's Prometheus agent.

**Security review:**

- Threat-model the peer-CID trust assumption with the security team.
- Confirm no plaintext bytes from TLS passthrough are logged.
- Review allowlist parser for pathological patterns (ReDoS-adjacent if we
  ever move to regex).

**Deployment wiring (not in this repo):**

- systemd unit with `Restart=always`, `LimitNOFILE`, non-root user.
- Prometheus scrape config + dashboard + alerts on `*_errors_total` and
  `config_reloads_total{result="failure"}`.
- Log shipping (journald → central collector).
- Enclave-side `socat` (documented in README) wired via the enclave's
  init script so `HTTP_PROXY`/`HTTPS_PROXY` work transparently.
