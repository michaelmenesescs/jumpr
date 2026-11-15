# Task 192 — `:changes` timeline

- **Milestone:** M18
- **Depends on:** 191, 072, 114
- **Parallel group:** —
- **Status:** draft

> **Draft.** Scope only. Before implementation the milestone's first task writes its BUILD-PLAN §14 subsection
> (contracts) and this file is expanded to the full format (Context, Files, Requirements, Acceptance, Pitfalls).

## Goal
SPEC §26: merged change events from k8s, journald unit transitions, Salt jobs, Chef runs, packages.

## Scope
- one event model; sources stream into one sorted view; pivots to native views

## Likely files
`internal/snapshot/changes.go`, `internal/tui/views/changes.go`

## Acceptance (sketch)
fixtures per source; lab: restart a unit + run a salt job → both appear in order
