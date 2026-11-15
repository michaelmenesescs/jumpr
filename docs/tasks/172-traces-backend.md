# Task 172 — Traces: Jaeger / Tempo API backend and span waterfall

- **Milestone:** M16
- **Depends on:** 098, 170
- **Parallel group:** —
- **Status:** draft

> **Draft.** Scope only. Before implementation the milestone's first task writes its BUILD-PLAN §14 subsection
> (contracts) and this file is expanded to the full format (Context, Files, Requirements, Acceptance, Pitfalls).

## Goal
SPEC §24.3: optional tracing backend over connection profiles; spans linked to graph nodes.

## Scope
- kinds `trace`, `span`; Jaeger `/api/traces` and Tempo `/api/search` + `/api/traces/<id>` JSON, stdlib only
- waterfall view; span → node by `service.name`, `k8s.pod.name`, `host.name`

## Likely files
`internal/backend/traces/…`, `internal/tui/views/waterfall.go`

## Acceptance (sketch)
recorded API fixtures; lab test when the lab tracing container exists (lab request L4)
