# Task 097 — Nodegroups, beacons, schedules, salt-ssh awareness, `salt-call` local mode (M7 exit)

- **Milestone:** M7
- **Depends on:** 092, 093, 094, 096
- **Parallel group:** M7-B (with 095)
- **Status:** todo

## Goal
The remaining rows of SPEC §8.6.

## Files
- create `internal/backend/salt/nodegroups.go`, `internal/backend/salt/local.go`, `internal/backend/salt/roster.go`
  and their `_test.go` files
- modify `internal/backend/salt/target.go`, `internal/tui/commands_salt.go`, `internal/backend/salt/source.go`,
  `docs/HANDOFF.md`
- create `internal/backend/salt/lab_m7_test.go` (`//go:build lab`)

## Requirements
1. Nodegroups: `salt-run config.get nodegroups` → `map[name]expr` (value may be a string or a list of minion
   IDs). `:salt nodegroups` lists them with member counts (`--preview-target -N <name>`). Selector set syntax
   `@salt:<name>` resolves through `-N` preview; the targeting compiler emits `N@<name>` when a selector consists
   only of such a set.
2. Beacons and schedules per minion: `beacons.list return_yaml=False` and `schedule.list return_yaml=False` as
   jobs → tables (`name`, `enabled`, config summary / next run). Read-only.
3. salt-ssh awareness: if `salt.roster` (config key, default `/etc/salt/roster`) is readable on the master
   (`sudo cat`, parsed as YAML: `id: {host, user, port}`), expose roster entries as an inventory source
   `salt-roster` (label `salt.roster=true`). Executing through `salt-ssh` is refused unless
   `salt.allow_salt_ssh: true`; when allowed, `:salt run --ssh …` builds `salt-ssh` calls through the same
   runner and every plan carries the warning `salt-ssh copies a runtime to /var/tmp on the target (not
   zero-footprint)`.
4. `salt-call` local mode (`local.go`): for one host over the normal SSH executor:
   `sudo -n salt-call --local --out=json grains.items`, and `sudo -n salt-call --out=json state.apply test=True`
   (this one contacts the master from the minion; 120 s timeout). `:salt local state [--host h]` opens the same
   state rows view as the dry run for that single host. Used automatically as a fallback in the host overview's
   "salt" section when the master is `HealthDown`.
5. Lab test: nodegroup `webs` resolves to the three web hosts; `salt-call` local dry run on `web-01` parses;
   pillar tree for `web-01` contains keys and no values; events stream sees a `job-ret` for a `test.ping`.

## Acceptance
```
go test ./internal/backend/salt/ -run 'Nodegroup|Beacon|Schedule|Roster|Local' -v
make check && make lab-test
```

## Pitfalls
- `salt-call` output is `{"local": …}`: unwrap the `local` key.
- Do not enable salt-ssh execution by default under any circumstances.
