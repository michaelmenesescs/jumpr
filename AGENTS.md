# Agent instructions — jumper

Read these files in order before doing anything:

1. `docs/HANDOFF.md` — current state, the next task, anything blocked
2. `docs/tasks/README.md` — the task index and the per-task workflow
3. The task file you are about to do (`docs/tasks/NNN-*.md`)
4. `docs/BUILD-PLAN.md` — the contract: package layout, Go interfaces, messages, protocol, error kinds, dependencies
5. `docs/SPEC.md` — the spec (what the product does and why)
6. `docs/INTERVIEW.md` — the user's actual answers (wins over the spec if they conflict)

## Rules that always apply

- Go, single static binary (`CGO_ENABLED=0`), macOS client. Zero footprint on remote nodes: no installs, no file
  writes, only pre-existing commands over SSH.
- Neutral repo: no employer-specific names, hosts, or conventions. Use `example.internal` in examples.
- Never commit secrets or real hostnames/IPs. Sanitize any captured `testdata/`.
- Follow the task order in `docs/tasks/README.md` (milestones M0 → M12). Keep `go test ./...` green, make small
  commits, and update `docs/HANDOFF.md` before ending a session.

## Rules for the implementing agent

1. **One task at a time, in order.** Do exactly what the task file says. Touch only the files it lists. If you
   believe another file must change, say why in `docs/HANDOFF.md` and keep the change minimal.
2. **The contract is frozen.** Never change an interface, type, message, frame, error kind or config key defined
   in `docs/BUILD-PLAN.md` (§4, §5, §6.2, §7, §11). If a task cannot be done without such a change, **stop**:
   write the problem under "Blocked" in `docs/HANDOFF.md` and end the session. Do not work around it.
3. **No new dependencies.** Only the modules listed in `docs/BUILD-PLAN.md` §10, at the pinned versions. Bubble
   Tea **v1** import paths only (`github.com/charmbracelet/bubbletea`); never `charm.land/.../v2`. No `go get`
   unless the task says so.
4. **All helper and dev tooling is Go.** Lab controller, fault injection, mock proxies, example auth helpers,
   capture/sanitize tools, golden-file updates (`go test -update`), release tooling: Go programs under `cmd/` or
   `examples/`, logic under `internal/`. No bash, Python or other script files. The Makefile only calls `go` and
   `gofmt`. (Shell one-liners that jumper sends to remote nodes are the product, not helpers. The embedded
   provisioning snippets inside `cmd/@@` belong to the lab track.)
5. **Do not edit `cmd/@@/` or `lab/`** except `lab/jumper/` (the jumper config for the lab). They belong
   to the lab track. If the lab lacks something a task needs, add it to "Requests to the lab track" in
   `docs/HANDOFF.md`.
6. **Tests must not need the lab, the network, an SSH agent or a real terminal.** Anything that needs the lab is
   in a file starting with `//go:build lab` and is named `TestLab…`. End-to-end PTY tests use `//go:build e2e`.
   `go test ./...` must pass on a laptop with the lab shut down.
7. **Detach-safety rules** (`docs/BUILD-PLAN.md` §6.1) from the first line of code: only `cmd/` and
   `internal/app` touch `os.Stdin/Stdout/Stderr/Exit/Getenv`; never `tea.ExecProcess`; never print to
   stdout/stderr from `internal/`; sizes only from `tea.WindowSizeMsg`.
8. **The UI never blocks.** No I/O, sleeps or locks held across I/O in `Update`, `View` or `Render`. Remote work
   runs in a `tea.Cmd`; streams use `tui.NextBatch`. Every remote call has a timeout.
9. **Every write goes through the action bus.** Nothing outside `internal/bus` calls `action.Step.Run`. Backends
   only plan. A dry run is not a write; anything else that changes a node, a cluster, a Salt key or a silence is.
10. **Remote commands are built with `internal/shq` only.** No `fmt.Sprintf` into a shell line. User input never
    reaches a command line unquoted or unvalidated.
11. **Errors that cross a package boundary are `*errs.Error`** with the right kind. Remote output never causes a
    panic; parsers skip what they do not understand.
12. **Secrets:** never log, record or display credentials, passphrases, pillar values, cookies or tokens. Pass
    text through `internal/redact` before storing or showing it.
13. **No drive-by changes:** no refactors, renames, reformatting of untouched code, or "improvements" outside the
    task. No `utils`/`common` packages. No `init()` registration. No package-level mutable state.
14. **When something is ambiguous, pick the option that matches the task's acceptance commands**, write one line
    about the choice in `docs/HANDOFF.md`, and move on. When something contradicts the contract, stop (rule 2).
15. **Done means:** the task's acceptance commands pass, `make check` passes, the task's status is updated in
    `docs/tasks/README.md` and in the task file, `docs/HANDOFF.md` is updated, and there is exactly one commit
    named `task NNN: <summary>`. Never commit a failing tree. Never push; there is no remote.
