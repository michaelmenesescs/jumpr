# Task 044 — Linux backend: host, unit, process, socket kinds

- **Milestone:** M2
- **Depends on:** 036, 038, 039, 040, 041, 043
- **Parallel group:** — (serial)
- **Status:** done

## Goal
`backend.Backend` named `linux` that serves the four core kinds through probes over any `transport.Executor`.

## Context
BUILD-PLAN §4.4. SPEC §8.1. Mockup tables for hosts, units, procs, sockets.

## Files
- create `internal/backend/linux/backend.go`, `internal/backend/linux/kinds.go`,
  `internal/backend/linux/probes.go`, `internal/backend/linux/list.go`
- create `internal/backend/linux/backend_test.go`

## Requirements
1. `func New(o Options) *Backend` with `Options { Exec transport.Executor; Inventory inventory.Inventory;
   Concurrency int; Timeout time.Duration; Nonce func() string; Now func() time.Time; Log *slog.Logger }`.
2. Kinds (`kinds.go`), column IDs in order:
   - `host` (aliases `hosts`, `h`; ScopeGlobal): `host`, `load` (L1), `mem` (percent), `psi_io` (percent, some
     avg10), `failed` (number), `units` (text: first failed unit names, comma-separated, max 3), `os`, `kernel`,
     `uptime` (duration), `state` (systemd state). Status: Crit when `failed > 0` or `state == degraded`; Warn when
     mem > 90 or psi_io > 10.
   - `unit` (aliases `units`, `u`; ScopeHost): `unit`, `load`, `active`, `sub`, `description`; plus `host` as the
     first column when the query spans more than one host.
   - `process` (aliases `procs`, `ps`, `p`; ScopeHost): `pid`, `user`, `cpu`, `mem`, `rss` (bytes), `elapsed`,
     `command`. Default sort `-cpu`.
   - `socket` (aliases `sockets`, `ss`, `s`; ScopeHost): `proto`, `state`, `recvq`, `sendq`, `local`, `peer`,
     `process`.
3. `probes.go`: the section definitions. Overview batch: `loadavg` (`cat /proc/loadavg`), `meminfo`, `psi`
   (`for f in cpu memory io; do echo "== $f"; cat /proc/pressure/$f 2>/dev/null; done`), `os`
   (`cat /etc/os-release`), `kernel` (`uname -r`), `uptime` (`cat /proc/uptime`), `nproc`, `sysstate`
   (`systemctl is-system-running`), `failed` (`systemctl list-units --state=failed --plain --no-legend
   --no-pager`). Units: `systemctl list-units --all --plain --no-legend --no-pager`. Processes: the `ps` command
   of task 043. Sockets: the `ss` command with `Sudo: true`.
4. `List(ctx, q)`: hosts = `q.Hosts`, else `Inventory.Select(q.Selector)`, plus `Args["host"]`. No hosts →
   error `errs.Config` "no hosts match". Run the kind's batch with `fanout.Run`; for each `HostResult` emit one
   `backend.Result` (`Host`, `Items` or `Err`, `Done`, `Total`). Section-level problems do not fail the host:
   put the section error into `Result.Err` only when **no** rows could be produced for that host; otherwise the
   affected cells are `nil` and the resource gets `Labels["_degraded"] = "<section>: <kind>"` (for example a
   host row whose `psi` section is missing, or a socket list without process names because `sudo -n` failed:
   retry that one section without sudo and mark the rows degraded).
   - `rc 1` from `systemctl is-system-running` is normal (degraded); do not treat non-zero RC as failure where
     the tool uses it for state.
5. `Get(ctx, ref)`: runs the kind's batch for `ref.Host` and returns the matching row (`errs.NotAvailable` if
   absent).
6. `Links(r)`: host → `unit` (key `u`), `process` (`p`), `socket` (`s`), each with `Query.Hosts = [host]`;
   unit → none yet (journal arrives in 045); process/socket → none.
7. `Health`: `HealthOK` when the last 20 host results had < 50 % transport errors, `Degraded` otherwise,
   `Unconfigured` when the inventory is empty. No network call in `Health`.
8. Every `Resource`: `Ref{Backend: "linux", Kind, Host, Name}`, `Labels` copied from the inventory host,
   `FetchedAt`, typed `Raw` (the parsed struct).

## Out of scope
Journal (045), actions (062), other kinds (M6), the overview view (047).

## Acceptance
```
go test -race ./internal/backend/linux/ -run Backend -v
# with transporttest.Fake replying with golden outputs wrapped in batch markers (helper in the test builds the
# marked-up reply from a map of section → text using the same Batch and nonce "deadbeef"):
# TestListHosts (3 hosts, one failing with errs.Timeout → 2 item results + 1 err result, Done/Total right),
# TestListUnitsSortedFailedFirst, TestListProcessesDefaultSort, TestSocketsNeedPrivilegeDegrades,
# TestListNoHosts, TestGetUnit, TestLinksFromHost, TestOneRoundTripPerHost (fake.Calls() == number of hosts)
```

## Pitfalls
- One `Exec` per host per List. If you find yourself calling `Exec` once per section, you have defeated the
  batch.
- The nonce must be injectable so tests can pre-compute marked-up replies.
- Do not import `tui` here.
