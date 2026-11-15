# Task 035 — `jumpr doctor ssh` and the first lab test

- **Milestone:** M2
- **Depends on:** 033
- **Parallel group:** — (serial; touches `app` and `main`)
- **Status:** done

## Goal
A CLI check that proves SSH works end to end, and the lab test harness all later lab tests reuse.

## Context
SPEC §14 (`jumpr doctor`), §22 (lab). BUILD-PLAN §9, §12 (risk 1).

## Files
- create `internal/app/doctor.go`, `internal/app/doctor_test.go`
- create `internal/app/build_ssh.go` (builds Resolver, Pool, Transport from config; reused by task 052)
- create `internal/labtest/labtest.go` (helpers for `//go:build lab` tests; the file itself has the `lab` tag)
- create `internal/transport/ssh/lab_test.go` (`//go:build lab`)
- create `lab/jumpr/config.yaml`
- modify `cmd/jumpr/main.go` (subcommand `doctor`)

## Requirements
1. `jumpr doctor ssh <host> [--config-dir d]` prints, one line each, with `ok`/`FAIL`:
   resolved chain (`web-01.lab.example.internal via bastion.lab.example.internal`), dial + auth time, host key
   status, `echo ok` round trip time, `sudo -n true` result (`ok`, or `needs-privilege`), remote
   `uname -sr` and `/etc/os-release` `PRETTY_NAME`. Exit code 0 when dial and echo succeed, else 1. Output goes
   to the writer passed in (`app.Doctor(ctx, o Options, args []string, out io.Writer) int`).
   Prompts (unknown host key, passphrase) are answered on the terminal by a simple line-based `Prompter` defined
   in `main.go` (stdin/stderr are allowed there).
2. `lab/jumpr/config.yaml` (committed; the lab track owns the rest of `lab/`): `ssh.config_files: [lab/ssh_config]`, `inventory.sources: [{type: ssh_config}]`,
   `salt.master: salt.lab.example.internal`, `knowledge.root: lab/.knowledge`. Relative paths in a config file
   resolve against the current working directory (document this in the file's header comment).
3. `labtest`: `func Config(t *testing.T) config.Config` (finds the repo root by walking up to `go.mod`, loads
   `lab/jumpr/config.yaml`, and `t.Skip("lab not running: lab/ssh_config missing; run: go run ./cmd/@@
   up")` when the ssh config is absent); `func SSH(t *testing.T) *ssh.Transport` (strict host keys off for lab:
   `accept-new`, agent or the lab key from ssh_config); `func Hosts() []resource.HostID` (the nine lab hosts);
   `func Fault(t *testing.T, name string)` runs `go run ./cmd/@@ fault <name>` and registers
   `t.Cleanup` with `go run ./cmd/@@ fault <name> --undo` (faults today: `haproxy-443`, `disk-fill`,
   `oom-pod`, `unit-flap`; see `go run ./cmd/@@ fault list`); `func Break(t *testing.T, host
   resource.HostID, do, undo string)` runs a lab-only shell command with sudo on a lab host through the SSH
   transport and registers `undo` as cleanup (for situations no packaged fault covers, e.g. stopping a minion).
4. `lab_test.go`:
   - `TestLabExecAllHosts`: `echo ok` on all nine hosts through the bastion.
   - `TestLabSudo`: `sudo -n id -u` → `0` on `web-01`.
   - `TestLabFanoutStress`: 5 rounds × 9 hosts × 4 concurrent commands each with pool limits
     `MaxConnsPerBastion=2, MaxChannelsPerConn=4`; no errors; bastion connections ≤ 2 (`pool.Stats`).
   - `TestLabReconnectAfterIdleReap`.

## Out of scope
`doctor` for other backends (124).

## Acceptance
```
go test ./internal/app/ -run Doctor -v      # against sshtest servers: output lines and exit codes
make check
# with the lab up:
make lab-test                               # TestLab* in internal/transport/ssh pass
./jumpr doctor ssh web-01.lab.example.internal --config-dir lab/jumpr     # all lines ok
```

## Pitfalls
- `go test ./...` must not compile `internal/labtest` or any `lab_test.go` without `-tags lab`: put
  `//go:build lab` as the first line of each, followed by a blank line.
- Lab tests shell out to `go run ./cmd/@@ …`; that command is owned by the lab track. If its fault names
  differ from what a test expects, read `go run ./cmd/@@ fault -h` and use the real names; do not edit
  `cmd/@@`.
- If a lab test fails because of a limit (`MaxSessions`, `MaxStartups`), record the numbers in
  `docs/HANDOFF.md`; the defaults in config may need tuning and that is a decision for the architect.
