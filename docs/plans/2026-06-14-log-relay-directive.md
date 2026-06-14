# log_relay Directive (host-side vsock log sink + enrichment)

## Overview

Add a new top-level `log_relay` config directive to vsockd. It accepts vsock
connections on a configured port, reads the incoming **NDJSON stream
line-by-line**, enriches each record with host-only metadata (the authoritative
peer CID, plus a configurable set of host tags), and writes the enriched lines
to a local sink — either an append-only file or stdout. It is the host-side
endpoint for enclave log delivery: an in-enclave supervisor (separate, later
plan) connects out to the parent (CID 3) on this port and ships framed NDJSON
logs; `log_relay` enriches and lands them on the host where a delivery agent
(CloudWatch agent / vector / fluent-bit) or journald takes over.

Problem it solves: inside an AWS Nitro Enclave, stdout/stderr only reaches the
enclave console, readable solely via `nitro-cli console --debug-mode`, which
zeroes attestation PCRs and is unusable in production. vsock to the parent is
the only channel out. `log_relay` is the host-side receiver for that channel.

**Two-layer enrichment.** Each side adds what only it knows, in its own
namespace:
- The **supervisor** (inside the enclave) sets identity it knows from within —
  e.g. `service`, `version` — under `tags` in each record it frames.
- **vsockd `log_relay`** (on the host) adds metadata only the host has — the
  un-spoofable peer `cid` and host-environment tags (region, instance, …) the
  enclave cannot see — under a separate `host` key, plus top-level `cid`.

Integration: extends the existing `internal/outbound` Server with a new
listener mode (like `vsock_to_tcp`), rather than adding a parallel subsystem.
Unlike `vsock_to_tcp`'s raw byte copy, `log_relay` is line-aware so it can
inject host fields per record.

## Context (from discovery)

- Files/components involved:
  - `internal/config/config.go` — `Config` struct, `Validate()`, the shared
    `seenPort` vsock-port uniqueness map (config.go:203-299).
  - `internal/outbound/server.go` — `listener` struct (mode discriminator +
    `upstream atomic.Pointer[string]`), `NewServer`, accept loop (`l.run`),
    `PrepareApply`/`CommitApply`/`AbortApply` reload diff keyed by `.port`,
    connection tracking, shutdown.
  - `internal/outbound/tcp.go` — `handleTCP` (the raw-copy analog).
  - `internal/metrics/metrics.go` — `VsockToTCP{Connections,Bytes,Errors}`
    construction + `reg.MustRegister`.
  - `internal/app/app.go` — `app.New` builds `outbound.NewServer`, `Reload`
    calls `PrepareApply`.
  - `internal/vsockconn/loopback.go` — `NewRegistry`, `ListenLoopback`,
    `NewLoopbackDialer` test harness (AF_VSOCK-free); `Conn.PeerCID()` gives the
    peer CID used for enrichment.
  - `examples/vsockd.yaml`, `README.md`, `CHANGELOG.md` — docs.
- Related patterns found: `vsock_to_tcp` was itself added as a `mode` on the
  shared `listener` (`modeVsockToTCP`), with the per-connection mutable bit
  (`upstream`) held in an `atomic.Pointer` and swapped on reload. `log_relay`
  mirrors this with a `sink` + enrichment config instead of an `upstream`.
- Dependencies identified: standard library `os`, `bufio`, `encoding/json`
  (only `json.Valid` for the splice path; values are not decoded). No new deps.

## Development Approach

- **Testing approach**: TDD (tests first) — matches the repo's existing
  discipline (loopback vsock harness, table-driven config tests).
- Complete each task fully before moving to the next.
- Make small, focused changes.
- **CRITICAL: every task MUST include new/updated tests** for code changes in
  that task; tests are a required checklist item, listed separately from
  implementation.
- **CRITICAL: all tests must pass before starting the next task.**
- **CRITICAL: update this plan file when scope changes during implementation.**
- Run `go test ./...` after each change. Maintain backward compatibility:
  configs without a `log_relay` section behave exactly as before.

