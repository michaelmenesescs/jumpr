# Task 090 — cgroups, kernel log, file viewer, packages

- **Milestone:** M6
- **Depends on:** 089, 013
- **Parallel group:** — (serial)
- **Status:** done

## Goal
The remaining read-only Linux kinds of SPEC §8.1.

## Files
- create `internal/backend/linux/kind_misc.go`, `internal/backend/linux/parse_misc.go`,
  `internal/backend/linux/parse_misc_test.go`
- create `internal/tui/commands_file.go`
- modify `internal/backend/linux/kinds.go`, `internal/backend/linux/probes.go`

## Requirements
1. `cgroup` (aliases `cgtop`): `systemd-cgtop -b -n 2 -d 1 --order=memory` (take the second iteration; columns
   `cgroup`, `tasks`, `cpu`, `memory`, `input/s`, `output/s`; `-` → nil).
2. `kernel` (aliases `dmesg`): `journalctl -k -o json -p 4 --no-pager -n 300` for the session window (reuse the
   journal parser and view; sudo). This avoids `dmesg -T` timestamp drift.
3. `pkg` (aliases `packages`): detect by `/etc/os-release` `ID_LIKE`/`ID`: rpm →
   `rpm -qa --qf '%{NAME}\t%{VERSION}-%{RELEASE}\t%{ARCH}\t%{INSTALLTIME}\n'`; deb →
   `dpkg-query -W -f '${Package}\t${Version}\t${Architecture}\t${db:Status-Abbrev}\n'`. Columns `name`, `version`,
   `arch`, `installed` (rpm only). Loaded on demand only (never part of an overview); 60 s timeout.
   `:probe pkg <name>` (parameterized catalog probe) prints the version of one package across the fleet.
4. `:file <path> [--host h] [--tail N]` — read-only viewer: `stat -c '%s %U:%G %a %y' <path>` then
   `head -c 1048576 <path>` (or `tail -n N`), sudo, path quoted with `shq.Quote`, shown in a text view with a
   header line (size, owner, mode, mtime, `truncated at 1 MiB`). Binary content (NUL in the first 8 KiB) → refuse
   with a message. Opening a file is recorded as a `view` entry (path only, never content). Paths matching the
   built-in sensitive list (`/etc/shadow`, `*.key`, `*.pem`, `id_*`, `*/.ssh/*`, `*secret*`, `*token*`) require a
   y/N confirmation and their content passes through `redact`.
   `:file` on a multi-host target set → fan-out view of `sha256sum <path>` grouped (config drift check).

## Acceptance
```
go test ./internal/backend/linux/ -run 'Cgtop|Pkg|Rpm|Dpkg' -v
go test -race ./internal/tui/ -run 'FileCommand' -v     # quoting of a path with spaces and quotes; sensitive path
                                                        # asks; multi-host → sha256sum fan-out
```

## Pitfalls
- `:file` must never use a shell redirection or any command that could write.
- `systemd-cgtop` prints two frames with `-n 2`; frames are separated by a blank line.
