# Task 196 — `jumpr mcp`: stdio MCP server

- **Milestone:** M19
- **Depends on:** 195, 083
- **Parallel group:** —
- **Status:** draft

> **Draft.** Scope only. Before implementation the milestone's first task writes its BUILD-PLAN §14 subsection
> (contracts) and this file is expanded to the full format (Context, Files, Requirements, Acceptance, Pitfalls).

## Goal
SPEC §27.4: JSON-RPC 2.0 over stdio, MCP initialize/tools/list/tools/call, attaches to the session server; `--readonly`.

## Scope
- stdlib only; protocol conformance tests from recorded client transcripts
- proposals appear in the TUI approval queue of the attached session

## Likely files
`internal/mcp/{server.go,jsonrpc.go}`, `cmd/jumpr` subcommand

## Acceptance (sketch)
transcript tests; manual: add to Claude Code as an MCP server and list tools
