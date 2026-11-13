# Task 034 — SSH ShellOpener (interactive PTY sessions)

- **Milestone:** M2
- **Depends on:** 033
- **Parallel group:** — (serial)
- **Status:** done

## Goal
`transport.ShellOpener` over the pool: a PTY session whose bytes can be wired to a terminal pane.

## Context
BUILD-PLAN §4.3 (`Shell`, `PTYRequest`). SPEC §7.1 (interactive shells).

## Files
- create `internal/transport/ssh/shell.go`, `internal/transport/ssh/shell_test.go`

## Requirements
1. `func (t *Transport) OpenShell(ctx, host, req) (transport.Shell, error)` — lease a client (the lease is held
   until the shell closes); `NewSession`; `RequestPty(req.Term or "xterm-256color", rows, cols,
   ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 38400, ssh.TTY_OP_OSPEED: 38400})`; `StdinPipe`,
   `StdoutPipe` (stderr is merged by the PTY); `session.Shell()` when `req.Command == ""`, else
   `session.Start(req.Command)`.
2. The returned `Shell`: `Read` from stdout, `Write` to stdin, `Resize(cols, rows)` → `session.WindowChange(rows,
   cols)`, `Wait()` → `session.Wait()` (an `*ssh.ExitError` is returned as nil; the pane does not care about the
   code), `Close()` closes stdin and the session and releases the lease exactly once.
3. No `sudo`, no command wrapping, no `LC_ALL`: the user gets their normal login shell.

## Out of scope
The emulator and the pane (049, 050). Recording input (064).

## Acceptance
```
go test -race ./internal/transport/ssh/ -run Shell -v
# against the sshtest echo shell: TestShellEcho (write "hello\n" → read contains "hello"),
# TestShellResize (Resize(100,30) → read contains "resize 100x30"), TestShellCloseReleasesLease (pool.Stats),
# TestShellWaitReturnsOnExit (write "exit\n")
```

## Pitfalls
- `RequestPty` takes `(term, height, width, modes)`; `WindowChange` takes `(height, width)`. Rows first.
- `Close` can be called from the pane and from a context cancel at once: guard with `sync.Once`.
