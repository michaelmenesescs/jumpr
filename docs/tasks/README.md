# Tasks

One file per implementation task, in order. This index is the single place where status is tracked.

- Contract and design: `../BUILD-PLAN.md` (read §1–§9 once; re-read the sections a task names under "Context").
- Product behaviour: `../SPEC.md`. Rules: `../../AGENTS.md`. Current state: `../HANDOFF.md`.

## Workflow (every task, no exceptions)

1. Read `AGENTS.md`, `docs/HANDOFF.md`, then the task file top to bottom. Read the BUILD-PLAN and SPEC sections it
   lists under **Context**.
2. Check **Depends on**: every listed task must be `done` in the table below. If not, do those first.
3. Set the task's status to `in-progress` (in the task file and in this table).
4. Implement exactly the **Requirements**, touching only the paths under **Files**. Types and signatures come
   from `docs/BUILD-PLAN.md` §4–§6; copy them, do not re-invent them.
5. Write the tests named under **Acceptance** (test names are suggestions; the behaviours are required).
6. Run, in this order, and fix until all pass:
   ```
   gofmt -l .            # must print nothing
   go vet ./...
   go test ./...         # must pass without the lab
   <the task's Acceptance commands>
   make check
   ```
   Lab commands (`make lab-test`, anything using `lab/`) only when the task lists them and the lab is up
   (`go run ./cmd/@@ status`). If the lab is down, say so in `docs/HANDOFF.md`; do not fake results.
7. Re-read **Out of scope** and **Pitfalls**; confirm you did not do the former or fall into the latter.
8. Set status to `done` (task file + this table). Update `docs/HANDOFF.md`: next task, a line in the session log,
   measured numbers if the task asked for them.
9. Commit once: `git add -A && git commit -m "task NNN: <short summary>"`. One task, one commit. Do not push.
10. Stop conditions — set status to `blocked`, describe the problem under "Blocked" in `docs/HANDOFF.md`, commit
    the docs change only (`task NNN: blocked — <reason>`), and end the session when:
    - the task needs a change to anything defined in `docs/BUILD-PLAN.md` §4, §5, §6.2, §7 or §11;
    - an acceptance command cannot pass after two honest attempts at fixing the cause;
    - a dependency outside BUILD-PLAN §10 seems necessary;
    - the task and the spec contradict each other.

Statuses: `todo` · `in-progress` · `done` · `blocked`.

## Parallel work

Tasks with the same **parallel group** (for example `M2-D`) touch disjoint files and have no dependencies on
each other: they may be implemented concurrently by separate workers once their own dependencies are `done`,
each on its own branch or worktree, merged in numeric order. Tasks with `—` are serial: they integrate several
pieces or touch shared files (`internal/app`, `internal/tui/root.go`, `go.mod`). `go.mod`/`go.sum` are changed
only by tasks 001, 098, 105 and 115.

## Index

### M0 — Skeleton

| ID | Title | Depends on | Parallel group | Status |
|---|---|---|---|---|
| [001](001-repo-bootstrap.md) | Repo bootstrap: module, Makefile, pinned dependencies | — | — | done |
| [002](002-errs.md) | Error taxonomy (`internal/errs`) | 001 | — | done |
| [003](003-config.md) | Config paths, schema, loader, validation | 002 | M0-A | done |
| [004](004-selector-timewindow.md) | Selector and time-window parsers | 002 | M0-A | done |
| [005](005-resource-types.md) | Resource model types | 004 | — | done |
| [006](006-backend-registry.md) | Backend interface and registry | 005 | — | done |
| [007](007-fake-backend.md) | Fake backend for `--demo` and tests | 006 | M0-B | done |
| [008](008-render-fit-table.md) | Rendering primitives: fit and table | 002 | M0-A | done |
| [009](009-render-spark-human.md) | Sparklines, bars, human-readable numbers | 002 | M0-A | done |
| [010](010-theme-keymap.md) | Theme and keymap | 005 | M0-B | done |
| [011](011-tui-core.md) | TUI core: View, Env, messages, NextBatch, ViewStack | 006, 010 | — | done |
| [012](012-table-view.md) | Generic table view | 007, 008, 009, 011 | M0-C | done |
| [013](013-text-view.md) | Text view (scrollable text, key/value detail) | 008, 011 | M0-C | done |
| [014](014-command-bar.md) | Command bar and command registry | 011 | M0-C | done |
| [015](015-statusline-hints-dialogs.md) | Status line, hint bar, flash, dialog frame, help | 011 | M0-C | done |
| [016](016-root-app.md) | Root model, `app.Run`, `jumper --demo` | 003, 007, 012, 013, 014, 015 | — | todo |

