# Task 144 — Homelab integration suite (real Ubuntu 24.04 over SSH)

- **Milestone:** M2 (integration, before any refactor is allowed)
- **Depends on:** 031, 033, 035, 038, 040, 041, 042, 052
- **Parallel group:** —
- **Status:** done

## Why
The user's own targets, already reachable from this laptop:

- `ssh macmini` → **Ubuntu 24.04.5 LTS, x86_64, systemd 255, journalctl present** (Tailscale 100.120.74.69, user `mike`).
  This is the jumpr target: systemd, journald, `/proc`, `ss`.
- `ssh mini` → **Darwin 24.6.0 arm64** (the M4 Mac mini). A macOS *target* is out of scope — SPEC §3 excludes
  non-systemd nodes — so it is a client host and a future `launchd` backend, nothing more. Do not write systemd probes
  against it.

Everything is resolved through `~/.ssh/config` (both entries are plain `HostName` + `User`, no ProxyJump), which is what
task 028 already parses. The point of this task: prove the SSH → probe → parse → `Resource` path against a real machine,
so the parsers and the backend are not refactored the moment they meet reality.

## Files
- create `internal/transport/ssh/homelab_test.go` (`//go:build homelab`)
- create `internal/backend/linux/homelab_test.go` (`//go:build homelab`)
- modify `Makefile` (one target: `homelab-test`) — this is the single allowed Makefile edit in the parallel run
- modify `docs/HANDOFF.md`? **No** — report findings; the coordinator records them.

## Requirements
1. **Skip cleanly, never fail the default suite.** No file here may compile into `go test ./...`. Every test starts with
   `host := os.Getenv("JUMPR_HOMELAB_UBUNTU"); if host == "" { t.Skip("JUMPR_HOMELAB_UBUNTU not set") }` and then a
   dial with a 3 s timeout; unreachable → `t.Skipf`, not `t.Fatal` (a laptop away from the tailnet must still pass CI).
2. **Read-only, always.** Only these commands may run remotely: `id -un`, `uname -sr`, `cat /proc/loadavg`,
   `systemctl list-units`, `systemctl show <unit>`, `journalctl -o json -n 50`, `ss -tunap`, `ps -eo …`, `sudo -n true`.
   Add a test that asserts the probe batch under test contains no `systemctl start|stop|restart|enable|disable`, `kill`,
   or `rm`.
3. Tests, each ≤ 10 s per remote call:
   - `TestHomelabSSHConfigResolve`: `internal/sshcfg` resolves `JUMPR_HOMELAB_UBUNTU` to a non-empty user + host +
     port 22; the alias must come from the real `~/.ssh/config`, not a fixture.
   - `TestHomelabExecRoundTrip`: dialer + `Executor` run `id -un`; output is non-empty, no trailing escape bytes,
     exit code 0; a deliberately failing command (`sh -c 'exit 127'`) is classified by `internal/shq.Classify` as
     `NotAvailable`.
   - `TestHomelabProbeOverview`: the real overview probe batch (038) against the host parses through 040 into a
     `Resource` with a finite load average, non-zero memory total, and `os-release` naming Ubuntu; no field may be
     `"—"`/empty that the probe actually returned.
   - `TestHomelabUnits`: `systemctl list-units` output parses through 041; ≥ 5 units, at least one `active`, and unit
     names survive round-trip unchanged.
   - `TestHomelabJournal`: 50 JSON lines parse through 042; ≥ 1 entry, timestamps monotonic, no panic on a unit filter
     that matches nothing.
   - `TestHomelabTiming`: records dial, first-byte and full-probe latency with `t.Logf` so the handoff can quote real
     numbers against SPEC §15 (< 1.5 s single-host overview via a warm bastion).
4. `make homelab-test` runs `go test -tags homelab -count=1 ./internal/transport/ssh/ ./internal/backend/linux/ -v`.
   It must never be part of `make check`.
5. Report (in the agent's final message) the exact `t.Logf` numbers and any probe that failed on the real host, with the
   command and its output — that list is the refactor risk register.

## Out of scope
AWS, macOS/launchd targets, sudo-dependent probes, anything that writes to the remote host, capture tooling (053).

## Acceptance
```
make check                                   # unchanged, must stay green without a network
JUMPR_HOMELAB_UBUNTU=macmini make homelab-test    # all pass, or skip with a stated reason
```

## Pitfalls
- `systemctl list-units` output differs by locale and by `--plain`; set `LC_ALL=C` through `shq.Wrap` and parse the
  same text the production probe will see — do not build a special test-only command.
- `journalctl -o json` emits one JSON object per line with a `__REALTIME_TIMESTAMP` microsecond field; do not assume
  RFC 3339.
- The Ubuntu host is x86_64 and macOS client is arm64: no CGO, no arch-specific assumptions (CGO_ENABLED=0 is enforced
  for release builds).
