# Task 062 — Linux planner: systemctl verbs and kill

- **Milestone:** M3
- **Depends on:** 055, 044
- **Parallel group:** M3-D (with 063)
- **Status:** done

## Goal
`linux` backend implements `action.Planner`.

## Context
SPEC §8.1 (actions). BUILD-PLAN §4.5. Mockup: restart nginx, stop haproxy, denied stop sshd.

## Files
- create `internal/backend/linux/actions.go`, `internal/backend/linux/actions_test.go`

## Requirements
1. Specs (IDs `linux.systemctl.<verb>`): `start`, `stop` (destructive), `restart`, `reload`, `enable`, `disable`
   (destructive), `mask` (destructive), `unmask`, plus `linux.systemctl.daemon-reload` (host selection),
   `linux.kill` (process selection; param `signal`, choices `TERM KILL HUP INT USR1 USR2`, default `TERM`;
   destructive), `linux.journal.vacuum` (host selection; param `size` default `1G`; destructive).
2. `Actions(selection)`: unit rows → the systemctl specs (hide `start` for active units and `stop`/`restart`/
   `reload` for inactive ones only when **all** selected rows agree); process rows → kill; host rows →
   daemon-reload, vacuum. Mixed kinds → nothing.
3. `Plan(ctx, req)`: one step per target. Display and script are identical strings built with `shq.Join`, e.g.
   `systemctl restart nginx.service`, run with `transport.Command{Sudo: true}`. `Display` is the short readable
   form `sudo -n systemctl restart nginx.service` (policy patterns are written against this form), not the
   fully wrapped `sudo -n -- sh -c '…'` line.
   Validate unit names (`shq.ValidUnitName`) and signals; invalid → `errs.Config`.
   - `Preview`: for systemctl verbs on ≤ 20 targets, a read-only batch gets `systemctl is-active <unit>` and
     `NRestarts` per host and renders `web-01: failed (restarts 5)` lines; more targets → grouped counts
     (`18 active · 2 failed`). A preview failure adds a warning; it does not fail the plan.
   - `Title`: `restart nginx.service on 12 hosts`.
   - Step `Run`: `Exec` with a 60 s timeout; `StepResult{RC, Output: stdout+stderr, Err}`; then for
     start/restart/reload wait 1 s and append the output of `systemctl is-active <unit>`; if it is not `active`
     the step fails (`RC = 3`) with output `unit is <state> after restart`.
4. `Owns(specID)` → prefix `linux.`.

## Out of scope
`:run` (063). File edits. Package operations.

## Acceptance
```
go test -race ./internal/backend/linux/ -run 'Action|Plan' -v
# TestActionsForUnitSelection, TestPlanRestartDisplay ("sudo -n systemctl restart nginx.service"),
# TestPlanRejectsBadUnitName ("nginx; rm -rf /"), TestPreviewShowsStates, TestStepFailsWhenUnitNotActive,
# TestKillPlan ("sudo -n kill -TERM 2231"), TestStopSSHDDeniedByDefaultPolicy (through a real bus.New)
```

## Pitfalls
- `Plan` must not write. The preview uses only `is-active` and `show`.
- PIDs are numbers from the parsed row, never free text.