### M1 — Multiplexer (in-process)

| ID | Title | Depends on | Parallel group | Status |
|---|---|---|---|---|
| [017](017-mux-layout-tree.md) | Mux layout tree (pure) | 002 | M1-A | done |
| [018](018-mux-prefix-machine.md) | Prefix key state machine and mux key table | 010 | M1-A | done |
| [019](019-mux-session-state.md) | Windows and session state (pure) | 017 | — | done |
| [020](020-view-pane.md) | View pane content | 016, 019 | M1-B | todo |
| [021](021-mux-model.md) | Mux model: panes on screen, routing, ops | 018, 019 | M1-B | todo |
| [022](022-root-with-mux.md) | Root integration: panes in the app, window tabs, mux commands | 020, 021 | — | todo |
| [023](023-copy-mode-clipboard.md) | Copy mode and clipboard | 022 | — | todo |
| [024](024-session-persistence.md) | Session persistence (layout survives restart) | 023 | — | todo |
| [025](025-io-agnostic-run.md) | Prove the app runs over a plain byte stream (detach de-risk) | 024 | — | todo |

### M2 — SSH + Linux core

| ID | Title | Depends on | Parallel group | Status |
|---|---|---|---|---|
| [026](026-transport-types-fake.md) | Transport types and scripted fake executor | 005 | M2-A | done |
| [027](027-shell-quoting-classify.md) | Shell quoting, sudo wrapping, remote error classification | 002 | M2-A | done |
| [028](028-ssh-config.md) | Resolve hosts from ssh_config | 003 | M2-A | done |
| [029](029-ssh-auth-hostkeys.md) | SSH authentication and host key verification | 028 | M2-B | done |
| [030](030-ssh-test-server.md) | In-process SSH test server | 026 | M2-B | todo |
| [031](031-ssh-dial.md) | SSH dialer: direct and through a ProxyJump chain | 029, 030 | — | todo |
| [032](032-ssh-pool.md) | SSH connection pool with bastion multiplexing and limits | 031 | — | todo |
| [033](033-ssh-exec-stream.md) | SSH Executor and Streamer | 027, 032 | — | todo |
| [034](034-ssh-shell.md) | SSH ShellOpener (interactive PTY sessions) | 033 | — | todo |
| [035](035-doctor-ssh-lab.md) | `jumper doctor ssh` and the first lab test | 033 | — | todo |
| [036](036-fanout-runner.md) | Fan-out runner | 026 | M2-B | done |
| [037](037-group-diff.md) | Grouping identical outputs, summary headline, unified diff | 005 | M2-A | done |
| [038](038-probe-batch.md) | Probe batch compiler and section splitter | 026, 027 | M2-B | done |
| [039](039-inventory-core.md) | Inventory: hosts, sources, merge, identity, cache | 028 | M2-D | done |
| [040](040-linux-parse-overview.md) | Linux parsers: host overview | 005 | M2-D | done |
| [041](041-linux-parse-units.md) | Linux parsers: systemd units | 005 | M2-D | done |
| [042](042-linux-parse-journal.md) | Linux parser: journal (JSON lines) | 005 | M2-D | done |
| [043](043-linux-parse-ps-ss.md) | Linux parsers: processes and sockets | 005 | M2-D | done |
| [044](044-linux-backend.md) | Linux backend: host, unit, process, socket kinds | 036, 038, 039, 040, 041, 043 | — | todo |
| [045](045-journal-kind-view.md) | Journal: windowed query, follow, view | 042, 044, 013 | M2-E | todo |
| [046](046-targets.md) | Target sets: current set, saved sets, selection | 039, 022 | M2-E | todo |
| [047](047-host-overview-view.md) | Host overview view | 044, 045, 013 | — | todo |
| [048](048-probe-fanout-view.md) | Read-only probes and the grouped fan-out view | 036, 037, 046 | — | todo |
| [049](049-vt-emulator-adapter.md) | Terminal emulator adapter (spike with a go/no-go list) | 008 | M2-D | todo |
| [050](050-terminal-pane.md) | Terminal pane and `:ssh` | 034, 049, 022, 046 | — | todo |
| [051](051-prompt-dialogs.md) | Interactive prompts in the TUI (host keys, passphrases) | 029, 022 | M2-F | todo |
| [052](052-wire-real-backends.md) | Wire SSH, inventory and the Linux backend into the app (M2 exit) | 035, 044, 045, 047, 048, 050, 051 | — | todo |
| [053](053-dev-capture-tool.md) | `@@ capture`: golden files from the lab (Go tool) | 035 | M2-F | todo |

