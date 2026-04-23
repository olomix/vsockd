# vsockd

`vsockd` is a single static Go binary that bridges network traffic between TCP
and `AF_VSOCK`. It runs on the parent EC2 host of AWS Nitro Enclaves or inside
the enclave itself, depending on which features you use: the HTTP-aware
features (SNI/Host fan-out, forward-proxy with per-CID allowlists) are
host-role-specific, while the raw-TCP passthrough features (`tcp_to_vsock` /
`vsock_to_tcp`) work symmetrically on either side. On the host, one daemon
can front multiple enclaves sharing the same inbound ports and enforce a
per-CID outbound egress allowlist.

## What it does

- **Inbound (external → enclave).** Terminates TCP on the host's public
  interfaces (typically 80 and 443) and routes each connection to the right
  enclave's vsock endpoint based on the HTTP `Host` header or the TLS SNI
  extension. TLS is **not** terminated on the host — the enclave owns the
  certificate. vsockd only sniffs SNI and forwards the original ClientHello
  bytes untouched.
- **Outbound (enclave → external).** Accepts vsock connections from enclaves
  and speaks the standard HTTP/HTTPS forward-proxy protocol (`CONNECT` for
  HTTPS, absolute-URI `GET`/`POST` for HTTP), so an app inside the enclave
  can simply set `HTTP_PROXY` and `HTTPS_PROXY`. Each request is checked
  against a per-(port, CID) allowlist before any upstream dial.
- **Raw TCP → vsock (`tcp_to_vsock`).** Listens on a TCP port and pipes
  raw bytes to a fixed vsock endpoint — no HTTP Host, no TLS SNI, no
  allowlist. Works symmetrically: on the host it carries external TCP
  into an enclave; inside the enclave it carries local TCP to the parent
  host's vsock. Use it for databases, gRPC, or any non-HTTP protocol.
- **Raw vsock → TCP (`vsock_to_tcp`).** The mirror image: listens on a
  vsock port and pipes raw bytes to a fixed TCP upstream. On the host
  that means enclave vsock out to an external TCP service; inside the
  enclave it lets an enclave-local vsockd forward to a TCP service on the
  parent host. Also no application-layer parsing.

Multiple enclaves may share the same inbound host port under distinct
hostnames. The same CID may NOT appear under more than one outbound port —
this is enforced at config-load time.

## Architecture

The four listener flavors are grouped by config section. `inbound` and
`outbound` are HTTP-aware and host-only; `tcp_to_vsock` and `vsock_to_tcp`
are raw-byte pipes that work on either side of the vsock boundary.

```
                                            vsockd (host or enclave)
                                    ┌────────────────────────────────┐
                                    │                                │
  external client ── TCP 443 ─────▶ │  inbound (host only)           │
  (TLS ClientHello w/ SNI)          │   └─ sniff SNI                 │
                                    │   └─ lookup hostname → CID     │ ── vsock(CID, port) ──▶ enclave
                                    │   └─ dial vsock                │
                                    │   └─ replay ClientHello + copy │
                                    │                                │
  external service ◀── TCP ──────── │  outbound (host only)          │
  (e.g. api.stripe.com:443)         │   └─ accept vsock, read peer CID
                                    │   └─ CID in port's CID set?    │ ◀── vsock(port) ─────── enclave
                                    │   └─ read HTTP request         │     (CONNECT/GET/POST)
                                    │   └─ allowlist(host:port)?     │
                                    │   └─ dial + stream             │
                                    │                                │
  TCP client ── TCP ──────────────▶ │  tcp_to_vsock (host or enclave)│
  (any protocol)                    │   └─ no sniff, no routing      │ ── vsock(vsock_cid,    ─▶ peer
                                    │   └─ dial fixed vsock target   │      vsock_port)
                                    │   └─ raw byte pipe             │
                                    │                                │
  TCP upstream ◀── TCP ──────────── │  vsock_to_tcp (host or enclave)│
  (any protocol, fixed upstream)    │   └─ no HTTP parse, no allowlist
                                    │   └─ dial fixed upstream       │ ◀── vsock(port) ─────── peer
                                    │   └─ raw byte pipe             │      (raw bytes)
                                    │                                │
                                    │  /metrics (Prometheus, TCP or  │
                                    │             vsock — optional)  │
                                    └────────────────────────────────┘
```

