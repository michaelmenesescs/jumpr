# Task 083 — End-to-end detach test through a real PTY; behaviour inside tmux

- **Milestone:** M5
- **Depends on:** 082
- **Parallel group:** — (serial)
- **Status:** todo

## Goal
Prove the whole chain with the real binary and a real terminal device, and finish the nested-in-tmux details.

## Context
BUILD-PLAN §9 (session server tests), D7. SPEC §21.4 ("Inside tmux").

## Files
- create `internal/app/e2e_test.go` (build tag `e2e`; add `make e2e` = `go test -tags e2e -run TestE2E ./internal/app/ -count=1`)
- modify `Makefile`, `internal/app/session.go` (title, tmux handling), `internal/clip/clip.go` if needed

## Requirements
1. `TestE2EDetachReattach` (uses `github.com/creack/pty` and the vt adapter from task 049 to read the screen):
   build the binary. `--demo` implies standalone, so add a hidden flag `--demo-server` that makes the **server**
   use the fake backend; run `sitrep -S <tmp>/s.sock --demo-server
   --config-dir <tmp>` on a PTY of 120×36; feed the output to a `vt.Emulator`; wait until the screen shows
   `web-ewr-001`; type `:alerts` + Enter → screen shows `HighErrorRate`; press prefix `%` → two panes; press
   prefix `d` → the process exits 0 and its last output line is `[detached (from session main)]`; the terminal
   modes are restored (PTY is no longer in raw mode: check with `term.GetState`/`unix.IoctlGetTermios` that
   `ICANON` and `ECHO` are set); start a second client on a **new** PTY of 100×30 → the screen shows both
   panes and `HighErrorRate`, laid out for 100×30; `sitrep -S … ls` prints one attached session;
   `kill-server` → the client exits with `[server exited]`.
2. `TestE2EClientKilled`: `kill -9` the client; a new client can attach and the session is intact.
3. `TestE2EResize`: `pty.Setsize` to 90×25 → the status line becomes 90 columns wide.
4. Inside tmux (`TMUX` in the attach env): prefix switches to `mux.prefix_in_tmux`; the clipboard uses OSC 52 in
   tmux passthrough form in addition to `pbcopy`; the session sets the terminal title with
   `ESC ] 2 ; sitrep: <session> BEL` on attach (written in the preamble) — unit tests for these three in
   `internal/app/session_test.go`.
5. Local panes export `SITREP=<session>` (used by the nested-attach refusal in task 081); since local shell panes
   are still an open question, only define the constant and the check now.

## Out of scope
Testing inside a real tmux server.

## Acceptance
```
make e2e          # 3 tests pass, each under 20 s
go test -race ./internal/app/ -run 'Tmux|Title' -v
make check
```

## Pitfalls
- Read the PTY continuously in a goroutine into the emulator; a PTY whose output is not drained blocks the
  client.
- Poll the emulator screen with a deadline; never `time.Sleep` for a fixed duration and then assert.
- `e2e` tests must not run under plain `go test ./...` (build tag), because they build a binary and need PTYs.
