# Task 143 — `:kind` in the muxed app fails with "no view factory for kind unit"

- **Milestone:** M0 fix (found by driving `jumper --demo` in a PTY)
- **Depends on:** 016, 020, 022, 014
- **Parallel group:** —
- **Status:** todo

## Symptom (reproduced)
In `jumper --demo` (110x16 PTY): `:u` + `enter` leaves the host table in place and flashes
`no view factory for kind unit`. `:units` behaves the same. The command bar itself is fine: `Commands.Exec` resolves the
alias and emits `OpenQueryMsg`, and the message does reach a pane.

## Root cause (read from the code — do not re-diagnose from scratch)
`internal/tui/root.go:174` resolves a factory with a **default**: `f := r.factory`, then lets
`env.Factories[spec.Name]` override it. `internal/tui/viewpane.go:108` has no default:

```go
f := p.env.Factories[spec.Name]
if f == nil { return nil, fmt.Errorf("no view factory for kind %s", spec.Name) }
```

Only `journal` is registered in `views.Factories()` (task 045). In the muxed app every query goes through a `ViewPane`,
so every kind except journal — `host`, `unit`, `alert`, and later every real backend kind — fails. The unmuxed path
worked because the root still had its default factory.

## Goal
`ViewPane` resolves views exactly like `Root` does: a registered factory wins, otherwise the default table factory is
used. `:units`, `:hosts`, `:u`, `:alerts` and `:journal` all open the right view inside a pane, with the crumb and
banner updating, and `esc` returning to the previous view.

## Files
- modify `internal/tui/viewpane.go` (hold a default `Factory`; `NewViewPane` takes it as a parameter)
- modify `internal/tui/root.go` (the `NewViewPane(r.env, id, nil, v)` call site passes `r.factory`)
- modify `internal/tui/viewpane_test.go` (the existing "no view factory" test now asserts the **default** path; keep a
  case where the default is nil and the error survives)
- create `internal/tui/cmdnav_test.go`

## Requirements
1. `NewViewPane(env *Env, id mux.PaneID, ctx context.Context, root View, defaultFactory Factory) *ViewPane` — not a
   BUILD-PLAN contract, so the extra parameter is allowed; update every call site and test.
2. Resolution order in `viewFor`: `env.Factories[spec.Name]` if present and non-nil, else `p.defaultFactory`, else the
   current `no view factory for kind %s` error. Mirror `root.viewFor` exactly — same order, same messages.
3. A default-factory view for a `unit` query in the demo backend must be a `*views.Table` with the `unit` spec's
   columns; `journal` must still get its registered `journalFactory` (existing behaviour, existing test passes).
4. No double-push: one `OpenQueryMsg` pushes exactly one view; `esc` pops back to the previous view and its crumb.
5. Unknown kind still flashes `unknown kind: <x>` (from `Registry.Lookup`); unknown command still flashes
   `unknown command: <x>`.

## Out of scope
New factories, the kind picker dialog, command parsing changes (§4.8 is frozen).

## Acceptance
```
go test ./internal/tui/ -run "TestViewPane|TestCmdNav" -v
go test ./...
```
Plus a recorded manual check in the report: 110x16 PTY, `jumper --demo`, `:units⏎` → a `unit` table (crumb `unit`),
`esc` → hosts; `:journal⏎` → journal view; `:bogus⏎` → unknown-command flash, view unchanged.

## Pitfalls
- `TestViewPaneNoFactory` (viewpane_test.go:250) asserts today's broken behaviour; update its intent, keep the
  nil-default case.
- Do not make `env.Factories` non-nil everywhere just to dodge the fallback — the fallback is the fix.
