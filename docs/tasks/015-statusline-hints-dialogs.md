# Task 015 — Status line, hint bar, flash, dialog frame, help

- **Milestone:** M0
- **Depends on:** 011
- **Parallel group:** M0-C (with 012, 013, 014)
- **Status:** todo

## Goal
The fixed chrome around the panes, as in the mockup: status line with backend health dots on top; flash line and
key hints at the bottom; a modal dialog frame with a help dialog as its first user.

## Context
SPEC §13 (status line: green ok, half degraded, red down, grey unconfigured). Mockup status bar and hint bar.

## Files
- create `internal/tui/statusline.go`, `internal/tui/statusline_test.go`
- create `internal/tui/hints.go`
- create `internal/tui/dialog.go`, `internal/tui/dialog_help.go`, `internal/tui/dialog_test.go`

## Requirements
1. `type StatusData struct { Session string; Windows []WindowTab; Prefix string; Context string; Investigation
   string; Window string; Targets int; Health []HealthDot; ReadOnly bool; Sync int }`, `WindowTab{Index int; Name
   string; Active bool}`, `HealthDot{Name string; State backend.HealthState}`.
2. `func RenderStatus(t *theme.Theme, d StatusData, w int) string` — exactly `w` cells, one line. Left to right:
   `sitrep`, session name, window tabs (`0:hosts*`), then `inv:<id>`, `window:<label>`, `targets:<n>`, then
   right-aligned: `RO` badge if read-only, `SYNC ×n` badge if `Sync > 0`, the prefix (`^A`), health dots
   (`●salt ●prom ◐grafana`, glyphs: OK `●` green, degraded `◐` yellow, down `●` red, unconfigured `○` grey).
   When too narrow, drop in this order: context, window label, health names (keep dots), window tabs except the
   active one.
3. `func RenderHints(t *theme.Theme, hints []KeyHint, w int) string` — `<enter> open  <u> units …`; drops hints
   from the right when they do not fit; always ends with `<:> cmd  <?> help` if there is room.
4. `func RenderFlash(t *theme.Theme, m FlashMsg, w int) string`.
5. `type Dialog interface { Update(msg tea.Msg) (Dialog, tea.Cmd); View(maxW, maxH int) string; Title() string }`.
   A dialog closes itself by returning `CloseDialogMsg` from a command. Add `OpenDialogMsg{Dialog}` to `msgs.go`.
6. `func RenderDialog(t *theme.Theme, d Dialog, screenW, screenH int, background string) string` — draws the
   dialog centred in a rounded border with its title, over `background` (replace the covered lines; no
   transparency needed). Output is exactly `screenW × screenH`.
7. `func NewHelpDialog(t *theme.Theme, title string, sections []HelpSection) Dialog` with
   `HelpSection{Title string; Hints []KeyHint}`; scrollable with up/down; `esc`, `q` or `?` close it.
8. `func HealthPoll(reg *backend.Registry, interval time.Duration) tea.Cmd` — a command that checks each backend's
   `Health` with a 5 s timeout (all backends concurrently) and returns `tea.Batch` of `HealthMsg`; the root
   re-arms it with `tea.Tick`.

## Out of scope
Confirm/plan dialogs (061). The root model (016).

## Acceptance
```
go test ./internal/tui/ -run 'Status|Hints|Flash|Dialog|Help' -v
# TestStatusExactWidth (w = 40, 80, 132, 200), TestStatusDropsOrder, TestHealthDotGlyphs,
# TestHintsFit, TestDialogCentredExactSize, TestHelpDialogCloses
```

## Pitfalls
- Overlaying a dialog on styled text: work line by line with `ansi.Truncate` / `ansi.TruncateLeft`, or simply
  replace whole lines of the background with dialog lines padded by spaces. Byte slicing will corrupt colours.
- Width tests must strip ANSI before measuring (`render.Width` does).
