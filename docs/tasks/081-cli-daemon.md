# Task 081 — CLI subcommands and on-demand server start

- **Milestone:** M5
- **Depends on:** 079, 080
- **Parallel group:** — (serial; touches `main` and `app`)
- **Status:** todo

## Goal
`jumper` just works: it starts the server when needed and attaches. All the tmux-style subcommands exist.

## Context
SPEC §21.4 (command list). BUILD-PLAN §6.

## Files
- create `internal/app/serve.go` (adapter `app.Session` → `server.SessionHandle`; `RunServer`)
- create `internal/app/spawn.go` (start the server process), `internal/app/cli.go` (argument parsing, pure),
  `internal/app/cli_test.go`, `internal/app/spawn_test.go`
- modify `cmd/jumper/main.go`

## Requirements
1. Parsing (`cli.go`, pure, table-tested): `jumper [global flags] [command] [flags]`.
   Global: `--config-dir`, `--debug`, `-S <socket>`, `--standalone`, `--demo`, `--readonly`, `--fresh`,
   `--version`. Commands: (none) = `attach` to the last session or create `main`; `attach [-t name]`;
   `new [-s name] [-d]`; `ls`; `kill-session -t name`; `kill-server`; `server [--foreground]`; `doctor …`;
   `version`. `--demo` implies `--standalone`. Socket: `-S`, else `$JUMPER_SOCKET`, else
   `<cache>/jumper.sock`.
2. `RunServer(ctx, opts)`: build `Core`, call `server.Serve` with `NewSession` → `core.NewSession`; log to
   `<cache>/server.log`; handle `SIGTERM`/`SIGINT` (in `main.go`, passed as ctx cancel).
3. `spawn.go`: `func EnsureServer(ctx, socketPath string, exe string, args []string, env []string, logPath
   string) error` — try to dial; if it fails: start `exe server` with `exec.Command`, `SysProcAttr{Setsid:
   true}`, stdin from `/dev/null`, stdout/stderr appended to the log file, working directory `/`, environment =
   current env plus `JUMPER_SERVER=1`; do not wait for it; poll the socket every 50 ms for up to 5 s; on timeout
   return `errs.Unreachable` including the last 20 lines of the log file.
4. `main.go` for attach-type commands: resolve paths → `EnsureServer` (unless `--standalone`) → build `Hello`
   (size via `term.GetSize(os.Stdout.Fd())`, env `TERM`, `COLORTERM`, `SSH_AUTH_SOCK`, `TMUX`) → `server.Attach`
   with `MakeRaw` = `term.MakeRaw(os.Stdin.Fd())`/`term.Restore`, `Resizes` from `SIGWINCH`, `Hangup` from
   `SIGHUP`/`SIGTERM` → print `Exit.UserMessage()` → exit code 0 (1 for `error`).
   - stdin or stdout not a terminal → `jumper: not a terminal` (exit 2), except for `ls`, `kill-*`, `version`,
     `doctor`.
   - `$SITREP` set (we are inside a jumper local pane) → refuse to attach: `jumper: already inside jumper`.
5. `ls` output, one line per session: `main: 2 windows (created Nov 12 14:02) (attached)`. No server running →
   `no server running` and exit 1.
6. The server is started with the **client's** config dir and debug flag; a client whose `--config-dir` differs
   from the running server's gets a warning line before attaching.

## Out of scope
launchd integration (not wanted: on-demand start only). Remote attach.

## Acceptance
```
go test ./internal/app/ -run 'CLI|Spawn' -v
# CLI parse table (every command and flag; errors for unknown ones);
# TestEnsureServerStartsAndConnects: builds the binary into t.TempDir() with `go build`, spawns `server` with a
#   temp socket and --demo config, then Control(ls) answers; kill-server; the process exits
make check
./jumper ls ; ./jumper new -d -s test ; ./jumper ls ; ./jumper kill-server     # manual
```

## Pitfalls
- The spawned server must not hold the client's terminal: `Setsid` and no inherited stdio, or closing the
  terminal kills it with SIGHUP.
- Use `os.Executable()` for `exe`, not `os.Args[0]`.
- `go build` inside a test is slow; guard that test with `testing.Short()`.
