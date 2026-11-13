# Task 086 — Unit-file diff across hosts and the unit fleet view

- **Milestone:** M6
- **Depends on:** 048, 085
- **Parallel group:** — (serial)
- **Status:** todo

## Goal
`:unitdiff nginx.service` on the target set: identical unit files grouped, differences one keystroke away. Plus
"the same unit across N hosts" as a table.

## Files
- create `internal/tui/commands_unit.go`, `internal/tui/commands_unit_test.go`
- modify `internal/backend/linux/catalog.go` (parameterized probe `unit-file`), `internal/backend/linux/kinds.go`
  and `list.go` (kind `unit-fleet`)

## Requirements
1. `:unitdiff <unit>` → fan-out view (048) of `systemctl cat <unit> --no-pager` with a normalizer that drops
   nothing but trailing whitespace; hosts where the unit does not exist form their own group (`no such unit`).
   Unit name validated. Title `unit-file <unit>`.
2. `:unit <unit>` (kind `unit-fleet`, `Args["unit"]`): one row per target host: `host`, `active`, `sub`,
   `since`, `restarts`, `enabled`, `mem`. One `systemctl show <unit> -p ActiveState,SubState,
   ActiveEnterTimestamp,NRestarts,UnitFileState,MemoryCurrent` per host. `enter` → unit detail on that host; `x`
   → actions on marked rows (so "restart nginx on these 12" starts from here).
3. From the unit detail view: `D` runs `:unitdiff` for that unit on the session targets.

## Acceptance
```
go test -race ./internal/tui/ -run 'UnitDiff|UnitFleet' -v     # fake executor: 5 hosts, 4 identical files, 1 with
                                                               # an extra drop-in → "4 identical · 1 differ"
go test -race ./internal/backend/linux/ -run 'UnitFleet' -v
```

## Pitfalls
- `systemctl cat` prints `# /etc/systemd/system/nginx.service.d/override.conf` headers; keep them — a host with
  a drop-in must not group with one without.
