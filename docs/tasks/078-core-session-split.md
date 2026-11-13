# Task 078 — Split `app` into Core and Session; switchable I/O; in-process attach/detach

- **Milestone:** M5
- **Depends on:** 076
- **Parallel group:** M5-A (with 077)
- **Status:** done

## Goal
A session whose terminal can be unplugged and plugged back in while everything keeps running — proven in-process,
before any socket exists.

## Context
BUILD-PLAN §6 (rendering in the server, switchable pipes, on-attach behaviour), D4. SPEC §21.4.

## Files
- create `internal/app/core.go`, `internal/app/session.go`, `internal/app/switchio.go`
- create `internal/app/session_test.go`, `internal/app/switchio_test.go`
- modify `internal/app/app.go`, `internal/app/build.go`

## Requirements
1. `Core` (built once per process by `func NewCore(ctx, cfg, paths, baseEnv map[string]string, log) (*Core,
   error)`): SSH resolver/pool/transport, inventory, backends + registry, salt runner/executor, policy, redactor,
   knowledge index, agent-socket holder, clipboard factory. `Core.Close()`.
2. `Session` (built per session by `func (c *Core) NewSession(name string) (*Session, error)`): its own
   investigation log, bus (root context = session context), `tui.Env`, commands, prompter, mux model (restored
   from the session file when present), root model and **one `tea.Program` for its whole life**, started in
   `NewSession` with `tea.WithInput(sess.in)`, `tea.WithOutput(sess.out)`, `tea.WithAltScreen()`,
   `tea.WithoutSignalHandler()`, `tea.WithContext(sessionCtx)`.
3. `switchio.go`:
   - `SwitchWriter`: `Write` goes to the current writer under a mutex, or is discarded when none; a write error
     from the current writer detaches it (reported once through an `onError` callback) and is **not** returned to
     Bubble Tea. `Set(w io.Writer)`, `Clear()`.
   - `SwitchReader`: `Read` blocks until a source is set and has data. When the current source returns any error
     (including `io.EOF`) the reader **does not** return it: it clears the source (reporting through `onError`)
     and keeps blocking. `Set(r io.Reader)`, `Clear()`, `Close()` (the only thing that makes `Read` return
     `io.EOF`).
4. `func (s *Session) Attach(in io.Reader, out io.Writer, size Size, env map[string]string) error`:
   detach any current client first (its `onDetach("replaced")` callback fires); update the agent-socket holder
   from `env["SSH_AUTH_SOCK"]`; choose the prefix from `env["TMUX"]`; write the preamble to `out`
   (`ESC[?1049h` alt screen, `ESC[?25l` hide cursor, `ESC[?2004h` bracketed paste, `ESC[2J ESC[H`); set the
   writer, then the reader; `program.Send(tea.WindowSizeMsg{…})` (this forces Bubble Tea to repaint everything);
   mark the prompter attached.
5. `func (s *Session) Detach(reason string)`: clear reader and writer; write the postamble to the old `out`
   (`ESC[?2004l ESC[?25h ESC[?1049l`); prompter detached; call the client's `onDetach(reason)`.
6. `func (s *Session) Resize(size Size)`; `func (s *Session) Done() <-chan struct{}` (program ended: `:q`, last
   pane closed); `func (s *Session) Kill()`; `func (s *Session) Info() Info{Name, Windows, Attached, Created,
   Activity}`.
7. The root model gets an `OnDetach func()` hook: `OpDetach` (prefix `d`, `:detach`) calls it via a `tea.Cmd`;
   standalone mode leaves it nil and keeps the flash from task 022.
8. `app.Run` (standalone) is now: `NewCore` → `NewSession("main")` → `Attach(os stdin/stdout via Options)` → wait
   for `Done()`. For a real TTY the size comes from `Options.Size`/`Resizes` filled by `main.go` using
   `golang.org/x/term` and `SIGWINCH` (move that detection into `main.go`; the program no longer has a TTY
   output to query).

## Out of scope
Sockets, daemon, CLI (079–081).

## Acceptance
```
go test -race ./internal/app/ -run 'Switch|Session' -v -count=2
# SwitchReader: blocks with no source; EOF from the source does not surface; Close returns EOF
# SwitchWriter: discards when cleared; failing writer is dropped once
# TestAttachDetachReattach: demo core; attach pipe A → frames arrive; start a slow fake fan-out; Detach;
#   wait; attach pipe B → B receives the alt-screen preamble and a full frame that contains the fan-out's final
#   headline (work continued while detached); A received the postamble
# TestSecondAttachReplacesFirst, TestResizeAfterAttach, TestPausedRolloutStaysPausedAcrossDetach,
# TestSessionDoneOnQuit, TestRunOverPipes (from task 025) still passes
make check
```

## Pitfalls
- If the reader ever returns `io.EOF` to Bubble Tea, its input loop ends for good and the session is deaf after
  the first detach. This is the single most important detail of M5.
- Bubble Tea v1 diffs against its last frame; a new terminal is blank, so the `WindowSizeMsg` after `Set` is
  mandatory, even when the size did not change.
- `program.Send` blocks before the program has started and is a no-op after it ended; wait for a "ready" signal
  from the root's first `Init`.
- With a non-TTY output Bubble Tea does not enter raw mode or the alt screen on the user's terminal: the
  preamble/postamble here and raw mode in the client (080) replace that.
