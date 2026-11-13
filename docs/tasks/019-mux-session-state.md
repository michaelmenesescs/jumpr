# Task 019 — Windows and session state (pure)

- **Milestone:** M1
- **Depends on:** 017
- **Parallel group:** — (serial)
- **Status:** todo

## Goal
The data model of a session: ordered windows, each with a layout, a focused pane, a last-focused pane and a zoom
flag. Still no Bubble Tea.

## Context
SPEC §21.1. BUILD-PLAN §4.9 (`ContentState`).

## Files
- create `internal/mux/content.go` (`Content` interface, `ContentState`, `PaneMsg`, `ForPane`)
- create `internal/mux/session.go`, `internal/mux/session_test.go`

## Requirements
1. `content.go`: `Content`, `ContentState` verbatim from BUILD-PLAN §4.9; `PaneMsg` and `ForPane` from §5
   (`ForPane(id, nil)` returns nil; it wraps the produced message, and if the message is a `tea.BatchMsg` it wraps
   each inner command).
2. `type Window struct { ID WindowID; Name string; Layout *Layout; Focus, Last PaneID; Zoomed bool; AutoName bool }`.
3. `type Session struct { Name string; Windows []*Window; Active int; … }` with:
   - `NewSession(name string) *Session` — no windows yet.
   - `NewWindow(name string) (*Window, PaneID)` — appends a window with one new pane, makes it active.
   - `Split(horizontal bool) (PaneID, error)` — splits the focused pane of the active window, focuses the new one;
     clears zoom. Refuses (`errs.Config`, "pane too small") when the resulting panes would be under 10×3 for the
     last known size (`SetSize(w, h)`).
   - `KillPane(id PaneID) (windowClosed bool, sessionEmpty bool)` — closes the pane; if the window becomes empty
     remove it and select the previous window.
   - `Focus(d string)` (`left|right|up|down|next|last`), `Resize(d string, cells int)`, `ToggleZoom()`,
     `SelectWindow(i int) bool`, `NextWindow()`, `PrevWindow()`, `KillWindow() (sessionEmpty bool)`,
     `RenameWindow(name string)`, `Rename(name string)`, `SwapPane(dir string)`, `NextLayout()` (cycles
     even-horizontal → even-vertical → main-left, implemented with `Layout.Even` and rebuilding the tree for
     main-left: first pane left at ratio 0.6, the rest stacked on the right).
   - `ActiveWindow() *Window`, `FindPane(id PaneID) (*Window, bool)`, `AllPanes() []PaneID`.
   - Pane IDs and window IDs are allocated from counters in the session and never reused.
4. `Rects(area Rect) map[PaneID]Rect` for the active window: when zoomed, only the focused pane with the whole
   area.

## Out of scope
Contents themselves, rendering, persistence (024).

## Acceptance
```
go test ./internal/mux/ -run 'Session|Window|ForPane' -v
# TestSplitFocusesNew, TestKillPaneFocusesSibling, TestKillLastPaneClosesWindow, TestKillLastWindowEmpty,
# TestZoomRects, TestFocusLast, TestWindowSelectBounds, TestIDsNeverReused, TestNextLayout,
# TestForPaneWrapsBatch
```

## Pitfalls
- `Last` must be updated on every focus change so `prefix ;` works.
- Closing a zoomed pane must clear `Zoomed`.
- `tea.BatchMsg` is `[]tea.Cmd`; wrap each element, do not call them inside `ForPane` synchronously beyond the
  one command you were given.
