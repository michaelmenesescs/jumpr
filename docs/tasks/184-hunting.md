# Task 184 — Hunting: auth, lineage, network, change

- **Milestone:** M17
- **Depends on:** 180, 191
- **Parallel group:** —
- **Status:** draft

> **Draft.** Scope only. Before implementation the milestone's first task writes its BUILD-PLAN §14 subsection
> (contracts) and this file is expanded to the full format (Context, Files, Requirements, Acceptance, Pitfalls).

## Goal
SPEC §25.4.

## Scope
- journal auth bursts and new sources; daemon → shell lineage; /tmp, /dev/shm, deleted exe; new outbound peers and listeners vs baseline; new units/timers/drop-ins; eBPF execve when enabled

## Likely files
`internal/security/hunt_*.go`

## Acceptance (sketch)
lab fault `sec-hunt` (spawned shell under nginx, new listener, extra key) → findings
