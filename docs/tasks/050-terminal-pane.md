# Task 050 — Terminal pane and `:ssh`

- **Milestone:** M2
- **Depends on:** 034, 049, 022, 046
- **Parallel group:** — (serial)
- **Status:** todo

## Goal
An interactive SSH shell inside a pane, next to views, with resize, scrollback, reconnect and safe teardown.

## Context
SPEC §21.1, §21.2 (`prefix S`, `:ssh`), §11.1 (shells are an escape hatch; opening is recorded later).
BUILD-PLAN §4.3 (`Shell`), §4.9 (`Content`).

## Files
- create `internal/mux/termpane/termpane.go`, `internal/mux/termpane/keys.go`,
  `internal/mux/termpane/termpane_test.go`, `internal/mux/termpane/keys_test.go`
- create `internal/tui/commands_ssh.go`
- modify `internal/tui/env.go` (`Shells transport.ShellOpener`), `internal/tui/root.go` (pane factory handles
  `ContentState.Type == "term"`; `OpSSH` prompt), `internal/mux/model.go` only if `AddPane` needs a fix

## Requirements
1. `func New(id mux.PaneID, ctx context.Context, opener transport.ShellOpener, host resource.HostID, o Options)
   *Pane` implementing `mux.Content`; `Options { Scrollback int; Term string; Now func() time.Time }`.
2. States: `connecting` → `connected` → `closed` (remote exit) or `failed` (error kind shown). In `closed`/
   `failed` the pane shows the last screen dimmed plus a line `[disconnected: <reason> — r reconnect · x close]`;
   `r` reconnects, any other key is ignored.
3. `Init`: a command that calls `OpenShell` (15 s timeout) with the pane's current interior size and returns a
   private `connectedMsg{shell}` or `failedMsg{err}`.
4. After connect, two goroutines owned by the pane (stopped by `Close()`):
   - reader: `shell.Read` → `emu.Write`; coalesce and notify the UI at most every 16 ms by sending on a
     `chan struct{}` (buffer 1) that a `tea.Cmd` waits on, producing a private `dirtyMsg` → re-render and re-arm.
     On read error/EOF → `closedMsg`.
   - responder: `io.Copy(shell, emu.Responses())`.
5. `Update(tea.KeyMsg)`: encode with `keys.Encode(msg) []byte` and `shell.Write`. Encoding table (`keys.go`):
   runes → UTF-8; `enter` → `\r`; `tab` → `\t`; `backspace` → `0x7f`; `esc` → `0x1b`; `ctrl+<letter>` → `0x01–0x1a`;
   `ctrl+@`/`ctrl+space` → `0x00`; `ctrl+\` `ctrl+]` `ctrl+^` `ctrl+_` → `0x1c–0x1f`; arrows `ESC[A/B/C/D`; `home`
   `ESC[H`, `end` `ESC[F`; `pgup` `ESC[5~`, `pgdown` `ESC[6~`; `delete` `ESC[3~`; `insert` `ESC[2~`; `f1`–`f12`
   xterm sequences; `alt+<rune>` → `ESC` + rune; `shift+tab` → `ESC[Z`. Bracketed paste: a `tea.KeyMsg` with
   `Paste == true` is sent as its runes.
6. `View(w, h)`: if the size changed, `emu.Resize` and `shell.Resize`; return `emu.Render()`; draw the cursor by
   reversing the cell at the cursor position when focused and visible.
7. `Title()` = `ssh <short host>`; `RawKeys()` = true while connected, false otherwise; `Busy()` = connected;
   `CopyLines()` = scrollback + visible screen (plain); `State()` = `{Type: "term", Host: host}`; restoring a
   term pane starts in `closed` state with reason `restored` (no automatic reconnect).
8. `:ssh <host>` (completion from the inventory; no argument → host of the focused view's `Subject()`; none →
   prompt): resolves through the inventory, adds a pane by splitting the focused one horizontally, focuses it.
   `prefix S` does the same via the prompt dialog. In `--demo` (no opener) flash `no SSH in demo mode`.
9. `OpSendPrefix` (double prefix) arrives as a normal key message for the prefix key: it is encoded and sent
   like any other key.

## Out of scope
Recording shell opens to the investigation and read-only refusal (064). Synchronized input (064). Local shells.
Mouse.

## Acceptance
```
go test -race ./internal/mux/termpane/ -v
# with transporttest.Fake shells: TestConnectsAndRendersOutput (inject "hello\r\n" → View contains hello),
# TestKeysAreSent (type "ls", enter → shell received "ls\r"), TestKeyEncodeTable (every row above),
# TestResizePropagates, TestRemoteExitShowsDisconnected, TestReconnect, TestCloseStopsGoroutines,
# TestViewExactSize, TestOpenFailureShowsKind (errs.Auth → "auth")
go test -race ./internal/tui/ -run 'SSH' -v      # ":ssh web-ewr-001" with a fake opener adds a second pane
# lab (manual, after 052): :ssh web-01 → run `top`, `less /etc/os-release`, `vim`, resize the terminal, prefix z
```

## Pitfalls
- Never write to the shell from `View`, except the resize calls; never block in `Update` (a `shell.Write` to a
  stalled connection can block: write through a buffered channel drained by a writer goroutine).
- The pane must not read `tea.WindowSizeMsg`; its size is whatever `View(w, h)` is called with.
- `ctrl+m` is Enter and `ctrl+i` is Tab at the byte level; Bubble Tea already reports them as `enter`/`tab`.
- Forgetting the responder goroutine makes programs like `vim` hang at startup waiting for a terminal reply.
