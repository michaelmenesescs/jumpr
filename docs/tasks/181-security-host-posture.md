# Task 181 — Host posture checks

- **Milestone:** M17
- **Depends on:** 180
- **Parallel group:** —
- **Status:** draft

> **Draft.** Scope only. Before implementation the milestone's first task writes its BUILD-PLAN §14 subsection
> (contracts) and this file is expanded to the full format (Context, Files, Requirements, Acceptance, Pitfalls).

## Goal
SPEC §25.2 Exposure + Host posture rows.

## Scope
- wildcard listeners, sshd -T, sudoers wildcards, writable unit files/drop-ins, bounded SUID scan on demand, sysctl keys, security units state

## Likely files
`internal/security/checks_host.go` + parsers + goldens from the three lab distros

## Acceptance (sketch)
lab fault `sec-host` (lab request L6) produces the expected findings; clean lab produces none above info
