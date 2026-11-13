# Task 014 — Command bar and command registry

- **Milestone:** M0
- **Depends on:** 011
- **Parallel group:** M0-C (with 012, 013, 015)
- **Status:** todo

## Goal
`:` opens a one-line prompt with history and completion; commands are data.

## Context
BUILD-PLAN §4.8 (`Command`), SPEC §13 (navigation). Mockup command line.

## Files
- create `internal/tui/cmdbar.go`, `internal/tui/cmdbar_test.go`
- create `internal/tui/commands.go` (registry + built-ins), `internal/tui/commands_test.go`

## Requirements
1. `type Commands struct` with `Register(c Command) error` (duplicate name/alias → error), `Lookup(name)`,
   `All() []Command` (sorted), and
   `Exec(env *Env, line string) tea.Cmd`: split the first word from the rest; look it up; if not a command, try
   `env.Registry.Lookup(word)` and treat the line as `<kind> <selector>` → `OpenQuery` with the parsed selector;
   otherwise `Flash(FlashError, "unknown command: %s", word)`. A selector parse error becomes a flash, not a
   panic.
2. Built-in commands registered by `func RegisterBuiltins(c *Commands)`: `q`/`quit` (emits `tea.Quit`), `help`
   (emits `OpenDialogMsg` later — for now `RunCommandMsg{"help"}` is handled by the root in task 016; here just
   return a `HelpRequestedMsg{}`), `kinds` (pushes a text view listing kinds — return a `KindsRequestedMsg{}`;
   root handles it), `window` is **not** registered here (mux owns that name). `time <spec>` (alias `t`) → parses
   with `resource.ParseTimeWindow` and emits `WindowChangedMsg` or an error flash.
3. `type CmdBar struct`: `Open(prefill string)`, `Active() bool`, `Update(msg tea.KeyMsg) (cmd tea.Cmd, done bool)`,
   `View(w int) string`.
   - Uses `bubbles/textinput` internally. `enter` → returns `RunCommandMsg{Line}` as a command, appends to history
     (no consecutive duplicates, max 200), closes. `esc` closes. `up`/`down` walk history. `tab` completes: first
     word against command names + kind names + aliases; later words through `Command.Complete` if set; a single
     candidate is inserted, several are shown after the input in `Theme.Dim` (`hosts  help  health`), repeated
     `tab` cycles.
   - `View` returns exactly `w` cells: `:` + input.
4. History is in memory here; `func (b *CmdBar) History() []string` / `SetHistory([]string)` exist so persistence
   can be added by task 024.

## Out of scope
Fuzzy matching (prefix match is enough). Mux commands (022). Persisting history.

## Acceptance
```
go test ./internal/tui/ -run 'CmdBar|Commands' -v
# TestExecKindWithSelector (":hosts role=web dc=ewr" → OpenQueryMsg with 2 terms),
# TestExecUnknown, TestExecBadSelectorFlashes, TestHistoryNoDuplicates, TestTabCompletesKind,
# TestTimeCommand (":time -1h" → WindowChangedMsg)
```

## Pitfalls
- To inspect a `tea.Cmd` in a test, call it: `msg := cmd()`. `tea.Batch` returns a `tea.BatchMsg` (a slice of
  commands).
- `textinput` needs `Focus()` before it accepts keys, and its `Update` must receive the `tea.KeyMsg`.
