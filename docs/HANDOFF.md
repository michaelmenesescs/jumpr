# Handoff — pick up here

_Last updated: 2026-11-14 (consolidated on the laptop, renamed to jumpr, v0.3 planned — read "v0.3 — start here")_


## v0.3 — start here (2026-11-14)

**Where the code lives now:** the laptop (`~/workspace/sitrep`) is canonical. Commit `898b9da` = the mini's `main`
(`29862a2`, 132/132 tasks) + the mini's uncommitted work (read-only guard; app/serve/spawn and k8s kind changes) +
`ui.color: auto` resolved from `TERM`/`COLORTERM`/`NO_COLOR` (the detached session server had no TTY, so it never
coloured) + the rename **jumper → jumpr** everywhere. The old laptop line is branch `backup/laptop-main-2026-11-14`.
`origin` (the mini) has not been updated.

**Verified on the laptop, 2026-11-14:** `make check` green before and after the rename; e2e PTY suite 3/3
(detach/reattach, client killed, resize); homelab suite against `macmini` green; `jumpr doctor` with
`~/.config/jumpr`: salt 7/7 minions, chef 4 nodes, colour 24-bit. **Lab suite: 26 pass, 10 fail, 14 skip** — the
failures are tasks 145 (salt hosts dialled by IP, 5 tests), 146 (first-file-only `IdentityFile`, found by doctor),
147 (Cinc 19, 2 tests) and lab requests L1 (2 tests) and L7 (1 test) below.

**Observed, not yet a task:** a jumpr process started under a tmux server got `connect: no route to host` for
192.168.x addresses while the same binary outside tmux reached them (macOS Local Network privacy for third-party
binaries). Document in the user docs; `jumpr doctor` could detect it (dial the bastion and report EHOSTUNREACH with a
hint).

**Direction (INTERVIEW "Follow-up — 2026-11-14"):** one live system graph; systemd tree, service map, request flow,
defensive security lens, time travel, local-first agent + `jumpr mcp`. eBPF opt-in only; defensive only; neutral.
SPEC v0.3 §23–§27, BUILD-PLAN §2 (M14–M19) and §14 (contracts), task index "v0.3".

**Next task:** 145, 146, 147 (parallel group M14-A), then **160 → 161 → 162 → 163** — 163 is `:tree`, the first
thing the user asked to see. Tasks 170–199 are `draft`: expand each milestone's files when it starts.

### Requests to the lab track (v0.3)

| ID | Request | Needed by |
|---|---|---|
| L1 | `fault haproxy-443` must exit 0 when nginx fails to start — that failure *is* the fault (it currently fails TestLabW1NginxConflict and TestLabFailedUnitsAfterFault) | M14 |
| L2 | A small demo app, "shop": on VMs `web-0x` nginx → `cart` service (systemd unit, any tiny HTTP server already in the distro or a Go binary built by the lab tool) → postgres on `db-01`; on k3s the same as a Deployment + Service + Ingress `shop.lab.example.internal` | 164, 165, 170 |
| L3 | `fault flow-latency`: add latency on one hop of "shop" (e.g. `tc qdisc netem` on db-01) with `--undo` | 171 |
| L4 | Optional tracing container (Jaeger all-in-one) and the demo app emitting traces | 172 |
| L5 | `bpftrace` installed on one lab host only (`web-03`), so eBPF is tested both present and absent | 174 |
| L6 | Security faults: `sec-host` (PermitRootLogin yes, sudoers wildcard, world-writable drop-in, extra SUID), `sec-k8s` (privileged pod with hostPath, broad ClusterRoleBinding), `sec-hunt` (shell spawned under nginx, new listener, unexpected authorized key); all with `--undo` | 181–184 |
| L7 | TestLabSaltKeyAcceptThroughBus: key `jumpr-stray` never becomes pending — check the stray-minion provisioning after the rename | M14 |

## State

- [x] Interview + follow-up → `docs/INTERVIEW.md`
- [x] Spec **v0.2** → `docs/SPEC.md` (tmux-style multiplexer and sessions §21, deeper Salt §8.6, isd parity
      §8.1.1, lab §22)
- [x] End-to-end build plan → `docs/BUILD-PLAN.md` (decisions, milestones, package layout, **frozen Go
      contracts**, messages, session-server protocol, errors, concurrency, tests, dependencies, risks)
- [x] Task breakdown → `docs/tasks/` (one file per task; index and workflow in `docs/tasks/README.md`)
- [x] Lab controller `cmd/@@` and `lab/` (separate track; see `lab/README.md`)
- [x] **Task 001 done:** `go.mod`/`go.sum` with every M0–M7 dependency pinned (BUILD-PLAN §10), `Makefile`
      (`build test lint lab-test check tidy`), `cmd/jumpr/main.go` (version only), `internal/deps/deps.go`
      (blank imports under `//go:build deps`). `make check` green; `./jumpr version` → `jumpr dev`.

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

