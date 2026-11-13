# Task 025 — Prove the app runs over a plain byte stream (detach de-risk)

- **Milestone:** M1
- **Depends on:** 024
- **Parallel group:** — (serial; touches the root or `app`)
- **Status:** done

## Goal
Run the entire app with no TTY: input and output are pipes, the size is injected. Plus an automated guard for the
detach-safety rules. After this, the session server (M5) is only transport.

## Context
BUILD-PLAN §6 and §6.1, D4, D5, §12 (risk: detach/reattach).

## Files
- modify `internal/app/app.go` (size injection)
- create `internal/app/pipe_test.go`
- create `internal/app/rules_test.go`

## Requirements
1. Extend `app.Options` with `Size *Size` (`type Size struct{ Cols, Rows int }`) and `Resizes <-chan Size`.
   When `Size` is non-nil, `Run` sends `tea.WindowSizeMsg{Width, Height}` to the program right after start and
   again for every value from `Resizes` (goroutine that stops with the context). When nil (real TTY), Bubble Tea
   detects the size itself.
2. Add `Options.Ready chan<- struct{}` (optional): closed once the program is running and the first size was
   sent. Tests wait on it instead of sleeping.
3. `pipe_test.go` — `TestRunOverPipes`: start `Run` in a goroutine with `In` = read end of an `io.Pipe`, `Out` = a
   concurrency-safe buffer, `Demo: true`, `Size{120, 36}`, temp config/cache dirs, colour profile `none`.
   - assert the buffer eventually contains `web-ewr-001`;
   - write `:alerts\r` to the input pipe → eventually contains `HighErrorRate`;
   - write the prefix byte `0x01` then `%` → eventually two pane borders (assert on two occurrences of a pane
     title pattern);
   - send a resize to `80×24` → later frames are 80 columns wide (check a status line length after stripping ANSI);
   - cancel the context → `Run` returns within 2 s; no goroutine is still writing to the buffer afterwards.
4. `rules_test.go` — `TestDetachSafetyRules`: walk every non-test `.go` file under `internal/` (use `go/parser`
   and `go/ast`) and fail, listing file and line, on:
   - selector expressions `os.Stdin`, `os.Stdout`, `os.Stderr`, `os.Exit`, `os.Getenv`, `os.LookupEnv` outside
     `internal/app`;
   - calls to `tea.ExecProcess`, `tea.Suspend`, `tea.Printf`, `tea.Println`;
   - calls to `fmt.Print`, `fmt.Println`, `fmt.Printf`, `log.Print*`, `log.Fatal*`, `log.Panic*`;
   - an import of `os/exec` outside `internal/app`, `internal/transport/sysssh`, `internal/transport/httpx`.
   The walk root is found from the test's working directory (`../..`).

## Out of scope
Sockets, daemons, swapping the output writer while running (M5).

## Acceptance
```
go test -race ./internal/app/ -run 'RunOverPipes|DetachSafetyRules' -v -count=3
make check
```

## Pitfalls
- A raw `ctrl+a` is the single byte `0x01`; Enter is `\r` (not `\n`) in raw terminal input.
- Bubble Tea writes frames asynchronously (about 60 fps); poll the buffer with a deadline, never assert right
  after a write.
- The output buffer needs a mutex; `bytes.Buffer` alone is a data race under `-race`.
- If `rules_test.go` flags existing code, fix the code (pass the value in from `app`), do not add exceptions.