### M3 — Actions + investigations

| ID | Title | Depends on | Parallel group | Status |
|---|---|---|---|---|
| [054](054-redact.md) | Secret redaction | 002 | M3-A | todo |
| [055](055-action-types-confirm-rollout.md) | Action types, confirm level, rollout plan (pure) | 005 | M3-A | done |
| [056](056-policy.md) | Policy: deny rules, protected targets, limits, read-only | 055 | M3-B | todo |
| [057](057-bus-engine.md) | Action bus engine: propose, execute, canary, rolling, record | 054, 056 | — | todo |
| [058](058-investigation-files.md) | Investigation and audit Markdown files | 054 | M3-B | todo |
| [059](059-investigation-log-ui.md) | Investigation lifecycle, recorder, commands, timeline | 058, 046, 055 | — | todo |
| [060](060-knowledge-index-panel.md) | Knowledge index and side panel | 059 | M3-C | todo |
| [061](061-action-dialogs.md) | Action UI: menu, plan, confirmation, denial, rollout progress | 057, 059 | M3-C | todo |
| [062](062-linux-actions.md) | Linux planner: systemctl verbs and kill | 055, 044 | M3-D | todo |
| [063](063-run-command.md) | `:run`: ad-hoc command through the bus | 057, 048 | M3-D | todo |
| [064](064-synchronized-input.md) | Synchronized input (broadcast line, raw sync) and shell guard rails | 061, 050 | — | todo |
| [065](065-m3-wiring-lab.md) | M3 wiring, read-only flag, lab scenario test | 060, 061, 062, 063, 064 | — | todo |

### M4 — Salt core (first fully usable release, v0.5)

| ID | Title | Depends on | Parallel group | Status |
|---|---|---|---|---|
| [066](066-salt-runner.md) | Salt runner: run Salt CLIs on the master and decode their output | 026, 027 | — | todo |
| [067](067-salt-capture-goldens.md) | Capture Salt golden files from the lab (Go tool) | 053, 066 | — | todo |
| [068](068-salt-minions-keys.md) | Salt backend skeleton: minions, keys, health | 066, 039 | M4-B | todo |
| [069](069-salt-grains-inventory.md) | Grains as an inventory source | 066, 039 | M4-B | todo |
| [070](070-salt-targeting-compiler.md) | Targeting compiler: jumper selectors ↔ Salt targets (pure) | 004, 066 | M4-A | todo |
| [071](071-salt-job-engine.md) | Salt job engine: async submit, poll, per-minion results | 066 | M4-B | todo |
| [072](072-salt-jobs-browser.md) | Jobs browser: live and historical jobs, grouped job detail | 068, 071, 048 | M4-C | todo |
| [073](073-salt-executor.md) | Salt as an execution transport | 071, 036 | M4-C | todo |
| [074](074-salt-state-returns.md) | State returns: parse, normalize, group by identical diff (pure) | 037, 066 | M4-A | todo |
| [075](075-salt-state-view-actions.md) | `:salt state` dry-run view and Salt actions through the bus | 057, 061, 070, 071, 072, 074 | — | todo |
| [076](076-salt-wiring-lab.md) | Salt wiring, `:salt` commands, `--via salt`, pivots, lab tests (M4 exit) | 068, 069, 072, 073, 075 | — | todo |

