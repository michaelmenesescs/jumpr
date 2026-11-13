# Task 084 — Unit detail view with tabs: status, show, cat, journal

- **Milestone:** M6
- **Depends on:** 045, 062
- **Parallel group:** M6-A (with 088)
- **Status:** done

## Goal
isd parity for one unit: `Enter` on a unit opens a tabbed detail (SPEC §8.1.1).

## Context
SPEC §8.1.1 table (status / show / cat / journal rows). Parsers from task 041 (`ParseShow`, `UnitDetail`).

## Files
- create `internal/backend/linux/unit_detail.go`, `internal/backend/linux/unit_detail_test.go`
- create `internal/tui/views/unit.go`, `internal/tui/views/tabs.go`, `internal/tui/views/unit_test.go`
- modify `internal/backend/linux/backend.go` (unit `enter` link → kind `unit-detail`), `internal/tui/views/factories.go`

## Requirements
1. `func (b *Backend) UnitInfo(ctx, host, unit) (UnitInfo, error)` — one batch with sections `status`
   (`systemctl status <u> --no-pager -n 0 --full`; rc 3 is normal for inactive units), `show`
   (`systemctl show <u> --no-pager`), `cat` (`systemctl cat <u> --no-pager`; rc 1 when the unit has no file).
   `UnitInfo{Status string; Props map[string]string; Detail Detail; Cat string; DropIns []string}`. The unit name
   is validated with `shq.ValidUnitName`.
2. `tabs.go`: a reusable tab strip (`1 status  2 show  3 cat  4 journal  5 deps`), `tab`/`shift+tab` and number
   keys switch; each tab is a `tui.View`; the container forwards `Capturing`, `Hints`, `CopyLines` of the active
   tab.
3. Tabs: **status** — key facts first (state coloured, since, restarts, main PID, memory, tasks, result,
   unit-file state) then the raw status text; **show** — `NewKV` of all properties, filterable; **cat** — the
   unit file with `# /path` header lines accented and drop-in sections marked `drop-in`; **journal** — the
   journal view (045) for this unit, follow available. The **deps** tab is added by task 085.
4. `x` opens actions for this unit (062). `r` refreshes all tabs. Crumb = unit name.
5. `CommandLine()`: `unit-detail --host <h> --arg unit=<u>`.

## Out of scope
Dependencies, timers (085). Drop-in preview (087).

## Acceptance
```
go test -race ./internal/backend/linux/ -run UnitInfo -v     # golden status/show/cat; inactive unit rc 3 ok;
                                                             # unit without file; invalid name rejected
go test -race ./internal/tui/views/ -run 'Unit|Tabs' -v      # tab switching, exact size, actions key, filter in show
```

## Pitfalls
- `systemctl status` exit codes: 0 active, 3 inactive/failed, 4 no such unit. Only 4 is "not found".
- One batch for all three sections; the journal tab loads lazily on first visit.
