# Task 017 — Mux layout tree (pure)

- **Milestone:** M1
- **Depends on:** 002
- **Parallel group:** M1-A (with 018)
- **Status:** todo

## Goal
The binary split tree that decides where every pane is. Pure data and arithmetic; no Bubble Tea, no I/O.

## Context
BUILD-PLAN §4.9 (`Layout`, `Rect`, `Direction`, `PaneID`). SPEC §21.1.

## Files
- create `internal/mux/ids.go` (`PaneID`, `WindowID`, `Rect`, `Direction`)
- create `internal/mux/layout.go`, `internal/mux/layout_json.go`, `internal/mux/layout_test.go`

## Requirements
1. Node: either a leaf (`pane PaneID`) or a split (`horizontal bool` = children side by side; `ratio float64` =
   share of the first child, 0.1–0.9; `a`, `b` children).
2. Implement every method listed in BUILD-PLAN §4.9 with these semantics:
   - `SplitH(target, new)`: the leaf `target` becomes a horizontal split with `target` left and `new` right,
     ratio 0.5. `SplitV`: `target` on top, `new` below. Unknown target or duplicate `new` → error.
   - `Close(id)`: remove the leaf; its sibling replaces the parent split. `next` is the first pane (depth-first)
     of the sibling subtree. Closing the only pane → `empty = true`.
   - `Rects(total)`: recursive. A horizontal split gives the first child `round(W*ratio)` columns and the second
     the rest (no gap; panes draw their own borders). Vertical likewise with rows. Clamp so each child gets at
     least 10 columns / 3 rows when the total allows; the rectangles always tile `total` exactly.
   - `Neighbor(id, d, total)`: using `Rects`, the pane whose rectangle touches `id`'s edge in direction `d` and
     overlaps it on the other axis the most; ties → the one with the smallest X then Y. None → `false`.
   - `Resize(id, d, cells, total)`: find the nearest ancestor split whose axis matches `d` and where moving the
     divider in direction `d` is meaningful for `id`; change `ratio` by `cells / axisSize`; clamp 0.1–0.9 and
     the minimum sizes. No matching ancestor → no-op.
   - `Swap(a, b)` swaps two leaves. `Even(horizontal)` sets ratios so that all leaves in each maximal chain of
     splits along that axis get equal size.
   - `Panes()` depth-first, first child first.
3. `func (l *Layout) Contains(id PaneID) bool`, `func (l *Layout) Count() int`.
4. `layout_json.go`: `MarshalJSON`/`UnmarshalJSON`. Form: leaf `{"pane":3}`; split
   `{"dir":"h"|"v","ratio":0.5,"a":{…},"b":{…}}`. Unmarshal validates ratios and unique pane IDs.

## Out of scope
Zoom (a window flag, task 019). Rendering borders (021). Preset layouts beyond `Even`.

## Acceptance
```
go test ./internal/mux/ -run Layout -v
# TestRectsTile: for 200 random split/close sequences and several totals (80x24, 132x40, 203x51):
#   rectangles do not overlap, cover the total area exactly, and each is >= 10x3 when total allows
# TestSplitClose, TestNeighborGrid (2x2 grid: all 4 directions from each pane), TestResizeClamps,
# TestSwap, TestEven, TestJSONRoundTrip, TestCloseLastPaneEmpty
```

## Pitfalls
- Rounding: compute the second child as `total − first`, never round both.
- `Neighbor` must work from rectangles, not from tree position; tree siblings are not always visual neighbours.
- Do not store rectangles in the tree; they depend on the window size and are recomputed.
