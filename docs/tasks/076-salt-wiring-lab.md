# Task 076 — Salt wiring, `:salt` commands, `--via salt`, pivots, lab tests (M4 exit)

- **Milestone:** M4
- **Depends on:** 068, 069, 072, 073, 075
- **Parallel group:** — (serial; touches `app`)
- **Status:** done

## Goal
Salt is a first-class citizen in the running app. End of the first fully usable release (v0.5).

## Context
BUILD-PLAN §2 (M4 demo), SPEC §8.6.

## Files
- modify `internal/app/build.go`
- create `internal/tui/commands_salt.go`, `internal/tui/commands_salt_test.go`
- modify `internal/tui/commands_run.go`, `internal/tui/commands_probe.go` (`--via`)
- modify `internal/backend/linux/backend.go` (host → minion links via an injected `LinkProvider`)
- create `internal/backend/salt/lab_test.go` (`//go:build lab`)
- modify `docs/HANDOFF.md`, `README.md`

## Requirements
1. `app`: when `salt.master` is set: runner (over the SSH transport) → salt backend → register; grains inventory
   source when configured (`type: salt`); salt executor registered in `execs["salt"]` for `generic.run` and
   probes; salt planner registered on the bus. Health dot `salt` appears in the status line.
2. Commands:
   - `:minions`, `:keys`, `:jobs [--arg fun=state.]` (plain kind navigation already works; make sure aliases do).
   - `:salt state [sls]` → the dry-run view on the session targets.
   - `:salt -C '<expr>'` / `-G` / `-E` / `-L` / `-N` / glob → `ParseNative` → `--preview-target` on the master →
     map minions to hosts → set the session targets (label = the native expression) → open `:hosts` for them.
     Minions without an inventory host are counted in the flash (`14 minions → 12 hosts, 2 unmapped`).
   - `:salt target` shows how the current target set compiles (`Compiled.Explain` + the CLI string).
   - `:salt run <fun> [args…] [k=v…]` → proposes `salt.cmd.run`-style generic module execution through the bus
     (spec `salt.module.run`: any function; destructive; display is the full CLI line).
   - `:salt ping` → `test.ping` on the targets as a job; opens the job detail.
3. `--via ssh|salt` on `:run` and `:probe`. When the target set is larger than `salt.suggest_over` and Salt is
   healthy and `--via` was not given, the plan dialog shows a hint `1,512 targets: consider --via salt`.
   First use of the Salt transport in a session shows the job-cache warning (SPEC §8.6 table) once.
4. Pivots: host rows get links `salt minion` (key `M`) and `salt jobs for this host` (key `J`) when the host
   maps to a minion; minion rows link back (task 068). Implement with a small interface in `backend`:
   `type LinkProvider interface { ExtraLinks(r resource.Resource) []resource.Link }` that the root consults for
   every registered backend when building a row's links (so `linux` does not import `salt`).
5. Lab tests: `TestLabSaltMinionsUp` (all lab minions up), `TestLabSaltGrainsInventory` (role/dc labels on
   hosts; `role=web` → 3 hosts; `dc=lab` on all), `TestLabSaltTargetCompile` (`role=web` compiles natively and
   `--preview-target` returns the same 3 minions), `TestLabSaltJobPing`, `TestLabSaltExecutorMatchesSSH` (same
   `uname -r` per host via both transports), `TestLabSaltDryRunGrouped` (create drift on one web host with
   `labtest.Break`: append a comment to a Salt-managed file → 2 groups), `TestLabSaltApplyThroughBus` (apply `nginx` sls to the drifted host: canary = that
   host; afterwards dry run is clean), `TestLabSaltNoReturn` (`labtest.Break` stops `salt-minion` on `web-03` → that
   minion is `no return` within the timeout; restarted in cleanup), `TestLabSaltNodegroupTarget` (`-N webs`).
6. `README.md`: a short "what works today" section (M0–M4) and how to run against the lab. `docs/HANDOFF.md`:
   M4 done; v0.5 checklist; anything the Salt captures contradicted.

## Out of scope
State tree, pillar, mine, key management, events (M7). Detach (M5).

## Acceptance
```
make check
make lab-test
./jumper --config-dir lab/jumper       # manual: BUILD-PLAN §2 M4 demo script
go test -race ./internal/tui/ -run 'Salt' -v
# TestSaltNativeTargetSetsTargets (fake runner preview → targets count + flash), TestSaltTargetExplain,
# TestRunViaSalt (plan dialog shows "via salt:" and the root warning), TestSuggestSaltHint
```

## Pitfalls
- `--preview-target` needs a function argument on some versions (`salt -C '…' --preview-target test.ping`); pass
  `test.ping` always — nothing is executed in preview mode. Verify against `testdata/salt/preview-compound.*`.
- Do not make `linux` import `salt` or the reverse; the `LinkProvider` hook exists to avoid that.
