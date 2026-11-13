# Task 026 — Transport types and scripted fake executor

- **Milestone:** M2
- **Depends on:** 005
- **Parallel group:** M2-A (with 027, 028, 037)
- **Status:** todo

## Goal
The transport contract and a fake that every backend test will use instead of SSH.

## Context
BUILD-PLAN §4.3, §9.

## Files
- create `internal/transport/transport.go`
- create `internal/transport/transporttest/fake.go`, `internal/transport/transporttest/fake_test.go`

## Requirements
1. `transport.go`: types and interfaces verbatim from BUILD-PLAN §4.3.
2. `transporttest.Fake` implements `Executor`, `Streamer` and `ShellOpener`.
   - `New() *Fake`; `Name()` = `"fake"`.
   - `On(match string, r Reply) *Fake` — the first registered rule whose `match` is a substring of
     `cmd.Script` wins. `OnHost(host, match, r)` is host-specific and takes precedence.
     `type Reply struct { Stdout, Stderr string; RC int; Err error; Delay time.Duration; Lines []string }`.
   - `OnFunc(func(host resource.HostID, cmd transport.Command) (Reply, bool))` for dynamic replies.
   - No rule → `RC 127`, stderr `sh: <first word>: command not found`.
   - `Exec` honours `ctx` and `cmd.Timeout` during `Delay` (returns `errs.Timeout` / `errs.Canceled`), sets
     `Host`, `Duration`, `Via "fake"`, and applies `MaxOutput` truncation with `Truncated = true`.
   - `Start` emits `Reply.Lines` one by one with `Delay` between them, then closes; `Close()` stops early.
   - `OpenShell` returns an in-memory `Shell`: bytes written are echoed back; `Resize` is recorded; the test can
     inject output with `fake.ShellOutput(host, []byte)`.
   - Records calls: `Calls() []Call{Host, Command}`; safe for concurrent use; `InFlightMax() int` reports the
     highest number of concurrent `Exec` calls seen (for fan-out limit tests).

## Out of scope
Real SSH. Sudo semantics (the fake just records `cmd.Sudo`).

## Acceptance
```
go test -race ./internal/transport/... -v
# TestFakeRuleOrder, TestFakeHostRuleWins, TestFakeNoRule127, TestFakeTimeout, TestFakeCancel,
# TestFakeTruncates, TestFakeStreamLinesAndClose, TestFakeShellEcho, TestFakeInFlightMax
```

## Pitfalls
- `Exec` returns `(ExecResult, error)`: a non-zero `RC` comes back with a nil error.
- `Stream.Lines()` must be closed exactly once, also when `Close()` races with natural end (use `sync.Once`).
