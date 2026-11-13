# Task 018 — Prefix key state machine and mux key table

- **Milestone:** M1
- **Depends on:** 010
- **Parallel group:** M1-A (with 017)
- **Status:** todo

## Goal
Turn key presses into multiplexer operations the tmux way: prefix, then one key. Pure and time-injected.

## Context
SPEC §21.2 (key table). BUILD-PLAN §5 (`mux.OpMsg`), D7.

## Files
- create `internal/mux/ops.go` (`OpMsg`, op name constants)
- create `internal/mux/prefix.go`, `internal/mux/prefix_test.go`

## Requirements
1. `ops.go`: `type OpMsg struct { Op string; Arg string }` and constants for every op: `OpSplitH = "split-h"`,
   `OpSplitV = "split-v"`, `OpKillPane`, `OpZoom`, `OpFocus` (Arg `left|right|up|down|next|last`), `OpResize` (Arg
   `left|right|up|down`), `OpSwap` (Arg `prev|next`), `OpLayoutNext`, `OpNewWindow`, `OpKillWindow`,
   `OpSelectWindow` (Arg index), `OpNextWindow`, `OpPrevWindow`, `OpWindowList`, `OpRenameWindow`,
   `OpRenameSession`, `OpSessionList`, `OpSSH`, `OpCopyMode`, `OpSync`, `OpCommand`, `OpDetach`, `OpHelp`,
   `OpSendPrefix`.
2. `type KeyTable map[string]OpMsg` and `func DefaultKeyTable() KeyTable` implementing SPEC §21.2 exactly (keys
   are Bubble Tea key strings: `"%"`, `"\""`, `"|"`, `"-"`, `"left"`, `"h"`, `"H"`, `"z"`, `"x"`, `"c"`, `"&"`,
   `"n"`, `"p"`, `"0"`…`"9"`, `"w"`, `","`, `"$"`, `"s"`, `"S"`, `"["`, `"B"`, `":"`, `"d"`, `"?"`, `"o"`, `";"`,
   `"{"`, `"}"`, `" "`).
3. `type Prefix struct` with `NewPrefix(prefixKey string, table KeyTable, now func() time.Time) *Prefix` and
   `func (p *Prefix) Feed(key string) (op *OpMsg, consumed bool)`:
   - idle + prefix key → armed, consumed, no op.
   - armed + prefix key → `OpSendPrefix` (the focused terminal gets a literal prefix), back to idle.
   - armed + key in table → that op; back to idle, **except** resize ops (`H J K L`): stay in "repeat" state for
     600 ms, during which further resize keys produce ops without the prefix; any other key leaves repeat state
     and is **not** consumed.
   - armed + `esc` → idle, consumed, no op. armed + unknown key → idle, consumed, no op.
   - armed state times out after 2 s (next key is then treated as idle).
   - `func (p *Prefix) Armed() bool` for the status line.
   - `func (p *Prefix) SetPrefix(key string)`.

## Out of scope
Executing ops (021). Loading the table from `keymap.yaml` (127).

## Acceptance
```
go test ./internal/mux/ -run Prefix -v
# table-driven with a fake clock: every default binding; double prefix; repeat resize within and after 600ms;
# armed timeout; esc cancels; unknown key consumed; keys in idle state are not consumed
```

## Pitfalls
- `"` must be written `"\""` in Go; `|` and `-` are alternative split keys because the user's tmux uses them.
- Lower-case `h j k l` focus; upper-case `H J K L` resize. Bubble Tea reports shifted letters as upper-case runes.
- No real timers or goroutines here; compare timestamps from `now()`.
