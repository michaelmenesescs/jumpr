# Task 126 — Crash safety, logging and leak audit

- **Milestone:** M12
- **Depends on:** 083, 123
- **Parallel group:** — (serial)
- **Status:** done

## Goal
SPEC §15 "Crash safety" and "Logs", verified rather than assumed.

## Files
- modify `internal/app/crash.go`, `internal/app/log.go`, tests in `internal/app/`
- create `internal/app/audit_test.go`

## Requirements
1. A panic in any goroutine jumper starts (fan-out worker, poller, pane reader, bus step) is recovered at that
   goroutine's top, logged with a stack, converted to an `errs.Internal` result where one is expected, and
   surfaced as a flash; it never kills the session. Add a small `safe.Go(log, name, fn)` helper in a new leaf
   package `internal/safe` (create `internal/safe/safe.go` and its test) and use it for every `go` statement
   outside tests (the audit test enforces this by
   AST: bare `go` statements under `internal/` are listed and must be on an explicit allowlist).
2. A panic in the Bubble Tea goroutine: terminal restored (standalone) or client receives `EXIT{error}` (server
   mode), crash report written, session layout was autosaved ≤ 1 s before.
3. Debug log: every remote command logs host, a redacted command line, duration, rc and byte counts — never
   output, never stdin. A test runs a scenario with secrets in commands and outputs and greps the log file.
4. Log rotation: `jumper.log` and `server.log` rotate at 10 MiB, keep 3.
5. `jumper kill-server` and SIGTERM shut down within 3 s with shells closed and rollouts aborted between batches
   (recorded as aborted).

## Acceptance
```
go test -race ./internal/app/ ./internal/safe/ -run 'Panic|Audit|Log|Shutdown' -v
make check && make e2e
```

## Pitfalls
- Recovering a panic must not hide it: it is always logged and counted (`:health` shows `internal errors: N`).
