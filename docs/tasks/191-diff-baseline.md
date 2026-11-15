# Task 191 — Baselines and `:diff`

- **Milestone:** M18
- **Depends on:** 190
- **Parallel group:** —
- **Status:** draft

> **Draft.** Scope only. Before implementation the milestone's first task writes its BUILD-PLAN §14 subsection
> (contracts) and this file is expanded to the full format (Context, Files, Requirements, Acceptance, Pitfalls).

## Goal
SPEC §26: `:baseline save`, active baseline, node/edge diff view.

## Scope
- diff by NodeID/edge key; attr changes; grouping by host/namespace

## Likely files
`internal/snapshot/diff.go`, `internal/tui/views/diff.go`

## Acceptance (sketch)
fixture snapshots → expected added/removed/changed; lab: fault after baseline shows only the fault's changes