Inside the enclave, only `tcp_to_vsock` and `vsock_to_tcp` apply — the
HTTP-aware `inbound` / `outbound` sections depend on host-role vsock
addressing and CID-based routing that are meaningless enclave-side.
An enclave-local vsockd with just those two sections can replace the
classic `socat` stub entirely (see below).

## Install

vsockd requires Go 1.26+ to build. The runtime binary is fully static (no
libc) and has no external runtime dependencies.

### From source

```sh
git clone https://github.com/olomix/vsockd.git
cd vsockd
make build          # produces ./vsockd
sudo install -m 0755 vsockd /usr/local/bin/vsockd
sudo install -d -m 0755 /etc/vsockd
sudo install -m 0644 examples/vsockd.yaml /etc/vsockd/vsockd.yaml
```

### Docker

```sh
make docker                                  # vsockd:dev
docker run --rm --network host \
  -v /etc/vsockd:/etc/vsockd:ro \
  vsockd:dev
```

The image is built on `gcr.io/distroless/static-debian12:nonroot` and
contains only the vsockd binary. The image `EXPOSE`s port 9090 as the
conventional `/metrics` port; if you opt in to TCP metrics by setting
`metrics.bind: 0.0.0.0:9090` (or pass `-metrics-addr :9090`), add
`-p 9090:9090` when not running with `--network host`. Metrics are
disabled unless you set one of `metrics.bind`, `metrics.vsock_port`, or
the `-metrics-addr` flag.

### systemd unit

A hardened example unit lives at
[`examples/vsockd.service`](examples/vsockd.service). Install it with:

```sh
sudo install -m 0644 examples/vsockd.service /etc/systemd/system/vsockd.service
sudo useradd --system --no-create-home --shell /usr/sbin/nologin vsockd
sudo systemctl daemon-reload
sudo systemctl enable --now vsockd
```

`CAP_NET_BIND_SERVICE` lets the non-root `vsockd` user bind to ports 80
and 443. The unit sets `TimeoutStopSec=35s` so systemd does not SIGKILL
the process inside the default 30 s `shutdown_grace` drain window —
bump both together if you raise `shutdown_grace` in YAML. The kernel's
`AF_VSOCK` support is required on the host (standard on Nitro-capable
instance types).

## Configuration

vsockd is driven by a single YAML file. See
[`examples/vsockd.yaml`](examples/vsockd.yaml) for a fully annotated example;
the schema is implemented and documented in
[`internal/config/config.go`](internal/config/config.go). Unknown YAML fields
are rejected at load time.

Minimal shape:

```yaml
inbound:
  - bind: 0.0.0.0
    port: 443
    mode: tls-sni            # or http-host
    routes:
      - hostname: api.example.com
        cid: 16
        vsock_port: 8443

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

# Raw TCP → vsock: listens on bind:port, forwards bytes to (vsock_cid,
# vsock_port). Works on host or inside the enclave.
tcp_to_vsock:
  - bind: 0.0.0.0
    port: 5432
    vsock_cid: 16
    vsock_port: 5432

# Raw vsock → TCP: listens on vsock port, forwards bytes to upstream.
vsock_to_tcp:
  - port: 9000
    upstream: 10.0.0.5:5432

metrics:
  bind: 0.0.0.0:9090         # TCP form (host side)
  # vsock_port: 9090         # vsock form (enclave side) — mutually exclusive

shutdown_grace: 30s
log_format: json             # json | text | auto
```

Allowlist pattern syntax:

- `host:port` — exact match (case-insensitive host).
- `*.example.com:443` — suffix wildcard. Matches any subdomain of
  `example.com` on port 443. Does not match `example.com` itself.
- `*:443` — any host on port 443.
- `*` — unrestricted (any host, any port).

Command-line flags:

- `-config PATH` — path to the YAML config. Default `/etc/vsockd/vsockd.yaml`.
- `-metrics-addr ADDR` — TCP listen address for the Prometheus `/metrics`
  endpoint. Default is empty (no listener). An explicit non-empty value
  overrides `metrics.bind` / `metrics.vsock_port` from the YAML. Leave
  both the flag and the YAML `metrics` section unset to disable the
  endpoint entirely.
