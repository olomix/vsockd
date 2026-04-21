# vsockd

`vsockd` is a single static Go binary that runs on the parent EC2 host of AWS
Nitro Enclaves and bridges network traffic in both directions over vsock. One
daemon can front multiple enclaves sharing the same inbound ports and enforce
a per-CID outbound egress allowlist.

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

Multiple enclaves may share the same inbound host port under distinct
hostnames. The same CID may NOT appear under more than one outbound port —
this is enforced at config-load time.

## Architecture

```
                                            parent EC2 host
                                    ┌────────────────────────────────┐
                                    │                                │
  external client ── TCP 443 ─────▶ │  inbound listener              │
  (TLS ClientHello w/ SNI)          │   └─ sniff SNI                 │
                                    │   └─ lookup hostname → CID     │ ── vsock(CID, port) ──▶ enclave
                                    │   └─ dial vsock                │
                                    │   └─ replay ClientHello + copy │
                                    │                                │
  external service ◀── TCP ──────── │  outbound listener             │
  (e.g. api.stripe.com:443)         │   └─ accept vsock, read peer CID
                                    │   └─ CID in port's CID set?    │ ◀── vsock(port) ─────── enclave
                                    │   └─ read HTTP request         │     (CONNECT/GET/POST)
                                    │   └─ allowlist(host:port)?     │
                                    │   └─ dial + stream             │
                                    │                                │
                                    │  /metrics (Prometheus)         │
                                    └────────────────────────────────┘
```

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
contains only the vsockd binary. The image `EXPOSE`s port 9090 for
`/metrics`; add `-p 9090:9090` (or change the listen address) when not
running with `--network host`.

### systemd unit

```ini
# /etc/systemd/system/vsockd.service
[Unit]
Description=vsockd — Nitro Enclave vsock proxy
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/vsockd -config /etc/vsockd/vsockd.yaml
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=2
User=vsockd
Group=vsockd
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
```

`CAP_NET_BIND_SERVICE` lets the non-root `vsockd` user bind to ports 80 and
443. The kernel's `AF_VSOCK` support is required on the host (standard on
Nitro-capable instance types).

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin vsockd
sudo systemctl daemon-reload
sudo systemctl enable --now vsockd
```

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

metrics:
  bind: 0.0.0.0:9090

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
- `-metrics-addr ADDR` — Prometheus listen address. Default `:9090`. An
  explicit flag overrides `metrics.bind` from the YAML.
- `-log-format FORMAT` — `json`, `text`, or `auto`. Overrides the YAML.
  `auto` picks `text` when stderr is a TTY and `json` otherwise.
- `-version` — print the version and exit.

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

## Metrics

vsockd exposes Prometheus metrics at `/metrics` on the address configured by
`metrics.bind` (or the `-metrics-addr` flag). All series use a private
registry — there is no global Prometheus state.

| Metric | Labels | Description |
|---|---|---|
| `inbound_connections_total` | `route` | TCP connections accepted on inbound listeners, by matched route. |
| `inbound_bytes_total` | `route`, `direction` | Bytes proxied inbound, where `direction` is `in` (client → enclave) or `out` (enclave → client). |
| `inbound_errors_total` | `route`, `kind` | Inbound errors by kind: `sniff`, `route`, `dial`, `copy`, `accept`. |
| `outbound_connections_total` | `cid`, `result` | vsock accepts on outbound listeners; `result` is `allowed`, `denied`, or `error`. |
| `outbound_bytes_total` | `cid`, `direction` | Bytes proxied outbound, by CID and direction. |
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
  route/CID tables atomically — new connections see the new rules, in-flight
  connections keep the rules they started with. A failed reload is logged,
  `config_reloads_total{result="failure"}` increments, and the running
  config is left untouched.
- **Graceful shutdown.** On `SIGTERM` / `SIGINT` vsockd stops accepting new
  connections, waits up to `shutdown_grace` (default 30s) for active
  connections to drain, then force-closes the rest. A second signal is not
  required.
- **Logging.** All logs are structured via `log/slog`. `-log-format json`
  (the default in production) is a one-line-per-event JSON stream; use
  `text` locally. No plaintext bytes from TLS passthrough are logged.
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
