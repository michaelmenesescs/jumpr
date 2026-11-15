# Task 170 — Flow resolver: entry resolution and the route walk

- **Milestone:** M16
- **Depends on:** 167
- **Parallel group:** —
- **Status:** draft

> **Draft.** Scope only. Before implementation the milestone's first task writes its BUILD-PLAN §14 subsection
> (contracts) and this file is expanded to the full format (Context, Files, Requirements, Acceptance, Pitfalls).

## Goal
SPEC §24.1: resolve a URL / host:port / Service / pod to an entry node and walk `routes`/`connects` edges following host and path rules.

## Scope
- entry resolution: `getent ahosts` on the bastion, k8s Ingress/Service by name, listener by address
- walk honouring nginx/haproxy/Ingress host+path matching from 166/165; fan-out pools as one hop with members
- hop limit (default 12), cycle safety, unresolved hops as `external`
- writes BUILD-PLAN §14.8 (`internal/flow` contract) first

## Likely files
`internal/flow/resolve.go`, `internal/flow/walk.go`, tests over the fake graph

## Acceptance (sketch)
`go test ./internal/flow/` on fixtures: demo URL resolves to ingress → service → 2 pods → postgres
