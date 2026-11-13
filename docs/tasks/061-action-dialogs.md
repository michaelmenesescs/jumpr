# Task 061 — Action UI: menu, plan, confirmation, denial, rollout progress

- **Milestone:** M3
- **Depends on:** 057, 059
- **Parallel group:** M3-C (with 060)
- **Status:** todo

## Goal
The dialogs from the mockup, driven by the bus.

## Context
SPEC §11. BUILD-PLAN §4.5, §5 (`ProposalMsg`, `RunEventsMsg`). Mockup dialogs: actions menu, plan with canary and
batch numbers, typed-count confirmation, policy denial, rollout log with pause after canary.

## Files
- create `internal/tui/dialog_actions.go`, `internal/tui/dialog_plan.go`, `internal/tui/dialog_run.go`,
  `internal/tui/actions.go` (glue), `internal/tui/actions_test.go`
- modify `internal/tui/env.go` (`Bus bus.Bus`, `Planners map[string]action.Planner`), `internal/tui/msgs.go`,
  `internal/tui/root.go`, `internal/tui/views/table.go`, `internal/tui/views/fanout.go` (`x`)

## Requirements
1. `x` in a view collects the selection (marked rows, else the current row; in the fan-out view the hosts of the
   selected group) and asks the root to open the **actions menu**: specs from every planner's `Actions(selection)`,
   de-duplicated by `Spec.ID`, destructive ones marked `!`. Empty → flash `no actions for <kind>`.
2. Choosing a spec with `Params` opens a small form (one text input per param, defaults filled, choices cycle
   with left/right), then a command calls `Bus.Propose` → `ProposalMsg`. While planning, the dialog shows
   `planning…` with a spinner and `esc` cancels the context.
3. **Plan dialog** (from a `ProposalMsg`): title; target count and the first 10 hosts (`+N more`, `l` shows all in
   a scrollable list); the exact command (`Steps[0].Display`; when displays differ per host say
   `per-host commands (l to list)`); `Via`; warnings; `Preview` (scrollable) when present; rollout line
   `canary 1 → pause → 4 batches of 6 · stop if >5% fail`; then the confirmation control:
   - `ConfirmYesNo` / `ConfirmList`: `y` confirms, `n`/`esc` cancels.
   - `ConfirmTypedCount`: a text input `type 1512 to confirm`; enter with the right number confirms; wrong →
     shake/flash, stay.
   - `Denied`: no confirmation control; shows `denied by policy`, the rule and the reason; only `esc`.
4. On confirm: `Bus.Execute` → **run dialog**: a log of lines as events arrive (`canary · 1 host`, per-host
   `ok`/`failed rc=1 <first line>`, `batch 2/4 · 6 hosts · ok 6`), counters, elapsed time. On `PhasePaused`:
   `canary ok — c continue · esc abort`. `esc` during batches → confirm `abort remaining batches?`. On finish:
   summary line, `enter` closes, `o` opens the full results as a fan-out-style grouped view (reuse
   `views.NewFanoutFromResults`: add that constructor to task 048's file).
5. The run dialog can be hidden with `h` (rollout continues; a status-line badge `▶ rollout 2/4` shows it; `:runs`
   lists active runs from `Bus.Active()` and reopens the dialog).
6. After a run finishes, the focused view gets a refresh.

## Out of scope
Planners (062, 063). Agent approval queue (SPEC §17).

## Acceptance
```
go test -race ./internal/tui/ -run 'Action|Plan|RunDialog' -v
# with bustest planner + teatest: TestMenuListsSpecs, TestPlanShowsCommandAndRollout,
# TestYesNoConfirmExecutes, TestTypedCountWrongThenRight (21 targets), TestDeniedShowsRule (no execute possible),
# TestCanaryPauseContinue, TestAbort, TestHideAndReopenRun
```

## Pitfalls
- Never call `Bus.Propose`/`Execute` in `Update`; they do I/O. Use commands.
- Reading run events uses the `NextBatch` pattern on `Run.Events()`.
- The typed count compares against the number of targets in the proposal, not the number visible on screen.
