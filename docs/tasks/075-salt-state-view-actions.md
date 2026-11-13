# Task 075 — `:salt state` dry-run view and Salt actions through the bus

- **Milestone:** M4
- **Depends on:** 057, 061, 070, 071, 072, 074
- **Parallel group:** — (serial)
- **Status:** todo

## Goal
SPEC W4: dry-run highstate on the target set grouped by identical diff, then apply from the same view with a
mandatory preview, canary and rolling batches.

## Context
SPEC §8.6 (states, actions), §11. BUILD-PLAN §4.5.

## Files
- create `internal/backend/salt/actions.go`, `internal/backend/salt/actions_test.go`
- create `internal/tui/views/saltstate.go`, `internal/tui/views/saltstate_test.go`
- modify `internal/tui/views/factories.go`, `internal/tui/views/saltjob.go` (use `StateRun.Normalized` for state
  functions)

## Requirements
1. `func (b *Backend) DryRun(ctx, minions []string, sls string, sel resource.Selector) (*Job, Compiled, error)` —
   `state.apply test=True` (or `state.sls <sls> test=True`) on the compiled target. Do not pass `queue=True`:
   a minion busy with another state run returns an error string, which is shown as its own group.
2. View `views.NewSaltState(env, saltBackend, sls string)` (kind `salt-state`, `Args["sls"]`): runs `DryRun` on
   the session targets (mapped to minions; hosts without a minion are listed as `not a minion`), streams
   `MinionResult`s, parses each with `ParseStateReturn`, groups by `Normalized()`.
   - banner: `24 minions · 22 clean · 2 would change · 0 failed · 0 no return` + the compiled target
     (`Explain`).
   - rows: one per group: `MINIONS`, `SUMMARY` (`clean (38 states)` / `2 would change`), first changed state id.
   - `enter` → group detail: per-state rows (`RenderRows`) for a representative minion + member list; `d` → the
     normalized text diffed against the clean/majority group; `o` → full normalized text.
   - `x` → actions for the selected group's minions (or marked groups).
3. Planner (`Owns` prefix `salt.`), specs:
   - `salt.state.apply` (destructive; param `sls`, empty = highstate), `HasDryRun`.
   - `salt.service.restart` (param `name`), `salt.cmd.run` (param `cmd`; destructive),
     `salt.saltutil.refresh_pillar`, `salt.saltutil.sync_all`, `salt.job.kill` (target = a running job; runs
     `saltutil.kill_job <jid>` on the job's minions; destructive).
   - `Actions(selection)`: salt-minion rows and state-view groups → state.apply, service.restart, cmd.run,
     refresh_pillar, sync_all; salt-job rows in state `running` → job.kill; host rows that map to minions →
     state.apply.
4. `Plan`:
   - one `Step` per minion; `Display` = the Salt CLI line for that minion's batch as the user would type it,
     e.g. `salt -L web-01 state.apply`; `Via` = `salt:<master>`; warning `runs as root via the Salt master`.
   - `salt.state.apply`: **mandatory preview** — run the dry run for exactly these targets (60 s timeout),
     `Preview` = the grouped summary (`22 clean · 2 would change`) followed by the normalized text of each
     non-clean group with its minion list. If the dry run itself fails for every minion → return the error (no
     plan). Minions with nothing to change are still included (apply is idempotent) but called out.
   - Step `Run`: `Start` a job for that single minion (the executor's micro-batching does not apply here; within
     a rollout batch the bus runs steps concurrently, and each step is its own job) and wait for its result:
     state functions → `ParseStateReturn`; failed states or errors → `RC 1`; output = `Summary()` + normalized
     text. Other functions → rc from `Retcode`, output = compact return.
   - Rolling batches are the bus's (SPEC: Salt's own `-b` is not used).
5. Timeouts: state apply step 15 min (`Options.StateTimeout`), others 60 s.

## Out of scope
State tree browser (092). Pillar. Orchestrate runners.

## Acceptance
```
go test -race ./internal/backend/salt/ -run 'DryRun|Planner|Plan' -v
# TestActionsForSelections, TestPlanStateApplyHasPreview (preview contains "would change" and the diff),
# TestPlanFailsWhenDryRunFailsEverywhere, TestStepFailsOnFailedState, TestKillJobPlan,
# TestDisplayStrings ("salt -L web-01 state.apply nginx")
go test -race ./internal/tui/views/ -run SaltState -v
# TestStateViewGroups (24 minions: 22 clean, 2 same diff → 2 groups, banner text), TestStateViewNotAMinion,
# TestStateViewActionsOnGroup
```

## Pitfalls
- A dry run is read-only for sitrep's purposes and does not go through the bus; an apply always does.
- Never run `state.apply` without `test=True` outside a bus step.
- One job per minion per step is deliberate: it keeps abort, thresholds and per-host records identical to every
  other action. Do not "optimize" it into one big job.
