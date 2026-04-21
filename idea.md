> **Note:** This document is a historical design doc. Its host-only
> framing and original scope are preserved for context, but they no
> longer reflect the current shape of `vsockd` — see `README.md` for
> the source of truth.

**Project: `vsockd` — a bidirectional proxy for AWS Nitro Enclaves**

A single Go binary running on the parent EC2 host that bridges network traffic between the outside world and applications inside one or more AWS Nitro Enclaves over vsock.

### Responsibilities

**1. Inbound proxy (external → enclave)**
- Listens on configurable TCP ports on the host's network interface (e.g., 80, 443).
- Routes each connection to the correct enclave's vsock based on:
  - HTTP `Host` header for plain HTTP
  - TLS SNI for HTTPS (transparent passthrough — TLS is **not** terminated; the enclave owns the cert)
- Each route: `hostname → (vsock CID, vsock port)`.
- Must support multiple enclaves sharing the same host port with distinct hostnames.

**2. Outbound proxy (enclave → external)**
- Listens on vsock (one port per enclave CID, or a single port with CID-based dispatch).
- Speaks the standard HTTP/HTTPS forward-proxy protocol (`CONNECT` for HTTPS, absolute-URI `GET`/`POST` for HTTP) so the JS app inside the enclave can simply set `HTTP_PROXY` / `HTTPS_PROXY`.
- Enforces a per-enclave egress allowlist (destination host + port, with domain-suffix wildcards) before dialing out.
- Rejects disallowed requests with a clear HTTP 403.

### Configuration

- Single declarative config file (YAML):
  - Inbound listeners: bind address, port, mode (http-host / tls-sni)
  - Inbound routes: `hostname → CID:port`
  - Outbound listeners: vsock port(s), CID-to-rules mapping
  - Outbound egress rules: per-CID allowlist of `host:port` patterns
- Reload on `SIGHUP` without dropping existing connections.

### Non-functional requirements

- Go 1.26+, single static binary, minimal dependencies
- `github.com/mdlayher/vsock` for vsock
- Structured logging via `log/slog`
- Prometheus `/metrics`: connection counts, bytes in/out, errors, broken out per route and per CID
- Graceful shutdown on `SIGTERM`
- Unit tests for: Host-header parsing, SNI parsing, allowlist matching, config loading
- Integration smoke tests using loopback TCP as a vsock stand-in where the CI lacks vsock support

### Out of scope for v1

- TLS termination on the host (passthrough only)
- Authentication beyond vsock CID (CID is treated as trusted identity)
- Rate limiting / quotas
- WebSocket-specific handling (falls out of CONNECT tunneling anyway)

### Deliverables

- `cmd/<name>/main.go` entry point
- Internal packages: `config`, `inbound`, `outbound`, `vsockconn`, `allowlist`, `metrics`
- Example config file
- README with host-side install instructions and a minimal enclave-side `socat` recipe for the client transport
- Dockerfile for the host-side build