- [x] **Task 004 done:** `internal/resource` — `Selector`/`Term`/`Op` exactly as §4.2 with the quote-aware
      tokenizer, earliest-operator split (`!=`/`!~` before `=`/`~`), key validation, regexes compiled once into
      an unexported `Term.re`, `Matches` (AND, missing keys satisfy the negated forms, sets ignored),
      canonical `String()` that round-trips, and `ParseSelector` (empty → zero, errors `errs.Config` naming the
      token). `TimeWindow` with relative offsets (`s m h d w`), ranges (`now`, relative, RFC 3339,
      `2006-01-02 15:04`, `2006-01-02T15:04`, `15:04` today in `now`'s location), `Bounds`, `IsZero`, `String`.

- [x] **Task 005 done:** `internal/resource` `resource.go` (HostID, KindName, Ref, Status, Resource, ColumnKind,
      Align, Column, Scope, KindSpec, Query, Link verbatim from §4.2; `Ref.String`, `ParseRef`,
      `NormalizeHostID`, `KindSpec.Column`) and `fields.go` (`Str`, `Int`, `Float`, `Time`, `Compare`).

- [x] **Task 006 done:** `internal/backend` — `backend.go` (HealthState, Health, Result, EventType, Event,
      Backend, Watcher, LinkProvider verbatim from §4.4; `HealthState.String`) and `registry.go`
      (`NewRegistry`, `Register`, `Replace`, `Get`, `Backends`, `Lookup`, `Kinds`; one `sync.RWMutex`; kind/alias
      keys lower-cased; `Register` errors `errs.Config` on nil/empty name, duplicate backend name and any kind or
      alias already taken; `Replace` releases the old backend's keys, swaps in place and restores them if the new
      keys collide; `Kinds()` sorted by name; `Backends()` in registration order). Tests use a stub `Backend`.

- [x] **Task 007 done:** `internal/backend/fake` — `New(Options{Latency, FailHosts, Now})`, `Name() = "fake"`,
      kinds `host` (hosts/h), `unit` (units/u, host-scoped) and `alert` (alerts/al) with the mockup columns;
      `world.go` derives every value from `fnv64a(name+":"+key)` (1,512 hosts = 504 each in ewr/lax/ord, labels
      host/role=web/dc/env=prod, sparkline, six alerts), with `web-ewr-007` and `web-ewr-012` carrying a failed
      `nginx.service` and an active `haproxy.service`; `List` streams ≤100 rows per `Result` with `Done/Total`,
      honours `ctx` while sleeping and sending, and turns `FailHosts` into `Result{Host, Err}`; `Get`, `Links`
      (`u` → unit) and `Health` (`ok`/`fake data`) implemented. Tests cover determinism, 504/1,008/1,512 sizes,
      batching, failed hosts (both kinds), cancel, Get/Links/Health and unknown kinds.

- [x] **Task 008 done:** `internal/render` (pure, no lipgloss) — `fit.go` (`Width` via `ansi.StringWidth`,
      `Truncate` with a `…` tail and a closing reset for cut coloured text, `PadRight`/`PadLeft`, `Fit` returning
      exactly `h` lines of exactly `w` cells with `h-1` newlines) and `table.go` (`Cell`, `TableColumn`,
      `ColumnWidths`, `Row`, `Header`). Tests cover wide runes, ANSI colour, truncation narrower than the text, and
      a 500-case property test on `Fit`.

- [x] **Task 009 done:** `internal/render` formatters — `spark.go` (`Spark`: eight-block ramp scaled to the finite
      peak, NaN → space, averaging downsample when too long, left-pad when too short, exactly `width` cells;
      `Bar`: `█`/`·`, fraction clamped 0..1) and `human.go` (`Int` with `,` groups and a sign-safe `MinInt64`
      path, `Bytes` IEC with one decimal above bytes, `Duration` two units max (`d h m s`, sub-second → `0s`),
      `Age` (`—` for zero time), `Percent` (`71%` at ≥ 10, `3.2%` below)). Tests cover every format, NaN and
      negative inputs, MinInt64 and a width property over 8 series × 20 widths.

- [x] **Task 010 done:** `internal/tui/theme` — `Flash` and `Theme` with all 21 `lipgloss.Style` fields of the
      task, `Dark()` using fixed 256-colour codes (252 base, 245/240 dim/faint, 42/214/203 ok/warn/crit, 75
      accent, 237 selected background), `SetProfile` (`auto|truecolor|256|16|none`, unknown names ignored) and
      `(*Theme).StatusStyle` (Unknown→Base, OK/Warn/Crit→their colours, NA→Faint). `internal/tui/keymap` —
      `Binding{Keys,Help}` with `Matches`, `Map` with all 23 global bindings and `Default()`, `ParsePrefix`
      (`C-a`→`ctrl+a`, `C-\`→`ctrl+\\`, `C-@`→`ctrl+@`, `C-]`/`C-^`/`C-_`; `errs.Config` otherwise) and
      `DisplayKey` (`ctrl+a`→`C-a`, everything else unchanged). Tests cover the palette under profiles
      256/16/none, every status, the full prefix table, matching, a ctrl-key display round-trip and the default
      key table.

- [x] **Task 011 done:** `internal/tui` core — `view.go` (`Subject`, `KeyHint`, `View`, `Factory` verbatim §4.8),
      `env.go` (`Env` with `Registry/Session/Theme/Keys/Now/Factories/Log`, `SessionState` with
      `Name/Window/ReadOnly`), `msgs.go` (navigation, flash + 4 levels, close-dialog, run-command, window-changed,
      health, list-batch and lines messages; `Push`/`Pop`/`Flash`/`OpenQuery` command constructors), `stream.go`
      (`NextBatch[T]` — first item blocks, then drains to `max` or 50 ms, returning `closed` together with the
      last items; `ListBatch(seq, ch)`, 200 results per message) and `stack.go` (`Stack`: `NewStack`, `Top`, `Len`,
      `Crumbs`, `Push`, `Pop` never the root, `Replace`, `Update` handling the three navigation messages and
      routing everything else to the top view, `Render` through `render.Fit`). Tests: 7 NextBatch/ListBatch cases
      and 7 Stack cases.

- [x] **Task 012 done:** `internal/tui/views` — `table.go` (generic `Table` implementing `tui.View`: streaming
      `Init`/`restart` on a cancellable `context.Background()` with `Seq` guards, batch append + per-host errors +
      re-sort + cursor kept on the same `Ref`, keys from `env.Keys` for up/down/page/top/bottom/`/`/`r`/`enter`/
      `esc`/`e` plus `s`/`S` sort cycling and backend `Link.Key` pivots, case-insensitive substring and `~`-regex
      filter, windowed `Render` with banner + header + selected row + Crit error rows, `Crumb`/`Hints`/`Subject`/
      `CopyLines`) and `table_format.go` (`cellText` by `ColumnKind`: Number/Bytes/Percent/Duration/Time/Spark/
      Status, nil → `—`). Tests: 8 `TestTable…` cases over the fake 1,512-host fleet.

- [x] **Task 013 done:** `internal/tui/views` `text.go` — `Text` implementing `tui.View` with `NewText`/`NewKV`
      (tabs expand to 4 spaces, keys styled `Theme.Dim`), `SetLines`/`Append`/`AppendError`/`SetSeq`/`Seq`, a
      50,000-line cap that drops the oldest lines and shifts scroll and match indices, follow (`w`) pinning the
      bottom, wrap (`W`, off by default) with `ansi.Wrap`, horizontal scrolling in 8-cell steps (`left`/`right`,
      wrapless) via `ansi.TruncateLeft`, `/` search (capturing; `enter` jumps, `n`/`N` cycle, matches highlighted
      with `Theme.Selected` through cell-based `ansi.Cut`), `esc` clearing the search then popping, the
      `line n/total · follow · /pattern (n matches)` footer, `tui.LinesMsg` gated on `Seq` (closed+err appends a
      Crit `stream ended: <kind>`), `Crumb`/`Hints`/`Subject`/`Capturing`/`CopyLines` (ANSI stripped). Tests: 8
      `TestText…` cases.

- [x] **Task 014 done:** `internal/tui/commands.go` — `Command` verbatim §4.8, `Commands`
      (`Register`/`Lookup`/`All` sorted/`Exec`), `RegisterBuiltins` (`q`/`quit`, `help` → `HelpRequestedMsg`,
      `kinds` → `KindsRequestedMsg`, `time`/`t` → `resource.ParseTimeWindow` → `WindowChangedMsg`); a line whose
      first word is a registered kind or alias becomes `Query{Kind: spec.Name, Selector: ParseSelector(args)}`.
      `cmdbar.go` — `CmdBar` over `bubbles/textinput` with `Open`/`Active`/`Update`/`View` exactly `w` cells,
      in-memory history (no consecutive duplicates, max 200) with `History`/`SetHistory`, up/down walking, and tab
      completion (commands + kinds + aliases for the first word, `Command.Complete` for arguments; one candidate
      is inserted, several are listed in `Theme.Dim` and further tabs cycle). Tests: 9 `TestCmdBar…`/`TestTab…`
      and 11 `TestCommands…`/`TestExec…`/`TestTime…` cases.

## Next task

**`docs/tasks/015-statusline-hints-dialogs.md`** (needs 011, done) is the last open M0-C task; it touches
`internal/tui/statusline.go`, `hints.go`, `dialog.go`, `dialog_help.go` and does not overlap with 014. 016 then needs
003, 007, 012, 013, 014, 015.

Task 016 must build the command bar as `tui.NewCmdBar(env, cmds)` (it needs both for tab completion) and route
`HelpRequestedMsg`/`KindsRequestedMsg` — both are defined in `internal/tui/commands.go`.

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

The lab (`cmd/@@`, `lab/`) is built separately. Things the tasks would like it to provide; until then the
lab tests create these situations themselves with `labtest.Break` (lab-only SSH commands with cleanup):

- fault `minion-down <host>` (stop salt-minion), `salt-drift <host>` (edit a managed file), `pending-key` (an
  unaccepted minion key), `failing-state` — used by tasks 067, 076, 095
- a `reset` (undo all faults) subcommand, or keep `fault <name> --undo` for each
- bastion sshd with `MaxSessions 10` and default `MaxStartups` (to exercise pool limits, task 035)
- observability stack + mock SSO cookie proxy (Go) for M8: Prometheus, Alertmanager, Grafana, Elasticsearch, with
  an alert that fires when `haproxy-443` is injected
- a `lab/jumpr/` directory is owned by the implementation tasks (jumpr's config for the lab), not by the lab
  track

## Ground rules (from the user)

- Nothing runs or installs on server nodes. Client-side only, macOS only.
- Work tool: **no employer names, hostnames, or conventions** anywhere in the repo. Use `example.internal`.
- Never commit secrets. Credentials go through Keychain/helpers only.
- Reliability over breadth. Each milestone must end usable.
- All helper tooling is written in Go.
- Small commits (one per task); update this file at the end of every session.

## Repo

- Local: `~/workspace/sitrep` (git, branch `main`; directory keeps its old name, the module/binary/paths are `jumpr`).
  **No remote yet.** Personal use now, so a private personal GitHub repo is the sane default when you want one.
- Toolchain: go 1.27.2 darwin/arm64.

## Session log

_Append one entry per session: date, tasks done, anything surprising, measured numbers._

- 2026-11-12 — Scoping: spec v0.2, build plan, tasks 001–127 written. Dependency set verified to compile
  together with `CGO_ENABLED=0` (Bubble Tea v1.3.10 + `charmbracelet/x/vt` v0.1.0 and the rest of BUILD-PLAN §10).
- 2026-11-12 — Task 001: repo bootstrap. All 17 pinned M0–M7 modules in `go.mod` (held by the blank imports in
  `internal/deps`), `Makefile`, `cmd/jumpr` (version printing), `.gitignore` additions. `make check` green;
  `go mod tidy` idempotent; `GOARCH=amd64` cross-build OK; `cmd/@@` still builds. Choice: `lint` is two
  recipe lines, `test -z "$(gofmt -l .)"` then `go vet ./...`, so unformatted files fail it (task Pitfalls).
- 2026-11-12 — Task 002: `internal/errs`. Choice: `Is(nil, "")` is `true`, because task requirement 6 defines
  `Is` as literally `KindOf(err) == kind` and `KindOf(nil)` is `""`; the first test draft assumed otherwise and was
  corrected, not the code. `make check` green (`internal/errs` tests 1.3 s under `-race`).
- 2026-11-12 — Task 003: `internal/config`. Choices: (1) `Load` expands `~` and then calls `Validate`, so a bad
  value fails at load time with the file path attached; (2) `ssh.known_hosts` is `[]string` because §11's default
  is `[]`; (3) `MarshalYAML` emits the canonical `time.Duration.String()` form (`5m` → `5m0s`), so the round-trip
  test compares parsed durations, not source text. `make check` green (config tests 1.2 s under `-race`).
- 2026-11-12 — Task 004: `internal/resource` selector and time window. Choices: (1) term operators are found at
  the earliest position with two-character ops winning ties, so `label~^a=b` stays a regex value while `a!=b`
  never parses as key `a!` (the task's ordering rule, satisfied without scanning every operator form first);
  (2) inside quotes only `\"` is special, other backslashes are kept so quoted RE2 values survive (the spec says
  "backslash escapes a quote", not "escapes everything"); (3) `ParseSelector(s.String())` equality is checked on
  terms/sets, since `Raw` necessarily differs after canonicalisation. `make check` green (resource tests 1.3 s
  under `-race`).
