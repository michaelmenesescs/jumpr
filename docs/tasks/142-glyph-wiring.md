# Task 142 — Wire the glyph set end to end (it is currently inert)

- **Milestone:** M0 (UX parity, follow-up to 141)
- **Depends on:** 016, 141, 015
- **Parallel group:** —
- **Status:** done

## Goal
141 built the mechanism and stopped there: `internal/tui/views/table.go` still hardcodes `render.Glyphs("unicode")`,
nothing reads `ui.glyphs` or `JUMPER_GLYPHS`, and the empty-cell placeholder is still a literal em dash. Verified
against a real PTY: `JUMPER_GLYPHS=ascii ./jumper --demo` still renders `●`, `▃▇▃` and `—`. This task makes the setting
real: config → app → every view, with `ascii` producing a frame that contains **no rune above 0x7f**.

## Context
SPEC §13, §16. BUILD-PLAN §11 (`ui.glyphs`), §4.9 (`render.GlyphSet`), §6.1 (only `cmd/` and `internal/app` may touch
`os.Getenv`).

## Files
- modify `internal/render/glyphs.go` (add `Empty rune` to the struct and to all three sets; add `AgeWith`)
- modify `internal/render/glyphs_test.go`
- modify `internal/render/human.go` (`Age` keeps its signature; add `AgeWith(set, time) string`)
- modify `internal/tui/views/table.go` (`NewTable` takes the active set instead of hardcoding `unicode`)
- modify `internal/tui/views/table_format.go` (`cellText` uses `set.Empty` for nil/unknown, and `AgeWith`)
- modify `internal/tui/statusline.go` (health dot from `set.Status*`)
- modify `internal/app/app.go` (resolve once: `ui.glyphs` from config; `JUMPER_GLYPHS` env wins; `auto` → `unicode`)
- tests: `internal/app/glyphs_test.go`, extend `internal/tui/views/glyphs_test.go`

## Requirements
1. `GlyphSet.Empty rune` — `unicode`/`nerd`: `—` (U+2014); `ascii`: `-`.
2. `func AgeWith(set GlyphSet, t time.Time) string` — identical to `Age` except a zero time renders `set.Empty`.
   `Age` stays as it is (other callers exist).
3. Resolution lives in `internal/app` (rule 7): `resolveGlyphs(cfg.UI.Glyphs, os.Getenv("JUMPER_GLYPHS")) (render.GlyphSet, error)`.
   Precedence: non-empty env (`ascii`/`nerd`/`unicode`) → that set; else config value; `auto` → `unicode`. An unknown
   value in either place is an `errs.Config` error naming the source, not a silent fallback.
4. Every consumer takes the set as a parameter; **no package-level mutable state and no `init()`** (AGENTS rule 13).
   `views.NewTable` gains the set (or a `WithGlyphs` default that is no longer `unicode` — pick one and keep every
   caller consistent); `StatusLine` and `cellText` get it the same way.
5. Acceptance is behavioural, not unit-level: a PTY run of `JUMPER_GLYPHS=ascii ./jumper --demo` must emit a frame
   with zero non-ASCII runes, including the header, banner, empty cells and the status line.

## Out of scope
Nerd icon tables, per-kind icons, colour changes, font detection.

## Acceptance
```
go test ./internal/render/ -run "Glyphs|Age" -v
go test ./internal/tui/views/ -run "Glyphs|AsciiFrame" -v
go test ./internal/app/ -run "Glyphs" -v
go test ./...                        # 141 and 015 tests must still pass, unmodified where they assert unicode
```
Behaviours:
- `Glyphs("ascii").Empty == '-'`; `unicode`/`nerd` are `—`; every rune of the ascii set (including `Empty`) < 0x80.
- `resolveGlyphs("auto", "ascii")` → ascii; `resolveGlyphs("nerd", "ascii")` → ascii (env wins);
  `resolveGlyphs("bogus", "")` → `errs.Config`.
- `TestAsciiFrame`: build the root model with the ascii set against the fake backend, render one full frame at
  120×34, assert `strings.IndexFunc(frame, func(r rune) bool { return r > 0x7f }) == -1`.
- Manual (recorded in the report, not automated): `JUMPER_GLYPHS=ascii ./jumper --demo` in a PTY, same assertion.

## Pitfalls
- Do not read the env from `internal/tui` or `internal/render` — rule 7 puts `os.Getenv` in `cmd/` or `internal/app`.
- Do not change `render.Age` or `render.Spark` signatures; other packages call them.
- The wide-rune trap: `Fit` must still return exactly `w` cells for every set; assert it for `nerd` too.
