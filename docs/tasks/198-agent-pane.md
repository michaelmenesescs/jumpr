# Task 198 — Agent loop and `:ask` pane

- **Milestone:** M19
- **Depends on:** 197, 061
- **Parallel group:** —
- **Status:** draft

> **Draft.** Scope only. Before implementation the milestone's first task writes its BUILD-PLAN §14 subsection
> (contracts) and this file is expanded to the full format (Context, Files, Requirements, Acceptance, Pitfalls).

## Goal
SPEC §27.3: plan → tool calls → answer with citations; budgets; investigation + knowledge as context; proposals into the approval queue.

## Scope
- loop with step/token/time budgets; cancellation from the pane
- pane: streaming text, collapsible tool calls, citations that open the cited node/log/probe

## Likely files
`internal/agent/loop.go`, `internal/tui/views/ask.go`, `internal/tui/commands_agent.go`

## Acceptance (sketch)
scripted fake model drives a full loop in tests; `--demo` with a fake model answers the mockup incident
