# jumpr

**It feels like k9s + isd-tui + tmux + the Salt and Chef CLIs + everyday Linux tools, and it needs
nothing on the servers.** One central terminal interface you control, learn once, and operate any
infrastructure through: from an alert to the hosts behind it, to the units, processes, sockets,
logs and metrics on those hosts, to the pods on them, to the config management that put them
there — and then to the fix, with everything you did written down.

Agentless and client-side: SSH plus commands that already exist on RHEL 8+/Ubuntu 20.04+, a
kubeconfig or an HTTP API when you configure one. Nothing is installed, copied or left behind on a
node.

## Quick start

```sh
tar xzf jumpr-v1.2.3-darwin-arm64.tar.gz && install -m 0755 jumpr ~/bin/jumpr
jumpr --demo                 # the built-in fake fleet: the interface, no configuration
jumpr init                   # ~/.config/jumpr/{config,connections,policy,keymap}.yaml
$EDITOR ~/.config/jumpr/config.yaml   # ssh.config_files: [~/.ssh/config]
jumpr doctor                 # checks config, SSH, agent, bastions, backends
jumpr                        # :hosts role=web  →  enter  →  u/j/p/s  →  x  (actions)
```

`:kind selector` is the interface, `C-a` is the tmux-style prefix, `?` is help. Detach with
`C-a d`; the session keeps running and `jumpr` attaches again.

## Documentation

- [docs/USER-GUIDE.md](docs/USER-GUIDE.md) — install, `jumpr init`, SSH and Salt, the nine
  workflows, sessions and detach, the safety model, investigations and knowledge files,
  connection profiles, plugins, troubleshooting with `doctor`.
- [docs/KEYS.md](docs/KEYS.md) — every key binding, generated from the defaults; all of them are
  rebindable in `keymap.yaml`.
- [docs/SPEC.md](docs/SPEC.md) — what jumpr does and why.
- [docs/BUILD-PLAN.md](docs/BUILD-PLAN.md) — the contracts: packages, interfaces, messages,
  protocol, errors, dependencies.
- [docs/HANDOFF.md](docs/HANDOFF.md) — current state of the build.
- [docs/tasks/README.md](docs/tasks/README.md) — the task index and workflow.
- [lab/README.md](lab/README.md) — the local test lab.
- [docs/INTERVIEW.md](docs/INTERVIEW.md) — the requirements interview.
- [AGENTS.md](AGENTS.md) — rules for agent-assisted changes.

## Lab observability stack (M8, M11)

The M8 tests (`internal/backend/prom/lab_test.go`, run by `make lab-test`) use
the lab's Prometheus and Alertmanager behind a mock SSO cookie proxy; the M11
tests (`internal/backend/grafana/lab_test.go`,
`internal/backend/elastic/lab_test.go`) use the same proxy for Grafana's
dashboards and for central logs. The stack is configured in
`lab/jumpr/config.yaml` (`prometheus:`, `alertmanager:`, `grafana:`,
`elasticsearch:`) and `lab/jumpr/connections.yaml` (the `lab-sso` connection
profile).

```sh
go build -o lab/.keys/auth-helper ./examples/auth-helper-cookie
export JUMPR_AUTH_COOKIE=<the cookie the mock proxy handed out>
make lab-test
./jumpr doctor conn lab-sso https://prom.lab.example.internal --config-dir lab/jumpr
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
- serve the `elasticsearch:` log sources of `lab/jumpr/config.yaml`
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

## Generic Kubernetes resources (M9, task 112)

Besides the typed kinds (pods, deployments, nodes, …), the `k8s` backend serves
every listable API resource the cluster declares, CRDs included. Discovery runs
once at startup and is cached for ten minutes; the kinds are named by their
plural with the short and singular names as aliases, and a resource the typed
kinds already serve is skipped. Columns come from the server's own Table
response (`Accept: application/json;as=Table;v=v1;g=meta.k8s.io`), so a CRD's
printer columns appear exactly as kubectl prints them, with numbers
right-aligned; rows are listed with the same Table request, metadata only.
`:kinds` shows each generic kind with its API group in the title. Describe and
YAML reuse the M9 object reader (`d`, `y`).

There are no local editor panes in v1, so `e` offers `k8s.apply-patch`: the
operator pastes a JSON merge patch into the action dialog's text parameter, the
plan shows the result of a server-side dry run as a unified diff, and the write
happens only through the action bus.

```sh
go test ./internal/backend/k8s/ -run Dynamic -v     # discovery, Table rows, patch plan (no cluster)
make lab-test                                        # the k3s tests: TestLabK8s…
```

## What works today (v0.5, M0–M4)

- **M0 — skeleton.** k9s-style shell over the fake 1,512-host fleet (`--demo`): `:kind selector` navigation,
  `/` filter, breadcrumbs, sort, health dots, help, command bar with tab completion.
- **M1 — multiplexer.** Panes, windows, the `C-a` prefix (keymap configurable), zoom, resize, copy mode; the
  layout survives a restart.
- **M2 — SSH + Linux core.** One connection pool through bastions; hosts, units, journal, processes, sockets,
  mounts, disks, interfaces; fleet probes grouped by identical output; an interactive SSH shell in a pane;
  `jumpr doctor ssh <host>`.
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
./jumpr --config-dir lab/jumpr         # M4 demo: :minions → :jobs → :hosts salt.role=web
                                         #          → :salt state → x apply → :run --via salt 'uptime'
make lab-test                            # lab-only suite; skips when the lab is down
go run ./cmd/@@ down                     # stop the lab
```

`lab/jumpr/config.yaml` points jumpr at `lab/ssh_config` (written by the lab controller), the lab's Salt
master and the lab's k3s kubeconfig (`lab/.keys/kubeconfig`, context `@@`); the Salt source adds the grains
labels (`role`, `dc`) the demo selectors use. Run jumpr from the repository root so the relative paths
resolve. Everything else — `go test ./...`, `make check` — runs without the lab, the network or an SSH agent.