## Testing Strategy

- **Unit tests**: required for every task. Config validation is table-driven
  (mirror `config_test.go`). Listener/handler behavior uses the loopback vsock
  harness (`vsockconn.NewRegistry` + `newLoopbackListenFunc` + a loopback
  dialer with a known CID), mirroring `internal/outbound/tcp_test.go`.
- **E2E tests**: the repo has `test/e2e/e2e_test.go`. Add an end-to-end case
  that dials a `log_relay` port over the loopback backend and asserts enriched
  NDJSON lands in the target file, if it fits the existing e2e harness;
  otherwise the unit + handler tests cover it.
- No UI; no Playwright/Cypress.

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document issues/blockers with ⚠️ prefix.
- Keep this plan in sync with actual work done.

## What Goes Where

- **Implementation Steps** (`[ ]`): all code, tests, and in-repo docs.
- **Post-Completion** (no checkboxes): the in-enclave supervisor binary
  (separate plan), real-enclave verification, and delivery-agent wiring.

## Key Design Decisions (settled in planning)

1. **NDJSON-line-aware enrichment (not a verbatim byte pipe).** `log_relay`
   reads the connection line-by-line and emits one enriched line per input
   line. This is a deliberate change from the `vsock_to_tcp` raw-copy model:
   host-only metadata (peer CID, host tags) can only be injected here.
2. **Splice, don't re-encode.** vsockd does NOT fully decode/re-marshal each
   record — that would silently mangle the enclave's payload (JSON decode turns
   all numbers into float64, losing int64 precision on pids/timestamps, and
   reorders keys). Instead: if a line is a valid JSON object (`json.Valid` +
   leading `{`), splice the precomputed host prefix (`"cid":N,"<hostKey>":{…},`)
   immediately after the opening brace, preserving every original byte. Handle
   the empty-object `{}` case (no trailing comma). A line that is not a valid
   JSON object is wrapped as a `raw` record:
   `{"cid":N,"<hostKey>":{…},"type":"raw","msg":<json-quoted original>}`. Output
   is therefore always well-formed NDJSON.
3. **Enrichment namespaces & provenance — no merging (deliberate).** The
   supervisor owns `tags` (service/version, set inside the enclave). The host
   adds top-level `cid` and a host-tags object under a **configurable key**
   (`enrich.host_key`, default `host`). vsockd deliberately does NOT merge host
   data into the enclave's `tags` (or anywhere else): merging would force a full
   decode/re-encode (mangling values) and impose a collision policy vsockd has
   no business deciding. Reshaping/flattening/merging the record is the job of
   the downstream log-processing pipeline, outside vsockd's scope. This
   separation is intentional and must be documented as such (see Task 6).
4. **One connection at a time per listener (serial handling) for v1.** With a
   single supervisor producer this is the simplest correct design (one scanner,
   no write-side locking). Note: because each emitted line is complete and
   self-describing (carries its own `cid`), line-aware enrichment *removes* the
   byte-interleaving hazard that forced serial handling for raw copy — so
   concurrent connections from multiple enclave CIDs on one port is a clean
   future relaxation (a sink write-mutex), explicitly out of scope for v1.
5. **`output` is required; `path` required iff `output: file`.** Omitting
   `output`, or setting `path` with `output: stdout`, or omitting `path` with
   `output: file`, are all validation errors (strict, fail-loud — consistent
   with the rest of `config.Validate`). Both `file` and `stdout` are valid
   outputs; picking `stdout` means relayed logs share vsockd's process stdout
   (its own slog goes to stderr), a documented consequence.
6. **Enrichment config is optional.** With no `enrich` block, the listener
   still emits valid NDJSON but adds no `cid`/host-tags (it remains line-framed
   pass-through — long lines are still bounded, see decision 8). `enrich.cid`
   (bool) toggles the top-level `cid` field; `enrich.tags` (string→string map)
   populates the host-tags object; `enrich.host_key` (string, default `host`)
   names the key that object is emitted under.