- `-log-format FORMAT` — `json`, `text`, or `auto`. Overrides the YAML.
  `auto` picks `text` when stderr is a TTY and `json` otherwise.
- `-debug` — force the log level to `debug`. Equivalent to setting
  `VSOCKD_LOG_LEVEL=debug` or `log_level: debug` in the YAML, but takes
  precedence over both.
- `-version` — print the version and exit.

## TCP passthrough (`tcp_to_vsock` / `vsock_to_tcp`)

Two top-level config sections forward raw bytes in both directions without
touching the application layer — no HTTP Host header, no TLS SNI, no
allowlist. Use them for non-HTTP protocols (Postgres, MySQL, gRPC, Redis,
SSH, anything) and for running vsockd *inside* the enclave, where the
HTTP-aware `inbound` / `outbound` sections don't apply.

```yaml
# TCP → vsock: accept TCP on bind:port and shuttle bytes to a fixed
# (vsock_cid, vsock_port). Works on the host (external TCP → enclave)
# or inside the enclave (local TCP → parent host vsock).
tcp_to_vsock:
  - bind: 0.0.0.0
    port: 5432
    vsock_cid: 16
    vsock_port: 5432

# vsock → TCP: accept vsock on this port and shuttle bytes to a fixed
# upstream host:port. No per-CID allowlist; any peer CID that connects
# reaches the upstream. Works on the host (enclave vsock → external TCP)
# or inside the enclave (host vsock → local TCP).
vsock_to_tcp:
  - port: 9000
    upstream: 10.0.0.5:5432
```

Both listener types participate in the same SIGHUP reload and the same
`shutdown_grace` drain as the HTTP listeners. `vsock_to_tcp` `upstream`
is swapped atomically (new connections use the new upstream, existing
ones drain on the old one); `tcp_to_vsock` `vsock_cid` / `vsock_port`
cannot change at runtime — a reload that tries to edit them on an
already-bound TCP port is rejected with a "restart required" error, and
the running listener keeps forwarding to the original target.

### Debug logging

Setting the log level to `debug` emits one structured log event per TCP
connection on open and close. Byte totals are reported on close. The
shapes below show the message and the structured attrs; the actual
wire format depends on `-log-format` (JSON by default, text for a TTY).

`vsock_to_tcp` debug logs, attrs: `cid`, `port`, `listen_port`,
`total_bytes` (on close only):

```
level=DEBUG msg="vsock_to_tcp connection opened" cid=3 port=1234 listen_port=9000
level=DEBUG msg="vsock_to_tcp connection closed" cid=3 port=1234 listen_port=9000 total_bytes=12345
```

`tcp_to_vsock` debug logs, attrs: `remote`, `listen`, `total_bytes`
(on close only):

```
level=DEBUG msg="tcp_to_vsock connection opened" remote=192.168.1.10:54321 listen=0.0.0.0:5432
level=DEBUG msg="tcp_to_vsock connection closed" remote=192.168.1.10:54321 listen=0.0.0.0:5432 total_bytes=12345
```

Upstream dial failures are logged at `WARN` independent of the debug
setting, so alerts keyed on level pick them up. `vsock_to_tcp` attrs:
`cid`, `port`, `listen_port`, `upstream`, `err`. `tcp_to_vsock` attrs:
`remote`, `listen`, `target_cid`, `target_port`, `err`. Dials cancelled
by `Shutdown` during graceful teardown do not emit the warn line or
increment `{tcp_to_vsock,vsock_to_tcp}_errors_total{reason=dial_fail}` —
they are expected shutdown artefacts rather than upstream faults.

Three ways to toggle debug, in order of precedence:

1. `-debug` CLI flag.
2. `VSOCKD_LOG_LEVEL=debug` environment variable.
3. `log_level: debug` in the YAML config.

Any other value than `debug` or `info` for the env var or YAML is a
fatal config error at startup. Omitting the setting keeps the `info`
default.

## Enclave-side socat recipe

The outbound listener inside vsockd speaks the standard HTTP forward-proxy
protocol — but only over vsock, not TCP. To let a plain app inside the
enclave set `HTTP_PROXY` / `HTTPS_PROXY=http://127.0.0.1:3128` and have the
requests reach vsockd, run a `socat` stub in the enclave that bridges a local
TCP port to the parent host's vsock port.

