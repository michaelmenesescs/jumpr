# Task 052 — Wire SSH, inventory and the Linux backend into the app (M2 exit)

- **Milestone:** M2
- **Depends on:** 035, 044, 045, 047, 048, 050, 051
- **Parallel group:** — (serial; touches `app`)
- **Status:** done

## Goal
`./jumpr --config-dir lab/jumpr` works against the lab: the M2 demo script end to end.

## Context
BUILD-PLAN §2 (M2 demo), §3 (`app` is the composition root), §8.

## Files
- modify `internal/app/app.go`, `internal/app/build_ssh.go`
- create `internal/app/build.go` (assembles everything from config), `internal/app/build_test.go`
- create `internal/backend/linux/lab_test.go` (`//go:build lab`)
- create `internal/tui/commands_health.go` (`:health`, `:inventory refresh`)
- modify `docs/HANDOFF.md`

## Requirements
1. `build.go`: from `config.Config` build, in order: logger → `Prompter` → SSH `Resolver`/`Pool`/`Transport`
   (agent socket getter reads the latest value from a mutex-protected holder seeded from `Options.Env`) →
   inventory sources per config (`ssh_config`: exclude hosts that are only used as jumps; `static`) → inventory
   `Store` (`LoadCache`, then `Refresh` in the background; flash on source errors) → Linux backend → registry →
   `Env` (`Exec`, `Shells`, `Inventory`, `Sets`) → commands → root. Return a `Close()` that shuts the pool down.
2. Not `--demo` and no usable source → start anyway with an empty inventory and a flash explaining how to add
   hosts; `--demo` keeps using the fake backend only.
3. Start view: `:hosts` when the inventory has hosts.
4. Background inventory refresh every `inventory.cache_ttl` via `tea.Tick` → `tea.Cmd`; the status line shows
   `inv stale` when `Age()` exceeds twice the TTL.
5. `:health` opens a text view: per backend state + detail, pool stats (bastions, targets, leases), inventory age
   and per-source errors. `:inventory refresh` forces a refresh.
6. Lab tests (`internal/backend/linux/lab_test.go`): `TestLabHostsOverview` (all nine hosts return a row with a
   non-empty `os`; three distinct distros seen), `TestLabUnitsContainSSH`, `TestLabSocketsWithSudo` (port 22
   listener has a process), `TestLabJournalWindow`, `TestLabFailedUnitsAfterFault` (`labtest.Fault(t,
   "haproxy-443")` → `nginx.service` failed on the affected host; probe grouping shows it as the differing
   group).
7. Update `docs/HANDOFF.md`: M2 done, how to run the demo, measured timings (single host overview warm/cold,
   9-host probe).

## Out of scope
Actions, investigations, Salt.

## Acceptance
```
go test -race ./internal/app/ -run Build -v     # builds from a temp config with a static inventory and sshtest
                                                # servers; root starts; ":hosts" lists them; Close() leaves no
                                                # goroutines dialing
make check
make lab-test                                   # with the lab up
./jumpr --config-dir lab/jumpr                # manual: BUILD-PLAN §2 M2 demo script
```

## Pitfalls
- Never block startup on the network: the first frame comes from the cache (or empty) and data streams in.
- Bastion hosts should not appear as targets for fleet probes by default; they are still reachable by name
  (`:ssh bastion`).
