# Task 033 — SSH Executor and Streamer

- **Milestone:** M2
- **Depends on:** 027, 032
- **Parallel group:** — (serial)
- **Status:** done

## Goal
`transport.Executor` and `transport.Streamer` over the pool.

## Context
BUILD-PLAN §4.3, §7. SPEC §7.1 (timeouts, output cap, sudo).

## Files
- create `internal/transport/ssh/exec.go`, `internal/transport/ssh/exec_test.go`
- create `internal/transport/ssh/stream.go`, `internal/transport/ssh/stream_test.go`
- create `internal/transport/ssh/capwriter.go`

## Requirements
1. `type Transport struct` built by `func New(pool *Pool, o Options) *Transport` with
   `Options { ExecTimeout time.Duration; MaxOutput int; Log *slog.Logger }`. `Name()` = `"ssh"`.
2. `Exec`: lease a client (errors classified by 031 are returned with the host set); `client.NewSession()`; a
   channel-open failure here means the session limit of the server was hit → retry once after 200 ms, then
   `errs.Unreachable` "session rejected (MaxSessions?)"; set stdin from `cmd.Stdin`; stdout/stderr into
   `capWriter`s of `MaxOutput`; `session.Run(shq.Wrap(cmd.Script, cmd.Sudo))` in a goroutine; on ctx done or
   timeout: `session.Signal(ssh.SIGKILL)` (best effort), `session.Close()`, return `errs.Timeout`/`errs.Canceled`
   with whatever output was collected.
   - exit status: `*ssh.ExitError` → `RC = ExitStatus()`, nil error; `*ssh.ExitMissingError` or other →
     `errs.Unreachable` (connection lost) and `pool.Forget(host)`.
   - after a completed run call `shq.Classify(rc, stderr, cmd.Sudo)`; a `Sudo` classification is returned as the
     error (with the result); `NotAvailable` is **not** returned as an error here (batch scripts handle rc 127
     per section) — leave RC for the caller.
   - fill `Duration`, `Via` (from the lease), `Truncated`.
3. `capWriter`: accepts all writes, keeps the first `limit` bytes, reports `Truncated()`; never returns an error
   (so the remote command is not killed by a full buffer).
4. `Start` (Streamer): same setup, `session.Start`; a goroutine reads stdout with a `bufio.Reader` (lines up to
   64 KiB; longer lines are split) into a buffered channel (256); `Wait()` returns the result (stderr tail up to
   8 KiB); `Close()` kills the session and releases the lease; ctx cancel does the same. The lease is held for
   the stream's lifetime.

## Out of scope
PTY shells (034). Fan-out (036).

## Acceptance
```
go test -race ./internal/transport/ssh/ -run 'Exec|Stream|CapWriter' -v
# TestExecStdoutStderrRC, TestExecNonZeroIsNotError, TestExecSudoWrapsCommand (handler sees "sudo -n -- sh -c"),
# TestExecSudoPasswordRequired (handler prints "sudo: a password is required", rc 1 → errs.Sudo),
# TestExecTimeout (handler sleeps → errs.Timeout near the timeout), TestExecCancel,
# TestExecTruncates (5 MiB output, MaxOutput 1 MiB → Truncated, command still exits 0),
# TestExecThroughBastion, TestExecServerDiesMidCommand (errs.Unreachable, next Exec reconnects),
# TestStreamLines, TestStreamCloseStopsRemote, TestStreamLongLineSplit
```

## Pitfalls
- Always `lease.Release()` (defer) — a leaked lease blocks the host forever.
- `session.Run` returns only after stdout/stderr are drained; with a timeout you must close the session to
  unblock it.
- Do not request a PTY for exec; sudo output and line endings change with a PTY.
