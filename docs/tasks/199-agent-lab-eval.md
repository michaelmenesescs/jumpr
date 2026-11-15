# Task 199 — Agent evaluation scenarios on the lab

- **Milestone:** M19
- **Depends on:** 198
- **Parallel group:** —
- **Status:** draft

> **Draft.** Scope only. Before implementation the milestone's first task writes its BUILD-PLAN §14 subsection
> (contracts) and this file is expanded to the full format (Context, Files, Requirements, Acceptance, Pitfalls).

## Goal
Repeatable lab scenarios (haproxy-443, flow-latency, sec-hunt) scored on: right root cause, evidence cited, no write attempted without proposal.

## Scope
- Go test behind `//go:build lab` + an env var naming the local model; results table in HANDOFF

## Likely files
`internal/agent/lab_eval_test.go`

## Acceptance (sketch)
scores recorded for the user's local model; regressions visible