7. **Sink opened once at listener start**, not per connection. The file is
   opened `O_APPEND|O_CREATE|O_WRONLY`. stdout is never closed by the listener.
8. **Bounded line length.** Reading line-by-line needs an explicit max line
   size (the default `bufio.Scanner` 64 KiB token cap is too small for some log
   lines and silently errors). Use a configurable `max_line_bytes` (sensible
   default, e.g. 1 MiB); an over-long line is truncated-and-flagged (emit it as
   a `raw` record with a `truncated:true` marker) and counted, never silently
   dropped or used to wedge the reader.
9. **Reload mirrors the `upstream` swap.** The sink and the enrichment config
   live in `atomic.Pointer`s; a same-port reload swaps them, and an in-flight
   relay keeps using the old sink/enrichment until its connection closes —
   identical semantics to the `vsock_to_tcp` upstream swap. Added/removed ports
   bind/close normally.
10. **Port uniqueness.** `log_relay` ports join the shared `seenPort` map so a
    collision with outbound / `vsock_to_tcp` / `metrics.vsock_port` is rejected
    at load. (The Node app's `3128`/`3000` are TCP loopback ports, a different
    namespace — no collision.)

## Implementation Steps

### Task 1: Add `log_relay` config schema and validation

- [x] add `LogRelayListener` struct to `internal/config/config.go`:
      `Port uint32 \`yaml:"port"\``, `Output string \`yaml:"output"\``,
      `Path string \`yaml:"path"\``, `MaxLineBytes int \`yaml:"max_line_bytes"\``,
      `Enrich *LogRelayEnrich \`yaml:"enrich"\``; and a `LogRelayEnrich`
      struct `{ CID bool \`yaml:"cid"\``; `Tags map[string]string
      \`yaml:"tags"\``; `HostKey string \`yaml:"host_key"\`` }`. Add
      `LogRelay []LogRelayListener \`yaml:"log_relay"\`` on `Config`.
- [x] add output constants `LogRelayOutputFile = "file"`,
      `LogRelayOutputStdout = "stdout"`, and a `defaultMaxLineBytes` const.
- [x] add `(*LogRelayListener).validate()`: port in `1..vsockPortAny-1`;
      `output` one of the two constants (empty → error); `path` required and
      non-empty iff `output == file`, empty iff `output == stdout`;
      `max_line_bytes` ≥ 0 (0 → default); if `enrich` set, every tag key and
      value must be non-empty, and `host_key` defaults to `host` when empty (a
      set `host_key` must be non-empty / valid as a JSON object key).
- [x] wire into `Config.Validate()`: include `LogRelay` in the "no listeners
      configured" emptiness check; loop `LogRelay`, call `validate()`, and add
      each port to the shared `seenPort` map (reuse the existing collision
      error pattern so the message names the conflicting section).
- [x] write tests (success): file output with path; stdout output without path;
      multiple listeners on distinct ports; `enrich` with cid+tags; a custom
      `host_key`; `host_key` defaulting to `host` when omitted; absent
      `enrich`; `max_line_bytes` defaulting.
- [x] write tests (error/edge): missing `output`; `output: file` without
      `path`; `output: stdout` with a `path`; bad `output`; port out of range;
      port colliding with an outbound port, a `vsock_to_tcp` port, and
      `metrics.vsock_port`; empty tag key/value; negative `max_line_bytes`.
- [x] run `go test ./internal/config/...` — must pass before Task 2.

### Task 2: Add `log_relay` listener mode, sink, and enrichment to outbound

