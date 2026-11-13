# Task 094 — Mine and grains explorer

- **Milestone:** M7
- **Depends on:** 093
- **Parallel group:** — (serial)
- **Status:** done

## Goal
`:mine` and `:grains <minion>` (SPEC §8.6).

## Files
- create `internal/backend/salt/mine.go`, `internal/backend/salt/mine_test.go`,
  `internal/tui/views/saltgrains.go`, `internal/tui/views/saltgrains_test.go`
- modify `internal/backend/salt/kinds.go`, `internal/tui/commands_salt.go`

## Requirements
1. Kind `salt-mine` (alias `mine`; global): `salt-run cache.mine tgt='*'` → rows `minion`, `function`, `value`
   (compact, 80 chars), `size`. `enter` → full value pretty-printed (redacted).
2. `:grains <minion>` — all grains from the cache (`cache.grains tgt=<id>`) as a filterable key/value view;
   `L` on a key shows the distribution of that grain across the fleet (count per value, from the full cache) and
   `enter` on a value sets the session targets to the hosts with that value.
3. `:grains --live <minion>` runs `grains.items` on the minion (job) and diffs against the cache (stale cache
   detection).

## Acceptance
```
go test ./internal/backend/salt/ -run 'Mine' -v
go test -race ./internal/tui/views/ -run 'Grains' -v
```

## Pitfalls
- Mine data is whatever admins configured (often `network.ip_addrs`); it can be large. Cap rendering at 256 KiB.
