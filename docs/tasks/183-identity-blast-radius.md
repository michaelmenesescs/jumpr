# Task 183 — Identity edges and `:blast`

- **Milestone:** M17
- **Depends on:** 180, 164, 165
- **Parallel group:** —
- **Status:** draft

> **Draft.** Scope only. Before implementation the milestone's first task writes its BUILD-PLAN §14 subsection
> (contracts) and this file is expanded to the full format (Context, Files, Requirements, Acceptance, Pitfalls).

## Goal
SPEC §25.3: identity collector (users, sudo, authorized_keys fingerprints, SA → RBAC, Salt/Chef admin paths) and the blast-radius walk.

## Scope
- identity layer collector emitting `user`, `sshkey`, `serviceaccount`, `role` nodes and `can-login`, `can-sudo`, `grants` edges
- `:blast <node>` ranked reachable set with shortest paths and evidence; `security.sensitive` selectors
- `security.allowed_keys` → findings for unexpected keys

## Likely files
`internal/graph/collect/identity.go`, `internal/security/blast.go`, `internal/tui/views/blast.go`

## Acceptance (sketch)
fixture graph: compromised pod reaches postgres via SA token + service; lab: shared key on all hosts is ranked first
