# Task 141 — Glyph sets: render correctly on the fonts people actually use

- **Milestone:** M0 (UX parity)
- **Depends on:** 003, 009, 010
- **Parallel group:** —
- **Status:** todo

## Goal
Every glyph jumper emits comes from one declared set, so the UI looks right in a Nerd Font terminal (JetBrainsMono NF,
what k9s/isd/nvim users run) and still looks right — not like boxes — over ssh into a stock console font.
`ui.glyphs` selects it; `ascii` is guaranteed printable in any font.

## Context
SPEC §13 (TUI/UX), §15 (rendering targets). BUILD-PLAN §11 (config keys — **this task adds `ui.glyphs`; the amendment
is already in §11 and in the example config**), §4.9 (`render.Spark`, `render.Bar`).

## Files
- create `internal/render/glyphs.go`, `internal/render/glyphs_test.go`
- modify `internal/config/config.go`, `internal/config/config_test.go` (`ui.glyphs`, default `auto`, validation)
- modify `internal/tui/theme/theme.go` (status markers and separators come from the active set)
- modify `internal/tui/views/table_format.go` (a `Spark` cell renders with the active set)

## Requirements
1. `type GlyphSet struct { Name string; Spark []rune; BarFull, BarEmpty rune; Ellipsis rune; StatusOK, StatusWarn, StatusCrit, StatusNA, StatusUnknown rune; Separator, Arrow rune }` with
   `Glyphs(name string) (GlyphSet, error)` and `GlyphNames() []string`.
2. Three sets, exactly:
   - `unicode` (default for `auto`): Spark `▁▂▃▄▅▆▇█`, Bar `█`/`·`, Ellipsis `…`, status `● ● ● ○ ?`, Separator `·`, Arrow `→`.
   - `nerd`: as `unicode`, but status markers are the Nerd Font pair `󰄬`(ok)`󰀦`(warn)`󰅚`(crit)`󰄱`(na/unknown)` and Separator `│`.
     (These are Private Use Area codepoints — only valid when the terminal font is a Nerd Font.)
   - `ascii`: Spark `_.,-~+=#`, Bar `#`/`-`, Ellipsis `~`, status `+ ! x . ?`, Separator `|`, Arrow `>`.
     **Every rune in this set must be < 0x80**, and that is asserted by a test.
3. `auto` resolves at startup: `nerd` if `$JUMPER_GLYPHS` is `nerd`/`ascii` (explicit override wins), otherwise `unicode`.
   Never detect by probing the font — there is no portable way to ask a terminal; `auto` must stay deterministic and
   pure. Document that `nerd` is opt-in.
4. `Spark` and `Bar` keep their current behaviour for the `unicode` set (task 009's tests must keep passing unchanged).
   Add `SparkWith(set GlyphSet, values []float64, width int) string` and `BarWith(set GlyphSet, fraction float64, width int) string`;
   the existing `Spark`/`Bar` become thin wrappers over the `unicode` set.
5. Theme: `StatusStyle` and the status-marker helpers use the active set's runes. `theme.Dark()` stays colour-only;
   the glyph set is passed in, not global state (no package-level mutable state, per AGENTS rule 13).
6. Config: `ui.glyphs: auto|unicode|nerd|ascii`, default `auto`, validated like `ui.color` with the file path in the error.

## Out of scope
Icon fonts for kinds, per-backend icon tables, automatic font detection, colour changes.

## Acceptance
```
go test ./internal/render/ -run TestGlyphs -v
go test ./internal/config/ -run TestGlyphs -v
go test ./internal/tui/views/ -run TestTableAsciiGlyphs -v
go test ./...        # task 009's Spark/Bar tests must be untouched and still pass
```
Behaviours required:
- `GlyphNames()` returns all three names, sorted; `Glyphs("nope")` errors.
- Every rune of `ascii` `< 0x80` (loop over the struct via reflection, assert).
- `SparkWith(ascii, …)` returns exactly `width` ASCII cells for 8 series × 20 widths (same property as task 009).
- A table with a `Spark` column rendered under `ascii` contains no rune above 0x7f in the whole frame.
- `ui.glyphs: bogus` fails `Validate()` with an `errs.Config` naming `ui.glyphs`.

## Pitfalls
- Do not change `render.Spark`/`Bar` signatures — other packages already call them (task 009 contract).
- The nerd markers are two-codepoint pairs in some nerd releases; store one rune each and accept the width-2 case via
  `ansi.StringWidth` in tests, or pick a single-codepoint variant — but `Fit` must still return exactly `w` cells.
- No `init()` registration and no global mutable glyph state.