- [x] write tests first (`internal/outbound/logrelay_test.go`) using the
      loopback harness with a known peer CID: send several NDJSON object lines
      and assert each output line has `"cid":<peer>` and the host-tags object
      under the configured key spliced in while all original fields/bytes are
      preserved (incl. a large int64 value, unchanged); assert a custom
      `host_key` places the object under that key and the default lands under
      `host`; assert the enclave's own `tags` are left untouched (no merge);
      send an empty object `{}` and assert valid output (no trailing comma);
      send a non-JSON line and assert it becomes a `raw` record carrying the
      original text in `msg`; send an over-long line and assert truncated `raw`
      + flag; with no `enrich`, assert lines pass through framed but unmodified;
      dial a stdout listener and assert output reaches the injected writer;
      assert serial handling (second dial waits); assert Shutdown force-closes
      an in-flight relay within grace.
- [x] add `modeLogRelay = "log_relay"` constant in
      `internal/outbound/server.go`.
- [x] define a `sink` (`io.Writer` + `Close() error`; stdout's Close is a
      no-op) and `openSink(cfg) (sink, error)` — `os.OpenFile(path,
      O_APPEND|O_CREATE|O_WRONLY, 0o640)` for file, an un-closing `os.Stdout`
      wrapper for stdout (target injectable via the package-level
      `stdoutSinkWriter` seam for tests).
