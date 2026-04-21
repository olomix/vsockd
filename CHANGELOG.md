# Changelog

All notable changes to this project are documented in this file. The format
is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Hard breaking change: TCP passthrough listeners moved out of
`inbound` / `outbound` into their own top-level sections, renamed from
the host-role vocabulary to what they actually do. The `metrics` section
is enclave-friendly: no implicit `:9090` default and a new vsock bind.
There is no YAML back-compat shim, no metric alias, and no implicit
metrics listener — old configs fail loudly at startup.

### Added

- Top-level `tcp_to_vsock` YAML section — listens on a TCP port and
  forwards raw bytes to a fixed vsock endpoint. Works identically on
  the host and inside the enclave.
- Top-level `vsock_to_tcp` YAML section — listens on a vsock port and
  forwards raw bytes to a fixed TCP upstream. Works identically on the
  host and inside the enclave.
- `metrics.vsock_port` — expose `/metrics` over vsock
  (`VMADDR_CID_ANY`) so an enclave-side vsockd can be scraped from the
  parent host. Mutually exclusive with `metrics.bind`.

### Changed

- Prometheus metrics for TCP passthrough renamed in lockstep with the
  new section names:

  | Old | New |
  |---|---|
  | `tcp_inbound_connections_total` | `tcp_to_vsock_connections_total` |
  | `tcp_inbound_bytes_total` | `tcp_to_vsock_bytes_total` |
  | `tcp_inbound_errors_total` | `tcp_to_vsock_errors_total` |
  | `tcp_outbound_connections_total` | `vsock_to_tcp_connections_total` |
  | `tcp_outbound_bytes_total` | `vsock_to_tcp_bytes_total` |
  | `tcp_outbound_errors_total` | `vsock_to_tcp_errors_total` |

- Debug / warn log messages on the passthrough path renamed:
  `"inbound tcp connection"` → `"tcp_to_vsock connection opened"`,
  `"tcp connection closed"` → `"tcp_to_vsock connection closed"`,
  `"inbound vsock connection"` → `"vsock_to_tcp connection opened"`,
  `"vsock connection closed"` → `"vsock_to_tcp connection closed"`.
  Structured attributes (`cid`, `port`, `listen_port`, `remote`,
  `listen`, `total_bytes`) are unchanged.
- Metrics endpoint is **disabled by default**. The `-metrics-addr` flag
  default dropped from `":9090"` to `""`. The endpoint starts only when
  at least one of `-metrics-addr`, `metrics.bind`, or
  `metrics.vsock_port` is set, in that precedence order. Unset =
  silently disabled; startup log reads `metrics=disabled`.
- The startup metrics log key changed from `metrics_addr` to `metrics`,
  with a richer value: `"disabled"`, `"tcp <addr>"`, or
  `"vsock port=<n>"`.
- Project description and README updated to drop host-only framing —
  vsockd is now a general-purpose vsock↔TCP bridge whose HTTP-aware
  features (`http-host` / `tls-sni` routing, forward proxy) remain
  host-role-specific.

### Removed

- `mode: tcp` under `inbound` and `outbound`. Old configs fail at parse
  time with a strict-YAML "unknown field" error pointing at
  `target_cid` / `target_port` (inbound) or `mode` / `upstream`
  (outbound).
- Implicit `:9090` metrics default. Hosts that previously relied on the
  flag default must now opt in explicitly — otherwise the endpoint is
  silently off and monitoring will stop receiving data.

### Migration

Old YAML:

```yaml
inbound:
  - bind: 0.0.0.0
    port: 5432
    mode: tcp
    target_cid: 16
    target_port: 5432

outbound:
  - port: 9000
    mode: tcp
    upstream: 10.0.0.5:5432
```

New YAML:

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

To keep metrics behavior equivalent to the old implicit default, add:

```yaml
metrics:
  bind: 0.0.0.0:9090
```

Or, for an enclave-side vsockd you want the parent host to scrape:

```yaml
metrics:
  vsock_port: 9090
```

Grafana dashboards and Prometheus recording / alerting rules that
reference the renamed metrics can be migrated with:

```sh
sed -i 's/tcp_inbound_/tcp_to_vsock_/g; s/tcp_outbound_/vsock_to_tcp_/g' <files>
```

Log-based alerting that greps for `"inbound tcp connection"`,
`"inbound vsock connection"`, `"tcp connection closed"`, or
`"vsock connection closed"` no longer matches — update patterns to the
new strings listed in the Changed section above.

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

[Unreleased]: https://github.com/olomix/vsockd/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/olomix/vsockd/releases/tag/v0.1.0
