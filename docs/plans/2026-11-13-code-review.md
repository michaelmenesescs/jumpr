# Code review 2026-11-13 — Opus 5.5 and Fable 5.2 on the merged tree

Two independent reviewers read the tree at 121/132 merged tasks and reported without
running anything. Their findings overlap heavily on the session server, which is the newest and
least-covered code. This file is the record: what was fixed in place, and what is deliberately left
as a task because it needs a contract decision or a design, not a patch.

Reviewer prompts and raw output: `~/.hermes/cache/scratch/review_opus.txt`,
`~/.hermes/cache/scratch/review_fable.txt` (not committed — they contain host names from the code).

## Fixed in place (same day)

| Finding | Where | Fix |
|---|---|---|
| klog still wrote to stderr, corrupting the TUI and breaking detach-safety rule 7 | `internal/backend/k8s/client.go` `redirectKlog` | `klog.LogToStderr(false)` before `SetOutput` (commit 8ad74b9) |
| Plugin parse errors lost their cause (`errs.New(KindOf(err))`), breaking `errors.Is/As` for `Timeout`/`Canceled` | `internal/plugin/http.go` `parse` | `errs.Wrap(...).WithHost(name)` (commit 8ad74b9) |
| A failed k8s context switch silently moved the session to the kubeconfig's `current-context` — wrong-cluster writes | `internal/backend/k8s/client.go` `SwitchContext` | remember the previous context, restore it when the new one cannot be built; `ResetLocked` on failure of both (folded into task 109's commit by the scheduler's amend — see the note at the bottom) |

## Open — the reviewers agree these are real

1. **Re-attach can kick the new client ("replaced").** `internal/server/conn.go:125` ends the
   previous connection but never detaches the handle, so `internal/app/session.go:276`
   `Detach("replaced")` fires into the *new* connection's callback. Root cause: `Session.Detach`
   does not say which attachment it ends. Fix: a generation number per attach; stale detaches are
   no-ops. Test: two `sessionHandle.Attach` calls with distinct callbacks under `-race`.
2. **Per-terminal state lives on the shared `Core`.** `core.Agent` (SSH_AUTH_SOCK),
   the prompter's attached/detached flag, the `uiBridge` fan-out (every prompt goes to every
   session; first answer wins), and the k8s client's context/namespace are process-global, so
   session B's attach changes session A's world — and `:ctx prod` in A leaves B writable against a
   protected context.
3. **Plugin/remote input can reach a shell or a credentialed URL.**
   `RenderCommand` quotes with `shq.Quote`, which is only safe in an unquoted word position: a
   template such as `journalctl -u "{{ .Param "unit" }}"` puts `$(...)` inside double quotes and
   it expands. Params and link args come from row fields, i.e. remote output.
   `plugin/http.go` `RenderURL` similarly lets a host or param land in the scheme/host of the URL,
   and `httpx` then attaches the profile's Basic/Bearer credentials to whatever host it reaches.
   `emits_hosts` turns a remote-emitted name into the SSH dial target with only `NormalizeHostID`.
4. **Unbounded teardown.** `Serve` returns without joining connection goroutines or session
   `Done()`; `RunServer` closes the SSH pool under live work; `server.go:132` blocks on
   `ctx.Done()` forever when the server stops by kill-server or idle; a `Snapshot()` call from a
   conn goroutine races Bubble Tea's `Update` on the mux tree (`root.Snapshot` has no lock).
5. **Salt executor serialises `Exec` behind one job** (`executor.go:362`, unbuffered queue,
   `context.Background()` inside `runGroup`), so a cancel does not stop a job and concurrent reads
   queue for `timeout+5s`.
6. **Smaller, still real:** `switchio` pump keeps the old source's reader; `salt/events.go:150`
   holds `h.mu` across a network dial; `plugin/save.go` writes the raw `:run` line to a 0644 file
   without `redact`; `CheckReadCommand` is a heuristic that `\rm`, `sed -i`, `dd of=` and
   `systemctl --no-block restart` all pass, and a saved sudo command becomes an unconfirmed
   privileged read; server error kinds (`errs.Protocol` for "no such session") are wrong and
   `messageOf` drops the kind on the wire.

## Tasks these findings become

| Task | Scope |
|---|---|
| 154 | Attach generation numbers in `Session`/`sessionHandle`; no-op stale detaches; adapter replace test under `-race` |
| 155 | Per-session agent socket, prompter and prompt routing; k8s context+namespace per session; read-only follows the session that switched |
| 156 | Plugin safety: substitution only outside quoted regions (or a template parser that knows the quoting context), URL host allowlist per connection profile, `emits_hosts` validation, `save.go` through `redact` at 0600 |
| 157 | Server teardown: join connection goroutines and session `Done()` in `Serve`, cancel-then-close order for the SSH pool, lock `root.Snapshot`, fix the idle goroutine |
| 158 | Salt executor concurrency: per-call contexts, buffered dispatch, no `context.Background()` inside `runGroup` |

## Process note

The scheduler's merge step runs `git add -A && git commit --amend`, so *any* uncommitted change
sitting on `main` while it merges gets folded into the task commit it is finishing (this happened
to the k8s rollback fix, now inside `28496be task 109: …`). Fixes to `main` must be committed
immediately, or the scheduler paused first.
