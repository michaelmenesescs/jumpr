# Task 022 — Root integration: panes in the app, window tabs, mux commands

- **Milestone:** M1
- **Depends on:** 020, 021
- **Parallel group:** — (serial; touches the root)
- **Status:** todo

## Goal
`sitrep --demo` with real panes and windows: the M1 demo script up to zoom.

## Context
SPEC §21.2. BUILD-PLAN §2 (M1 demo), §5.

## Files
- modify `internal/tui/root.go`, `internal/tui/root_test.go`
- create `internal/tui/commands_mux.go`, `internal/tui/commands_mux_test.go`
- create `internal/tui/dialog_prompt.go` (one-line text prompt dialog), `internal/tui/dialog_list.go` (pick list)
- modify `internal/app/app.go` (prefix selection)

## Requirements
1. The root's body is now a `*mux.Model` instead of a bare stack. The pane factory creates a `ViewPane` whose
   root view is the start query (new panes open the same kind as the pane they were split from; fall back to the
   start query).
2. Key routing order: dialog → command bar → `mux.Update` for keys **when** the prefix is armed or the key is the
   prefix or the focused content has `RawKeys()` or is `Capturing()`; otherwise root global keys (`:`, `?`,
   `ctrl+c`, `q` at stack root with a single pane and window) and then `mux.Update`.
3. Root-level messages coming back from `mux.Update` are handled as before (flash, dialogs, `RunCommandMsg`, …).
   New: `OpCommand` opens the command bar; `OpHelp` opens help with a "Panes and windows" section generated from
   the key table; `OpRenameWindow`/`OpRenameSession` open the prompt dialog; `OpWindowList` opens the list dialog
   (enter selects); `OpDetach` flashes `detach needs the session server (coming in M5); use :q to quit`;
   `OpCopyMode`, `OpSync`, `OpSSH`, `OpSessionList` flash `not available yet`.
4. Status line: session name, window tabs from `mux.Tabs()`, prefix shown as `^A` and highlighted while armed.
   Hints come from the focused pane.
5. `commands_mux.go` registers: `split`, `vsplit`, `kill-pane`, `zoom`, `focus <dir>`, `resize <dir> [n]`,
   `layout [even-h|even-v|main-left|next]`, `window [new|kill|next|prev|<n>]`, `rename <name>` (window),
   `rename-session <name>`, `detach`, `copy`, `sync`. Each returns a command producing the matching `mux.OpMsg`
   and has `Usage`, `Help` and argument completion.
6. Killing a pane or window whose content reports `Busy() bool == true` (optional interface) asks for confirmation
   with a y/N dialog first. (No content is busy yet; terminal panes will be.)
7. `app.Run`: prefix = `mux.prefix`, or `mux.prefix_in_tmux` when `o.Env["TMUX"] != ""`; converted with
   `keymap.ParsePrefix`. `main.go` fills `Options.Env` with `TERM`, `COLORTERM`, `TMUX`, `SSH_AUTH_SOCK`.

## Out of scope
Copy mode (023), persistence (024), terminal panes (050).

## Acceptance
```
go test -race ./internal/tui/... -run 'Root|Mux' -v
# TestRootSplitShowsTwoPanes: send ctrl+a then "%": output contains two pane titles
# TestRootCommandInSecondPane: after split, ":alerts" changes only the focused pane (other still shows hosts)
# TestRootNewWindowTabs: ctrl+a c → status line contains "0:" and "1:"; ctrl+a 0 returns
# TestRootZoom, TestRootKillPaneReturnsToOne, TestMuxCommandsRegistered (every command has Usage and Help),
# TestPrefixInTmux (Options.Env TMUX set → ctrl+b is the prefix, ctrl+a is passed to the view)
make check && ./sitrep --demo     # manual: M1 demo steps 1–5
```

## Pitfalls
- `:` typed in a view pane opens the command bar; after `prefix :` it must also open from any pane.
- The global `q` must not quit while more than one pane or window exists; it would be too easy to lose a layout.
- All earlier root tests must still pass; update them only where the frame legitimately changed.
