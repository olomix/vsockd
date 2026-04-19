# Changelog

All notable changes to this project are documented in this file. The format
is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] — 2026-04-19

Initial release. A single static Go binary that runs on the parent EC2 host
of AWS Nitro Enclaves and proxies traffic in both directions over vsock.

### Added

- Inbound TCP listeners with per-listener mode:
  - `http-host` — parse the HTTP `Host` header and route to the matching
    enclave vsock endpoint.
  - `tls-sni` — sniff the TLS ClientHello SNI extension and route without
    terminating TLS. The original ClientHello bytes are replayed upstream
    so the enclave owns the certificate.
- Outbound vsock forward proxy speaking the standard HTTP proxy protocol:
  - `CONNECT host:port` for HTTPS tunnels.
  - Absolute-URI `GET`/`POST http://host/...` for plain HTTP.
  - Per-port CID authorization plus per-CID egress allowlist enforced
    before any upstream dial.
- Allowlist matcher supporting exact `host:port`, suffix wildcard
  `*.example.com:443`, any-host wildcard `*:443`, and universal `*`.
- Single YAML configuration with strict parsing (unknown fields rejected)
  and validation: inbound mode whitelist, non-empty hostnames, CID ≥ 3,
  vsock port range, no duplicate hostnames per inbound listener, and no
  duplicate CIDs across outbound ports.
- `SIGHUP` reload that diffs listeners: removed listeners are closed,
  new listeners are started, and unchanged listeners swap their route
  and CID tables atomically. In-flight connections keep the rules they
  started with.
- `SIGTERM`/`SIGINT` graceful shutdown bounded by `shutdown_grace`
  (default 30s); remaining connections are force-closed when the grace
  window expires.
- Prometheus metrics on `/metrics` from an isolated registry:
  `inbound_connections_total`, `inbound_bytes_total`,
  `inbound_errors_total`, `outbound_connections_total`,
  `outbound_bytes_total`, `config_reloads_total`. Label cardinality is
  bounded by the config; no user-supplied strings become label values.
- Structured logging via `log/slog` with `json`, `text`, or `auto`
  output; `auto` picks `text` when stderr is a TTY.
- Loopback-TCP vsock backend for tests and local development, selected
  via the `VSOCKD_BACKEND=loopback` env var.
- Multi-stage `Dockerfile` producing a ~12.5 MB
  `gcr.io/distroless/static-debian12:nonroot` image.
- Example `examples/vsockd.yaml`, README with architecture, install,
  systemd unit, enclave-side `socat` recipe, and metrics reference.
- End-to-end integration tests covering inbound HTTP routing, outbound
  `CONNECT` allow/deny, and `SIGHUP` reload picking up a new route.

### Known limitations

- No TLS termination on the host; passthrough only.
- No authentication beyond the peer CID reported by the Nitro hypervisor.
- No rate limiting or per-destination quotas.
- No special handling for plain-HTTP WebSocket upgrades.
- No regex patterns in the allowlist — only exact, suffix, and universal.

[0.1.0]: https://github.com/olomix/vsockd/releases/tag/v0.1.0
