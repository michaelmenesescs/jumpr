# Task 020 — View pane content

- **Milestone:** M1
- **Depends on:** 016, 019
- **Parallel group:** M1-B (with 021)
- **Status:** todo

## Goal
`mux.Content` implementation that hosts a `tui.Stack`: the kind of pane that shows jumper views.

## Context
BUILD-PLAN §4.8, §4.9, §8 (pane context).

## Files
- create `internal/tui/viewpane.go`, `internal/tui/viewpane_test.go`

## Requirements
1. `func NewViewPane(env *Env, id mux.PaneID, ctx context.Context, root View) *ViewPane` implementing
   `mux.Content`. It owns a `*Stack` and a context cancelled by `Close()`.
2. `Update`: `OpenQueryMsg` is resolved here (moved from the root: registry lookup → factory → push; unknown kind
   → flash); stack messages go to the stack; every `tea.Cmd` returned to the caller is wrapped with
   `mux.ForPane(id, cmd)` so async results come back to this pane.
   Exception: commands that produce root-level messages must not be wrapped. Rule: the pane wraps everything, and
   the mux model unwraps and forwards to the root any `PaneMsg` whose inner message is one of the root-level types
   (`FlashMsg`, `OpenDialogMsg`, `CloseDialogMsg`, `RunCommandMsg`, `WindowChangedMsg`, `mux.OpMsg`, `tea.QuitMsg`,
   and later `TargetsChangedMsg`, `InvestigationChangedMsg`). Export the predicate from this package:
   `func IsRootMsg(msg tea.Msg) bool`.
3. `View(w, h)`: line 1 = crumbs (`hosts (24) › web-01 › units`, last one bold, truncated from the left with `…`
   when too long) followed by the top view's filter state if any; remaining `h-1` lines = `stack.Render(w, h-1)`.
4. `Title()` = last crumb. `RawKeys()` = false. `Capturing()` (extra method) = top view's `Capturing()`.
   `Hints()` (extra) = top view's hints. `Subject()` (extra) = top view's subject.
5. `State()`: `Type "view"`, `Commands` = for each stack entry that implements
   `interface{ CommandLine() string }`, its command line, stopping at the first entry that does not.
6. `CopyLines()` = top view's `CopyLines()`.
7. Give `views.Table` a `CommandLine()` method: `<kind> <selector>` plus ` --host a,b` when `Query.Hosts` is set
   and ` --arg k=v` per arg (sorted). Extend `Commands.Exec` (task 014 file) to parse `--host` and `--arg` when
   the first word is a kind.

## Out of scope
Terminal panes (050). The knowledge side panel (060).

## Acceptance
```
go test -race ./internal/tui/ -run ViewPane -v
# TestViewPaneWrapsCmds (async ListBatchMsg arrives as mux.PaneMsg with the right id),
# TestViewPaneRootMsgsDetected, TestViewPaneOpenQueryPushes, TestViewPaneStateRoundTrip
#   (State().Commands fed back through Commands.Exec rebuilds the same crumbs), TestViewPaneCloseCancelsContext
go test ./internal/tui/...
```

## Pitfalls
- Forgetting to wrap a command means its result is delivered to whichever pane has focus later. Wrap in one
  place only: the last line of `Update`.
- `Table.Init` must now use the pane context (pass it through `Env` copy or a `WithContext(ctx)` option on the
  table) so closing the pane stops the fan-out.
