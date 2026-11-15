# Task 162 — systemd collector: slices, targets, units, processes

- **Milestone:** M15
- **Depends on:** 161, 041, 090
- **Parallel group:** M15-A (with 165)
- **Status:** todo

## Context
SPEC §23.2 (systemd row), §23.3. BUILD-PLAN §14.3, §14.5 (the probe). Existing parsers: units (041),
dependencies (`internal/backend/linux/parse_deps_timers.go`), cgroups (090).

## Goal
For each host in scope, one sectioned SSH batch that turns into slice → unit → process nodes with `contains`,
`requires`, `wants`, `triggers` and `runs` edges.

## Files
- create `internal/graph/collect/systemd.go`, `internal/graph/collect/systemd_test.go`
- create `internal/backend/linux/parse_show_all.go`, `internal/backend/linux/parse_show_all_test.go`
- add `testdata/linux/systemd-graph/{rocky9,ubuntu2204,ubuntu2404}/…` (captured from the lab, sanitized)
- modify `internal/app/graph.go` (register layer `systemd`)

## Requirements
1. The batch exactly as §14.5, built with `shq`, `LC_ALL=C`, with the `systemctl show '*'` fallback in batches of 200.
2. `ParseShowAll` parses blank-line-separated `systemctl show` blocks into `map[unit]map[prop]string`; multi-value
   props (`Requires`, `Wants`, `TriggeredBy`, `Triggers`) split on spaces.
3. Slices come from each unit's `Slice` (nested slices `a-b.slice` contain `a-b-c.slice` by name); `-.slice` is the
   root under the `host` node. Scopes and services are `unit` nodes with attr `type`.
4. Processes come from `systemd-cgls` (reuse the 090 parser): pid, command line (redacted), and the unit whose
   cgroup holds it; `MainPID` gets attr `main=true`. Process IDs use `pid@<ControlGroup>` until 164 adds start
   times.
5. Unit status → `resource.Status`: failed → Crit, activating/deactivating/reloading → Warn, active → OK,
   inactive → Unknown (dimmed by the view).
6. Evidence: `systemctl`/`systemd-cgls`, host, timestamp, Configured for dependency edges, Observed for `runs`.
7. Partial output (a truncated section) still produces what it can and sets host attr `truncated=<section>`.

## Acceptance
```
go test ./internal/graph/collect/ ./internal/backend/linux/ -run 'Systemd|ShowAll' -v
make check
make lab-test   # add TestLabSystemdGraph: web-01 has nginx.service under system.slice with ≥ 1 process
```

## Pitfalls
- Unit names can contain `\x2d` escapes and `@` instances; keep them verbatim as names, never unescape for IDs.
- RHEL 8's systemd 239 prints `systemctl show` for `'*'` fine; Ubuntu 20.04's 245 too; still keep the fallback tested.
