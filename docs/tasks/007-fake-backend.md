# Task 007 — Fake backend for `--demo` and tests

- **Milestone:** M0
- **Depends on:** 006
- **Parallel group:** M0-B (with 010)
- **Status:** done

## Goal
A deterministic in-memory backend that mirrors the approved mockup so the whole TUI can be built and tested
without any network.

## Context
BUILD-PLAN §4.4. The mockup's fake world: 24 investigation hosts `web-ewr-001…024`, a fleet of 1,512 web hosts
across `ewr`, `lax`, `ord` (504 each), failed nginx on a few hosts, six alerts, eleven pods.

## Files
- create `internal/backend/fake/fake.go` (Backend implementation)
- create `internal/backend/fake/world.go` (data generation)
- create `internal/backend/fake/fake_test.go`

## Requirements
1. `func New(opts Options) *Backend` with `type Options struct { Latency time.Duration; FailHosts map[resource.HostID]errs.Kind; Now func() time.Time }`.
   `Name()` is `"fake"`.
2. Kinds (columns in this order):
   - `host` (aliases `hosts`, `h`), global scope: `host`, `load` (number), `mem` (percent), `psi_io` (percent),
     `failed` (number), `units` (text), `err_rate` (number), `spark` (spark). Labels on every host: `host`,
     `role=web`, `dc=<ewr|lax|ord>`, `env=prod`.
   - `unit` (aliases `units`, `u`), host scope: `unit`, `load`, `active`, `sub`, `description`. Requires
     `Query.Hosts` or `Args["host"]`.
   - `alert` (aliases `alerts`, `al`), global: `sev`, `alert`, `labels`, `source`, `start`, `state`.
3. Data is a pure function of the host name (hash-based, like the mockup's `rnd(name, key)`): same input, same
   rows, every run. Hosts `web-ewr-007` and `web-ewr-012` have `nginx.service` failed and `haproxy.service`
   active. Status: `StatusCrit` when `failed > 0`.
4. `List`: applies `Query.Selector.Matches(labels)` and `Query.Hosts`; streams results in batches of at most 100
   resources with `Done/Total` set; sleeps `Latency` per batch respecting `ctx`; for hosts in `FailHosts` emits a
   `Result{Host, Err}` instead of items. Closes the channel at the end. Unknown kind → error return.
5. `Get` returns one resource by `Ref`; `Links(host)` returns a link to `unit` for that host (key `u`).
6. `Health` returns `HealthOK` with detail `fake data`.

## Out of scope
Pods, journal, processes, sockets, actions (added by later tasks if needed for tests).

## Acceptance
```
go test -race ./internal/backend/fake/ -v
# must include: TestDeterministic (two Lists equal), TestSelectorFilters (dc=ewr → 504),
# TestFleetSize (1512), TestFailHostsYieldErr, TestListStopsOnCancel (no goroutine left writing)
```

## Pitfalls
- The producer goroutine must `select` on `ctx.Done()` when sending, or a cancelled view leaks it forever.
- Do not use `math/rand` with a time seed. Use `hash/fnv` on `name + ":" + key`.
- Host IDs are bare names here (`web-ewr-007`), already lower case.
