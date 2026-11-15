# Task 160 — `internal/graph`: nodes, edges, evidence, walks; fake collector for `--demo`

- **Milestone:** M15
- **Depends on:** 005, 007
- **Parallel group:** —
- **Status:** todo

## Context
SPEC §23.1. BUILD-PLAN §14.1, §14.2 (the contract: copy it verbatim), §14.6 (`graph:` keys).

## Goal
The in-memory graph every v0.3 feature stands on, plus config keys and a deterministic fake world so later views
work in `--demo` with no SSH.

## Files
- create `internal/graph/graph.go`, `internal/graph/walk.go`, `internal/graph/graph_test.go`, `internal/graph/walk_test.go`
- create `internal/graph/fake/fake.go`, `internal/graph/fake/fake_test.go`
- modify `internal/config/config.go`, `internal/config/config_test.go` (`graph:` section only)
- add `internal/config/testdata/full.yaml` keys for `graph:`

## Requirements
1. Types and methods exactly as §14.2. `ID(kind, scope, name)` escapes `/` and `:` inside `name` with `%2F`/`%3A`
   so IDs stay parseable.
2. `UpsertNode` merges attrs key by key, keeps the newest `Seen`, and returns false (setting `Truncated`) when a new
   node would exceed `MaxNodes`. `UpsertEdge` requires both ends, merges evidence newest first, keeps ≤ 8, and
   respects `MaxEdges` the same way.
3. `Out`/`In` filter by kinds (none = all) and return sorted slices that callers may modify (copies).
4. `Walk` is breadth first, visits each node once, honours depth (0 = only start), and returns steps in visit order
   with `Via` set. Cycles (units that `Wants` each other) terminate.
5. `Prune(scope, olderThan)` removes nodes whose `Host` is in `scope.Hosts` (or all hosts when empty) and whose
   `Seen` is older, plus their edges; returns the count.
6. `Version()` increments on every successful change and never on a no-op upsert (same attrs, same evidence head).
7. `graph/fake`: `Populate(g *graph.Graph, now func() time.Time)` builds, from the fake world of task 007, for each
   of the first 12 hosts: `-.slice` → `system.slice`/`user.slice` → ~25 units (nginx, haproxy, sshd, postgres on
   db hosts, …) → processes → listeners, with `web-ewr-007` carrying the failed `nginx.service` and
   `web-ewr-012` the active `haproxy.service` holding `:443` (the mockup's incident); `connects` edges from web
   nginx processes to db postgres listeners. Deterministic (same input → same graph).
8. Config `graph.max_nodes`, `graph.max_edges`, `graph.refresh`, `graph.prune_after` with defaults and `Validate()`
   rules from §14.6.
9. Concurrency: `go test -race` with 16 goroutines upserting and walking for 200 ms must pass.

## Acceptance
```
go test -race ./internal/graph/... ./internal/config/ -v
make check
```

## Out of scope
Collectors over SSH (162, 164–166), wiring into the app (161), any view.

## Pitfalls
- No package-level state; limits and `now` are passed to `New`.
- Evidence excerpts are redacted by the collector before upsert; the graph does not import `internal/redact`.
