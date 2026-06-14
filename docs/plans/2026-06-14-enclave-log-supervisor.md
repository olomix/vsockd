# Enclave Log Supervisor (`cmd/supervisor`)

## Overview

Add a new in-enclave `supervisor` binary to this repo. It is PID 1's child
(`tini -g` stays PID 1). It spawns the enclave's processes (vsockd sidecar +
the application, e.g. node), captures each process's stdout and stderr, frames
every line as NDJSON tagged with source/pid/stream, emits process lifecycle
events (start/exit with code+signal), and ships the combined stream over its
**own** vsock connection to the parent (`CID 3:<log_port>`) — where the
host-side `log_relay` directive (see `2026-06-14-log-relay-directive.md`)
receives it.

Problem it solves: inside an AWS Nitro Enclave, process stdout/stderr is lost
in production (console only via `nitro-cli --debug-mode`, which voids
attestation). vsock to the parent is the only egress. The supervisor is the
producer side of the log-delivery path; `log_relay` is the consumer side.

Why a separate binary (not folded into vsockd): process supervision is a
distinct concern from transport, and — critically — the supervisor opens its
**own** vsock connection for logs rather than routing through vsockd. That
breaks the dependency cycle so the supervisor can still capture and ship
**vsockd's own crash output**.

## Context (from discovery)

- Files/components involved (new + reused):
  - `cmd/supervisor/main.go` (new) — thin flag/config + signal loop, mirroring
    `cmd/vsockd/main.go`'s structure.
  - `internal/supervisor/` (new) — config, NDJSON envelope, ring buffer, vsock
    shipper, process manager; mirrors the `internal/app` + `internal/config`
    split used by vsockd so logic is testable without spawning a real process
    tree.
  - Reused: `internal/vsockconn` — `NewVsockDialer()` (prod) /
    `NewLoopbackDialer(reg, cid)` (tests) and the `Dialer.Dial(cid, port)`
    interface; `vsockconn.NewRegistry`/`ListenLoopback` for end-to-end tests
    against an in-process listener.
- Related patterns found: vsockd's strict-YAML config (`KnownFields(true)` +
  `Validate()`), structured `slog` logging, and the loopback vsock test
  harness all transfer directly.
- Dependencies identified: standard library `os/exec`, `os`, `bufio`,
  `encoding/json`, `context`, `sync`. No new third-party deps. `mdlayher/vsock`
  is already vendored via `vsockconn`.

## Development Approach

- **Testing approach**: TDD (tests first).
- Complete each task fully before the next; small focused changes.
- **CRITICAL: every task MUST include new/updated tests** as separate checklist
  items; **all tests must pass before the next task**; **update this plan when
  scope changes.**
- Process-spawning code is tested with the standard Go helper-process pattern
  (`os.Args[0]` re-exec guarded by an env var / `-test.run=TestHelperProcess`)
  so no external binaries are required. Pure components (envelope, ring buffer)
  are tested directly. The vsock shipper is tested over the loopback backend.
- Inject a clock (e.g. `func() time.Time`) so timestamp output is
  deterministic in tests.

## Testing Strategy

- **Unit tests**: envelope marshalling, ring-buffer drop/accounting,
  reconnect/backoff, config validation, per-process exit policy, crash-loop
  cap, restart-suspension-on-shutdown.
- **Integration test**: spawn a helper process that prints to stdout+stderr and
  exits with a chosen code; assert the NDJSON stream (log lines + start/exit
  events) arrives at an in-process loopback vsock listener verbatim and in
  order.
- No UI; no Playwright/Cypress.

## Key Design Decisions (settled in planning)

1. **Own vsock connection for logs.** The supervisor `Dial`s `CID 3:<log_port>`
   directly (same primitive vsockd uses), NOT through vsockd — so vsockd crash
   output is still captured. The enclave-side vsockd dying does not affect log
   shipping.
