# Task 048 — Read-only probes and the grouped fan-out view

- **Milestone:** M2
- **Depends on:** 036, 037, 046
- **Parallel group:** — (serial)
- **Status:** todo

## Goal
`:probe failed-units` on the target set → "N identical · M differ · K errors", drill into groups, diff against the
majority. This is SPEC W2.

## Context
SPEC §5 W2, §13 (fan-out view). Mockup fan-out screen. BUILD-PLAN §4.6, §5 (`FanoutBatchMsg`).

## Files
- create `internal/backend/linux/catalog.go`, `internal/backend/linux/catalog_test.go`
- create `internal/tui/views/fanout.go`, `internal/tui/views/fanout_members.go`,
  `internal/tui/views/fanout_test.go`
- create `internal/tui/dialog_diff.go`
- create `internal/tui/commands_probe.go`
- modify `internal/tui/msgs.go` (`FanoutBatchMsg`), `internal/tui/env.go` (`Exec transport.Executor`)

## Requirements
1. `catalog.go`: `type Probe struct { Name, Help, Cmd string; Sudo bool; Normalize func(string) string }` and
   `func Catalog() []Probe` with these built-ins (all read-only):
   `failed-units` (`systemctl list-units --state=failed --plain --no-legend --no-pager | awk '{print $1}' | sort`),
   `uptime` (`cat /proc/loadavg | cut -d' ' -f1-3`, normalize: none — expect many groups),
   `kernel` (`uname -r`), `os` (`. /etc/os-release; echo "$PRETTY_NAME"`),
   `listeners` (`ss -H -tlnp | awk '{print $4, $6}' | sort`, sudo, normalize `group.StripPIDs`),
   `disk-full` (`df -PT -x tmpfs -x devtmpfs | awk 'NR>1 && $6+0 >= 85 {print $7, $6}'`),
   `time-sync` (`timedatectl show -p NTPSynchronized --value`),
   `salt-minion` (`systemctl is-active salt-minion`), `unit-file <unit>` is added in task 086.
2. `:probe` with no argument lists the catalog (text view); `:probe <name>` runs it on the session targets
   (error flash when the set is empty: `no targets; try :hosts <selector> first`).
3. `views.NewFanout(env, title string, cmd transport.Command, hosts []resource.HostID, normalize)`:
   - `Init` starts `fanout.Run` and feeds a `group.Grouper` from `FanoutBatchMsg` batches (`NextBatch`, max 200).
     A non-zero RC is not an error: output and rc form the group.
   - Banner: `Summary.Headline()` + elapsed time, a progress bar while running.
   - Rows: one per group (`HOSTS`, `SHARE`, `GROUP` = `majority`/`differs`, `RC`, `OUTPUT` first line + `(+N
     lines)`), then one row per error kind (`timeout  2 hosts`).
   - Keys: `enter` → members view (hosts of the group; `enter` there opens the host overview); `d` → diff dialog
     of the selected group against the majority (`group.UnifiedDiff`, `+` green, `-` red, scrollable; flash when
     the majority itself is selected); `r` re-run; `o` shows the full output of the group in a text view; `space`
     on a group then `:targets marked` narrows the targets to that group's hosts (implement
     `MarkedHosts()` like the table); `esc` back (cancels a running fan-out).
   - `CopyLines()`: headline, then each group's hosts and output.
4. The members view is a small list: host, first output line.

## Out of scope
`:run` (063; it reuses this view). Actions on a group (061). Saving as a plugin.

## Acceptance
```
go test ./internal/backend/linux/ -run Catalog -v       # names unique, all commands non-empty, no "rm", ">" or "tee"
go test -race ./internal/tui/views/ -run Fanout -v
# with transporttest.Fake: 30 hosts, 27 same, 2 differ, 1 timeout:
# TestFanoutHeadline ("27 identical · 2 differ · 1 errors"), TestFanoutGroupsSorted, TestFanoutMembers,
# TestFanoutDiffDialogOpens, TestFanoutRerun, TestFanoutCancelOnPop, TestFanoutExactSize
```

## Pitfalls
- Feed the grouper only from `Update` (UI goroutine); it is not thread-safe by contract.
- 1,500 hosts: never keep per-host output for the majority group beyond the representative copy.
- The catalog test that bans `>` is there to keep probes read-only; do not weaken it, write the probe differently.
