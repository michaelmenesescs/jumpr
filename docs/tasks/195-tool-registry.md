# Task 195 — Agent tool registry

- **Milestone:** M19
- **Depends on:** 167, 180, 191
- **Parallel group:** —
- **Status:** draft

> **Draft.** Scope only. Before implementation the milestone's first task writes its BUILD-PLAN §14 subsection
> (contracts) and this file is expanded to the full format (Context, Files, Requirements, Acceptance, Pitfalls).

## Goal
SPEC §27.1: one registry of read-only tools + `propose_action`, JSON-schema'd inputs/outputs, actor recording.

## Scope
- writes BUILD-PLAN §14.11 (`internal/agent` tool contract)
- tools: kinds, list, get, probe (named only), graph_neighbours, tree, flow, findings, blast, diff, changes, logs, note, propose_action
- every call → investigation timeline with `actor=agent:<name>`; outputs redacted; size caps

## Likely files
`internal/agent/tools*.go`

## Acceptance (sketch)
table tests per tool against the fake world; a test that no tool reaches `action.Step.Run`
