# Task 082 — Detach UX: sessions in the TUI, prompts, environment, restore after restart

- **Milestone:** M5
- **Depends on:** 081
- **Parallel group:** — (serial)
- **Status:** todo

## Goal
Everything a user touches around sessions feels like tmux.

## Context
SPEC §21.2 (`d`, `s`, `$`), §21.4. BUILD-PLAN §6.

## Files
- create `internal/tui/commands_session.go`, `internal/tui/dialog_sessions.go`, `internal/tui/session_test.go`
- modify `internal/tui/root.go`, `internal/app/session.go`, `internal/app/serve.go`, `internal/server/server.go`

## Requirements
1. `prefix d` / `:detach` detach the client (server mode). In standalone mode: flash `standalone mode: nothing to
   detach from; :q quits`.
2. `:q` / last pane closed in server mode: confirm `end session "main"? shells and follows will stop (y/N)`
   — detaching is the non-destructive default and the dialog says so (`d` = detach instead).
3. `prefix s` / `:sessions`: list dialog (name, windows, attached, last activity); `enter` switches this client
   to the chosen session (server detaches from the current and attaches to the other without the client
   reconnecting: add a `SwitchTo(name)` request the session can raise to the server through a callback); `n`
   creates a new session (prompt for a name); `x` kills one (confirm).
4. `prefix $` / `:rename-session <name>`: renames; the autosave file is renamed too.
5. Autosave/restore across server restarts: sessions save to `<cache>/sessions/<name>.json` (task 024). When the
   server starts it lists saved sessions as **dormant**; `ls` shows them as `(saved)`; attaching restores the
   layout; terminal panes come back disconnected. `kill-session` on a dormant session deletes its file.
6. Prompts while detached: host-key and passphrase prompts wait for `interact.timeout` then fail with
   `errs.Interaction`; on the next attach a flash lists what was skipped (`2 hosts need a host-key decision:
   :health`).
7. Activity: any input or completed background work updates `Info().Activity`.
8. On attach, a one-line flash summarizes what happened while away: `while detached: rollout "restart nginx"
   paused at canary · 1 fan-out finished · 3 new journal errors` (collect counters in the root while no client
   is attached; reset on attach).

## Out of scope
Multiple clients on one session. Read-only spectators.

## Acceptance
```
go test -race ./internal/tui/ ./internal/app/ ./internal/server/ -run 'Session|Detach|Dormant|Switch' -v
# TestDetachKeepsWorkRunning, TestQuitAsksInServerMode, TestSwitchSessionKeepsConnection,
# TestDormantSessionRestoredOnAttach, TestKillDormantDeletesFile, TestPromptFailsWhenDetached,
# TestWhileDetachedSummary, TestRenameSessionRenamesFile
```

## Pitfalls
- Switching sessions must go through the same detach/attach code as a reconnect (preamble, forced repaint).
- Do not restore every saved session eagerly at server start; lazily on first attach.
