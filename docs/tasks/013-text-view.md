# Task 013 — Text view (scrollable text, key/value detail)

- **Milestone:** M0
- **Depends on:** 008, 011
- **Parallel group:** M0-C (with 012, 014, 015)
- **Status:** todo

## Goal
A scrollable, searchable text pane used for describe output, unit files, timelines, help, and anything that is not
a table.

## Context
BUILD-PLAN §4.8 (`View`), §5 (`LinesMsg`). SPEC §13.

## Files
- create `internal/tui/views/text.go`, `internal/tui/views/text_test.go`

## Requirements
1. `func NewText(env *tui.Env, crumb string, lines []string) *Text`; implements `tui.View`.
2. `func (t *Text) SetLines(lines []string)`, `func (t *Text) Append(lines ...string)`; with `Follow` enabled
   (`w` toggles, off by default) appending keeps the view pinned to the bottom. Keep at most 50,000 lines (drop
   oldest).
3. Keys: up/down/page/top/bottom scroll; `/` search (capturing; `enter` jumps to the next match, `n`/`N`
   next/previous, matches highlighted with `Theme.Selected`); `esc` clears the search, or pops when none;
   left/right scroll horizontally by 8 cells when `Wrap` is off; `W` toggles wrap (default off).
4. Lines may contain ANSI styling; widths use `render.Width`. Tabs are expanded to 4 spaces on input.
5. `func NewKV(env *tui.Env, crumb string, pairs [][2]string) *Text` renders aligned `key  value` lines (key in
   `Theme.Dim`).
6. A status footer line inside the view: `line 120/3,400 · follow · /pattern (12 matches)`.
7. Handles `tui.LinesMsg` (matching its own `Seq`, set with `SetSeq(int)`): appends lines; on `Closed` with `Err`
   appends a final Crit line `stream ended: <kind>`.

## Out of scope
Markdown rendering (plain text is fine). Starting streams (the caller does).

## Acceptance
```
go test ./internal/tui/views/ -run Text -v
# TestTextRenderSize, TestTextScrollBounds, TestTextSearchNextPrev, TestTextFollowPinsBottom,
# TestTextCapsLines, TestTextHorizontalScrollWithANSI
```

## Pitfalls
- Horizontal scrolling of ANSI-coloured lines: use `ansi.Cut`/`ansi.TruncateLeft` from `x/ansi`; do not slice
  bytes.
- Do not use `bubbles/viewport` here; it does not give the exact-size guarantee we need.
