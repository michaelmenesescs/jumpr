# jumper

Agentless, client-side infrastructure debugging TUI (Go).

- Start here: [docs/HANDOFF.md](docs/HANDOFF.md)
- Spec: [docs/SPEC.md](docs/SPEC.md)
- Build plan and contracts: [docs/BUILD-PLAN.md](docs/BUILD-PLAN.md)
- Tasks: [docs/tasks/README.md](docs/tasks/README.md)
- Test lab: [lab/README.md](lab/README.md)
- Requirements interview: [docs/INTERVIEW.md](docs/INTERVIEW.md)
- Agent rules: [AGENTS.md](AGENTS.md)

## Lab observability stack (M8, M11)

The M8 tests (`internal/backend/prom/lab_test.go`, run by `make lab-test`) use
the lab's Prometheus and Alertmanager behind a mock SSO cookie proxy; the M11
tests (`internal/backend/grafana/lab_test.go`,
`internal/backend/elastic/lab_test.go`) use the same proxy for Grafana's
dashboards and for central logs. The stack is configured in
`lab/jumper/config.yaml` (`prometheus:`, `alertmanager:`, `grafana:`,
`elasticsearch:`) and `lab/jumper/connections.yaml` (the `lab-sso` connection
profile).

```sh
go build -o lab/.keys/auth-helper ./examples/auth-helper-cookie
export JUMPER_AUTH_COOKIE=<the cookie the mock proxy handed out>
make lab-test
./jumper doctor conn lab-sso https://prom.lab.example.internal --config-dir lab/jumper
```

The proxy is a Go program owned by the lab track. The tests expect it to:

- require the `_oauth2_proxy` cookie, and answer `302` to a `Location`
  containing `login` when the cookie is missing or stale;
- answer `POST /__lab/sso/invalidate` (lab-only, no auth) with
  `{"cookie": "<renewed value>"}`, invalidating the current cookie — that is how
  the auth-helper refresh test gets a new value;
- fire an alert whose alertname (or another label/annotation) names `haproxy`
  for `web-01`/`web-02` within a few minutes of
  `go run ./cmd/sitrep-lab fault haproxy-443`;
- serve Grafana's `/api/search`, `/api/dashboards/uid/<uid>` and
  `/api/ds/query`, with at least one dashboard whose timeseries panel reaches
  the lab's Prometheus (so a panel over the last fifteen minutes has samples);
- serve the `elasticsearch:` log sources of `lab/jumper/config.yaml`
  (`logs-nginx-*`, `logs-system-*`, ECS field names) and ship the web hosts'
  nginx logs into them, so the `haproxy-443` fault produces a line for
  `web-01` inside the fault window.

### Central logs and the live tail (M11)

`:logs` searches a configured Elasticsearch source for the investigation's
hosts and window; `w` follows it. A tail polls the same search every 2 s (5 s
after three empty polls), continues with `search_after` from its last hit —
with the configured tiebreaker sort field, so lines that share a timestamp are
neither skipped nor repeated — keeps the newest 20,000 lines, stops when the
screen closes, and shows its lag (`now − the newest event it has seen`) in the
banner. From a host overview `l` opens that host's logs; from a unit detail `l`
opens them filtered on the source's `unit_field` (a source without one ignores
the filter).

When the stack, the proxy or the helper is absent, those tests skip with
`lab observability stack not running`.

## What works today (v0.5, M0–M4)

- **M0 — skeleton.** k9s-style shell over the fake 1,512-host fleet (`--demo`): `:kind selector` navigation,
  `/` filter, breadcrumbs, sort, health dots, help, command bar with tab completion.
- **M1 — multiplexer.** Panes, windows, the `C-a` prefix (keymap configurable), zoom, resize, copy mode; the
  layout survives a restart.
- **M2 — SSH + Linux core.** One connection pool through bastions; hosts, units, journal, processes, sockets,
  mounts, disks, interfaces; fleet probes grouped by identical output; an interactive SSH shell in a pane;
  `jumper doctor ssh <host>`.
- **M3 — actions + investigations.** Every write is planned, confirmed and executed through the action bus
  (policy, canary, rolling batches, typed confirmation). Markdown records in the knowledge root. `:run`,
  guarded synchronized input.
- **M4 — Salt core.** Minions and keys, grains as an inventory source, selector ↔ Salt target both ways,
  live and historical jobs grouped per minion, `state.apply test=True` grouped by identical diff, apply
  through the bus, Salt as a fan-out transport (`:run --via salt`, `:probe --via salt`), host → minion and
  minion → host pivots, `:salt ping`.

Salt is driven over SSH to the existing master (`salt`, `salt-run`, `salt-key` only); nothing is installed on
any node.

## Commands worth knowing

```
:hosts [selector]      :units :proc :sockets :journal :alerts :minions :keys :jobs
:probe [--via ssh|salt] <name>
:run [--sudo] [--via ssh|salt] [--no-rollout] <command…>
:salt state [sls]      # dry run on the session targets, grouped by identical diff
:salt -C '<expr>'      # -C -G -E -L -N -I and globs: native target → target set + :hosts
:salt target           # how the current selection compiles (Salt target + CLI line)
:salt run <fun> [args…] [k=v…]
:salt ping             # test.ping as a job; opens the job detail
```

## Running against the lab

The lab (OrbStack, false fleet, Salt master, k3s, Chef) is described in [lab/README.md](lab/README.md) and is
the only environment the `TestLab…` tests talk to.

```sh
go run ./cmd/@@ up                       # create + provision (idempotent)
go run ./cmd/@@ status
make build
./jumper --config-dir lab/jumper         # M4 demo: :minions → :jobs → :hosts salt.role=web
                                         #          → :salt state → x apply → :run --via salt 'uptime'
make lab-test                            # lab-only suite; skips when the lab is down
go run ./cmd/@@ down                     # stop the lab
```

`lab/jumper/config.yaml` points jumper at `lab/ssh_config` (written by the lab controller) and the lab's Salt
master; the Salt source adds the grains labels (`role`, `dc`) the demo selectors use. Run jumper from the
repository root so the relative paths resolve. Everything else — `go test ./...`, `make check` — runs without
the lab, the network or an SSH agent.