In the following, `8080` is the outbound vsock port configured on the host
and `CID 3` is the well-known vsock CID of the Nitro parent (host).

```sh
socat TCP-LISTEN:3128,reuseaddr,fork VSOCK-CONNECT:3:8080
```

Then in the enclave workload:

```sh
export HTTP_PROXY=http://127.0.0.1:3128
export HTTPS_PROXY=http://127.0.0.1:3128
export NO_PROXY=localhost,127.0.0.1
```

For a systemd-managed enclave image:

```ini
# /etc/systemd/system/vsock-proxy.service (inside the enclave)
[Unit]
Description=vsock forward-proxy bridge
After=network-online.target

[Service]
ExecStart=/usr/bin/socat TCP-LISTEN:3128,reuseaddr,fork VSOCK-CONNECT:3:8080
Restart=always

[Install]
WantedBy=multi-user.target
```

The enclave's inbound side (the thing vsockd routes hostnames to) is simply
the enclave process listening on `AF_VSOCK` at the `(cid, vsock_port)` the
host-side route points to — e.g. a Node.js server bound to vsock port 8443
for HTTPS or 8080 for HTTP. No socat stub is needed inbound.

### Running a second vsockd inside the enclave

Instead of `socat`, a second copy of vsockd running inside the enclave can
bridge local TCP to the parent host's vsock forward-proxy. This gives you
the same SIGHUP reload, `shutdown_grace` drain, Prometheus metrics, and
structured logging that the host-side daemon has — without pulling in
`socat`. Use `tcp_to_vsock` to target the Nitro parent at CID 3:

```yaml
# vsockd.yaml inside the enclave
tcp_to_vsock:
  - bind: 127.0.0.1
    port: 3128
    vsock_cid: 3              # parent host
    vsock_port: 8080          # host-side outbound forward-proxy port

metrics:
  vsock_port: 9090            # let the parent host scrape /metrics over vsock
```

Enclave workloads then set `HTTP_PROXY=http://127.0.0.1:3128` as usual and
their requests flow through the enclave-local vsockd, across vsock to the
host-side vsockd, and out to the approved upstream. The mirror direction
(anything the enclave needs to accept from the host over a non-HTTP
protocol) uses `vsock_to_tcp` in the enclave's config.

## Metrics

vsockd exposes Prometheus metrics at `/metrics`. The endpoint is
**disabled by default** — metrics start only if at least one of the
following is set: `metrics.bind` in YAML, `metrics.vsock_port` in YAML,
or a non-empty `-metrics-addr` flag. Omit all three to disable entirely;
the startup log will say `metrics=disabled`. All series use a private
registry — there is no global Prometheus state.

Two transports are supported, mutually exclusive:

- **TCP (`metrics.bind`)** — the host-side form. Serves `/metrics` on a
  TCP `host:port`. Scrape from Prometheus as usual.
- **vsock (`metrics.vsock_port`)** — the enclave-side form. Listens on
  `AF_VSOCK` bound to `VMADDR_CID_ANY` on the given port so the parent
  host can scrape `/metrics` over vsock. No CID is set on the listener
  — if you need to restrict which peer CID can scrape, put that control
  in front of vsockd rather than inside it.

```yaml
# Host-side
metrics:
  bind: 0.0.0.0:9090

# Enclave-side (mutually exclusive with bind)
metrics:
  vsock_port: 9090
```

An explicit non-empty `-metrics-addr` flag overrides both YAML fields
and forces the TCP transport.