### M5 — Session server (detach/reattach)

| ID | Title | Depends on | Parallel group | Status |
|---|---|---|---|---|
| [077](077-server-protocol.md) | Session-server wire protocol codec (pure) | 002 | M5-A | todo |
| [078](078-core-session-split.md) | Split `app` into Core and Session; switchable I/O; in-process attach/detach | 076 | M5-A | todo |
| [079](079-server.md) | Session server: socket, lock, session registry, control commands | 077, 078 | M5-B | todo |
| [080](080-client-attach.md) | Client: raw terminal, byte pumps, resize, clean restore | 077 | M5-B | todo |
| [081](081-cli-daemon.md) | CLI subcommands and on-demand server start | 079, 080 | — | todo |
| [082](082-detach-ux.md) | Detach UX: sessions in the TUI, prompts, environment, restore after restart | 081 | — | todo |
| [083](083-detach-e2e-tmux.md) | End-to-end detach test through a real PTY; behaviour inside tmux | 082 | — | todo |

### M6 — systemd + Linux depth

| ID | Title | Depends on | Parallel group | Status |
|---|---|---|---|---|
| [084](084-unit-detail-tabs.md) | Unit detail view with tabs: status, show, cat, journal | 045, 062 | M6-A | todo |
| [085](085-unit-deps-timers.md) | Dependencies tab and timers list | 084 | — | todo |
| [086](086-unit-file-diff.md) | Unit-file diff across hosts and the unit fleet view | 048, 085 | — | todo |
| [087](087-dropin-preview.md) | Drop-in override preview (no remote write) | 086 | — | todo |
| [088](088-disks-mounts.md) | Disks and mounts | 044 | M6-A | todo |
| [089](089-network-kinds.md) | Network: interfaces, addresses, routes, neighbours | 088 | — | todo |
| [090](090-cgroup-kernel-file-pkg.md) | cgroups, kernel log, file viewer, packages | 089, 013 | — | todo |
| [091](091-proc-drill-in.md) | Process drill-in from `/proc` | 044, 084 | — | todo |

### M7 — Salt depth

| ID | Title | Depends on | Parallel group | Status |
|---|---|---|---|---|
| [092](092-salt-state-tree.md) | Salt state tree browser | 076 | — | todo |
| [093](093-salt-pillar.md) | Pillar view, redacted by default | 092 | — | todo |
| [094](094-salt-mine-grains.md) | Mine and grains explorer | 093 | — | todo |
| [095](095-salt-key-management.md) | Salt key management actions | 076 | M7-B | todo |
| [096](096-salt-events.md) | Salt event stream | 094, 045 | — | todo |
| [097](097-salt-misc-salt-ssh-call.md) | Nodegroups, beacons, schedules, salt-ssh awareness, `salt-call` local mode (M7 exit) | 092, 093, 094, 096 | M7-B | todo |

### M8 — HTTP + Prometheus/Alertmanager

