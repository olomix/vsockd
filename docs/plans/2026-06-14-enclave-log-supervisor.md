# Enclave Supervisor (`cmd/supervisor`)

## Overview

Add a new in-enclave `supervisor` binary to this repo. It is PID 1's child
(`tini -g` stays PID 1). It **spawns and supervises** the enclave's processes
(application task(s) + a vsockd sidecar) under a role/restart policy, captures
each process's stdout and stderr **and its own operational logs**, frames every
line as NDJSON tagged with source/pid/stream, emits process lifecycle events
(start/exit with code+signal), and ships the combined stream over its **own**
vsock connection to the parent (`CID 3:<log_port>`) — where the host-side
`log_relay` directive (see `2026-06-14-log-relay-directive.md`) receives it.

Problem it solves: inside an AWS Nitro Enclave, process stdout/stderr is lost
in production (console only via `nitro-cli --debug-mode`, which voids
attestation). vsock to the parent is the only egress. The supervisor is the
producer side of the log-delivery path; `log_relay` is the consumer side. The
supervisor's *own* logs face the same blackout, so it ships them on the same
channel rather than writing only to an unreachable stderr.

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
    shipper, slog handler, process manager; mirrors the `internal/app` +
    `internal/config` split used by vsockd so logic is testable without spawning
    a real process tree.
  - Reused: `internal/vsockconn` — `NewVsockDialer()` (prod) /
    `NewLoopbackDialer(reg, cid)` (tests) and the `Dialer.Dial(cid, port)`
    interface; `vsockconn.NewRegistry`/`ListenLoopback` for end-to-end tests
    against an in-process listener.
- Related patterns found: vsockd's strict-YAML config (`KnownFields(true)` +
  `Validate()`), structured `slog` logging, and the loopback vsock test
  harness all transfer directly.
- Dependencies identified: standard library `os/exec`, `os`, `bufio`,
  `encoding/json`, `log/slog`, `context`, `sync`. No new third-party deps.
  `mdlayher/vsock` is already vendored via `vsockconn`.

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
- Inject a clock (e.g. `func() time.Time`) so timestamp output and the restart
  window are deterministic in tests.

## Testing Strategy

- **Unit tests**: envelope marshalling, ring-buffer drop/accounting,
  reconnect/backoff, config validation, the role/restart/on_failure policy
  (success vs. failure exit, `restart` modes, windowed crash-loop cap, task
  completion, terminate vs. continue), restart-suspension-on-shutdown, and the
  slog-handler-to-buffer path.
- **Integration test**: spawn a helper process that prints to stdout+stderr and
  exits with a chosen code; assert the NDJSON stream (log lines + lifecycle
  events + the supervisor's own log records) arrives at an in-process loopback
  vsock listener in order.
- No UI; no Playwright/Cypress.

## Key Design Decisions (settled in planning)

1. **Own vsock connection for logs.** The supervisor `Dial`s `CID 3:<log_port>`
   directly (same primitive vsockd uses), NOT through vsockd — so vsockd crash
   output is still captured. The enclave-side vsockd dying does not affect log
   shipping.
2. **`tini -g` stays PID 1; supervisor does NOT forward external signals.**
   `os/exec` leaves children in the supervisor's process group, which is in
   tini's group, so `tini -g` delivers SIGTERM/SIGINT to the children directly.
   The supervisor handles its **own** SIGTERM to begin graceful shutdown, and
   signals children by PID only when it itself initiates shutdown (decision 4).
   tini reaps any reparented grandchildren.
3. **Restart policy is suspended once shutdown has begun.** Once teardown is
   triggered (external signal, all-tasks-done, or a `terminate` give-up), child
   exits no longer restart anything. Otherwise SIGTERM → child exits →
   supervisor "restarts" it → the enclave never dies. The single most important
   correctness rule.
4. **Process roles & restart/failure policy.** Each process declares:
   - `role: task | sidecar` (**required**). *tasks* are what the supervisor
     exists to run to completion; *sidecars* support them.
   - `restart: no | on-failure | always` (default `on-failure`) — whether an
     exit warrants a restart: `always` = any exit, `on-failure` = exit ≠ 0,
     `no` = never. Mirrors systemd `Restart=` / Docker restart policies.
   - `max_restarts` + `restart_window` — windowed crash-loop cap (decision 4a).
   - `on_failure: terminate | continue` (default `terminate`) — what happens
     when a process *gives up* (a restart-warranting exit with the budget
     exhausted, or a non-zero exit under `restart: no`): `terminate` =
     gracefully shut everything down and exit non-zero; `continue` = abandon
     just this process and keep the rest running.

   Per-exit logic, while NOT already shutting down:
   - restart warranted (per `restart`) **and** within the windowed budget →
     restart (logged).
   - restart warranted **but budget exhausted** → give up → apply `on_failure`.
   - restart **not** warranted:
     - exit 0 → *task*: mark **done** (success); *sidecar*: tolerated, no action.
     - exit ≠ 0 (only under `restart: no`) → terminal failure → apply `on_failure`.

   **Task completion:** when **all tasks** reach a terminal state (done, or
   abandoned via `continue`), gracefully shut down the sidecars and exit. Exit
   code is 0 iff every task is done-success; non-zero if any task was abandoned.
   Multiple tasks are allowed (wait for all). Zero tasks = daemon mode (runs
   until an external signal or a `terminate` give-up).
4a. **Windowed restart counting.** Restart attempts are timestamped (injected
   clock); attempts older than `restart_window` are pruned. A process gives up
   only when it accumulates `max_restarts` restarts **within** the window — a
   genuine hot-loop — so a process that crashes occasionally but then runs
   healthily past the window gets a fresh budget. (Equivalent to systemd
   `StartLimitBurst`/`StartLimitIntervalSec`.)
5. **The supervisor ships its own logs.** A custom `slog.Handler` formats the
   supervisor's own log records (startup, each spawn, restart with attempt
   count, give-up, shutdown reason, exit codes) into NDJSON `log` records with
   `src:"supervisor"` and enqueues them to the same ring buffer as child output.
   They are also mirrored to stderr — as a debug-mode-console fallback and
   because *before* the buffer/shipper exist (e.g. a config-load failure) stderr
   is the only path. Inside the enclave this is the only way the supervisor's
   own logs escape.
