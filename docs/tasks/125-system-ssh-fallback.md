# Task 125 — system-ssh fallback transport

- **Milestone:** M12
- **Depends on:** 052
- **Parallel group:** M12-A (with 123, 124)
- **Status:** done

## Goal
Hosts the native client cannot handle (ProxyCommand, GSSAPI, exotic options) work through `/usr/bin/ssh`
(SPEC §7.1 "Fallback transport").

## Files
- create `internal/transport/sysssh/sysssh.go`, `sysssh_test.go`, `internal/transport/route.go`,
  `route_test.go`
- modify `internal/config/` (`ssh.transports: [{match: "*.legacy.example.internal", transport: system-ssh}]`),
  `internal/app/core.go`, `internal/app/rules_test.go` (already allows `os/exec` here)

## Requirements
1. `Executor`, `Streamer`, `ShellOpener` using `exec.CommandContext("/usr/bin/ssh", "-F", cfgFile, "-o",
   "BatchMode=yes", "-o", "ControlMaster=auto", "-o", "ControlPath=<cache>/cm/%C", "-o", "ControlPersist=5m",
   "-o", "ConnectTimeout=<n>", "--", host, wrappedCommand)`; shells use `-tt` with a local PTY
   (`creack/pty`) and `Resize` via `pty.Setsize`.
2. Exit code 255 = ssh's own failure: classify stderr (`Permission denied` → Auth; `Host key verification
   failed` → HostKey; `timed out` → Timeout; `Could not resolve`/`refused` → Unreachable).
3. `transport.Router`: an `Executor`/`Streamer`/`ShellOpener` that picks native or system ssh per host by glob
   patterns; `Name()` reports the one used in `Via`.
4. Automatic suggestion: when the native dial fails with the ProxyCommand `errs.Config`, the error message names
   the config stanza to add.

## Acceptance
```
go test -race ./internal/transport/... -run 'SysSSH|Router' -v     # uses a fake `ssh` executable: the test
                                                                   # re-executes itself as the fake via a path
                                                                   # option (no shell scripts)
```

## Pitfalls
- `BatchMode=yes` always: system ssh must never prompt on a terminal it does not have.
- The control path directory is `0700`; `%C` keeps paths short enough.
