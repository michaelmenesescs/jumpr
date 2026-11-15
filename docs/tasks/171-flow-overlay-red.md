# Task 171 — Flow overlay: rate, errors, latency per hop

- **Milestone:** M16
- **Depends on:** 170, 101
- **Parallel group:** —
- **Status:** draft

> **Draft.** Scope only. Before implementation the milestone's first task writes its BUILD-PLAN §14 subsection
> (contracts) and this file is expanded to the full format (Context, Files, Requirements, Acceptance, Pitfalls).

## Goal
SPEC §24.2: per-hop RED overlay from Prometheus when available, local signals otherwise.

## Scope
- PromQL templates per hop type (mesh `istio_requests_total`, ingress-nginx, plugin-mapped series); configurable
- fallbacks: `ss -i` retransmits/backlog, unit journal error lines in window, restarts
- worst-hop scoring

## Likely files
`internal/flow/overlay.go`, `internal/flow/overlay_prom.go`, tests with recorded Prometheus responses

## Acceptance (sketch)
worst hop is the faulted one in the lab `flow-latency` fault (lab request L3)