2. **`tini -g` stays PID 1; supervisor does NOT forward external signals.**
   `os/exec` leaves children in the supervisor's process group, which is in
   tini's group, so `tini -g` delivers SIGTERM/SIGINT to vsockd and node
   directly. The supervisor handles its **own** SIGTERM to begin graceful
   shutdown, and signals children by PID only to execute the `shutdown` exit
   action. tini reaps any reparented grandchildren.
3. **Restart policy is suspended once shutdown has begun.** External signal
   received, or a `shutdown`-policy child triggered teardown ⇒ no respawns.
   Otherwise SIGTERM → child exits → supervisor "restarts" it → the enclave
   never dies. This is the single most important correctness rule.
4. **Per-process exit policy** (config): `on_exit: restart | shutdown`.
   `restart` has a crash-loop cap (max restarts in a rolling window); exceeding
   it escalates to `shutdown` (logged). `shutdown` = SIGTERM the other
   children, grace, then SIGKILL stragglers, then exit.
5. **Wire format: NDJSON, `\n`-framed, one object per line.** Record types
   share one stream: `log` (with `msg`), `start`, `exit` (`code`, `signal`),
   and `drop` (`count`). Fields: `ts`, `src`, `pid`, `stream`, `type`, plus
   type-specific fields. We own both ends; no syslog/GELF.
5a. **Source-side enrichment: the supervisor owns `tags`.** A configurable
   `tags` map (identity known inside the enclave — e.g. `service`, `version`)
   is set on every record the supervisor frames, under the `tags` key. This is
   the source half of the two-layer enrichment: the host-side `log_relay`
   (plan 1) separately adds the authoritative peer `cid` and host-environment
   tags under a distinct `host` key, so the two namespaces never collide and no
   record is re-encoded on the host. The supervisor does NOT set `cid`/`host`.
6. **Loss policy in the supervisor, never block producers.** A bounded,
   **frame-granular** ring buffer (drop-oldest, evicting whole frames — never
   mid-frame, which would corrupt NDJSON). Dropped frames are counted; on the
   next successful send after a drop/reconnect, a `{"type":"drop","count":N}`
   record is emitted so loss is observable downstream. While the listener is
   down, frames buffer; only overflow is "skipped". Producers (the pipe
   readers) never block on the network.
7. **Flush within grace on shutdown.** On teardown, drain children's pipes to
   EOF, `waitpid` to record `exit` events, then best-effort flush the buffer to
   `log_relay` within the grace window; force-close and exit when it elapses.
8. **Same repo, reuse `internal/vsockconn`.** Logic in `internal/supervisor`,
   thin `cmd/supervisor/main.go`.

## What Goes Where

- **Implementation Steps** (`[ ]`): all code, tests, in-repo docs/examples.
- **Post-Completion** (no checkboxes): the EIF/entrypoint changes in the Node
  app repo, and real-enclave verification — out of this repo.

## Implementation Steps

### Task 1: Supervisor config schema and validation

- [ ] write tests first (`internal/supervisor/config_test.go`): valid config
      with two processes; missing command; duplicate process name; invalid
      `on_exit`; log_port out of range; non-positive buffer size; defaulting of
      `log_cid` (3) and backoff/crash-loop fields; a `tags` map (and empty
      key/value rejected, matching the log_relay enrich rule).