| Metric | Labels | Description |
|---|---|---|
| `inbound_connections_total` | `route` | TCP connections accepted on inbound listeners, by matched route. |
| `inbound_bytes_total` | `route`, `direction` | Bytes proxied inbound, where `direction` is `in` (client → enclave) or `out` (enclave → client). |
| `inbound_errors_total` | `route`, `kind` | Inbound errors by kind: `sniff`, `route`, `dial`, `copy`, `accept`. |
| `outbound_connections_total` | `cid`, `result` | vsock accepts on outbound listeners; `result` is `allowed`, `denied`, or `error`. |
| `outbound_bytes_total` | `cid`, `direction` | Bytes proxied outbound, by CID and direction. |
| `tcp_to_vsock_connections_total` | — | TCP connections accepted on `tcp_to_vsock` listeners. |
| `tcp_to_vsock_bytes_total` | `direction` | Bytes proxied on `tcp_to_vsock` connections; `direction` is `up` or `down`. |
| `tcp_to_vsock_errors_total` | `reason` | `tcp_to_vsock` errors; `reason` is `dial_fail` or `copy_error`. |
| `vsock_to_tcp_connections_total` | — | vsock connections accepted on `vsock_to_tcp` listeners. |
| `vsock_to_tcp_bytes_total` | `direction` | Bytes proxied on `vsock_to_tcp` connections; `direction` is `up` or `down`. |
| `vsock_to_tcp_errors_total` | `reason` | `vsock_to_tcp` errors; `reason` is `dial_fail` or `copy_error`. |
| `config_reloads_total` | `result` | SIGHUP reload attempts; `result` is `success` or `failure`. |

Label cardinality is bounded by the config. `route` is the hostname from the
YAML, or the fixed sentinel `unknown` for errors raised before a route has
been resolved (kinds `accept`, `sniff`, and `route`). `cid` is a CID number
already authorized in config, or the fixed sentinel `unauthorized` for
peers whose CID is not configured on that port. No user-supplied strings
are ever used as label values.

## Operational notes

- **SIGHUP reload.** Sending `SIGHUP` re-reads the config file and diffs it
  against the running set. Listeners whose `(bind, port)` disappeared are
  closed (existing connections continue until they end naturally). New
  listeners are started. Listeners whose bind:port is unchanged swap their
  route/CID tables atomically, and a `vsock_to_tcp` listener kept at the
  same vsock port atomically picks up a new `upstream` — new connections
  see the new rules or upstream, in-flight connections keep the values
  they started with. Listeners in `tcp_to_vsock` cannot change `vsock_cid`
  / `vsock_port` at runtime: a reload that edits them on an already-bound
  bind:port is rejected with a "restart required" error, and the running
  listener keeps forwarding to the original target. Changing the `mode`
  on an already-bound `inbound` bind:port or moving an `outbound` vsock
  port between the HTTP forward-proxy and the `vsock_to_tcp` section is
  rejected the same way. A failed reload is logged,
  `config_reloads_total{result="failure"}` increments, and the running
  config is left untouched.
- **Graceful shutdown.** On `SIGTERM` / `SIGINT` vsockd stops accepting new
  connections, waits up to `shutdown_grace` (default 30s) for active
  connections to drain, then force-closes the rest. A second signal is not
  required.
- **Logging.** All logs are structured via `log/slog`. `-log-format json`
  (the default in production) is a one-line-per-event JSON stream; use
  `text` locally. No plaintext bytes from TLS passthrough are logged.
  The log level defaults to `info`; set `log_level: debug` in the YAML,
  `VSOCKD_LOG_LEVEL=debug`, or pass `-debug` to get per-connection
  open/close events from TCP passthrough listeners.
- **Peer CID trust.** The outbound listener trusts the peer CID reported by
  the kernel — it is set by the Nitro hypervisor and cannot be spoofed by
  the enclave. This is the only authentication for outbound requests in v1.

## Limitations (out of scope for v1)

- **No TLS termination.** Host-side passthrough only; the enclave owns every
  certificate.
- **No authentication beyond the peer CID.** Anyone with the correct CID has
  the policy associated with that CID.
- **No rate limiting or per-destination quotas.**
- **No WebSocket-specific handling.** WebSockets work transparently inside a
  `CONNECT` tunnel; there is no special upgrade handling for plain-HTTP
  WebSockets.
- **No regex patterns in the allowlist** — only exact, suffix (`*.host`),
  and universal (`*`) matching. This is deliberate (keeps patterns cheap
  and predictable).

## Development

```sh
make test           # go test ./...
make vet            # go vet ./...
make lint           # staticcheck ./...
make build          # static binary
make docker         # container image
```

Integration tests use an in-process loopback backend instead of real vsock;
set `VSOCKD_BACKEND=loopback` only in test environments — the production
binary deliberately cannot be driven into a working loopback mode via env
alone.