6. **Wire format: NDJSON, `\n`-framed, one object per line.** Record types share
   one stream: `log` (with `msg`), `start`, `exit` (`code`, `signal`), and
   `drop` (`count`). Fields: `ts`, `src`, `pid`, `stream`, `type`, plus
   type-specific fields. We own both ends; no syslog/GELF.
7. **Source-side enrichment: the supervisor owns `tags`.** A configurable `tags`
   map (identity known inside the enclave — e.g. `service`, `version`) is set on
   every record the supervisor frames, under the `tags` key. This is the source
   half of the two-layer enrichment: the host-side `log_relay` (plan 1)
   separately adds the authoritative peer `cid` and host-environment tags under
   a distinct `host` key, so the namespaces never collide and no record is
   re-encoded on the host. The supervisor does NOT set `cid`/`host`.
8. **Loss policy in the supervisor, never block producers.** A bounded,
   **frame-granular** ring buffer (drop-oldest, evicting whole frames — never
   mid-frame, which would corrupt NDJSON). Dropped frames are counted; on the
   next successful send after a drop/reconnect, a `{"type":"drop","count":N}`
   record is emitted so loss is observable downstream. While the listener is
   down, frames buffer; only overflow is "skipped". Producers (the pipe readers
   and the slog handler) never block on the network.
9. **Flush within grace on shutdown.** On teardown, drain children's pipes to
   EOF, `waitpid` to record `exit` events, then best-effort flush the buffer to
   `log_relay` within the grace window; force-close and exit when it elapses.
10. **Same repo, reuse `internal/vsockconn`.** Logic in `internal/supervisor`,
    thin `cmd/supervisor/main.go`.

## What Goes Where

- **Implementation Steps** (`[ ]`): all code, tests, in-repo docs/examples.
- **Post-Completion** (no checkboxes): the EIF/entrypoint changes in the Node
  app repo, and real-enclave verification — out of this repo.

## Implementation Steps

### Task 1: Supervisor config schema and validation

- [x] write tests first (`internal/supervisor/config_test.go`): valid config
      with a task + a sidecar and with multiple tasks; missing command; missing
      `role`; bad `role`/`restart`/`on_failure` enum values; duplicate process
      name; zero tasks allowed (daemon mode); `max_restarts` ≥ 0 and
      `restart_window` > 0 required when restarts can occur (`restart` ≠ `no`
      and `max_restarts` > 0); defaulting of `restart`→`on-failure`,
      `on_failure`→`terminate`, `log_cid`→3; log_port out of range; non-positive
      buffer size; a `tags` map (empty key/value rejected).
- [x] add `internal/supervisor/config.go`: strict YAML (`KnownFields(true)`)
      with `Load`/`Validate` mirroring vsockd. Schema:
      `log_port` (uint32, required), `log_cid` (uint32, default 3),
      `tags` (string→string map, optional), `buffer` (`max_bytes` and/or
      `max_records`), and `processes:` —
      `[]{ name, command, args[], role: task|sidecar,
      restart: no|on-failure|always, max_restarts: int,
      restart_window: duration, on_failure: terminate|continue }`.
- [x] run `go test ./internal/supervisor/...` (config only) — pass before
      Task 2.

### Task 2: NDJSON envelope + encoder