- 2026-11-12 — Task 005: `internal/resource` model types and field helpers. Choices: (1) `ParseRef` validates
  only the part count (exactly 5, empty parts allowed), because `Ref.String` deliberately keeps empty parts;
  (2) `Str` renders `time.Time` as RFC 3339 nano and sparklines comma-separated, and falls back to `fmt.Sprint`
  for values outside the allowed set; (3) `Int` accepts `float64`/`float32` only when integral and in range
  (no silent truncation), `Compare` orders ints as `int64` and mixed numbers as `float64`, `nil` always last.
  `make check` green (resource tests 1.2 s under `-race`).
- 2026-11-12 — Task 006: `internal/backend` interface + registry. Choices: (1) `Lookup`/`Get` compare trimmed,
  lower-cased keys, so kind/alias matching is genuinely case-insensitive; (2) an empty kind name or alias is
  rejected as `errs.Config` too (not just the three listed cases), because an empty key would make `Lookup("")`
  resolve; (3) `Replace` deletes the old backend's keys, claims the new ones and re-inserts the old ones on
  failure, so a rejected Replace leaves the registry untouched — an unregistered name is an `errs.Config` error;
  (4) `Backend.Name()` is never called while the mutex is held (the registered name is stored in the entries).
  `make check` green (backend tests 1.2 s under `-race`).
