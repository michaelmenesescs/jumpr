# Task 173 — `:flow` view

- **Milestone:** M16
- **Depends on:** 170, 171
- **Parallel group:** —
- **Status:** draft

> **Draft.** Scope only. Before implementation the milestone's first task writes its BUILD-PLAN §14 subsection
> (contracts) and this file is expanded to the full format (Context, Files, Requirements, Acceptance, Pitfalls).

## Goal
SPEC §24.2 UI: vertical hop list with evidence, confidence and overlay; pivots `Enter`, `l`, `T`.

## Scope
- hop rows, worst hop highlighted, members expandable
- `:flow` command with completion for Services and recent URLs
- record the flow into the investigation timeline on `i`

## Likely files
`internal/tui/views/flow.go`, `internal/tui/commands_flow.go`

## Acceptance (sketch)
goldens in `--demo`; manual: `:flow https://shop.lab.example.internal/cart`
