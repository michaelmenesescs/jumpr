# Task 008 — Rendering primitives: fit and table

- **Milestone:** M0
- **Depends on:** 002
- **Parallel group:** M0-A (with 003, 004, 009)
- **Status:** todo

## Goal
Pure functions that turn rows into exactly-sized text. Every view depends on these being correct about width.

## Context
BUILD-PLAN §4.8 (`View.Render` must return exactly `h` lines of exactly `w` cells), SPEC §13.

## Files
- create `internal/render/fit.go`, `internal/render/fit_test.go`
- create `internal/render/table.go`, `internal/render/table_test.go`

## Requirements
`fit.go`:
1. `func Width(s string) int` — display width in cells, ignoring ANSI escape sequences
   (`ansi.StringWidth` from `github.com/charmbracelet/x/ansi`).
2. `func Truncate(s string, w int) string` — cut to at most `w` cells, ANSI-aware, adding `…` as the last cell
   when something was cut (`ansi.Truncate`). `w <= 0` → `""`.
3. `func PadRight(s string, w int) string`, `func PadLeft(s string, w int) string` — truncate then pad with
   spaces to exactly `w` cells.
4. `func Fit(s string, w, h int) string` — split on `\n`; every line truncated/padded to exactly `w`; exactly `h`
   lines (cut extra, add blank lines). Result has `h-1` newlines and no trailing newline. `h <= 0` or `w <= 0` →
   `""`.

`table.go`:
5. `type Cell struct { Text string; Style func(string) string }` (Style may be nil; it is applied **after**
   padding so backgrounds cover the cell).
6. `type TableColumn struct { Title string; Width int; AlignRight bool }` (`Width == 0` = flexible).
7. `func ColumnWidths(cols []TableColumn, total int) []int` — fixed widths as given; one space between columns;
   remaining width split evenly across flexible columns (minimum 4, leftover cells go to the last flexible
   column). If fixed columns do not fit, shrink from the right down to a minimum of 3.
8. `func Row(cells []Cell, widths []int, cols []TableColumn) string` — one line exactly `sum(widths) +
   len(widths)-1` cells wide.
9. `func Header(cols []TableColumn, widths []int) string`.

## Out of scope
Colours and themes (010). Scrolling and selection (012). Sparklines (009).

## Acceptance
```
go test ./internal/render/ -run 'Fit|Width|Truncate|Pad|Table|ColumnWidths|Row' -v
# must include wide runes ("日本語"), a string with ANSI colour codes, w smaller than the text,
# and a property-style test: for random inputs, every line of Fit(s,w,h) has Width == w and there are h lines
```

## Pitfalls
- `len(s)` is bytes, not cells. Never use it for layout.
- Truncating a coloured string must not leave the colour open: append `\x1b[0m` if the input contained an escape.
- Do not import `lipgloss` here; this package stays dependency-light and pure.
