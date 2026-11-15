# Task 185 — Findings into investigations and a report export

- **Milestone:** M17
- **Depends on:** 181, 182, 183, 184
- **Parallel group:** —
- **Status:** draft

> **Draft.** Scope only. Before implementation the milestone's first task writes its BUILD-PLAN §14 subsection
> (contracts) and this file is expanded to the full format (Context, Files, Requirements, Acceptance, Pitfalls).

## Goal
Findings attach to investigation Markdown with evidence; `:report` exports a redacted Markdown summary grouped by severity and ATT&CK tactic.

## Scope
- reuse investigation writer; no new storage

## Likely files
`internal/security/report.go`

## Acceptance (sketch)
golden Markdown from fixture findings; no secret patterns survive (redact test)
