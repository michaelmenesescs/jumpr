# Task 010 — Theme and keymap

- **Milestone:** M0
- **Depends on:** 005
- **Parallel group:** M0-B (with 007)
- **Status:** done

## Goal
One place for colours and one place for key names, so views never hard-code either.

## Context
SPEC §13 (dark, high contrast, 256-colour and truecolor), BUILD-PLAN §6 (colour profile is fixed per process),
§4.8 (`KeyHint`).

## Files
- create `internal/tui/theme/theme.go`, `internal/tui/theme/theme_test.go`
- create `internal/tui/keymap/keymap.go`, `internal/tui/keymap/keymap_test.go`

## Requirements
Theme:
1. `type Theme struct` with `lipgloss.Style` fields: `Base, Dim, Faint, Bold, Accent, OK, Warn, Crit, Header,
   Selected, Border, BorderFocused, StatusBar, HintKey, HintText, Flash{Info,OK,Warn,Error}, Title, Crumb`.
2. `func Dark() *Theme` using `lipgloss.AdaptiveColor`-free fixed colours given as `lipgloss.Color("…")` with
   256-colour codes (e.g. OK `"42"`, Warn `"214"`, Crit `"203"`, Accent `"75"`, Dim `"245"`, Faint `"240"`,
   Selected background `"237"`).
3. `func SetProfile(name string)` — `auto|truecolor|256|16|none` → `lipgloss.SetColorProfile(...)`. `auto` leaves
   the detected profile. This is the only package-level mutable state allowed (BUILD-PLAN §6.1 rule 7).
4. `func (t *Theme) StatusStyle(s resource.Status) lipgloss.Style` — Unknown → `Base`, OK → `OK`, Warn → `Warn`,
   Crit → `Crit`, NA → `Faint`. (`resource` is a leaf package; importing it here is fine.)

Keymap:
5. `type Binding struct { Keys []string; Help string }` where keys are Bubble Tea key strings (`"enter"`, `"esc"`,
   `"ctrl+a"`, `"u"`, `"?"`).
6. `type Map struct` with fields for the global bindings: `Up, Down, PageUp, PageDown, Top, Bottom, Open, Back,
   Command, Filter, Help, Refresh, Quit, Actions, Note, Finding, Knowledge, Timeline, Yank, Follow, TimeWindow,
   Diff, Errors`. `func Default() *Map` — defaults: `k/up`, `j/down`, `pgup/ctrl+u`, `pgdown/ctrl+d`, `g/home`,
   `G/end`, `enter`, `esc`, `:`, `/`, `?`, `r`, `q` (only at the root of a stack; views do not handle it), `x`,
   `n`, `F`, `K`, `T`, `y`, `w`, `t`, `d`, `e`.
7. `func (b Binding) Matches(key string) bool`.
8. `func ParsePrefix(s string) (string, error)` — converts config form `C-a` to the Bubble Tea key string
   `ctrl+a`; `C-\` → `ctrl+\\`, `C-@` → `ctrl+@`. Error (`errs.Config`) otherwise.
9. `func DisplayKey(teaKey string) string` — `ctrl+a` → `C-a`, `enter` → `enter`, `ctrl+u` → `C-u`.

## Out of scope
Loading `keymap.yaml` (task 127). Mux prefix table (task 018).

## Acceptance
```
go test ./internal/tui/theme/ ./internal/tui/keymap/ -v
# keymap: ParsePrefix table incl. errors; Matches; DisplayKey round-trip for ctrl keys
# theme: Dark() returns non-nil; StatusStyle covers all five statuses; with SetProfile("none") rendered text has no ESC
```

## Pitfalls
- In tests call `theme.SetProfile("none")` or `"256"` explicitly; never depend on the terminal running the test.
- Bubble Tea v1 names: `ctrl+a`, `shift+tab`, `pgup`, `pgdown`. A space key is `" "`.
