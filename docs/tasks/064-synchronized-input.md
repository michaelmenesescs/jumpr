# Task 064 — Synchronized input (broadcast line, raw sync) and shell guard rails

- **Milestone:** M3
- **Depends on:** 061, 050
- **Parallel group:** — (serial)
- **Status:** todo

## Goal
Type once, send to several terminal panes — safely. Plus the recording and read-only rules for shells.

## Context
SPEC §11.1 (normative), §21.2 (`prefix B`, `:sync`). BUILD-PLAN §4.5.

## Files
- create `internal/tui/sync.go`, `internal/tui/sync_planner.go`, `internal/tui/sync_test.go`
- modify `internal/mux/termpane/termpane.go` (`SendLine`, `Host()`, input line tracker),
  `internal/mux/model.go` (raw sync routing), `internal/tui/root.go`, `internal/tui/commands_mux.go`,
  `internal/tui/commands_ssh.go`

## Requirements
1. Shell guard rails: opening a terminal pane appends an `input` entry `shell opened on <host>` (investigation or
   audit); in read-only mode `:ssh` is refused unless `policy.shell_in_readonly`; panes on protected hosts show
   `protected` in the title.
2. **Broadcast line** — `prefix B` / `:sync` opens a prompt `broadcast to N panes ❯`. Targets = connected
   terminal panes in the active window (none → flash). Enter builds an `action.Request` for spec `mux.sync.line`
   (planner `mux`, defined in `sync_planner.go`): one step per pane, `Display` = the line, `Target.Host` = the
   pane's host; `Run` = `pane.SendLine(line)` (writes the line + `\r`). It goes through the normal plan dialog
   and bus rollout (canary = the focused pane if it is a terminal, else the first). Policy `CheckLine` applies
   through the plan's displays. The bus records it.
3. **Raw sync** — `:sync raw` (and `:sync off`): proposes spec `mux.sync.raw` whose single step turns the mode on
   (so it is confirmed and recorded). Refused when: more than `mux.max_sync_panes` terminal panes, any pane host
   is protected, read-only, or `policy.sync.raw == deny`. While on:
   - every key routed to a terminal pane in the window is delivered to all connected terminal panes of that
     window;
   - status line shows `SYNC ×N` in the Crit style; pane borders of synced panes use the Crit colour;
   - a line tracker per sync session reconstructs the typed line (printable runes, backspace, `ctrl+u`,
     `ctrl+w`; any escape sequence or `ctrl+r`/`up` marks the line as `…(edited)`); on `enter`:
     `Policy.CheckLine(line)` → if denied, the Enter is **not** sent, `ctrl+u` is sent instead, and a flash shows
     the rule; otherwise send and append an `input` entry `sync ×N: <line>`.
   - auto-off after 5 minutes without keys, on window change, and when a pane is added or removed.
4. The UI states the limit plainly in the raw-sync plan warning: `deny rules here are a seat belt, not a
   boundary: history, aliases and editors bypass them`.

## Out of scope
Sync across windows or to view panes. Recording raw keystrokes or shell output.

## Acceptance
```
go test -race ./internal/tui/ -run 'Sync|Broadcast|ShellGuard' -v
# with fake shells in 3 terminal panes:
# TestBroadcastLinePlansAndSends (all 3 shells receive "uptime\r" after confirm; canary first),
# TestBroadcastDeniedLine ("systemctl stop sshd" → denial, nothing sent),
# TestRawSyncRequiresConfirm, TestRawSyncFansOutKeys, TestRawSyncBlocksDeniedEnter, TestRawSyncAutoOff,
# TestRawSyncRefusedOverLimit, TestShellOpenRecorded, TestSSHRefusedInReadOnly
```

## Pitfalls
- Raw sync must never include view panes, and must never be on in a window the user is not looking at.
- The line tracker is best effort by design; when unsure, record `…(edited)` rather than a wrong line.
- Prefix keys are not broadcast; only keys that would have gone to the focused terminal are.
