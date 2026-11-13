# Task 123 — Scale test: 2,000 hosts

- **Milestone:** M12
- **Depends on:** 076
- **Parallel group:** M12-A (with 124, 125)
- **Status:** todo

## Goal
Enforce SPEC §15 targets with a fake transport.

## Files
- create `internal/scale/scale_test.go` (build tag `scale`), modify `Makefile` (`scale-test`)

## Requirements
1. 2,000 fake hosts, latency 20–400 ms (deterministic pseudo-random per host), 2 % timeouts, 1 % auth errors,
   0.5 % huge outputs (2 MiB).
2. Assertions: probe over all hosts completes < 60 s at concurrency 64 with first results < 3 s; never more than
   64 in flight; while the fan-out runs, a root model driven in-process handles 200 key messages with p99
   `Update`+`View` < 16 ms; UI messages from the fan-out ≤ 25 per second; heap after the hosts list with overview
   data loaded < 300 MB (`runtime.ReadMemStats` after GC); no goroutines left 2 s after cancel (count compared to
   baseline ± 5).
3. A rollout of `true` over 2,000 hosts through the bus (canary, 40 batches) finishes and records one grouped
   entry.
4. Report numbers with `t.Logf` and copy them into `docs/HANDOFF.md`.

## Acceptance
```
make scale-test
```

## Pitfalls
- Fix whatever this finds in the smallest way; if a fix needs a contract change, stop and report.
