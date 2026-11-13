# Task 065 — M3 wiring, read-only flag, lab scenario test

- **Milestone:** M3
- **Depends on:** 060, 061, 062, 063, 064
- **Parallel group:** — (serial; touches `app`)
- **Status:** done

## Goal
Everything from M3 assembled in `app`; the mockup's nginx scenario reproducible in the lab (SPEC W1 without
metrics).

## Context
BUILD-PLAN §2 (M3 demo), §3 (`app` is the composition root). SPEC §5 W1, §11, §16.

## Files
- modify `internal/app/build.go`, `cmd/jumper/main.go` (`--readonly`)
- create `internal/app/lab_w1_test.go` (`//go:build lab`)
- create `internal/tui/commands_policy.go` (`:policy`, `:readonly on|off`)
- modify `docs/HANDOFF.md`

## Requirements
1. Build order additions: redactor (user patterns from `policy.yaml` key `redact: []`) → policy → investigation
   log (`knowledge.root`) → knowledge index → bus (root context = session context) → register planners (`linux`,
   `generic`, `mux`) → `Env`.
2. `--readonly` flag and `:readonly on|off` (turning it **off** needs a y/N confirmation and is refused when the
   policy file says `readonly: true`). Status line `RO` badge.
3. `:policy` shows the effective policy (built-ins + file) in a text view.
4. `TestLabW1NginxConflict` (packages, not the TUI): inject fault `haproxy-443`; resolve targets
   `role=web`; open an investigation; run probe `failed-units` → the faulted host is in a differing group;
   journal for `nginx.service` contains `Address already in use`; sockets show `haproxy` on `:443`; propose
   `linux.systemctl.restart` nginx → execute → step **fails** (unit not active); propose `stop haproxy` → ok;
   restart nginx → ok; investigation file contains `**probe**`, three `**action**` bullets and no secrets; the
   fault is undone in cleanup.
5. `TestLabPolicyDeniesStopSSHD`: proposal for `stop sshd.service` on `web-02` is denied and recorded.
6. `TestLabRolloutCanary`: `:run`-equivalent `true` on all web hosts → canary event, pause, continue, done.
7. Update `docs/HANDOFF.md` (M3 done; demo script; anything surprising).

## Out of scope
Metrics and alerts (M8). Salt (M4). New features of any kind: this task only assembles and verifies.

## Acceptance
```
make check
make lab-test
./jumper --config-dir lab/jumper      # manual: BUILD-PLAN §2 M3 demo script
```

## Pitfalls
- The lab test asserts on files under a temp knowledge root, not `lab/.knowledge`.
- If the fault name differs in `@@`, use the real one (see task 035's note).
