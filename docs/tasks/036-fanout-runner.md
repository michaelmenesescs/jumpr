# Task 036 — Fan-out runner

- **Milestone:** M2
- **Depends on:** 026
- **Parallel group:** M2-B (with 029, 030, 038)
- **Status:** todo

## Goal
Run one command on many hosts with bounded concurrency and stream the results.

## Context
BUILD-PLAN §4.6, §8. SPEC §15 (1,000-host probe).

## Files
- create `internal/fanout/fanout.go`, `internal/fanout/fanout_test.go`

## Requirements
1. `Run` exactly as in BUILD-PLAN §4.6. Implementation: a results channel with buffer 64; a feeder goroutine
   that acquires a slot (`semaphore.Weighted` or a buffered channel) per host and starts a worker; workers call
   `ex.Exec(ctx, host, cmd)` and send one `HostResult`.
2. Cancellation: once `ctx` is done, hosts not yet started produce `HostResult{Host, Err:
   errs.New(errs.Canceled, "fanout", "canceled")}` without calling `Exec`; in-flight workers finish through their
   own ctx handling. The channel is closed after exactly `len(hosts)` results.
3. Duplicate hosts in the input are executed once (first occurrence kept).
4. A panic in an executor is recovered and reported as `errs.Internal` for that host.
5. `func Collect(ch <-chan HostResult) []HostResult` (helper for tests and small callers).

## Out of scope
Grouping (037). Retries. Per-bastion limits (the pool does that).

## Acceptance
```
go test -race ./internal/fanout/ -v
# with transporttest.Fake: TestAllHostsReportOnce (2000 hosts), TestConcurrencyBound (Concurrency 8, Delay 5ms →
# fake.InFlightMax() <= 8 and > 1), TestCancelMidway (every host still yields one result; total time bounded),
# TestErrorsDoNotStopOthers, TestDedup, TestPanicRecovered, TestZeroHostsClosesImmediately
```

## Pitfalls
- The contract is: the consumer always drains the channel until it is closed, and `Run` always closes it.
  Workers therefore send unconditionally. Say so in the doc comment; every test drains.
- Do not spawn 2,000 goroutines that all wait on the semaphore; acquire in the feeder, then spawn.
