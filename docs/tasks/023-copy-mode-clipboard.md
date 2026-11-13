# Task 023 — Copy mode and clipboard

- **Milestone:** M1
- **Depends on:** 022
- **Parallel group:** — (serial; touches the root or `app`)
- **Status:** todo

## Goal
`prefix [` freezes the focused pane's text, lets you move, search, select and yank to the clipboard.

## Context
SPEC §21.3. BUILD-PLAN §6.1 rule 6.

## Files
- create `internal/clip/clip.go`, `internal/clip/clip_test.go`
- create `internal/mux/copymode.go`, `internal/mux/copymode_test.go`
- modify `internal/tui/root.go` (handle `OpCopyMode`: overlay on the focused pane), `internal/tui/root_test.go`

## Requirements
Clipboard:
1. `type Clipboard interface { Copy(text string) error }`.
2. `func NewPbcopy(run func(name string, stdin []byte) error) Clipboard` — runs `pbcopy` (the `run` function is
   injected; `app` supplies one built on `os/exec` with a 2 s timeout).
3. `func NewOSC52(w io.Writer, inTmux bool) Clipboard` — writes `ESC ] 52 ; c ; <base64> BEL`; when `inTmux`, wraps
   it in the tmux passthrough `ESC P tmux; ESC <seq> ESC \`. Text over 74,000 bytes of base64 → error.
4. `func Multi(cs ...Clipboard) Clipboard` — tries each until one succeeds.

Copy mode:
5. `func NewCopyMode(lines []string, w, h int) *CopyMode` — cursor starts at the last line.
6. `Update(key string) (done bool, yank string)`: `h j k l`/arrows, `0`/`$`, `g`/`G`, `ctrl+u`/`ctrl+d`, `w`/`b`
   (word), `/` + text + `enter` search forward, `n`/`N`; `v` starts/stops character selection, `V` line selection;
   `y` or `enter` → returns the selection (or the current line when none) and `done`; `q`/`esc` → `done` with
   empty yank (first `esc` clears a selection).
7. `View() string` exactly `w × h`: the text window around the cursor, selection in reverse video, cursor cell
   highlighted, last line a status: `[copy] 120/3,400  v:select  y:yank  q:quit`.
8. Root: on `OpCopyMode` take `Focused()` content's `CopyLines()`, open copy mode in place of that pane's content
   (keys go to it first); on yank call the clipboard and flash `yanked N lines`.
   `app` builds `clip.Multi(pbcopy, osc52(out, inTmux))`.

## Out of scope
Mouse selection. `F` to mark a finding (task 059 adds it).

## Acceptance
```
go test ./internal/clip/ ./internal/mux/ -run 'Clip|OSC52|CopyMode' -v
# OSC52 exact bytes for "hi" with and without tmux wrapping; Multi falls through;
# CopyMode: movement bounds, char selection across lines, line selection, search wraps, yank text exact,
#   wide runes do not split
go test -race ./internal/tui/ -run 'Copy' -v     # prefix [ , V, j, y → fake clipboard received 2 lines
```

## Pitfalls
- Selection is in cells, text is in runes: convert with `runewidth` so wide characters are selected whole.
- Do not call `pbcopy` from the `clip` package directly; it must be injectable so tests and the server work.
