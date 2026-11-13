# Task 051 — Interactive prompts in the TUI (host keys, passphrases)

- **Milestone:** M2
- **Depends on:** 029, 022
- **Parallel group:** M2-F (with 053)
- **Status:** done

## Goal
When SSH needs the user (unknown host key, encrypted key), a modal dialog appears; background work waits for the
answer.

## Context
BUILD-PLAN §4.8.1, §5 (`PromptMsg`), §6 (prompts while detached). SPEC §7.1 (host keys), §21.4.

## Files
- create `internal/tui/prompter.go`, `internal/tui/dialog_interact.go`, `internal/tui/prompter_test.go`
- modify `internal/tui/msgs.go` (`PromptMsg`), `internal/tui/root.go`

## Requirements
1. `type Prompter struct` implementing `interact.Prompter`: `func NewPrompter(send func(tea.Msg), timeout
   time.Duration) *Prompter`. `Ask` creates a reply channel, calls `send(PromptMsg{…})`, and waits for the
   answer, `ctx.Done()`, or the timeout → `errs.Interaction`. Prompts are serialized with a mutex so only one is
   on screen. `send` is `program.Send` supplied by `app` (the one allowed use outside `app`/`server`).
2. `SetAttached(bool)`: when not attached, `Ask` waits up to the timeout for attachment, then fails with
   `errs.Interaction` (always attached before M5).
3. Dialogs:
   - Host key: title `Unknown host key`, body with host, key type, SHA256 fingerprint; choices
     `[o] once  [a] always (add to known_hosts)  [r] reject`; `esc` = reject.
   - Passphrase: masked text input; `enter` submits; `esc` cancels.
   - Confirm: `y`/`n`.
4. The root shows the prompt dialog above everything else, including other dialogs; the answer is sent on
   `PromptMsg.Reply` (buffered channel of 1) from a `tea.Cmd`.
5. Identical host-key prompts (same host + fingerprint) asked concurrently by several connections are answered
   once: cache "once" answers in the `Prompter` for the process lifetime.

## Out of scope
Wiring into the SSH pool (052 passes this prompter in `DialOptions`).

## Acceptance
```
go test -race ./internal/tui/ -run 'Prompter|Interact' -v
# TestAskReturnsAnswer (a fake send that replies), TestAskTimeout → errs.Interaction, TestAskCanceledCtx,
# TestSerialized (two concurrent Asks are shown one after the other), TestHostKeyOnceCached,
# TestRootShowsHostKeyDialog (teatest: PromptMsg injected with tm.Send → output contains the fingerprint; "o" →
#   reply received with Choice "once")
```

## Pitfalls
- `Ask` is called from SSH goroutines, never from `Update`. Calling it from `Update` deadlocks.
- Never log or record the passphrase; do not keep it in the dialog struct after submit.
