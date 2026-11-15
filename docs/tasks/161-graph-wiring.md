# Task 161 — Graph in the session server: scheduler, `GraphRequestMsg`, `Env.Graph`

- **Milestone:** M15
- **Depends on:** 160, 083
- **Parallel group:** —
- **Status:** todo

## Context
BUILD-PLAN §14.4, §6.1 (detach safety), §8 (concurrency). SPEC §23.1 rules 3–4.

## Goal
One graph per session server (and per standalone/demo process), filled on demand by a scheduler, readable by every
view through `tui.Env`.

## Files
- create `internal/app/graph.go`, `internal/app/graph_test.go`
- create `internal/tui/graph.go` (`GraphReader`, `GraphRequester`, `GraphRequestMsg`)
- modify `internal/tui/env.go` (add fields `Graph GraphReader`, `GraphReq GraphRequester`; nothing else)
- modify `internal/app/core.go` (construct graph + scheduler; demo mode calls `graph/fake.Populate`)

## Requirements
1. `GraphReader` = `Node`, `Out`, `In`, `Walk`, `Version`, `Truncated`, `HostErrs(scope graph.Scope) map[resource.HostID]error`.
2. `GraphRequester.Request(scope graph.Scope)` is non-blocking: it enqueues; collectors run in goroutines under
   `safe.Go`, at most `ssh.concurrency` hosts in parallel, singleflight per host+layer, skipped when the same
   host+layer finished less than `graph.refresh` ago.
3. Collectors are registered with the scheduler by layer name (a map passed to the constructor, no `init()`); this
   task registers none except the demo fake. 162/164/165/166 add theirs in their own `internal/app` lines.
4. After each collection, `Prune(scope, now - graph.prune_after)`.
5. Per-host errors from `Sink.HostErr` are kept per layer and returned by `HostErrs`, cleared on the next success.
6. Closing the core cancels running collections within 2 s (test with a blocking fake collector).

## Acceptance
```
go test -race ./internal/app/ -run 'Graph' -v
make check
```

## Pitfalls
- `Request` is called from `Update`; it must not lock anything held across I/O (AGENTS rule 8).