- 2026-11-12 — Task 007: `internal/backend/fake`. Choices: (1) the `unit` column `load` is systemctl's load state
  (`loaded`, text), because `active`/`sub` are the separate ACTIVE/SUB columns; (2) a healthy host's `units` field
  is nil so the table renders `—` as in the mockup, and only failed hosts carry `nginx`; (3) `Get` validates host
  names against the 1,512-host fleet and returns `not-available` otherwise, while `List` with explicit
  `Query.Hosts` accepts the given names (tests may inject hosts); (4) alert rows put `alertname`/`severity` plus
  the extra labels into `Resource.Labels` so selectors filter alerts, and the `labels` column renders the extra
  labels sorted as `k="v"`, `k="v"`; (5) the `unit` kind applies `Query.Selector` only when `Query.Hosts` is
  empty (Query's "Hosts wins" rule) and filters by `Args["unit"]` when set. `make check` green; full fleet list
  is ~30 ms under `-race`, cancellation stops the producer within one latency tick.
- 2026-11-12 — Task 008: `internal/render` fit + table. Choices: (1) `Truncate` appends `\x1b[0m` only when the
  input contained an escape and something was cut, so plain strings stay untouched; (2) `ColumnWidths` shrinks
  fixed columns strictly from the rightmost one down to 3, and when the total cannot fit every flexible column's
  minimum of 4 it returns the minimums rather than going below them (the row is then wider than `total`);
  (3) `Header` uses `Row`, so numeric titles follow `AlignRight`; (4) `Fit` splits on `\n` only (no CRLF
  normalisation). `make check` green; render tests 1.3 s under `-race`, `Fit` property test 500 random cases.
- 2026-11-12 — Task 009: `internal/render` spark/bar + human formatters. Choices: (1) `Spark` scales to the
  largest *finite* (non-NaN) value and treats a non-positive peak as flat `▁`, so all-negative series do not
  index below the ramp; (2) downsampled buckets average the non-NaN entries (all-NaN bucket → space), using
  fractional bucket edges `i*n/width`; (3) `Duration` shows the largest unit plus at most one smaller one, so
  `38m 12s` keeps its seconds while `41d 3h 20m` stops at hours; sub-second → `0s`; (4) `Int`/`Bytes` take the
  magnitude as `uint64` so `math.MinInt64` formats instead of wrapping; (5) `Bar` rounds to the nearest cell
  (`0.3` of 3 → one block) and treats NaN as zero. `make check` green; render tests 1.3 s under `-race`.
- 2026-11-12 — Task 010: `internal/tui/theme` + `internal/tui/keymap`. Choices: (1) `SetProfile` imports
  `muesli/termenv` for the profile constants — it is already pinned in `go.mod`/`go.sum` at v0.16.0 as lipgloss's
  own dependency and `lipgloss.SetColorProfile` takes a `termenv.Profile`, so no new module and no `go.mod` edit
  (`go mod tidy` would only move the `// indirect` marker); (2) an unknown profile name is a silent no-op because
  the task fixes the signature as `func SetProfile(name string)` with no error — config validation already rejects
  anything outside the set; (3) `ParsePrefix` accepts exactly the control keys Bubble Tea has names for (`a`–`z`,
  `@`, `\`, `]`, `^`, `_`) and rejects `C-[` because the key arrives as `esc`, so it could never match. `make
  check` green; the new tests take 1.2 s under `-race`.
- 2026-11-12 — Task 011: `internal/tui` core (`view.go`, `env.go`, `msgs.go`, `stream.go`, `stack.go`). Choices:
  (1) `NextBatch` starts its 50 ms window after the first item and waits inside that window for more, so a sparse
  stream costs up to one 50 ms tick per message — that is exactly the "~20 batches/s per stream" cap of §8, and
  the trickle test returns in 50 ms; `max < 1` is treated as 1 so the loop cannot spin; (2) `Push`/`Replace` ignore
  a nil `View` (no panic, no stack corruption), `Replace` may replace the root; `Update` ignores a nil view returned
  by a top view instead of storing it; (3) `OpenDialogMsg` and `Command` are **not** defined yet — the task's
  message list omits `OpenDialogMsg` (it needs `Dialog`, task 015) and the task's `view.go` list omits `Command`
  (task 014 owns the command registry); both are added by those tasks; (4) `Flash` does not run `Sprintf` when
  there are no args, so a format containing `%` is shown literally. `make check` green; tui tests 1.35 s under
  `-race`.
- 2026-11-12 — Task 012: `internal/tui/views` table. Choices: (1) the done banner counts `rows + errors`
  ("1,512 hosts · 2 errors") because the task's example counts the units attempted, not just rendered rows;
  (2) error rows put the host in the name column and the error kind at the start of the first non-name column,
  spilling into the blank remainder — the host table's LOAD column is 6 cells, so `render.Row` would cut
  `unreachable` to `unrea…`; (3) `s` advances to the next column ascending and `S` toggles direction (neither has
  a `keymap.Map` binding), and both keep the cursor on the same `Ref` via `cursorOn`; (4) filtering and sorting
  happen in `refilter` on data/filter changes only — `Render` slices the window first; header layout (per width),
  banner and stale count are cached, so a 100x20 frame over 1,512 rows averages **3.1 ms under `-race`**
  (0.3 ms without); (5) `~` regexes compile with `(?i)`; an invalid one sets `filterBad` and matches nothing;
  (6) `NewTable` falls back to `keymap.Default()` when `env.Keys` is nil so a pane-less table (tests, task 016
  bootstrapping) never panics. `make check` green; views tests 1.98 s under `-race`, acceptance suite 8 tests.
- 2026-11-12 — Task 013: `internal/tui/views` text view. Choices: (1) the scroll position is a **source line**
  index, so in wrap mode one key press moves a whole logical line and `G` stops on the last one (a source line can
  span several rows, so no attempt is made to scroll by row); (2) `Render` pads the body to `h-1` lines before the
  footer, because `render.Fit` would otherwise pull the footer up into the first line of a short document;
  (3) search is a case-insensitive substring (like the table filter, minus its `~` regex form) computed over the
  ANSI-stripped text, and `enter`/`n`/`N` scroll the match to the **top** of the body rather than merely making it
  visible, so a jump is deterministic and testable; (4) highlighting applies to each visible line at render time
  with cell-based `ansi.Cut` segments (match runes located on the plain text, then cut by cell position) so SGR
  sequences in the source survive; (5) `enter`/`esc`/`/`/`w`/up/down/page/top/bottom come from `env.Keys`, while
  `left`/`right`/`W`/`n`/`N` are hard-coded because `keymap.Map` has no bindings for them; (6) the footer spells
  `1 match`/`n matches` with a local helper — the shared `pluralWord` in `table.go` appends a bare `s`
  (`matchs`) and `table.go` belongs to task 012; (7) capping uses `append(lines[:0], lines[d:]...)` so the
  backing array is reused, and match indices shift by `d`. `make check` green; views tests 2.67 s under `-race`,
  acceptance suite 8 tests (the 50,000-line cap test is ~0.1 s).
- 2026-11-12 — Task 014: `internal/tui` command bar and registry. Choices: (1) `HelpRequestedMsg` and
  `KindsRequestedMsg` are defined in `commands.go`, not `msgs.go`, because the task's Files list allows only the
  four new files and `commands.go` is what emits them (016 routes them); (2) `NewCmdBar(env, cmds)` is the
  constructor root will use in 016 — the zero value and nil env/cmds work too (no completion, history still
  works), so a pane-less bar never panics; (3) `Register` also rejects a nil `Run` and an empty name/alias as
  `errs.Config`, besides the required duplicate collisions; a command repeating its own name as an alias is not a
  conflict; (4) tab completes the *last* word: the first word against command names + kind names + aliases
  (case-insensitive prefix, sorted), later words through `Command.Complete(env, args)` filtered by the partial
  word; a completed first word gains a trailing space, several candidates are listed after the input in
  `Theme.Dim` and further tabs cycle the insertion; (5) `View` reserves the candidate list's cells before setting
  `textinput.Width` (and truncates the input view) — `textinput` pads to `Width`, which otherwise pushed the
  candidates off the line; (6) `Exec` trims a leading `:` and returns nil for an empty line, and an unknown word
  with a nil/absent registry flashes `unknown command` rather than panicking. `make check` green; `internal/tui`
  tests 1.47 s under `-race`, acceptance suite passes.

- 2026-11-12 — Task 000 (rename): `sitrep` → `jumpr` across module path, binary (`cmd/jumpr`), imports, config/cache/
  crash paths (`~/.config/jumpr`, `~/.cache/jumpr`), env prefix (`JUMPR_`), Makefile, docs and all task files.
  Deliberately **not** renamed: the `sitrep-lab` / `sitrep-dev` binaries (lab track owns `cmd/sitrep-lab/` and `lab/`),
  and the repo directory, which stays `~/workspace/sitrep`. `go build ./...`, `go vet ./...`, `go test ./...` green.
  Also removed six orphaned, untracked task-015 WIP files (`internal/tui/{statusline,hints,dialog,dialog_help}.go` + two
  test files) left by an earlier aborted session: they failed their own tests and were not referenced by committed code.
  Task 015 is re-implemented from its task file.

- 2026-11-12 — Parallel wave 1 (tasks 015, 017, 018, 026, 027, 028, 037 merged on `main`, one commit per task,
  linear history, `go test ./...` green after each merge). Method: one git worktree per task under
  `~/workspace/wt/t<NNN>`, one agent per worktree, each given only its task file + the BUILD-PLAN sections it names;
  agents were told **not** to touch `docs/tasks/README.md`, `docs/HANDOFF.md`, `go.mod`, `go.sum` or the Makefile —
  the coordinator merges in numeric order after re-running `gofmt`/`go vet`/`go test` and diffing the branch against
  main for forbidden paths. Two got an extra manual read against the task text (018 prefix table, 027 `Wrap` strings).
  Notable: task 015 also added 3 lines to `internal/tui/msgs.go` (`OpenDialogMsg`), which its task file did not list —
  accepted because the dialog frame cannot be opened otherwise and `msgs.go` is otherwise untouched.
  Worktree dirs are outside the repo (`~/workspace/wt`), so `git status` stays clean.

- 2026-11-12 — Coordinator notes on the parallel run (deviations the agents reported, all accepted):
  - **016** `NewRoot` grew a fourth parameter (`defaultFactory Factory`) so the root can be tested without a real
    backend; the task text showed three. `NewRoot` is not defined in BUILD-PLAN, so no frozen contract moved. Two
    callers updated (`internal/app/app.go`, `internal/tui/root_test.go`). In M0 every non-`--demo` run returns the
    documented "no backends configured; try --demo" error.
  - **021** the acceptance command `-run Model` did not match the test names the task listed; kept the listed names as
    suffixes (`TestModel*`) so the command exercises them.
  - **029** (a) host keys: `knownhosts.Normalize` strips `:22`, but both `HostKeyCallback` and `HostKeyAlgorithms` need
    `host:port`, so the callback gets the joined form and the normalized form is used only for the line written to
    `known_hosts`. (b) The per-file decrypted-signer cache lives in the `Auth` value, not package state (AGENTS rule 13
    forbids the latter); it lasts as long as the `AuthMethods`/cleanup pair. (c) Missing/unreadable/passphrase-cancelled
    keys are skipped silently; a prompter error is returned; a wrong passphrase is asked 3 times.
  - **015** added 3 lines to `internal/tui/msgs.go` (`OpenDialogMsg`) beyond its file list — required to open a dialog.
  - `cmd/sitrep-lab/` and `lab/` still use the old `sitrep` name and are untouched by design (lab track).
  Tasks whose agents chose to keep the task's listed test names, or that reported "no other deviation", are not listed.

- 2026-11-12 — Merge-integrity notes (coordinator):
  - **Two cross-task collisions found only after merge**, both fixed on main with a `fix:` commit:
    (1) `internal/backend/salt` — tasks 068 and 071 each defined `collect` in their tests; renamed 071's to
    `collectJobs`. (2) `internal/tui` — task 022 called `filterPrefix`, a helper it never defined (its branch was
    marked failed for touching the index, so it never went through the verify gate); added it next to
    `dirCompletions`.
  - **Gate hardened:** the scheduler now rebases a branch onto main and then verifies *the rebased tree* before
    merging, because the rebased tree is exactly what main becomes. Verifying before the rebase missed both
    collisions above: each branch passed alone and failed together.
  - Tasks 022, 031, 080 had edited `docs/tasks/README.md` against instructions; the index hunk was dropped from each
    commit (`git checkout main -- docs/tasks/README.md`), then rebased and merged. Their task work was unaffected.

- 2026-11-12 — Task 142: glyph wiring. `internal/app.resolveGlyphs` is the only place that reads `JUMPR_GLYPHS`
  (rule 7); it beats `ui.glyphs`, and `auto` → unicode. The set goes `Env.Glyphs` → root → table/status line/format,
  via `render.FitWith` and `GlyphSet.Empty`/`AgeWith`. Verified end to end, not just by unit test: `JUMPR_GLYPHS=ascii
  ./jumpr --demo` in a 100x10 PTY renders a frame with **zero** runes above 0x7f (`+fake`, `|` separators, ASCII
  sparklines), asserted by `TestAsciiFrame` at 120x34. Two rebase conflicts were resolved by hand (root.go against the
  mux work from 022; a `NewRoot` call-site arity change in a test).


## Machine handoff (2026-11-12): build moved to the M4 Mac mini

The build now runs on the M4 Mac mini (`ssh mini`), not the laptop. Layout on that machine:

- repo: `~/workspace/sitrep` (same `main`, branch per task under `~/workspace/wt/t<NNN>`)
- runner: `~/jumpr-run/` — `scheduler.py`, `launch.sh`, `supervise.sh`, `status.sh`, `stop.sh`, `state.json`,
  per-task agent logs in `~/jumpr-run/logs/<NNN>.log`, scheduler output in `logs/supervisor.out`
- tmux session `jumpr` runs `supervise.sh` (restarts the scheduler if it dies; state.json makes restarts safe)
- one Hermes agent per task, its own worktree; the scheduler verifies the **rebased** tree (gofmt/vet/test +
  forbidden-path check) before an `--ff-only` merge, marks the task done in the index and amends the commit

Provider note: the first launch died on `deepseek-flash` (billing exhausted). The minis
## Machine handoff (2026-11-12): build moved to the M4 Mac mini

The build now runs on the M4 Mac mini (`ssh mini`), not the laptop. Layout on that machine:

- repo: `~/workspace/sitrep` (same `main`, branch per task under `~/workspace/wt/t<NNN>`)
- runner: `~/jumpr-run/` — `scheduler.py`, `launch.sh`, `supervise.sh`, `status.sh`, `stop.sh`, `state.json`;
  per-task agent logs in `~/jumpr-run/logs/<NNN>.log`, scheduler output in `logs/supervisor.out`
- tmux session `jumpr` runs `supervise.sh` (restarts the scheduler if it dies; `state.json` makes restarts safe)
- one Hermes agent per task, each in its own worktree; the scheduler verifies the **rebased** tree
  (gofmt/vet/test plus forbidden-path check) before an `--ff-only` merge, marks the task done in the index and
  amends that task's commit

Provider note: the first launch on the mini died on `deepseek-flash` (DeepSeek billing exhausted). The mini's
`~/.hermes/config.yaml` now uses the Nous provider (`deepseek/deepseek-v4.1-flash-1017`, same account as the
laptop), verified with a one-shot query before the run was restarted.

Capacity: 10 cores, ~11 GB free disk, and two long-lived CPU hogs (a 4-day `ffmpeg`, OrbStack) share the box, so the
run is capped at `MAX_CONC=8`.

## FINAL (2026-11-13): every task merged and verified

**132 / 132 tasks done.** The overnight scheduler reached `DONE: everything <= 127 is merged` at
09:25 and 144 merged before it. `main` is at `b023f64`.

Verification that actually ran (Mac mini, final tree):

| Check | Result |
|---|---|
| `make check` (gofmt + vet + `go test ./...` + build) | **exit 0, 43 packages ok** |
| `go test -race -count=1 ./...` | **42 packages, exit 0, no FAIL** |
| `go build ./cmd/jumpr` | 5.7 s, **56 MB** binary (gate is 80 MB) |
| `./jumpr --demo` in a real PTY (120x34) | alt screen entered, `jumpr main 0:* targets:0`, `host (1512)`, fake health dot, keystrokes accepted, clean exit (7,878 bytes) |
| `jumpr doctor` (temp HOME) | 13 checks, exit 0 |
| `jumpr doctor` with the real `~/.ssh/config` | 6 hosts parsed, bastions reported absent, host-key prompter rendered, dead host classified `timeout` |
| **`make homelab-test` against a real Ubuntu 24.04.5 over SSH** | **all TestHomelab* PASS** — overview 9 sections / 2,325 bytes / **7 ms** (dial 28 ms, first byte 1 ms), 310 units (192 active), 50 journal entries, 24 probe commands proven read-only |

Two real targets, both green:

- `ssh jumpr-lab` — OrbStack Ubuntu 24.04.5 on the OrbStack kernel, which has **no PSI**. This is the
  run that found the missing-`CONFIG_PSI` bug below.
- `ssh macmini` — the user's own Ubuntu 24.04 box (Linux 6.8.0-149, 8.0 GB). It was down all morning
  (TCP accepted, SSH banner exchanged, publickey answers never came, later resets), and passed
  7/7 once it came back: `id -un` → `mike` in 228 ms, 516 units / 349 active, 50 journal entries,
  overview **37 ms / 2,853 bytes / 9 sections**, dial 81 ms, first byte 4 ms. That is the normal
  path; `jumpr-lab` covered the no-PSI path.

Task 144's suite skips cleanly on a silent node (`homelabSkipIfSilent`), so both runs are repeatable:
`JUMPR_HOMELAB_UBUNTU=macmini make homelab-test`.

**Bug the real host found:** the PSI probe section exited 1 on a kernel without `CONFIG_PSI`
(`/proc/pressure/*` absent), so the overview reported a *failed* section instead of "not available".
Fixed in `internal/backend/linux/probes.go` (`|| true` after the cat), commit `b023f64`.

### Code review by Opus 5.5 and Fable 5.2
`docs/plans/2026-11-13-code-review.md` holds both reviews, the three findings fixed in place, and the
five tasks (154-158) that the rest become. The worst one both reviewers found independently is still
open: re-attaching to a session can kick the *new* client (`internal/server/conn.go:125`), because
`Session.Detach` does not identify which attachment it ends — fix is a generation number, task 154.

### Process notes for the next session
- The scheduler's merge step runs `git add -A && git commit --amend`: never leave an uncommitted
  change on `main` while it runs, or it lands inside some task's commit (this happened once).
- The gate now allows `go.sum` always and `go.mod` when every added line is `// indirect`; tasks that
  really add a dependency must list `go.mod` under Files (105 and 115 do).
- Worktrees and Go caches live on the Crucial X9 through the space-free `~/x9` symlink. A space in
  `TMPDIR` breaks ssh known_hosts handling in the tests — do not point the caches at
  `/Volumes/Crucial X9` directly.