| ID | Title | Depends on | Parallel group | Status |
|---|---|---|---|---|
| [098](098-http-connection-profiles.md) | HTTP connection profiles: proxy, TLS, static auth | 003, 054 | — | todo |
| [099](099-auth-helper-secrets.md) | Auth helper protocol, secrets, example helper (Go) | 098 | M8-A | todo |
| [100](100-doctor-conn.md) | `jumper doctor conn` | 098 | M8-A | todo |
| [101](101-prometheus-client-views.md) | Prometheus backend: targets, rules, PromQL, sparklines, chart | 099 | — | todo |
| [102](102-alertmanager.md) | Alertmanager: alerts view and silences | 101, 057 | — | todo |
| [103](103-alert-to-investigation.md) | Alert → investigation → hosts (SPEC W1 complete) | 101, 102, 059 | — | todo |
| [104](104-m8-lab.md) | M8 lab tests and demo | 100, 103 | — | todo |

### M9 — Kubernetes

| ID | Title | Depends on | Parallel group | Status |
|---|---|---|---|---|
| [105](105-k8s-bootstrap.md) | Kubernetes bootstrap: client-go, contexts, health, size check | 065 | — | todo |
| [106](106-k8s-core-kinds.md) | Kubernetes core kinds with informers | 105 | — | todo |
| [107](107-k8s-logs.md) | Pod logs | 106 | M9-A | todo |
| [108](108-k8s-describe-yaml.md) | Describe and YAML views | 106 | M9-A | todo |
| [109](109-k8s-actions.md) | Kubernetes actions through the bus | 106, 061 | M9-B | todo |
| [110](110-k8s-exec-portforward.md) | Pod exec in a terminal pane; port-forward | 106, 050 | M9-B | todo |
| [111](111-k8s-node-host-pivot.md) | Node ↔ host pivot and the k8s inventory source | 106 | M9-A | todo |
| [112](112-k8s-crds-lab.md) | Generic resources (CRDs), edit, and M9 lab tests | 107, 108, 109, 110, 111 | — | todo |

### M10 — Chef + plugins

| ID | Title | Depends on | Parallel group | Status |
|---|---|---|---|---|
| [113](113-chef-reads.md) | Chef backend: knife runner, nodes, status, inventory | 065 | M10-A | todo |
| [114](114-chef-drift-actions.md) | Chef drift signals and `chef-client` through the bus | 113, 061 | — | todo |
| [115](115-plugin-loader.md) | YAML plugin schema, loader, validation | 065 | M10-A | todo |
| [116](116-plugin-parsers.md) | Plugin output parsers | 115 | — | todo |
| [117](117-plugin-integration.md) | Plugins as kinds, links and actions; save `:run` as a plugin | 116, 063 | M10-B | todo |
| [118](118-plugin-http.md) | HTTP plugins and plugin inventory sources | 116, 099 | M10-B | todo |

### M11 — Grafana + Elasticsearch

| ID | Title | Depends on | Parallel group | Status |
|---|---|---|---|---|
| [119](119-grafana-browse.md) | Grafana: search and dashboards | 103 | — | todo |
| [120](120-grafana-panels.md) | Render Grafana panels in the TUI via `/api/ds/query` | 119, 101 | — | todo |
| [121](121-elastic-search.md) | Elasticsearch: log sources and search | 119 | — | todo |
| [122](122-elastic-tail-lab.md) | Elasticsearch live tail; M11 lab tests | 120, 121 | — | todo |

### M12 — Hardening

| ID | Title | Depends on | Parallel group | Status |
|---|---|---|---|---|
| [123](123-scale-test.md) | Scale test: 2,000 hosts | 076 | M12-A | todo |
| [124](124-init-doctor.md) | `jumper init` and full `jumper doctor` | 076 | M12-A | todo |
| [125](125-system-ssh-fallback.md) | system-ssh fallback transport | 052 | M12-A | todo |
| [126](126-crash-safety-audit.md) | Crash safety, logging and leak audit | 083, 123 | — | todo |
| [127](127-release-docs.md) | Keymap file, release build (Go tool), user documentation | 124, 126 | — | todo |

127 tasks; 69 of them sit in 27 parallel groups (the rest are serial).