- [x] define enrichment: precompute, per connection, the host prefix bytes
      (`"cid":N,"<host_key>":{…},`) from the listener's enrich config (incl. the
      configured `host_key`) + the conn's `PeerCID()`; an `enrichLine(dst,
      line)` that splices the prefix into a valid JSON object (empty-object
      aware) or wraps a non-object/over-long line as a `raw` record. The
      enclave's `tags` are never read or modified — host data is additive only.
- [x] add `sink atomic.Pointer[sink]` and `enrich atomic.Pointer[enrichConfig]`
      to the `listener` struct; `newLogRelayListener(cfg, s)` opens the sink and
      stores both.
- [x] extend `NewServer` to accept `[]config.LogRelayListener`, build these
      listeners, and update its signature + the `app.New` call site in
      `internal/app/app.go` (`outbound.NewServer(..., opts.Config.LogRelay)`).
- [x] dispatch `modeLogRelay` in the accept loop to `handleLogRelay`, handled
      **serially** per decision 4, with a comment explaining the rationale.
- [x] implement `handleLogRelay` (`internal/outbound/logrelay.go`): track the
      conn; `bufio` line reader bounded by `max_line_bytes`; per line, enrich
      and write to the current sink; close conn on return. No upstream dial.
      (Byte/line/error metric counting is deferred to Task 3, which adds the
      metric fields.)
- [x] run `go test ./internal/outbound/... ./internal/app/...` — must pass
      before Task 3.

### Task 3: Add `log_relay` metrics

- [x] write tests first: extend the Task 2 tests to assert
      `LogRelayConnections` increments per accepted connection,
      `LogRelayLines` advances per emitted line, `LogRelayBytes` advances by
      output bytes, and `LogRelayErrors{reason}` increments on sink-open
      failure, read error, and line-too-long.
- [x] add metric fields in `internal/metrics/metrics.go`:
      `LogRelayConnections` (`log_relay_connections_total`),
      `LogRelayLines` (`log_relay_lines_total`),
      `LogRelayBytes` (`log_relay_bytes_total`),
      `LogRelayErrors *prometheus.CounterVec{reason}`
      (`log_relay_errors_total`).
- [x] add reason constants (`LogRelayErrorSink = "sink_open"`,
      `LogRelayErrorRead = "read_error"`,
      `LogRelayErrorLineTooLong = "line_too_long"`).
- [x] construct the metrics in `New()` and add them to `reg.MustRegister`.
- [x] emit them in `handleLogRelay` (and on sink-open failure).
- [x] run `go test ./internal/metrics/... ./internal/outbound/...` — must pass
      before Task 4.

### Task 4: Support `log_relay` in SIGHUP reload (add/remove/swap)

- [x] write tests first: reload that adds a `log_relay` listener starts
      accepting on the new port; reload that removes one closes the listener and
      its file sink; reload that changes `path` or `enrich` on an existing port
      routes new connections to the new sink/enrichment while an in-flight relay
      keeps the old ones; a port mode change (`vsock_to_tcp` → `log_relay`) is
      rejected, matching existing behavior.
      (`internal/outbound/logrelay_reload_test.go`)
- [x] extend `applySwap` + the `ApplyPlan` build in `PrepareApply` to carry a
      new `sink` and `enrichConfig` for matched `log_relay` ports, mirroring the
      `upstream` swap; `PrepareApply` must accept `cfg.LogRelay`. The sink is
      reference-counted (`refSink`) so an in-flight relay holding the old sink
      keeps it open until it finishes — the fd closes only when the last
      reference drops, satisfying decision 9 without leaking.
- [x] in `CommitApply`, atomically replace sink + enrich on swapped listeners;
      ensure removed `log_relay` listeners close their sink; `AbortApply` closes
      any sink opened for a not-yet-committed listener (no fd leak).
- [x] update `internal/app/app.go` `Reload` to pass `cfg.LogRelay` into
      `out.PrepareApply`.
- [x] run `go test ./internal/outbound/... ./internal/app/...` — must pass
      before Task 5.

### Task 5: Verify acceptance criteria

- [x] verify every Key Design Decision is implemented (line-aware enrichment,
      splice-not-reencode incl. int64 preservation, namespaces, serial
      handling, output/path rules, optional enrich, bounded line length, reload
      swap, port uniqueness). Each decision has a covering test
      (FileSplicesHostFields, CustomHostKey, EmptyObject, NonJSONWrappedAsRaw,
      OverLongLineTruncated, NoEnrichPassThrough, SerialHandling,
      ReloadSwapSinkAndEnrich, plus config-package tests for output/path and
      port-uniqueness rules).
- [x] verify edge cases: empty config minus log_relay still valid; stdout sink
      not closed on shutdown; file fd released on listener close; empty object;
      non-JSON line; over-long line. (StdoutSink + no-op Close,
      ReloadRemovesListener, EmptyObject, NonJSONWrappedAsRaw,
      OverLongLineTruncated.)
- [x] run the full unit suite `go test ./...` (and `-race`). All green.
- [x] run the e2e suite (`go test ./test/e2e/...`); add a log_relay e2e case if
      it fits the existing harness. Added TestEndToEnd_LogRelay (file sink +
      cid/host-tags enrichment, int64 preservation, raw-wrapped non-JSON).
- [x] run the linter (`make lint` / staticcheck) — fix all issues. staticcheck
      and `go vet` clean.
- [x] verify coverage meets the project standard (80%+) for changed packages.
      config 96.1%, outbound 84.1% exceed it; the log_relay functions
      themselves are 80–100% covered and metrics.New (constructing all
      log_relay counters) is 100%. The metrics/app package totals sit below
      80% only because of pre-existing, unrelated infrastructure
      (NewVsockNetListener, app lifecycle) untouched by this feature.

### Task 6: Update documentation

- [x] add a documented `log_relay` section to `examples/vsockd.yaml` (a file
      sink with `enrich` cid+tags+`host_key`, and a stdout sink) covering
      `output`/`path` rules, the `enrich` namespaces and configurable
      `host_key`, `max_line_bytes`, the single-producer expectation, and the
      stdout stream-muxing caveat. (TestLoadExample now also asserts the
      example has a log_relay listener, guarding against drift.)
- [x] document the **deliberate no-merge / additive-only** decision (in
      `examples/vsockd.yaml` comments and the README): vsockd adds `cid` and the
      host-tags object alongside the enclave's record without merging into or
      rewriting `tags`; any reshaping/flattening/merging is the downstream
      log-processing pipeline's responsibility, intentionally outside vsockd.
- [x] update `README.md` directive list/section if it enumerates directives.
      (Added a "What it does" bullet, a minimal-config snippet, a dedicated
      "Log relay" section, metrics-table rows, and the SIGHUP reload note.)
- [x] update the `outbound` package doc and `config.go` package doc to mention
      the log-sink/enrichment mode.
- [x] add a `CHANGELOG.md` entry.

## Technical Details

- **Config shape**:
  ```yaml
  log_relay:
    - port: 5140
      output: file
      path: /var/log/enclave/app.ndjson
      max_line_bytes: 1048576      # optional; default 1 MiB
      enrich:
        cid: true                  # add top-level "cid": <peer CID>
        host_key: host             # optional; key for host tags (default "host")
        tags:                      # host-only metadata, emitted under host_key
          region: us-east-1
          instance: i-0abc123
    - port: 5141
      output: stdout               # no enrich → framed pass-through
  ```
- **Record flow** (supervisor → vsockd → sink), default `host_key`:
  ```json
  // from supervisor (it owns "tags"):
  {"ts":"…","src":"app","pid":42,"tags":{"service":"abc","version":"1.1.2"},"type":"log","msg":"…"}
  // after vsockd splices host fields (it adds "cid" + host_key; "tags" untouched):
  {"cid":16,"host":{"region":"us-east-1","instance":"i-0abc123"},"ts":"…","src":"app","pid":42,"tags":{"service":"abc","version":"1.1.2"},"type":"log","msg":"…"}
  ```
- **Enrichment**: precompute the prefix `"cid":N,"<host_key>":{…},` once per
  connection. Per line: if `json.Valid(line)` and first non-ws byte is `{`,
  splice the prefix after `{` (drop the trailing comma when the object is
  empty); else wrap as
  `{"cid":N,"<host_key>":{…},"type":"raw","msg":<quoted>,"truncated":<bool>}`.
  Values in the enclave's payload are never decoded, so int64s/key order survive
  intact, and `tags` is never read or merged — host data is additive only.
- **Sink**: `interface { io.Writer; Close() error }`. File =
  `os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)`. Stdout = a
  no-op-Close wrapper. Held in `atomic.Pointer[sink]` for reload swap.
- **Handler**: `handleLogRelay(ctx, c vsockconn.Conn)` → track conn → bounded
  line reader → enrich+write → count → close. No dial, no allowlist, no per-CID
  auth (any peer the port accepts is relayed, same trust model as
  `vsock_to_tcp`). Serial per listener for v1.
- **Metrics**: `log_relay_connections_total`, `log_relay_lines_total`,
  `log_relay_bytes_total`, `log_relay_errors_total{reason}`
  (`sink_open` | `read_error` | `line_too_long`).

## Open Items To Confirm

- **`max_line_bytes` default**: 1 MiB placeholder — tune to real log lines.

## Settled (resolved in review)

- **No merging / additive-only.** vsockd adds `cid` and a host-tags object
  (under the configurable `host_key`, default `host`) alongside the enclave's
  record. It deliberately does NOT merge into or rewrite the enclave's `tags`,
  because merging would require a full decode/re-encode (mangling int64s and key
  order) and a collision policy that is not vsockd's to make. Any
  reshaping/flattening/merging belongs to the downstream log-processing
  pipeline. This is intentional and documented (Task 6).

## Post-Completion

*Items requiring manual intervention or external systems — informational only.*

**Separate plan (next):**
- The in-enclave `cmd/supervisor` binary that spawns child processes, captures
  stdout/stderr, frames NDJSON with lifecycle events **and its own `tags`
  (service/version)**, and ships them over its own vsock connection to
  `CID 3:<log_relay port>` with a frame-granular drop-oldest ring buffer,
  reconnect, and per-process exit policy. Tracked in
  `docs/plans/2026-06-14-enclave-log-supervisor.md`.

**Real-enclave verification:**
- Build the EIF with `log_relay` configured host-side and the supervisor
  in-enclave; confirm app + supervisor logs land in the host file enriched with
  cid/host tags, and survive a host-listener restart (buffer + reconnect + drop
  accounting).

**Delivery-agent wiring:**
- Point a delivery agent (CloudWatch agent / vector / fluent-bit) or journald
  at the sink; the agent owns final routing, ingest-timestamping, and retention.
