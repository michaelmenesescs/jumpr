# Handoff — pick up here

_Last updated: 2026-11-12 (task 003 done; M0 in progress)_

## State

- [x] Interview + follow-up → `docs/INTERVIEW.md`
- [x] Spec **v0.2** → `docs/SPEC.md` (tmux-style multiplexer and sessions §21, deeper Salt §8.6, isd parity
      §8.1.1, lab §22)
- [x] End-to-end build plan → `docs/BUILD-PLAN.md` (decisions, milestones, package layout, **frozen Go
      contracts**, messages, session-server protocol, errors, concurrency, tests, dependencies, risks)
- [x] Task breakdown → `docs/tasks/` (one file per task; index and workflow in `docs/tasks/README.md`)
- [x] Lab controller `cmd/sitrep-lab` and `lab/` (separate track; see `lab/README.md`)
- [x] **Task 001 done:** `go.mod`/`go.sum` with every M0–M7 dependency pinned (BUILD-PLAN §10), `Makefile`
      (`build test lint lab-test check tidy`), `cmd/sitrep/main.go` (version only), `internal/deps/deps.go`
      (blank imports under `//go:build deps`). `make check` green; `./sitrep version` → `sitrep dev`.

- [x] **Task 002 done:** `internal/errs` exactly as BUILD-PLAN §4.1 — `Kind` constants, `Error`
      (`Kind/Op/Host/Msg/Err`), `New`, `Wrap`, `WithHost` (copy), `KindOf` (`errors.As` → ctx deadline/cancel →
      `Internal`, `nil` → `""`) and `Is`. `Error()` renders `op: host: msg: cause`, skipping empty parts and their
      separator; `Wrap(kind, op, nil)` stays non-nil with no cause. Table-driven tests cover formats, `errors.Is`
      unwrapping, copy semantics, and `KindOf` via direct, `%w`-wrapped and double-wrapped errors.

- [x] **Task 003 done:** `internal/config` — `Config` with all eight sections and every key of BUILD-PLAN §11
      (`yaml` tags, strict decode), `Default()`, `Duration` (go duration strings, `MarshalYAML` writes the
      canonical `time.Duration.String()`), `Paths`/`ResolvePaths` (home injected, no env access), `ExpandHome`,
      `Load(path, home)` (missing file → defaults, `KnownFields(true)`, every error `*errs.Error` kind `Config`
      naming the file) and `Validate()` (prefix regex, positive limits/timeouts, `ui.color` set, source types,
      static needs `path`, rewrites must compile). Testdata: `full.yaml`, `bad-prefix.yaml`.

## Next task

**`docs/tasks/004-selector-timewindow.md`** (or another M0-A task 008/009, which may run in parallel), then
continue in numeric order. Tasks that share a parallel group in `docs/tasks/README.md` may be done concurrently;
everything else is serial.

Workflow per task: see `docs/tasks/README.md` ("Workflow"). Rules: `AGENTS.md`.

## Milestones (detail in `docs/BUILD-PLAN.md` §2)

| M | Name | Tasks | State |
|---|---|---|---|
| M0 | Skeleton (fake data, k9s-style shell) | 001–016 | todo |
| M1 | Multiplexer in-process (panes, windows, prefix, copy mode) | 017–025 | todo |
| M2 | SSH + Linux core (bastion pool, probes, views, shell pane) | 026–053 | todo |
| M3 | Actions + investigations (bus, policy, Markdown records) | 054–065 | todo |
| M4 | Salt core — **first fully usable release (v0.5)** | 066–076 | todo |
| M5 | Session server (detach/reattach) | 077–083 | todo |
| M6 | systemd + Linux depth (isd parity) | 084–091 | todo |
| M7 | Salt depth | 092–097 | todo |
| M8 | HTTP layer + Prometheus/Alertmanager | 098–104 | todo |
| M9 | Kubernetes | 105–112 | todo |
| M10 | Chef + YAML plugins | 113–118 | todo |
| M11 | Grafana + Elasticsearch | 119–122 | todo |
| M12 | Hardening | 123–127 | todo |

## Blocked

