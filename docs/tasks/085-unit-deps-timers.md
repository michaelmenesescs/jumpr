# Task 085 — Dependencies tab and timers list

- **Milestone:** M6
- **Depends on:** 084
- **Parallel group:** — (serial)
- **Status:** todo

## Goal
SPEC §8.1.1: dependency tree for a unit; `:timers` per host.

## Files
- create `internal/backend/linux/parse_deps_timers.go`, `internal/backend/linux/parse_deps_timers_test.go`
- create `internal/tui/views/unit_deps.go`
- modify `internal/backend/linux/kinds.go`, `internal/backend/linux/backend.go`, `internal/tui/views/unit.go`
- add `testdata/linux/timers/…`, `testdata/linux/deps/…` (inline samples; captures via `sitrep-dev capture linux`
  after adding the commands to `RawCommands`)

## Requirements
1. `ParseDependencies(out string) []DepNode` for `systemctl list-dependencies <u> --plain --no-pager` (and with
   `--reverse`): indentation (2 spaces per level) gives depth; a leading `●`/`○`/`×` state glyph is optional.
   `DepNode{Unit string; Depth int}`.
2. Deps tab: two sections, "requires / wants" and "required by" (reverse), collapsed below depth 2, `enter`
   opens the selected unit's detail.
3. Kind `timer` (aliases `timers`; ScopeHost): columns `timer`, `next` (time), `left` (duration), `last`,
   `passed`, `activates`. Command: `systemctl list-timers --all --no-pager --no-legend`. The time columns contain
   spaces (`Thu 2026-11-12 14:00:00 UTC 21min left Thu 2026-11-12 13:00:00 UTC 38min ago logrotate.timer
   logrotate.service`): parse from the **right** (last two fields are timer and service), then split the rest on
   the `left`/`ago` markers; `n/a` and `-` mean never. Enrich with `systemctl show <timer> -p
   NextElapseUSecRealtime -p LastTriggerUSec` only if the text parse fails.
4. Links: timer → its unit detail (`enter`), → journal of the activated unit (`j`).

## Acceptance
```
go test ./internal/backend/linux/ -run 'Dependencies|Timers' -v   # samples from Rocky 9 and Ubuntu 24.04 layouts,
                                                                  # "n/a" rows, empty output
go test -race ./internal/tui/views/ -run 'Deps' -v
```

## Pitfalls
- Timer output layout differs between systemd versions (column count); never index columns from the left.
