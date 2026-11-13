# Task 047 — Host overview view

- **Milestone:** M2
- **Depends on:** 044, 045, 013
- **Parallel group:** — (serial)
- **Status:** done

## Goal
`Enter` on a host opens the one-screen summary from the mockup: system facts, pressure bars, memory, failed
units, recent journal errors.

## Context
Mockup host screen. SPEC §5 W1, §15 (single-host overview < 1.5 s warm).

## Files
- create `internal/tui/views/host.go`, `internal/tui/views/host_test.go`
- modify `internal/tui/views/factories.go`, `internal/backend/linux/backend.go` (host `enter` link → kind
  `host-overview`), `internal/backend/linux/kinds.go` (register `host-overview` as a single-row kind, alias
  `overview`)

## Requirements
1. `views.NewHost(env, linuxBackend, host)`; `Init` runs two commands in parallel: `Get` of the host row (overview
   batch) and a journal `List` with priority ≤ 3, limit 8, for the session window. Each part renders as soon as
   it arrives; a part that fails shows its error kind in place.
2. Layout (text, fits 80 columns, degrades by dropping the right column under 100):
   ```
   web-01.lab.example.internal · fetched in 0.8s via bastion.lab.example.internal · sudo -n ✓
   SYSTEM    os Rocky Linux 9.4 · kernel 5.14.0-427 · uptime 41d 3h · systemd degraded · 1 failed unit
   PRESSURE  cpu ██········ 1.2%   mem ·········· 0.0%   io ███······· 3.2%
   LOAD      4.12 3.98 2.10 (8 cpu)        MEMORY  ███████··· 71% of 31.2 GiB
   FAILED UNITS
     nginx.service   failed (Result: exit-code)                      press u
   JOURNAL · errors in window (4 shown)
     13:41:07 nginx: bind() to 0.0.0.0:443 failed (98: Address already in use)
   ```
   Colours: bars Warn above 2 %, Crit above 10 % for PSI; memory Crit above 90 %.
3. Keys: `u` units, `j` journal, `p` processes, `s` sockets (each `OpenQuery` with this host), `r` refresh, `esc`
   back. `Hints()` lists them. `Subject()` = this host (+ first failed unit).
4. `CommandLine()`: `overview --host <host>`. `Crumb()`: the short host name.
5. Reloads the journal part on `WindowChangedMsg`.

## Out of scope
Prometheus sparkline (103 adds a section). Knowledge panel (060). Actions (061/062).

## Acceptance
```
go test -race ./internal/tui/views/ -run Host -v
# with a fake linux backend (transporttest): TestHostRendersSections (contains SYSTEM, PRESSURE, FAILED UNITS,
# nginx.service), TestHostPartialFailure (journal part errs.Sudo → shows "needs-privilege"/"sudo", rest renders),
# TestHostExactSize (80x24, 132x40), TestHostKeysOpenQueries
```

## Pitfalls
- Two independent loads mean two `Seq`-guarded message types; do not block one on the other.
- Exact size: build lines, then `render.Fit`.
