# Task 011 — TUI core: View, Env, messages, NextBatch, ViewStack

- **Milestone:** M0
- **Depends on:** 006, 010
- **Parallel group:** — (serial)
- **Status:** todo

## Goal
The small set of types every view and command is written against.

## Context
BUILD-PLAN §4.8 (View, Subject, KeyHint, Env, Factory, Command), §5 (messages, streaming pattern), §8.

## Files
- create `internal/tui/view.go` (Subject, KeyHint, View, Factory)
- create `internal/tui/env.go` (Env, SessionState — only the fields whose packages exist; see below)
- create `internal/tui/msgs.go`
- create `internal/tui/stream.go`, `internal/tui/stream_test.go`
- create `internal/tui/stack.go`, `internal/tui/stack_test.go`

## Requirements
1. `view.go`: verbatim from BUILD-PLAN §4.8.
2. `env.go`: for now `Env` has `Registry`, `Session`, `Theme`, `Keys`, `Now`, plus
   `Factories map[resource.KindName]Factory` and `Log *slog.Logger`. `SessionState` has `Name`, `Window`,
   `ReadOnly`. Later tasks add the other fields (BUILD-PLAN "Growing structs").
3. `msgs.go`: define now: `PushViewMsg`, `PopViewMsg`, `ReplaceViewMsg`, `OpenQueryMsg`, `FlashLevel` (+ 4
   constants), `FlashMsg`, `CloseDialogMsg`, `RunCommandMsg`, `WindowChangedMsg`, `HealthMsg`, `ListBatchMsg`,
   `LinesMsg` (mux requests are `mux.OpMsg`, defined in task 018, not here). Add helper constructors that return `tea.Cmd`: `Push(v View) tea.Cmd`, `Pop() tea.Cmd`,
   `Flash(level FlashLevel, format string, args ...any) tea.Cmd`, `OpenQuery(q resource.Query) tea.Cmd`.
4. `stream.go`: `NextBatch[T any](ch <-chan T, max int) (items []T, closed bool)` exactly as specified: block for
   the first item (or close), then keep draining without blocking longer than 50 ms total or `max` items. Also
   `func ListBatch(seq int, ch <-chan backend.Result) tea.Cmd` which returns a command producing a `ListBatchMsg`
   (max 200 results per message).
5. `stack.go`: `type Stack struct` holding `[]View`.
   - `NewStack(root View) *Stack`, `Top() View`, `Len() int`, `Crumbs() []string`
   - `Push(v View) tea.Cmd` (returns `v.Init()`), `Pop() bool` (never pops the last view; returns false then),
     `Replace(v View) tea.Cmd`
   - `Update(msg tea.Msg) tea.Cmd`: handles `PushViewMsg`, `PopViewMsg`, `ReplaceViewMsg` itself; everything else
     goes to `Top().Update` and the returned view replaces the top.
   - `Render(w, h int) string` delegates to the top view and passes the result through `render.Fit(…, w, h)` as a
     safety net.

## Out of scope
The root model, panes, command bar, concrete views, `OpenQueryMsg` handling (016).

## Acceptance
```
go test -race ./internal/tui/ -run 'NextBatch|Stack' -v
# NextBatch: single item; burst of 500 → first call returns ≤ max; closed channel → closed=true with remaining
#   items; returns within ~60ms when a slow producer keeps trickling
# Stack: push/pop/replace, crumbs order, cannot pop root, Update routes to top only
```

## Pitfalls
- `NextBatch` must return `closed=true` **together with** the last items, not on a separate later call only; the
  caller stops asking after `closed`.
- Views are interface values: always store the view returned by `Update` back into the stack.
- Do not call `tea.Program.Send` anywhere in this package.
