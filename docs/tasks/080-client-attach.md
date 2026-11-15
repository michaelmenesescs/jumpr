# Task 080 — Client: raw terminal, byte pumps, resize, clean restore

- **Milestone:** M5
- **Depends on:** 077
- **Parallel group:** M5-B (with 079)
- **Status:** done

## Goal
The thin client of BUILD-PLAN §6: connect, say hello, pump bytes both ways, restore the terminal no matter how
it ends.

## Context
BUILD-PLAN §6 (the client is a dumb byte pump), §6.2 (frames). SPEC §21.4.

## Files
- create `internal/server/client.go`, `internal/server/client_test.go`

## Requirements
1. `type ClientOptions struct { SocketPath string; Hello Hello; In io.Reader; Out io.Writer; MakeRaw func()
   (restore func(), err error); Size func() (cols, rows int); Resizes <-chan struct{}; Hangup <-chan struct{} }`
   — everything OS-specific is injected by `cmd/jumpr` so this package stays testable and obeys §6.1.
2. `func Attach(ctx context.Context, o ClientOptions) (Exit, error)`:
   - dial the socket (2 s); send `HELLO` (with the current size); read `WELCOME` or `EXIT`.
   - `MakeRaw()`; defer `restore()` — it must run on every path, including panics.
   - goroutine A: `In` → `STDIN` frames (read buffer 4 KiB). goroutine B: frames → `STDOUT` payloads to `Out`;
     `EXIT` → return it.
   - on each `Resizes` tick: `RESIZE` with `Size()`. On `Hangup` (SIGHUP/SIGTERM) or `ctx` done: send `DETACH`,
     wait up to 1 s for `EXIT`.
   - server closes the connection without `EXIT` → `Exit{Reason: "error", Message: "server connection lost"}`.
   - before returning, write a safety postamble to `Out` in case the server died mid-frame:
     `ESC[?1049l ESC[?25h ESC[?2004l ESC[0m`.
3. `func Control(ctx context.Context, socketPath string, h Hello) (Result, error)` — for `ls`, `kill-session`,
   `kill-server`, `new -d`: hello, read `RESULT` (or `EXIT` → error).
4. The client never interprets input bytes. There is no client-side detach key.
5. `func (e Exit) UserMessage() string` — `[detached (from session main)]`, `[session ended]`,
   `[detached: another client attached]`, `[server exited]`, or the error message.

## Out of scope
Starting the server (081). TTY detection (`cmd/jumpr`).

## Acceptance
```
go test -race ./internal/server/ -run 'Client|Attach|Control' -v
# against the real Serve with the echo SessionHandle:
# TestClientRoundTrip (bytes in → same bytes out), TestClientRestoreCalledOnExit / OnServerDeath / OnCtxCancel,
# TestClientSendsResize, TestClientHangupSendsDetach, TestControlLs, TestExitMessages
```

## Pitfalls
- A blocked `In.Read` (stdin) cannot be interrupted; do not wait for goroutine A when returning. The process
  exits right after `Attach` returns.
- Write the postamble only after the reader goroutine has stopped writing to `Out`.
