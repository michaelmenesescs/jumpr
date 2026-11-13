# Task 012 — Generic table view

- **Milestone:** M0
- **Depends on:** 007, 008, 009, 011
- **Parallel group:** M0-C (with 013, 014, 015)
- **Status:** done

## Goal
The k9s-style list that renders any kind from any backend: streaming rows, cursor, sort, `/` filter, error rows.

## Context
BUILD-PLAN §4.8, §5 (ListBatchMsg, Seq). SPEC §13. Mockup tables (hosts, units).

## Files
- create `internal/tui/views/table.go`
- create `internal/tui/views/table_format.go` (field → cell text/style by `ColumnKind`)
- create `internal/tui/views/table_test.go`

## Requirements
1. `func NewTable(env *tui.Env, b backend.Backend, spec resource.KindSpec, q resource.Query) *Table`; `*Table`
   implements `tui.View`.
2. `Init` starts `b.List` with a cancellable context derived from `context.Background()` (the pane context comes
   in task 020) and returns `tui.ListBatch(seq, ch)`. On `ListBatchMsg` with a matching `Seq`: append items,
   record per-host errors, re-sort, keep the cursor on the same `Ref` if it still exists, and request the next
   batch unless `Closed`. Stale `Seq` → ignore.
3. Keys (use `env.Keys`): up/down/page/top/bottom move the cursor; `r` refresh (cancel, `Seq++`, clear, reload);
   `/` enters filter mode (`Capturing() == true`): typing edits the filter, `enter` keeps it, `esc` clears it;
   `enter` on a row emits `tui.OpenQuery` for the first `b.Links(row)` link if any (drill-in), else a flash
   `no drill-in for <kind>`; `esc` with no filter emits `tui.Pop()`; a key equal to a link's `Key` opens that link;
   `e` flashes the first error (the errors dialog comes later); `s` cycles the sort column, `S` reverses.
4. Filter: case-insensitive substring over the row's rendered cells; prefix `~` means regex. Invalid regex shows
   the filter in the Crit style and matches nothing.
5. Render: header row, then visible rows only (windowed around the cursor), selected row in `Theme.Selected`,
   row text style from `resource.Status`. First line above the header is a one-line banner:
   `loading 812/1,512 …` while streaming, `1,512 hosts · 4 errors · 2.1s` when done, plus `/filter` when set.
   When `Stale` rows exist add `stale`. Errors are counted, and hosts with errors are listed as rows at the bottom
   with the error kind in the first non-name column, styled Crit.
6. Cell formatting by `ColumnKind`: Number (`render.Int` or 2 decimals for floats), Bytes, Percent, Duration, Time
   (`15:04:05`, or `Jan 02` when older than 24 h), Spark (`render.Spark` to the column width), Status; `nil` → `—`
   in Faint.
7. `Crumb()`: `<kind> (<n>)` or `<kind> <selector> (<n>)`; `Hints()`: enter/links/filter/refresh/back;
   `Subject()`: hosts of the selected row; `CopyLines()`: header plus all rows as plain text.

## Out of scope
Multi-select (task 046). Actions (`x`). Knowledge panel. Watch/live updates.

## Acceptance
```
go test -race ./internal/tui/views/ -run Table -v
# with the fake backend and a fixed 100x20 size, driving Update by hand (no tea.Program):
#   TestTableStreamsAllRows (1512 after draining), TestTableRenderSize (every Render is exactly h lines of w cells),
#   TestTableFilter, TestTableSortKeepsCursor, TestTableErrorsShown, TestTableRefreshIgnoresStaleSeq,
#   TestTableRendersOnlyVisibleRows (Render time for 1512 rows < 5ms average over 100 runs)
```

## Pitfalls
- Never render all 1,512 rows and then cut; slice to the visible window first.
- Sorting on every batch is fine; sorting on every `Render` is not.
- While filtering, `j`/`k`/`q`/`:` are text, not navigation. That is what `Capturing()` is for.
- Keep the cursor inside `[0, len(filtered))` after every data or filter change.
