# Task 068 — Salt backend skeleton: minions, keys, health

- **Milestone:** M4
- **Depends on:** 066, 039
- **Parallel group:** M4-B (with 069, 071)
- **Status:** done

## Goal
`backend.Backend` named `salt` with the read-only kinds `salt-minion` and `salt-key`, and the health dot.

## Context
SPEC §8.6 (minions and keys; health dot). Golden files from task 067 when present.

## Files
- create `internal/backend/salt/backend.go`, `internal/backend/salt/kinds.go`,
  `internal/backend/salt/minions.go`, `internal/backend/salt/minions_test.go`

## Requirements
1. `func New(o Options) *Backend` with `Options { Runner *Runner; Inventory inventory.Inventory; Master
   resource.HostID; Now func() time.Time; Log *slog.Logger }`.
2. Parsers (inline samples; prefer `testdata/salt/*.json`):
   - `salt-key -L` → `{"minions": ["web-01"], "minions_pre": ["new-01"], "minions_rejected": [],
     "minions_denied": []}` → `[]Key{ID, State}` with states `accepted|pending|rejected|denied`.
   - `salt-key -F` → `{"minions": {"web-01": "aa:bb:…"}, "minions_pre": {…}, "local": {"master.pub": "…"}}` →
     fingerprint per ID.
   - `salt-run manage.status` → `{"up": [...], "down": [...]}`.
   - `salt-run manage.versions` → `{"Master": "3007.1", "Up to date": [...] | {"id": "ver"}, "Minion requires
     update": {...}}` — tolerate both list and map shapes; unknown → empty version.
3. Kind `salt-minion` (aliases `minions`, `minion`; global): columns `minion`, `status` (`up|down|unaccepted`),
   `key`, `os`, `role`, `dc`, `version`, `host` (the inventory host it maps to, `—` when none). One `List` =
   three runner calls in parallel (`key -L`, `manage.status`, `cache.grains` via task 069's parser when
   available; before 069 leave grain columns nil). Status: down → Crit, pending key → Warn.
4. Kind `salt-key` (aliases `keys`; global): `id`, `state`, `fingerprint`. Pending first.
5. `MinionHost(id string) (resource.HostID, bool)` — inventory `Resolve(id)`; `HostMinion(host) (string, bool)` —
   the host's `salt.id` label, else an alias that is an accepted key, else the short name if it is an accepted
   key. Cache the accepted-key set for 60 s.
6. `Links`: minion → host overview (`enter`, when mapped), → `salt-job` list filtered to that minion (key `J`;
   kind arrives in 072).
7. `Health`: cached result of `manage.status` refreshed at most every 30 s by `Health` itself with a 5 s timeout:
   all accepted minions up → OK (`12/12 minions up`); some down → Degraded (`10/12 up`); runner error → Down with
   the error kind; `master == ""` → Unconfigured.

## Out of scope
Jobs, states, key actions (095), grains parsing (069).

## Acceptance
```
go test -race ./internal/backend/salt/ -run 'Key|Minion|Health|ManageVersions' -v
# with transporttest.Fake serving the JSON: TestListMinionsMerged (up, down, pending), TestKeysPendingFirst,
# TestMinionHostMapping (short id ↔ FQDN host), TestHealthStates (ok/degraded/down/unconfigured),
# TestHealthIsCached (two calls within 30s → one manage.status)
```

## Pitfalls
- `manage.status` pings every minion and can take 10+ s on a large master; never call it from `List` and
  `Health` at the same moment — share one in-flight call (`singleflight`).
- Minion IDs are not always FQDNs; never assume `id == HostID`.