- [x] write tests first: each record type (`log`/`start`/`exit`/`drop`)
      marshals to exactly one `\n`-terminated line with the expected fields;
      `exit` carries `code` and optional `signal`; timestamps come from the
      injected clock; embedded newlines in a captured line do not break framing
      (each input line → one record); every record carries the configured
      `tags` (and no `cid`/`host`).
- [x] add `internal/supervisor/event.go`: the envelope struct + a constructor
      per record type and a writer that emits one framed line per record. Clock
      injected as `func() time.Time`.
- [x] run tests — pass before Task 3.

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
      frames enqueued reach an in-process listener in FIFO order; when the
      listener is absent then appears, buffered frames flush and a `drop` record
      is emitted iff overflow occurred while disconnected; a broken connection
      triggers reconnect with bounded backoff; `Flush(ctx)` drains within a
      deadline and returns when the buffer is empty or the deadline passes.
- [ ] add `internal/supervisor/shipper.go`: a goroutine that dials
      `log_cid:log_port` via an injected `vsockconn.Dialer`, drains the ring
      buffer to the conn, reconnects with bounded backoff on error, prepends a
      `drop` record after a gap, and exposes `Flush(ctx)` for shutdown.
- [ ] run tests (`-race`) — pass before Task 5.

### Task 5: Supervisor slog handler (own logs → buffer)

- [ ] write tests first: an `slog.Logger` built on the handler produces, per
      call, one `log` record with `src:"supervisor"`, the right level/msg/attrs,
      and the configured `tags`, enqueued to the buffer; the same line is also
      written to the stderr mirror; logging never blocks when the buffer is full
      (drop-oldest still applies).
- [ ] add `internal/supervisor/sloghandler.go`: a `slog.Handler` that formats
      records into the NDJSON envelope (`type:"log"`, `src:"supervisor"`) and
      enqueues them, plus a stderr mirror writer.
- [ ] run tests — pass before Task 6.

### Task 6: Process manager (spawn, capture, role/restart/failure policy)

- [ ] write tests first using the helper-process pattern: a child that writes to
      stdout and stderr and exits with a chosen code produces the expected
      `start`, interleaved `log` (correct `src`/`pid`/`stream`), and `exit`
      (correct `code`) records; `restart: on-failure` does not restart exit 0
      but restarts a non-zero exit; `restart: always` restarts exit 0;
      `restart: no` never restarts; the **windowed** cap gives up only after
      `max_restarts` within `restart_window` and resets after a healthy run
      past the window; **`on_failure: terminate` give-up shuts everything down,
      exit non-zero**; **`on_failure: continue` abandons just that process and
      others keep running**; a **task** exit 0 marks it done and when **all
      tasks** are done the supervisor shuts down sidecars and exits 0; a sidecar
      exit 0 under `on-failure`/`no` is tolerated; **once shutdown has begun,
      nothing is restarted** (regression for decision 3); SIGTERM to the
      supervisor drains pipes, records `exit`, flushes, returns.
