# Task 143 — `:kind` from the command bar must actually switch the view

- **Milestone:** M0 fix (found by smoking `jumper --demo`)
- **Depends on:** 016, 022, 014
- **Parallel group:** —
- **Status:** todo

## Symptom (reproduced, not theorised)
In `jumper --demo`, typing `:` then `units` then `enter` leaves the host table untouched: no new view, no crumb change,
no flash. Same for `:u`. Meanwhile `Commands.Exec` does build an `OpenQueryMsg` (its own tests pass), and
`internal/tui/viewpane.go` handles `OpenQueryMsg` — but `internal/tui/root.go` and the mux in front of it do not, so in
the muxed app the message is dropped before it reaches a pane.

## Goal
The command bar's primary verb works end to end in the real app: `:units`, `:u`, `:hosts`, `:alerts` switch the focused
pane to that kind's view, the crumb and banner update, and an unknown word flashes an error instead of doing nothing.

## Files
- modify `internal/tui/root.go` (route `OpenQueryMsg`; it owns the stack the panes wrap)
- modify `internal/mux/` routing so a pane-level message reaches the focused pane's `ViewPane` (only if the root-level
  route is genuinely insufficient — decide from the code and say which in the report)
- modify `internal/tui/root_test.go`, `internal/tui/mux_pane_test.go` (or the mux test file that exists)
- create `internal/tui/cmdnav_test.go`

## Requirements
1. `Commands.Exec` already returns the right message — do **not** change its contract (§4.8). Fix routing, not parsing.
2. From the command bar, `:` + a registered kind or alias + `enter` must replace the focused pane's view with a new
   `Table` (or the registered factory's view) for that kind, keeping the old view reachable with `esc`.
3. The crumb, banner and hint bar must reflect the new view (they already do once the view is on the stack).
4. A selector argument works: `:units failed` shows only failed units; a bad selector or unknown kind flashes
   `FlashError` and leaves the current view alone.
5. No double-open: the same query must not push two views (guard on the existing cursor/stack semantics).
6. In the demo backend, `:units` must show unit rows (`unit` kind, host-scoped), and `esc` must return to hosts.

## Out of scope
New commands, fuzzy ranking of kinds, the kind picker dialog (already covered by 014/140).

## Acceptance
```
go test ./internal/tui/ -run "TestCmdNav|TestOpenQueryRouting" -v
go test ./...
```
Plus a recorded manual check in the report: in a 110x18 PTY, `jumper --demo`, `:units⏎` shows a `unit` table with the
crumb `unit`, `esc` returns to `host`; `:bogus⏎` flashes an unknown-command error and stays put.

## Pitfalls
- Do not let the mux swallow the message when no pane has focus yet at startup.
- `OpenQueryMsg` must remain handled exactly once; both root and `ViewPane` handling it would push two views.
- Keep the keymap contract: `esc` pops, `:` opens the bar.
