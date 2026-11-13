# Task 049 — Terminal emulator adapter (spike with a go/no-go list)

- **Milestone:** M2
- **Depends on:** 008
- **Parallel group:** M2-D (with 039, 040, 041, 042, 043)
- **Status:** done

## Goal
Wrap `github.com/charmbracelet/x/vt` behind `vt.Emulator` (BUILD-PLAN §4.9) and prove it is good enough for
terminal panes.

## Context
BUILD-PLAN D6, §4.9, §12 (risk: terminal panes). Verified facts about `x/vt` v0.1.0: `vt.NewSafeEmulator(w, h)`
is the concurrency-safe type; it has `Write`, `Read`, `Resize(w, h)`, `Render() string`, `CursorPosition()`,
`CursorHidden()`, `Scrollback()`, `ScrollbackLen()`, `ScrollbackCellAt(x, y)`, `SetScrollbackSize(n)`,
`SendText`, `Close`. `Render()` returns rows joined by `\n` with SGR sequences; rows are **not** padded to the
full width.

## Files
- create `internal/mux/vt/vt.go`, `internal/mux/vt/vt_test.go`

## Requirements
1. Define the `Emulator` interface in this package exactly as in BUILD-PLAN §4.9.
2. `func New(cols, rows, scrollback int) Emulator` backed by `*vt.SafeEmulator`.
   - `Render()`: take the library's `Render()`, split into rows, and make every row exactly `cols` cells (pad with
     spaces after a reset, truncate ANSI-aware) and exactly `rows` rows.
   - `Cursor()`: position and `!CursorHidden()`.
   - `ScrollbackLines()`: plain text of each scrollback row (build from `ScrollbackCellAt`, trimming trailing
     spaces), oldest first, followed by nothing else (the visible screen is added by the caller).
   - `Responses()`: an `io.Reader` over the emulator's `Read` (terminal replies such as cursor position reports
     and device attributes). **The owner must drain it or `Write` can block.**
   - `Close()` closes the emulator.
3. Add `func PlainScreen(e Emulator) []string` — the visible screen as plain text lines (strip ANSI from
   `Render()`).
4. Go/no-go tests (all must pass, with a goroutine draining `Responses()`):
   - plain text + `\r\n` lands on the right rows;
   - SGR colour is preserved in `Render()` and reset at the end of each row;
   - cursor movement `ESC[5;10H` then text;
   - erase line / erase screen;
   - alternate screen enter, draw, leave restores the primary screen (what `less`/`vim`/`top` do);
   - scrolling 200 lines through a 24-row screen leaves 176 lines in scrollback with the right first and last;
   - `Resize(120, 40)` then `Render()` is 40 rows of 120 cells;
   - wide characters (`日本`) occupy 2 cells and rows stay exactly `cols` wide;
   - writing a device-attributes query `ESC[c` produces bytes on `Responses()` and does not block `Write`;
   - 5 MiB of mixed output written in 4 KiB chunks completes in < 3 s and `Render()` after each chunk never
     panics;
   - concurrent `Write` and `Render` under `-race`.
5. If any go/no-go item cannot be made to pass, **stop**: write what failed into `docs/HANDOFF.md` under
   "Blocked" and do not switch libraries on your own (the planned fallback is `github.com/vito/midterm` behind
   the same interface, but that is the architect's call).

## Out of scope
Input encoding (050), the pane (050), mouse.

## Acceptance
```
go test -race ./internal/mux/vt/ -v -count=1
```

## Pitfalls
- Start the `Responses()` drain goroutine in every test before the first `Write`.
- Row width must be measured in cells (`render.Width`), and padding goes **after** an SGR reset so background
  colours do not bleed into the pane border.
- Do not expose any `x/vt` or `ultraviolet` type outside this package.