- [ ] add `internal/supervisor/manager.go`: spawn each process with
      `StdoutPipe`/`StderrPipe`, per-stream line readers feeding the ring buffer
      with `src`/`pid`/`stream`; a `waitpid` loop emitting `start`/`exit` and
      applying decision 4 (restart decision → windowed budget → give-up →
      on_failure; task-completion tracking); a shutdown path that SIGTERMs the
      remaining children, escalates to SIGKILL after a per-child timeout, sets
      the shutting-down flag that suspends restarts, and records the resolved
      supervisor exit code (propagate the failing child's code / 128+signal).
      Log every decision via the supervisor logger (Task 5).
- [ ] run tests (`-race`) — pass before Task 7.

### Task 7: `cmd/supervisor` main wiring

- [ ] write tests first: an end-to-end test that loads a small config, runs the
      supervisor against helper processes with a loopback log listener, and
      asserts the full NDJSON stream (lifecycle + child logs + supervisor's own
      logs) is received; all tasks exiting 0 brings sidecars down with
      supervisor exit 0; a `terminate` give-up exits non-zero; SIGTERM shuts
      everything down within grace, exit 0.
- [ ] add `cmd/supervisor/main.go`: flag/config parsing, build the clock/dialer
      (prod `NewVsockDialer`, loopback for tests), wire config → buffer →
      shipper → slog handler → manager, install the signal handler, run, flush
      on shutdown, and propagate the resolved exit code. Mirror
      `cmd/vsockd/main.go` structure.
- [ ] run `go test ./cmd/supervisor/... ./internal/supervisor/...` — pass
      before Task 8.

### Task 8: Verify acceptance criteria

- [ ] verify every Key Design Decision is implemented (own vsock conn, no signal
      forwarding, restart-suspended-on-shutdown, role/restart/on_failure policy
      with windowed counting, task-completion + exit codes, own-logs-shipped,
      NDJSON record types, frame-granular drop with accounting,
      flush-within-grace).
- [ ] verify edge cases: listener never appears (producers still run, buffer
      caps, no deadlock); a child that ignores SIGTERM is SIGKILLed after the
      per-child timeout; vsockd-crash output is captured; config-load failure is
      logged to stderr before the buffer exists; `continue` abandonment runs the
      remaining tasks to completion and reports non-zero.
- [ ] run full suite `go test ./...` and with `-race`.
- [ ] run the linter — fix all issues.
- [ ] verify coverage meets the project standard (80%+) for new packages.

### Task 9: Documentation and example config

- [ ] add `examples/supervisor.yaml` (a `task` app + a vsockd `sidecar`,
      log_port matching the `log_relay` example) documenting `role`/`restart`/
      `max_restarts`/`restart_window`/`on_failure` and the shutdown triggers.
- [ ] update `README.md` to describe the supervisor's role and the enclave log
      path (supervisor → vsock → host `log_relay`), including that it ships its
      own logs.
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
    - name: app
      role: task
      command: /usr/local/bin/node
      args: ["server.js"]
      restart: on-failure   # no | on-failure | always (default on-failure)
      max_restarts: 3
      restart_window: 60s
      on_failure: terminate # terminate | continue (default terminate)
    - name: vsockd
      role: sidecar
      command: /usr/local/bin/vsockd
      args: ["-config", "/etc/vsockd/vsockd.yaml"]
      restart: always       # keep transport up for the lifetime of the tasks
      max_restarts: 5
      restart_window: 60s
      on_failure: terminate
  ```
- **Per-exit decision** (while not already shutting down):
  ```
  warrants restart? = always | (on-failure && code!=0) | (no => false)
    └ yes & within windowed budget → restart (log attempt N)
    └ yes & budget exhausted       → give up → on_failure
    └ no:
        code == 0 → task: mark DONE (success); sidecar: tolerate
        code != 0 → terminal failure → on_failure
  on_failure: terminate → graceful shutdown all, exit propagates failing code
  on_failure: continue  → abandon this process; keep the rest running
  all tasks terminal → shut down sidecars, exit 0 (all DONE) or non-zero (any abandoned)
  ```
- **NDJSON records** (as emitted by the supervisor, before host enrichment adds
  `cid`/`host`):
  ```json
  {"ts":"...","src":"app","pid":42,"stream":"stdout","tags":{"service":"abc","version":"1.1.2"},"type":"log","msg":"..."}
  {"ts":"...","src":"supervisor","pid":1,"stream":"-","tags":{"service":"abc","version":"1.1.2"},"type":"log","msg":"restarting vsockd (2/5 in 60s)"}
  {"ts":"...","src":"app","pid":42,"stream":"-","tags":{"service":"abc","version":"1.1.2"},"type":"start"}
  {"ts":"...","src":"vsockd","pid":12,"stream":"-","tags":{"service":"abc","version":"1.1.2"},"type":"exit","code":1,"signal":null}
  {"ts":"...","src":"supervisor","pid":1,"stream":"-","tags":{"service":"abc","version":"1.1.2"},"type":"drop","count":128}
  ```
- **Process group / signals**: children inherit the supervisor's process group
  (no `Setpgid`); `tini -g` fans out external signals. The supervisor signals
  children by PID only when it initiates a graceful shutdown. tini reaps
  reparented grandchildren.
- **Shutdown order**: trigger (signal / all-tasks-done / terminate give-up) →
  set shutting-down (suspends restart) → SIGTERM remaining children → drain
  pipes to EOF → `waitpid` for `exit` records → `Flush(ctx)` within grace →
  exit with the resolved code.

## Open Items To Confirm

- **Default `restart` per role**: a single global default (`on-failure`) is used
  for both roles; sidecars that must never stop set `restart: always`
  explicitly. Confirm vs. role-specific defaults (e.g. sidecars default
  `always`).
- **Buffer budget**: defaults (8 MiB / 10k records) are placeholders.
- **Backoff**: bounded exponential (e.g. 100ms → 5s); exact values in Task 4.

## Post-Completion

*External to this repo — informational only.*

- **Node app repo**: replace the `entrypoint.eif.sh` responsibility of starting
  vsockd + node with the supervisor as `tini -g`'s child; update
  `Dockerfile.eif` to install the supervisor binary and point it at
  `supervisor.yaml`.
- **Host side**: configure vsockd's `log_relay` on the matching port (plan 1).
- **Real-enclave verification**: confirm app + vsockd + supervisor logs land
  host-side and survive a host-listener restart (buffer + reconnect + drop
  accounting), and that vsockd crash output is captured.
- **Delivery agent**: point CloudWatch agent / vector / fluent-bit (or
  journald) at the host sink for routing, ingest-timestamping, and retention.
