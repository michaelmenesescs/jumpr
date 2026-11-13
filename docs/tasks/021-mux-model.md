# Task 021 — Mux model: panes on screen, routing, ops

- **Milestone:** M1
- **Depends on:** 018, 019
- **Parallel group:** M1-B (with 020)
- **Status:** done

## Goal
A Bubble Tea component that owns a `Session` and its `Content`s, draws all panes of the active window with
borders, routes messages and keys, and executes `OpMsg`s.

## Context
SPEC §21. BUILD-PLAN §4.9, §5.

## Files
- create `internal/mux/model.go`, `internal/mux/frame.go`, `internal/mux/model_test.go`

## Requirements
1. `type Factory func(id PaneID, st ContentState) (Content, tea.Cmd)` — creates content for a new pane (`st` zero
   value = default pane).
2. `func NewModel(s *Session, f Factory, prefixKey string, styles Styles) *Model` where `Styles { Border,
   BorderFocused, Title, TitleFocused lipgloss.Style }`.
3. `func (m *Model) Update(msg tea.Msg) (root tea.Msg, cmd tea.Cmd)`:
   - `tea.WindowSizeMsg` is not handled here; the owner calls `SetSize(w, h)` with the body area.
   - `PaneMsg`: if `IsRoot(inner)` (a predicate injected with `SetRootPredicate(func(tea.Msg) bool)`) return it as
     `root`; else deliver to that pane's content (even if its window is not active); unknown pane → drop.
   - `tea.KeyMsg`: `Prefix.Feed(key)`; an op → apply; `OpSendPrefix` → forward the prefix key to the focused
     content; not consumed → focused content's `Update`.
   - `OpMsg` → apply (table below). Ops the mux cannot perform itself (`OpCommand`, `OpDetach`, `OpHelp`,
     `OpWindowList`, `OpSessionList`, `OpRenameWindow`, `OpRenameSession`, `OpSSH`, `OpCopyMode`, `OpSync`) are
     returned as `root` for the owner.
   - Any other message → every content in every window (broadcast), commands batched.
4. Ops: split-h/v (create content through the factory, call `Init`, focus it), kill-pane (call `Close()` on the
   content; when the session becomes empty return `tea.Quit` as the command), zoom, focus, resize (2 cells),
   swap, layout-next, new-window (factory + focus), kill-window (close all its contents), select/next/prev
   window. Every focus change calls `SetFocus(false/true)` on the two contents involved.
5. `func (m *Model) View() string` — exactly the size given to `SetSize`:
   - one pane visible (single or zoomed): no border; the content gets the whole area. Zoomed adds ` Z` to the
     window name in `Tabs()`.
   - several panes: each pane rectangle gets a rounded border with ` <id>:<title> ` embedded in the top edge,
     focused pane in `BorderFocused`; the content gets `(W-2) × (H-2)`.
   - compose by rows: for each screen row, concatenate the row slices of the panes it crosses, ordered by X.
   - content output is passed through `render.Fit` (import `jumper/internal/render`; it is a leaf package).
6. Accessors for the owner: `Focused() (PaneID, Content)`, `Tabs() []Tab{Index, Name, Active, Zoomed}`,
   `PrefixArmed() bool`, `Session() *Session`, `Contents() map[PaneID]Content`, `AddPane(st ContentState, split
   string) (PaneID, tea.Cmd)` (used by `:ssh`, restore), `CloseAll()`.

## Out of scope
Copy mode, sync, persistence, the root chrome, `:` commands.

## Acceptance
```
go test -race ./internal/mux/ -run Model -v
# uses a stub Content that renders its id and records keys:
# TestViewExactSize (1, 2, 3, 4 panes; 80x24 and 131x37), TestBordersShowTitles, TestKeysGoToFocused,
# TestPrefixSplitCreatesPane ("ctrl+a" then "%"), TestPaneMsgRoutedToInactiveWindow, TestRootMsgReturned,
# TestKillLastPaneQuits, TestZoomHidesOthers, TestBroadcastReachesAll, TestSendPrefixForwarded
```

## Pitfalls
- Build each pane's box yourself (top edge string with the title, side bars per line, bottom edge). `lipgloss`
  `Border()` plus `Width()` counts padding/border differently between versions; hand-built boxes are exact.
- A pane with width 10 has an interior of 8; titles must be truncated to fit the top edge.
- Never call `Content.View` with non-positive sizes.
