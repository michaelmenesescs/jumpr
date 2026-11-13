# Task 088 — Disks and mounts

- **Milestone:** M6
- **Depends on:** 044
- **Parallel group:** M6-A (with 084)
- **Status:** todo

## Goal
Kinds `disk` and `mount` (SPEC §8.1).

## Files
- create `internal/backend/linux/parse_disk.go`, `internal/backend/linux/parse_disk_test.go`,
  `internal/backend/linux/kind_disk.go`
- modify `internal/backend/linux/kinds.go`, `internal/backend/linux/probes.go`
- add `testdata/linux/disk/…`

## Requirements
1. `mount` (aliases `mounts`, `df`; ScopeHost): `df -PTk` (skip `tmpfs`, `devtmpfs`, `overlay`, `squashfs` unless
   `Args["all"]`) joined with inode use from `df -Pi`: columns `mount`, `fstype`, `size` (bytes), `used`,
   `avail`, `use` (percent), `iuse` (percent), `source`. Status Warn ≥ 85 %, Crit ≥ 95 % (either percentage).
2. `disk` (aliases `disks`, `lsblk`; ScopeHost): `lsblk -J -b -o NAME,TYPE,SIZE,FSTYPE,MOUNTPOINT,ROTA,RO,MODEL`
   flattened with an indented name for children; plus IO rates from two samples of `/proc/diskstats` taken 1 s
   apart inside the same batch section (`cat /proc/diskstats; sleep 1; cat /proc/diskstats`): `r/s`, `w/s`,
   `util` (percent of the interval spent doing IO, field 13).
3. Links host → mounts (`m`), disks (`D`). Mount `enter` → a fan-out-free text view of
   `du -xh --max-depth=1 <mount> | sort -rh | head -25` (sudo, 20 s timeout, path quoted).
4. `lsblk` JSON: `mountpoint` may be `null`, or `mountpoints` (array) on newer util-linux; sizes may be strings
   or numbers. Handle both.

## Acceptance
```
go test ./internal/backend/linux/ -run 'DF|Lsblk|Diskstats|Mount|Disk' -v
```

## Pitfalls
- `df -P` wraps nothing, but mount points can contain spaces: fields 1–6 from the left (7 with `-T`), mount = the
  rest.
- `du` on a huge tree is slow; it has a timeout and is cancelled when the view closes.
