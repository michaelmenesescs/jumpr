# Task 140 — Fuzzy matching (fzf-style), wired into filter and completion

- **Milestone:** M0 (UX parity)
- **Depends on:** 012, 014
- **Parallel group:** —
- **Status:** todo

## Goal
The find-as-you-type behaviour people expect from fzf, telescope and k9s: `wbd` finds `web-db-01`, ranked by how well
the letters line up. Pure package plus two call sites; no external binary, no shelling out to `fzf`.

## Context
SPEC §13 ("k9s-style `:kind [selector]` with fuzzy autocomplete, `/` filter"), §8.1.1 ("unit list with fuzzy search").
BUILD-PLAN §4.8 (`Command`, `Commands`), §9 (table filtering).

## Files
- create `internal/fuzzy/fuzzy.go`, `internal/fuzzy/fuzzy_test.go`
- modify `internal/tui/keymap/keymap.go` (one binding: `FuzzyFind`)
- modify `internal/tui/views/table.go` (a fuzzy mode next to the existing substring/`~`regex filter)
- modify `internal/tui/cmdbar.go` (rank tab-completion candidates with `fuzzy.Rank`)

## Requirements
1. `func Match(pattern, s string) (score int, positions []int, ok bool)` — case-insensitive subsequence match.
   - No match → `ok == false`, score 0, nil positions.
   - Empty pattern → `ok == true`, score 0, nil positions (matches everything).
   - `positions` are rune indices into `s`, strictly increasing, one per pattern rune.
   - Scoring, fzf-shaped: contiguous runs beat scattered letters; a match at a word start (`_`, `-`, `.`, `/`, space,
     camelCase boundary, first rune) is strongly favoured; each gap costs; a match starting later in the string costs a
     small leading penalty. Concretely: +16 word-start, +8 contiguous, −1 per skipped rune, −3 for the first offset.
     The exact weights are the implementer's call as long as the ordering properties below hold.
2. `func Rank(pattern string, candidates []string) []int` — indices of candidates that match, best first; ties keep the
   input order (stable); non-matching candidates are dropped; empty pattern returns every index in order.
3. Table: `ctrl+f` opens a **fuzzy** filter prompt in addition to the existing `/` filter (which keeps its substring and
   `~`regex behaviour — do not change `/`). Enter applies, `esc` clears; the fuzzy filter matches against the row's name
   column and its `Labels` values, and must not disturb sorting or the cursor-on-`Ref` behaviour.
4. Command bar: when tab completion has more than one candidate and the typed word is not empty, order candidates with
   `fuzzy.Rank` instead of the current insertion order; a single candidate is still inserted directly.
5. No new dependencies, no `os/exec`, no shelling out to `fzf`. Pure Go, allocation-light on the hot path.

## Out of scope
Preview panes, multi-select, search history. Salt/journal fuzzy search (088/045 own their views).

## Acceptance
```
go test ./internal/fuzzy/ -v
go test ./internal/tui/views/ -run TestTableFuzzy -v
go test ./internal/tui/ -run TestCmdBarFuzzyRank -v
```
Behaviours required:
- `Match` table: empty pattern; exact; scattered (`abc` vs `aXbXc`); no match; case folding; unicode (`ı`/`I`);
  contiguous scores strictly greater than scattered for the same pattern and string.
- Ordering property: for 500 random printable strings, `Match` never panics, positions are strictly increasing, and
  `s[i]` at each position case-folds to the pattern rune.
- `Rank("wbd", {"web-ewr-001","web-db-01","db-01"})` returns web-db-01 before web-ewr-001 (word-start bonus wins).
- Table over the fake 1,512-host fleet: `ctrl+f` + `webewr012` leaves exactly `web-ewr-012`; `/web-ewr` still behaves
  exactly as before (both paths tested).

## Pitfalls
- Do not filter in `Render`; the table filters on data/filter changes only (§8 performance rule, task 012's `refilter`).
- Rune indices, not byte indices — `positions` are documented as rune offsets.
- Keep `Rank` stable; map indices back to the input slice, never sort the strings themselves.
