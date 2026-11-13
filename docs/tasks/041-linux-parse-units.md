# Task 041 — Linux parsers: systemd units

- **Milestone:** M2
- **Depends on:** 005
- **Parallel group:** M2-D (with 039, 040, 042, 043, 049)
- **Status:** done

## Goal
Parse `systemctl list-units` (plain text) and `systemctl show` (properties).

## Context
SPEC §8.1, §8.1.1. BUILD-PLAN D8 (plain text on purpose).

## Files
- create `internal/backend/linux/parse_units.go`, `internal/backend/linux/parse_units_test.go`
- create `testdata/linux/units/rocky9/list-units.txt`, `testdata/linux/units/rocky9/show-nginx.txt`

## Requirements
1. `ParseListUnits(out string) ([]Unit, error)` for
   `systemctl list-units --all --plain --no-legend --no-pager`:
   ```
   nginx.service                loaded    failed   failed    The nginx HTTP and reverse proxy server
   haproxy.service              loaded    active   running   HAProxy Load Balancer
   sshd.service                 loaded    active   running   OpenSSH server daemon
   logrotate.timer              loaded    active   waiting   Daily rotation of log files
   display-manager.service      not-found inactive dead      display-manager.service
   ```
   → `Unit{Name, Load, Active, Sub, Description string}`. First four whitespace-separated fields, the rest joined
   is the description. A leading `●` or `*` token (some versions print it for failed units even with `--plain`) is
   dropped. Lines with fewer than 4 fields are skipped. Blank output → empty slice, no error.
2. `func (u Unit) Type() string` — suffix after the last dot (`service`, `timer`, `socket`, …).
3. `func (u Unit) Status() resource.Status` — `failed` → Crit; `activating`/`deactivating`/`reloading` or
   `auto-restart` sub → Warn; `active` → OK; else Unknown.
4. `ParseShow(out string) map[string]string` for `systemctl show <unit> --no-pager`: split each line at the first
   `=`; later duplicates of a key are joined with `\n`.
5. `func UnitDetail(props map[string]string) Detail` extracting: `ActiveState`, `SubState`, `Result`, `NRestarts`
   (int), `ExecMainStatus` (int), `ExecMainPID`, `MemoryCurrent` (int64; `[not set]` and the max uint64
   `18446744073709551615` → −1), `TasksCurrent`, `ActiveEnterTimestamp` and `InactiveEnterTimestamp` (parsed with
   layout `Mon 2006-01-02 15:04:05 MST`; unparseable → zero time), `FragmentPath`, `DropInPaths` (space
   separated), `UnitFileState`.
6. `func SortUnits(us []Unit)` — failed first, then by type (`service` first), then by name.

## Out of scope
Timers and dependencies (085). Running the commands.

## Acceptance
```
go test ./internal/backend/linux/ -run 'ListUnits|Show|UnitDetail|SortUnits' -v
# includes: bullet-prefixed line, not-found unit, description with many spaces, empty output, show with "=" in
# the value (ExecStart={ path=/usr/sbin/nginx ; argv[]=… }), MemoryCurrent not set
```

## Pitfalls
- Timestamps carry a zone abbreviation (`EST`, `UTC`); Go parses unknown abbreviations as UTC offsets of zero —
  acceptable; do not fail on them.
- Do not use `--output=json` for list-units (D8).
