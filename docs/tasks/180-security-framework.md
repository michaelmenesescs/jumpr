# Task 180 — Security: check framework and `:findings`

- **Milestone:** M17
- **Depends on:** 167
- **Parallel group:** —
- **Status:** draft

> **Draft.** Scope only. Before implementation the milestone's first task writes its BUILD-PLAN §14 subsection
> (contracts) and this file is expanded to the full format (Context, Files, Requirements, Acceptance, Pitfalls).

## Goal
SPEC §25.1: Check interface, findings model, dedupe and grouping, findings table, attach to investigation.

## Scope
- writes BUILD-PLAN §14.9 (`internal/security` contract: Check, Finding, Severity, Evidence reuse from graph)
- `:findings [selector]` view, `i` attach, ATT&CK ID field
- defensive-only guard: checks may only use read-only probes (enforced by a test that inspects every check's commands)

## Likely files
`internal/security/{check.go,finding.go,registry.go}`, `internal/tui/views/findings.go`

## Acceptance (sketch)
framework tests with fake checks; `--demo` shows seeded findings
