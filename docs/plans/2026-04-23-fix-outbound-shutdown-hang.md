# Fix outbound shutdown hang + add systemd unit

## Overview

vsockd hangs indefinitely after the first SIGINT/SIGTERM when any outbound
vsock listener is configured. The shutdown log emits once and then the
process spins forever, emitting `outbound accept error ... retry_in=1s` at
1 Hz. Subsequent Ctrl-C presses are swallowed (they queue in `signal.Notify`'s
buffered channel while the main goroutine is blocked inside `a.Shutdown`),
and even `kill -QUIT` is reported to have no effect — only `kill -KILL`
terminates the process.

### Root cause

In `internal/outbound/server.go:530-585`, the accept loop exits only when
`ctx.Err() != nil || errors.Is(err, net.ErrClosed)`. During a normal
shutdown neither branch becomes true:

1. `cmd/vsockd/main.go:104` creates `serveCtx` but **never cancels it** on
   shutdown — an explicit design choice documented in the comment "Shutdown
   drives listener teardown and waits for drain."
2. `github.com/mdlayher/vsock` v1.2.1's `opError` rewraps every closed-FD
   error (EBADF, `os.ErrClosed`, poller "use of closed file") as a fresh
   `errors.New("use of closed network connection")` — the string matches
   `net.ErrClosed`'s message but the value does not. `errors.Is(err,
   net.ErrClosed)` therefore returns false.

So after `ln.Close()`, every `Accept()` returns a non-`ErrClosed` error, the
loop treats each one as transient, backs off up to 1 s, and repeats forever.
`Server.Shutdown` blocks on `s.wg.Wait()` waiting for those goroutines. The
30 s grace branch also does `<-done`, which never completes.

The inbound side is unaffected: `net.TCPListener.Accept` returns the real
`net.ErrClosed` sentinel, so `errors.Is` matches and those loops exit
cleanly. The fix is scoped to outbound.

A second latent consequence of the same bug: when SIGHUP removes a listener
during reload, its goroutine leaks in exactly the same way (serveCtx stays
live during reload, so only `errors.Is` could save it — and it doesn't).
The per-listener fix resolves both scenarios with one mechanism.

### Fix summary

Give each outbound listener a `sync.Once`-guarded `done` channel that
`close()` closes. The accept loop treats `done` as the authoritative "this
listener is intentionally torn down" signal, independent of the error that
Accept happens to return. Also watch `done` inside the backoff select so an
in-flight retry timer does not delay exit by up to a second.

### Out of scope

- Inbound server: TCP listeners return canonical `net.ErrClosed`; no fix
  needed.
- Metrics server: uses `http.Server.Shutdown` which closes the listener and
  handles Accept errors internally.
- Upstream fix in `mdlayher/vsock`: would be the ideal long-term resolution
  but does not unblock this repo.

## Context (from discovery)

- Files/components involved:
  - `internal/outbound/server.go` — listener struct, `close()`, `run()`
  - `internal/outbound/server_test.go` — new regression test
  - `examples/vsockd.service` — new systemd unit
- Related patterns found:
  - `internal/inbound/server.go:523-555` — symmetric accept loop for TCP
    listeners; does not need fixing but mirrors the structure.
  - `internal/outbound/server_test.go:758-784` — existing shutdown test
    uses loopback TCP listener, which masks the bug.
  - `internal/vsockconn/vsockconn.go` — Listener interface the test double
    must satisfy.
- Dependencies identified:
  - `github.com/mdlayher/vsock` v1.2.1 — upstream behaviour confirmed in
    `~/go/pkg/mod/github.com/mdlayher/vsock@v1.2.1/vsock.go:400-406`.

## Development Approach

- **Testing approach**: TDD (write the failing regression test first, then
  apply the fix).
- Complete each task fully before moving to the next.
- Make small, focused changes.
- Every task includes new/updated tests (the fix task re-uses the test
  written in the prior task).
- All tests must pass before the next task begins.
- Keep backward compatibility — the `listener` struct is internal, but the
  change must not affect the Apply / PrepareApply / CommitApply semantics.

## Testing Strategy

- **Unit tests**: required. A new regression test under
  `internal/outbound/` that injects a fake `vsockconn.Listener` whose
  `Accept` returns a non-`ErrClosed` error (plain `errors.New("use of
  closed network connection")`) after `Close`, then asserts `Shutdown`
  returns within a tight bound.
- **E2E tests**: the project has no UI e2e suite; skip.
- Project test command: `go test ./...`.

## Progress Tracking

- Mark completed items with `[x]` when done.
- New tasks discovered during implementation get `➕` prefix.
- Blockers get `⚠️` prefix.
- Update this file if scope changes.

## What Goes Where

- **Implementation Steps**: code changes, tests, the systemd unit file —
  all live in this repo.
- **Post-Completion**: manual verification on a real Nitro host
  (`systemctl start vsockd && systemctl stop vsockd` timing, `journalctl`
  inspection of the shutdown path) — cannot be automated here.

## Implementation Steps

### Task 1: Add failing regression test for vsock-style close error

- [x] Add `TestServerShutdownHandlesNonErrClosedAcceptError` to
      `internal/outbound/server_test.go`. The test wires a fake
      `vsockconn.Listener` whose `Accept` returns a plain
      `errors.New("use of closed network connection")` after `Close` —
      mirroring `mdlayher/vsock`'s `opError` rewrap.
- [x] Test starts the server with this fake listener, then calls
      `Shutdown` with a 500 ms timeout and asserts it returns `nil` well
      inside that bound.
- [x] Factor the fake into a small helper type in the same test file; keep
      it minimal (Accept, Close, Addr, plus PeerCID on the Conn).
- [x] Run `go test ./internal/outbound/ -run
      TestServerShutdownHandlesNonErrClosedAcceptError` — must **fail**
      (hang up to the test timeout) before Task 2. That failure is the
      evidence the fix is needed.
- [x] Confirm the rest of `go test ./internal/outbound/...` still passes
      (no regressions introduced by the fake).

### Task 2: Add per-listener done signal in outbound accept loop

- [x] Add `closeOnce sync.Once` and `done chan struct{}` fields to the
      `listener` struct in `internal/outbound/server.go`. Document why
      this is needed (the `mdlayher/vsock` rewrap behaviour) in the struct
      comment.
- [x] Initialise `done: make(chan struct{})` in both `newHTTPListener` and
      `newVsockToTCPListener`.
- [x] Replace the body of `listener.close()` with a `closeOnce.Do` wrapper
      that closes `l.done` and then closes `l.ln` (keep the `l.ln != nil`
      guard). Double-close of `close()` becomes safe.
- [x] In `listener.run`, after Accept returns a non-nil error, first
      `select { case <-l.done: return; default: }` before the existing
      `ctx.Err()` / `errors.Is` check. Keep the existing check as
      defence-in-depth.
- [x] In the backoff `select` (same function), add `case <-l.done:
      return` so a pending retry timer does not delay exit by up to a
      second.
- [x] Run `go test ./internal/outbound/ -run
      TestServerShutdownHandlesNonErrClosedAcceptError` — must **pass**.
- [x] Run `go test ./...` — all packages must pass.
- [x] Run `go vet ./...` and the project linter if present; fix any
      warnings.

### Task 3: Add example systemd unit

- [x] Create `examples/vsockd.service` with:
  - `Type=simple`, `ExecStart=/usr/local/bin/vsockd -config
    /etc/vsockd/vsockd.yaml`, `ExecReload=/bin/kill -HUP $MAINPID`.
  - `TimeoutStopSec=35s` with a comment explaining it must be ≥
    `shutdown_grace` from the yaml (default 30 s) so systemd does not
    SIGKILL mid-drain. Note that with the bug fixed this window is the
    max in-flight-connection drain window, not a safety net against the
    spin-loop.
  - `Restart=on-failure` with `RestartSec=5s`.
  - `User=vsockd` / `Group=vsockd` with a comment on how to create the
    user (`useradd --system --no-create-home --shell /usr/sbin/nologin
    vsockd`) and note that the user must have read access to
    `/dev/vsock` on the host.
  - `AmbientCapabilities=CAP_NET_BIND_SERVICE` +
    `CapabilityBoundingSet=CAP_NET_BIND_SERVICE` for inbound binds on
    ports < 1024.
  - Hardening: `NoNewPrivileges=true`, `PrivateTmp=true`,
    `ProtectSystem=strict`, `ProtectHome=true`,
    `ProtectKernelTunables=true`, `ProtectKernelModules=true`,
    `ProtectControlGroups=true`, `ReadOnlyPaths=/etc/vsockd`,
    `RestrictAddressFamilies=AF_INET AF_INET6 AF_VSOCK AF_UNIX`,
    `RestrictNamespaces=true`, `RestrictRealtime=true`,
    `RestrictSUIDSGID=true`, `LockPersonality=true`,
    `SystemCallArchitectures=native`,
    `SystemCallFilter=@system-service`,
    `SystemCallFilter=~@privileged @resources`.
  - `[Install] WantedBy=multi-user.target`.
  - Comment at top linking to `examples/vsockd.yaml` and noting the unit
    is an example — operators should adapt paths, user, and hardening to
    their environment.
- [x] No tests (static config file). Skip the "run tests" checkbox for
      this task.

### Task 4: Verify acceptance criteria

- [ ] `go test ./...` passes.
- [ ] `go vet ./...` clean.
- [ ] Manual read of the diff: the `closeOnce` / `done` fields are
      documented, the accept loop changes are minimal, and no unrelated
      code moved.
- [ ] Confirm via code inspection that Reload (`PrepareApply` →
      `CommitApply`) still works: removed listeners get `close()`, their
      goroutines now exit via `l.done` without waiting for a
      never-matching error. New listeners get a fresh `done` channel at
      construction.
- [ ] Confirm the systemd unit's `TimeoutStopSec` ≥
      `examples/vsockd.yaml`'s `shutdown_grace`.
- [ ] Build a local binary and sanity-check that `-version` still works:
      `go run ./cmd/vsockd -version`.

## Technical Details

### Listener struct changes (`internal/outbound/server.go`)

```go
type listener struct {
    port   uint32
    mode   string
    server *Server

    ln vsockconn.Listener

    // done is closed exactly once by close() and is the authoritative
    // "this listener is intentionally torn down" signal for the accept
    // loop. See the Fix summary in
    // docs/plans/2026-04-23-fix-outbound-shutdown-hang.md for why this
    // cannot be derived from Accept's error alone.
    closeOnce sync.Once
    done      chan struct{}

    matchers atomic.Pointer[map[uint32]*allowlist.Matcher]
    upstream atomic.Pointer[string]
}
```

### Accept loop changes

```go
func (l *listener) run(ctx context.Context) {
    var tempDelay time.Duration
    for {
        c, err := l.ln.Accept()
        if err != nil {
            select {
            case <-l.done:
                return
            default:
            }
            if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
                return
            }
            if tempDelay == 0 {
                tempDelay = 5 * time.Millisecond
            } else {
                tempDelay *= 2
            }
            if tempDelay > time.Second {
                tempDelay = time.Second
            }
            l.server.logger.Warn("outbound accept error",
                "port", l.port, "err", err, "retry_in", tempDelay)
            select {
            case <-time.After(tempDelay):
            case <-ctx.Done():
                return
            case <-l.done:
                return
            }
            continue
        }
        // ...unchanged dispatch
    }
}
```

### close() change

```go
func (l *listener) close() {
    l.closeOnce.Do(func() {
        close(l.done)
        if l.ln != nil {
            _ = l.ln.Close()
        }
    })
}
```

### Regression test sketch

```go
type hangingListener struct {
    accept chan error
    closed atomic.Bool
    addr   net.Addr
}

func (h *hangingListener) Accept() (vsockconn.Conn, error) {
    err := <-h.accept
    return nil, err
}

func (h *hangingListener) Close() error {
    if h.closed.CompareAndSwap(false, true) {
        // Mimic mdlayher/vsock v1.2.1 opError: the message matches
        // net.ErrClosed but the value does not.
        h.accept <- errors.New("use of closed network connection")
        close(h.accept)
    }
    return nil
}

func (h *hangingListener) Addr() net.Addr { return h.addr }
```

Server constructed with a ListenFunc that returns `&hangingListener{...}`.
Test calls `Start`, then `Shutdown` with a 500 ms budget, and fails if
`Shutdown` does not return promptly.

## Post-Completion

**Manual verification** (on a Nitro host or any Linux box with
`/dev/vsock`):

- Start `vsockd` with a config that has at least one `outbound` or
  `vsock_to_tcp` listener.
- `kill -TERM <pid>` — process should exit within the configured
  `shutdown_grace` window, with no `outbound accept error` spam in the
  log between the `vsockd shutting down` line and process exit.
- `kill -HUP <pid>` after a config reload that removes a listener —
  confirm (via `/proc/<pid>/task/` count or runtime profiling) that the
  removed listener's goroutine is gone.
- `systemctl start vsockd && systemctl stop vsockd` with the new unit
  file — confirm `systemd` observes a clean exit inside `TimeoutStopSec`.

**External system updates**:

- None. This repo owns the fix and the example unit end-to-end.
