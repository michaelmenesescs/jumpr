# Task 190 — Snapshot store

- **Milestone:** M18
- **Depends on:** 167
- **Parallel group:** —
- **Status:** draft

> **Draft.** Scope only. Before implementation the milestone's first task writes its BUILD-PLAN §14 subsection
> (contracts) and this file is expanded to the full format (Context, Files, Requirements, Acceptance, Pitfalls).

## Goal
SPEC §26: compressed, redacted graph snapshots with retention; `:snap`; automatic at investigation open/close.

## Scope
- writes BUILD-PLAN §14.10 (`internal/snapshot` contract, on-disk format with a version field)
- gzip JSON under the cache dir, `snapshots.keep` / `max_bytes`

## Likely files
`internal/snapshot/{store.go,format.go}`

## Acceptance (sketch)
round-trip and retention tests; corrupted file skipped with a warning
