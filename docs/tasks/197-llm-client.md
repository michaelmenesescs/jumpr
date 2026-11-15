# Task 197 — Local-first LLM client

- **Milestone:** M19
- **Depends on:** 195
- **Parallel group:** —
- **Status:** draft

> **Draft.** Scope only. Before implementation the milestone's first task writes its BUILD-PLAN §14 subsection
> (contracts) and this file is expanded to the full format (Context, Files, Requirements, Acceptance, Pitfalls).

## Goal
SPEC §27.3: OpenAI-compatible chat completions with tool calls and SSE streaming; loopback enforcement unless `agent.allow_remote`; key via auth helper for remote.

## Scope
- config `agent:` (§14.6); redaction of every outgoing message; timeouts and budgets

## Likely files
`internal/agent/llm.go`, `internal/agent/llm_test.go` (httptest server)

## Acceptance (sketch)
refuses non-loopback URL by default; streams tool calls from recorded LM Studio and llama.cpp responses
