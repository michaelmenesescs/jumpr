# Task 114 — Chef drift signals and `chef-client` through the bus

- **Milestone:** M10
- **Depends on:** 113, 061
- **Parallel group:** — (serial)
- **Status:** done

## Goal
SPEC §8.7 drift and actions: jumper runs `chef-client` on targets via its own fan-out, not `knife ssh`.

## Files
- create `internal/backend/chef/drift.go`, `actions.go`, `whyrun.go`, `_test.go` files, `lab_test.go` (lab tag)
- create `internal/tui/commands_chef.go`

## Requirements
1. `:chef drift` on the target set: table with last check-in age, last run status when available (node attribute
   path configurable `chef.run_status_attr`, default none → column hidden; SPEC §20 open question 3), and
   run-list differences grouped (`20 identical · 2 differ`).
2. `:chef whyrun`: fan-out `sudo -n chef-client --why-run --no-color -l warn` (cinc-client when
   `chef.client: cinc-client`), 10 min timeout, output normalized (strip timestamps, durations, PIDs) and
   grouped; the summary line (`Chef Infra Client finished, 2/45 resources would have been updated`) parsed into
   columns. Why-run is a dry run but can execute `ruby_block`s that are not why-run safe: the plan says so, and it
   goes **through the bus** as `chef.whyrun` (normal risk).
3. `chef.client.run` (destructive): preview = the why-run result for the same hosts (mandatory, like Salt);
   step = `sudo -n chef-client --no-color -l warn`; fails on non-zero rc.
4. Lab: db-01 shows a failed/stale signal; why-run on web hosts groups into one group.

## Acceptance
```
go test -race ./internal/backend/chef/ -run 'Drift|WhyRun|Plan' -v
make check && make lab-test
```

## Pitfalls
- `chef-client` holds a run lock; a second concurrent run waits. Use `--run-lock-timeout 30` and classify the lock
  message as a clear failure.
