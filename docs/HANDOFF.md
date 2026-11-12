# Handoff — pick up here

_Last updated: 2026-11-12_

## State

- [x] Interview done → `docs/INTERVIEW.md`
- [x] Spec v0.1 drafted → `docs/SPEC.md` (user has reviewed the summary; no changes requested yet)
- [ ] Spec open questions (SPEC §20); none block Phase 0
- [ ] **Phase 0 — Skeleton**: not started. No Go code exists yet.

## Next session: start Phase 0 (SPEC §19)

1. `go mod init` (module path TBD; ask the user, or use `sitrep` locally). Toolchain on the Mac mini: go 1.25.5
   darwin/arm64.
2. Package layout per SPEC §6 (`cmd/sitrep`, `internal/...`).
3. Config loader (`~/.config/sitrep/{config,connections,policy,keymap}.yaml`) with validation and `sitrep init`.
4. Resource model: `Kind`, `Resource`, `Ref`, `Link`, `Selector` (parser + tests), `TimeWindow`.
5. TUI shell (Bubble Tea): command bar `:kind selector`, `/` filter, view stack + breadcrumbs, status line with
   backend health dots, `?` help, async message pattern.
6. Fake backend to drive the UI. Wire up `go test ./...`, `golangci-lint`, and `CGO_ENABLED=0` build in a Makefile.
7. Exit: `sitrep` runs and navigates fake data; tests green. Then Phase 1 (SSH + Linux).

## Ground rules (from the user)

- Nothing runs or installs on server nodes. Client-side only, macOS only.
- Work tool: **no employer names, hostnames, or conventions** anywhere in the repo. Use `example.internal`.
- Never commit secrets. Credentials go through Keychain/helpers only.
- Reliability over breadth. Each phase must end usable.
- Small commits; update this file at the end of every session.

## Repo

- Local: `~/workspace/sitrep` (git, branch `main`). **No remote yet.** Ask before creating one (work project; it
  may need to live on a work Git host instead of personal GitHub).
