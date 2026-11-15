# Task 163 — `:tree`: the systemd tree for a host and for a fleet

- **Milestone:** M15
- **Depends on:** 162
- **Parallel group:** —
- **Status:** todo

## Context
SPEC §23.3 (behaviour and keys — the user's first ask, "see the systemctl units in the tree"). Existing views to
reuse on `Enter`: `views.NewUnit`, `views.NewJournal`, `views.NewProc`.

## Goal
A collapsible tree view over the graph: cgroup layout (default) and dependency layout, status flowing up, fleet
merge for a selector.

## Files
- create `internal/tui/views/tree.go`, `internal/tui/views/tree_fleet.go`, `internal/tui/views/tree_test.go`
- create `internal/tui/commands_graph.go` (`:tree [host|selector]`, alias `:t`)
- add goldens `internal/tui/views/testdata/tree/…`

## Requirements
1. `:tree` with no argument uses the current target set; one host → host mode; several → fleet mode. It calls
   `Env.GraphReq.Request` for layer `systemd` and renders whatever the graph has, re-rendering when
   `Env.Graph.Version()` changes; a `loading 3/12 …` banner like tables.
2. Host mode, cgroup layout: host → `-.slice` → slices → units → processes (pid, user, %cpu, rss, command). Units
   sort failed, activating, active, then the rest; within a level by name. Glyphs from `Env.Glyphs` (tree lines
   and status markers) so the ascii set works.
3. Dependency layout (`d`): `default.target` (from the probe) → `requires`/`wants` children; `D` reverses from the
   selected unit. A unit already shown higher in the same branch is printed once more as `↺ name` and not expanded.
4. Status flows up: a collapsed node shows `● N failed` / `◐ N activating` counts of its subtree.
5. Keys exactly as SPEC §23.3; every key also appears in `Hints()`. `Enter` on a unit pushes `NewUnit`, on a process
   `NewProc`; `j` journal for the unit; `x` opens the unit's action menu (existing bus flow).
6. Fleet mode: rows are unit names across the set; columns `hosts` (`3/3`), `state` (`active` or a breakdown
   `2 active · 1 failed`), `partial` marker when not on every host. `Enter` → per-host breakdown table with pivots.
7. `/` filter keeps ancestors of matches; `f` failed-only; `a` hides units without processes; expansion state
   survives refreshes (keyed by NodeID).
8. Render cost: a 2,000-unit host renders in < 20 ms (benchmark in the test file).

## Acceptance
```
go test ./internal/tui/views/ -run 'Tree' -v
go test ./internal/tui/views/ -run '^$' -bench Tree -benchtime 50x
make check
./jumpr --demo   →  :tree web-ewr-007  →  f  →  Enter on nginx.service  →  Esc  →  :tree role=web
```

## Pitfalls
- No I/O in `Update`/`Render`; the view reads the graph through `GraphReader` only.
- The tree must stay usable at 80×24: truncate commands, never wrap.
