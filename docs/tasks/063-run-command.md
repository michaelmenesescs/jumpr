# Task 063 — `:run`: ad-hoc command through the bus

- **Milestone:** M3
- **Depends on:** 057, 048
- **Parallel group:** M3-D (with 062)
- **Status:** todo

## Goal
SPEC W5: `:run 'ss -s'` on the target set — planned, confirmed, rolled out, grouped, recorded.

## Context
SPEC §5 W5, §8.1 (":run … always treated as a write"), §11.

## Files
- create `internal/bus/generic/run.go`, `internal/bus/generic/run_test.go`
- create `internal/tui/commands_run.go`, `internal/tui/commands_run_test.go`

## Requirements
1. Planner `generic` with spec `generic.run` (`Title "run command"`, `RiskDestructive`, params `command`, `sudo`
   (`yes|no`, default `no`), `via` (`ssh`, later `salt`)).
   `func New(execs map[string]transport.Executor, defaultVia string) *Planner`.
2. `Plan`: one step per target, `Display` = the command exactly as typed, prefixed with `sudo -n ` when sudo;
   script = the command; no preview; `Via` = executor name; warning `runs as root on every target` when via is a
   root transport. Step `Run` → `Exec`; `RC != 0` is a failed step.
3. `:run [--sudo] [--via ssh|salt] [--no-rollout] <command…>`: targets = session target set (empty → flash).
   Everything after the flags is the command, taken verbatim (no re-splitting or unquoting beyond removing one
   pair of surrounding quotes if the whole command is quoted).
   - Flow: propose → plan dialog → execute → run dialog → on finish open the grouped results view.
   - `--no-rollout` (only honoured for ≤ 20 targets and non-protected sets) runs all steps as one batch without a
     canary pause; still confirmed.
4. `:rerun` repeats the last `:run` line (shown in the plan again).
5. History: `:run` lines are kept in the command history like any other.

## Out of scope
Saving as a plugin (117). Salt transport (073 registers itself in `execs`).

## Acceptance
```
go test -race ./internal/bus/generic/ -v
go test -race ./internal/tui/ -run 'RunCommand' -v
# TestRunParsesFlags (":run --sudo ss -tlnp | wc -l" → command "ss -tlnp | wc -l", sudo yes),
# TestRunQuotedWholeCommand, TestRunNoTargetsFlashes, TestRunFlowToGroupedResults (teatest, fake executor),
# TestRunDeniedByPolicy (":run reboot" → denial dialog), TestRunRecordedInAudit (no investigation open)
```

## Pitfalls
- Do not try to detect "read-only" commands to skip confirmation. Every `:run` is a write by definition.
- The command is user text sent to a shell on purpose; it is quoted once as the `sh -c` argument by `shq.Wrap`
  and nowhere else.
