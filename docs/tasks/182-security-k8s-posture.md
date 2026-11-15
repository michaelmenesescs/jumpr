# Task 182 — Kubernetes posture checks

- **Milestone:** M17
- **Depends on:** 180, 165
- **Parallel group:** —
- **Status:** draft

> **Draft.** Scope only. Before implementation the milestone's first task writes its BUILD-PLAN §14 subsection
> (contracts) and this file is expanded to the full format (Context, Files, Requirements, Acceptance, Pitfalls).

## Goal
SPEC §25.2 Kubernetes row.

## Scope
- privileged/hostPath/hostNetwork/hostPID, root containers, automounted SA tokens with RBAC, broad ClusterRoleBindings, missing NetworkPolicy, unpinned images

## Likely files
`internal/security/checks_k8s.go`

## Acceptance (sketch)
lab fault `sec-k8s` (lab request L6)