_Nothing. When a task cannot proceed without changing a contract in BUILD-PLAN, or needs a decision, write it
here (task number, what is needed, what you tried) and stop._

## Decisions waiting for the user (none block M0–M2)

See SPEC §20 "Open". The ones that affect upcoming work:

1. Prefix key: `C-a`, switching to `C-b` inside tmux (current plan), or one fixed key? (affects task 018/022)
2. Raw synchronized input: keep the opt-in raw mode, or broadcast-line only? (task 064)
3. Salt transport leaves commands and output in the master's job cache: fine as a labelled trade-off? (task 073)
4. Drop-in edits: preview only (current plan) or allow applying as a recorded action? (task 087)
5. Local shell panes on the laptop: yes or no? (not scheduled; `SITREP` env guard is prepared in task 083)

## Requests to the lab track

The lab (`cmd/sitrep-lab`, `lab/`) is built separately. Things the tasks would like it to provide; until then the
lab tests create these situations themselves with `labtest.Break` (lab-only SSH commands with cleanup):

- fault `minion-down <host>` (stop salt-minion), `salt-drift <host>` (edit a managed file), `pending-key` (an
  unaccepted minion key), `failing-state` — used by tasks 067, 076, 095
- a `reset` (undo all faults) subcommand, or keep `fault <name> --undo` for each
- bastion sshd with `MaxSessions 10` and default `MaxStartups` (to exercise pool limits, task 035)
- observability stack + mock SSO cookie proxy (Go) for M8: Prometheus, Alertmanager, Grafana, Elasticsearch, with
  an alert that fires when `haproxy-443` is injected
- a `lab/sitrep/` directory is owned by the implementation tasks (sitrep's config for the lab), not by the lab
  track

## Ground rules (from the user)

- Nothing runs or installs on server nodes. Client-side only, macOS only.
- Work tool: **no employer names, hostnames, or conventions** anywhere in the repo. Use `example.internal`.
- Never commit secrets. Credentials go through Keychain/helpers only.
- Reliability over breadth. Each milestone must end usable.
- All helper tooling is written in Go.
- Small commits (one per task); update this file at the end of every session.

## Repo

- Local: `~/workspace/sitrep` (git, branch `main`). **No remote yet.** Ask before creating one (work project; it
  may need to live on a work Git host instead of personal GitHub).
- Toolchain: go 1.27.2 darwin/arm64.

## Session log

_Append one entry per session: date, tasks done, anything surprising, measured numbers._

- 2026-11-12 — Scoping: spec v0.2, build plan, tasks 001–127 written. Dependency set verified to compile
  together with `CGO_ENABLED=0` (Bubble Tea v1.3.10 + `charmbracelet/x/vt` v0.1.0 and the rest of BUILD-PLAN §10).
- 2026-11-12 — Task 001: repo bootstrap. All 17 pinned M0–M7 modules in `go.mod` (held by the blank imports in
  `internal/deps`), `Makefile`, `cmd/sitrep` (version printing), `.gitignore` additions. `make check` green;
  `go mod tidy` idempotent; `GOARCH=amd64` cross-build OK; `cmd/sitrep-lab` still builds. Choice: `lint` is two
  recipe lines, `test -z "$(gofmt -l .)"` then `go vet ./...`, so unformatted files fail it (task Pitfalls).
- 2026-11-12 — Task 002: `internal/errs`. Choice: `Is(nil, "")` is `true`, because task requirement 6 defines
  `Is` as literally `KindOf(err) == kind` and `KindOf(nil)` is `""`; the first test draft assumed otherwise and was
  corrected, not the code. `make check` green (`internal/errs` tests 1.3 s under `-race`).
- 2026-11-12 — Task 003: `internal/config`. Choices: (1) `Load` expands `~` and then calls `Validate`, so a bad
  value fails at load time with the file path attached; (2) `ssh.known_hosts` is `[]string` because §11's default
  is `[]`; (3) `MarshalYAML` emits the canonical `time.Duration.String()` form (`5m` → `5m0s`), so the round-trip
  test compares parsed durations, not source text. `make check` green (config tests 1.2 s under `-race`).