- [ ] add `internal/supervisor/config.go`: strict YAML (`KnownFields(true)`)
      with `Load`/`Validate` mirroring vsockd. Schema:
      `log_port` (uint32, required), `log_cid` (uint32, default 3),
      `tags` (string→string map, optional — emitted under each record's `tags`),
      `buffer` (`max_bytes` and/or `max_records`), and `processes:` —
      `[]{ name, command, args[], on_exit: restart|shutdown,
      max_restarts, restart_window }`.
- [ ] run `go test ./internal/supervisor/...` (config only) — pass before
      Task 2.

### Task 2: NDJSON envelope + encoder

- [ ] write tests first: each record type (`log`/`start`/`exit`/`drop`)
      marshals to exactly one `\n`-terminated line with the expected fields;
      `exit` carries `code` and optional `signal`; timestamps come from the
      injected clock; embedded newlines in a captured line do not break framing
      (each input line → one record).
- [ ] add `internal/supervisor/event.go`: the envelope struct + a constructor
      per record type and a writer that emits one framed line per record. Clock
      injected as `func() time.Time`. Every record carries the configured
      `tags` map (under `tags`); assert it appears on `log` and lifecycle
      records alike. No `cid`/`host` — those are the host's to add.
- [ ] run tests — pass before Task 3.

### Task 3: Frame-granular ring buffer with drop accounting

- [ ] write tests first: enqueue past capacity drops oldest **whole** frames
      and increments a drop counter; never blocks; `DrainTo(w)` writes buffered
      frames in FIFO order; after a drop, the next drain (or an explicit
      `TakeDrops()`) yields the dropped count exactly once; concurrent
      enqueue/drain is race-free (`-race`).
- [ ] add `internal/supervisor/buffer.go`: bounded queue of `[]byte` frames
      with a byte and/or record budget, drop-oldest eviction at frame
      granularity, atomic drop counter, non-blocking `Enqueue`.
- [ ] run tests (`-race`) — pass before Task 4.

### Task 4: vsock shipper (dial, drain, reconnect, drop record, flush)

- [ ] write tests first using `vsockconn.NewRegistry` + `ListenLoopback`:
      frames enqueued reach an in-process listener verbatim in FIFO order;
      when the listener is absent then appears, buffered frames flush and a
      `drop` record is emitted iff overflow occurred while disconnected;
      a broken connection triggers reconnect with backoff; `Flush(ctx)` drains
      within a deadline and returns when the buffer is empty or the deadline
      passes.
- [ ] add `internal/supervisor/shipper.go`: a goroutine that dials
      `log_cid:log_port` via an injected `vsockconn.Dialer`, drains the ring
      buffer to the conn, reconnects with bounded backoff on error, prepends a
      `drop` record after a gap, and exposes `Flush(ctx)` for shutdown.
- [ ] run tests (`-race`) — pass before Task 5.

### Task 5: Process manager (spawn, capture, waitpid, exit policy, signals)

- [ ] write tests first using the helper-process pattern: a child that writes
      to stdout and stderr and exits with a chosen code produces the expected
      `start`, interleaved `log` (correct `src`/`pid`/`stream`), and `exit`
      (correct `code`) records; `on_exit: restart` respawns a spontaneously
      dying child and the crash-loop cap escalates to shutdown after N restarts
      in the window; `on_exit: shutdown` tears the others down; **once shutdown
      has begun, no child is restarted** (regression test for decision 3);
      SIGTERM to the supervisor drains pipes, records `exit`, and returns.
- [ ] add `internal/supervisor/manager.go`: spawn each process with
      `StdoutPipe`/`StderrPipe`, per-stream line readers feeding the ring
      buffer with `src`/`pid`/`stream`; a `waitpid` loop emitting
      `start`/`exit` and applying the per-process policy; a shutdown path that
      SIGTERMs children (for the `shutdown` action), escalates to SIGKILL after
      a per-child timeout, and sets the "shutting down" flag that suspends
      restarts.
- [ ] run tests (`-race`) — pass before Task 6.

### Task 6: `cmd/supervisor` main wiring

- [ ] write tests first: an end-to-end test that loads a small config, runs the
      supervisor against helper processes with a loopback log listener, and
      asserts the full NDJSON stream (lifecycle + logs) is received; SIGTERM
      shuts everything down within grace and the process exits 0.
- [ ] add `cmd/supervisor/main.go`: flag/config parsing, build the
      logger/clock/dialer (prod `NewVsockDialer`, loopback for tests), wire
      config → buffer → shipper → manager, install the signal handler, run, and
      flush on shutdown. Mirror `cmd/vsockd/main.go` structure.
- [ ] run `go test ./cmd/supervisor/... ./internal/supervisor/...` — pass
      before Task 7.

### Task 7: Verify acceptance criteria

- [ ] verify every Overview / Key Design Decision is implemented (own vsock
      conn, no signal forwarding, restart-suspended-on-shutdown, crash-loop
      cap, NDJSON record types, frame-granular drop with accounting,
      flush-within-grace).
- [ ] verify edge cases: listener never appears (producers still run, buffer
      caps, no deadlock); a process that ignores SIGTERM is SIGKILLed after the
      per-child timeout; vsockd-crash output is captured.
- [ ] run full suite `go test ./...` and with `-race`.
- [ ] run the linter — fix all issues.
- [ ] verify coverage meets the project standard (80%+) for new packages.

### Task 8: Documentation and example config

- [ ] add `examples/supervisor.yaml` (vsockd + an app process, file/log_port
      matching the `log_relay` example).
- [ ] update `README.md` to describe the supervisor's role and the enclave log
      path (supervisor → vsock → host `log_relay`).
- [ ] add a `CHANGELOG.md` entry.

## Technical Details

- **Config shape**:
  ```yaml
  log_port: 5140
  log_cid: 3            # default; parent CID
  tags:                 # source-side identity, emitted under each record's "tags"
    service: abc
    version: "1.1.2"
  buffer:
    max_bytes: 8388608  # 8 MiB
    max_records: 10000
  processes:
    - name: vsockd
      command: /usr/local/bin/vsockd
      args: ["-config", "/etc/vsockd/vsockd.yaml"]
      on_exit: shutdown
    - name: app
      command: /usr/local/bin/node
      args: ["server.js"]
      on_exit: shutdown
      max_restarts: 0           # restart disabled; use >0 to enable
      restart_window: 60s
  ```
- **NDJSON records** (as emitted by the supervisor, before host enrichment adds
  `cid`/`host`):
  ```json
  {"ts":"...","src":"app","pid":42,"stream":"stdout","tags":{"service":"abc","version":"1.1.2"},"type":"log","msg":"..."}
  {"ts":"...","src":"app","pid":42,"stream":"-","tags":{"service":"abc","version":"1.1.2"},"type":"start"}
  {"ts":"...","src":"vsockd","pid":12,"stream":"-","tags":{"service":"abc","version":"1.1.2"},"type":"exit","code":1,"signal":null}
  {"ts":"...","src":"supervisor","pid":1,"stream":"-","tags":{"service":"abc","version":"1.1.2"},"type":"drop","count":128}
  ```
- **Process group / signals**: children inherit the supervisor's process group
  (no `Setpgid`); `tini -g` fans out external signals. The supervisor signals
  children by PID only for the `shutdown` action. tini reaps reparented
  grandchildren.
- **Shutdown order**: receive signal / trigger → set shutting-down (suspends
  restart) → drain pipes to EOF → `waitpid` for `exit` records → `Flush(ctx)`
  the buffer within grace → exit.

## Open Items To Confirm During Implementation

- **Restart default**: plan treats `max_restarts: 0` as "restart disabled".
  Confirm vs. a non-zero default.
- **Buffer budget**: defaults above (8 MiB / 10k records) are placeholders;
  tune once real log volume is known.
- **Backoff**: bounded exponential (e.g. 100ms → 5s); exact values set in
  Task 4.

## Post-Completion

*External to this repo — informational only.*

- **Node app repo**: replace the `entrypoint.eif.sh` responsibility of starting
  vsockd + node with the supervisor as `tini -g`'s child; update
  `Dockerfile.eif` to install the supervisor binary and point it at
  `supervisor.yaml`.
- **Host side**: configure vsockd's `log_relay` on the matching port (plan 1).
- **Real-enclave verification**: confirm app + vsockd logs land host-side and
  survive a host-listener restart (buffer + reconnect + drop accounting), and
  that vsockd crash output is captured.
- **Delivery agent**: point CloudWatch agent / vector / fluent-bit (or
  journald) at the host sink for enrichment, ingest-timestamping, and retention.
