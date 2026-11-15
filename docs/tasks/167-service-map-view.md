# Task 167 — `:map`: the service map across Linux and Kubernetes

- **Milestone:** M15 (exit)
- **Depends on:** 163, 164, 165, 166
- **Parallel group:** —
- **Status:** todo

## Context
SPEC §23.4. BUILD-PLAN §2 (M15 demo row).

## Files
- create `internal/tui/views/graphmap.go`, `internal/tui/views/graphmap_layout.go`, tests and goldens
- modify `internal/tui/commands_graph.go` (`:map [selector|namespace]`)

## Requirements
1. Application grouping (default): one box per unit/deployment-like group (same unit name across hosts, or one k8s
   Service/Deployment), members counted. Infrastructure grouping (`t`): one box per host/node.
2. Layered layout left → right (longest path from entry points), deterministic, crossings reduced with a median
   heuristic; pure Go in `graphmap_layout.go`, tested on fixtures.
3. Edge style by confidence (solid / dashed / dotted using the active glyph set; ascii fallback `-`, `.`, `:`).
4. Cursor moves between boxes with arrows/`hjkl`; `Enter` opens the native view (tree for units, pod for pods,
   host for hosts); `/` highlights matches.
5. Large graphs: more than 60 boxes collapse leaf groups into `+N more` with `Enter` to expand.
6. Works in `--demo` from the fake graph (160).

## Acceptance
```
go test ./internal/tui/views/ -run 'GraphMap|Layout' -v
make check
./jumpr --demo  →  :map  →  Enter on nginx  →  Esc  →  t
```
