# jumpr

**One terminal multitool for operating infrastructure: Kubernetes (the way k9s browses it), systemd
(the way isd browses it), networking, a service map and configuration checks — over plain SSH and a
kubeconfig, with nothing installed on any server.**

```
 jumpr  main  0:*  ctx:lab  targets:0                                        ●linux ●salt ●chef ●k8s
 units (2) · /ssh
 FLEET             │ UNIT            LOAD    ACTIVE  SUB      │ ssh.service
   hosts           │ ssh.service     loaded  active  running  │  1 status  2 show  3 cat  4 journal  5 deps
 SYSTEMD           │ ssh.socket      loaded  active  running  │ state      active (running)
 ▸ units           │                                          │ since      2026-11-12 18:19:18 (4d 3h)
   timers          │                                          │ main pid   5910
   journal         │                                          │ memory     11.6 MiB
 KUBERNETES        │                                          │ unit file  enabled  /usr/lib/systemd/…
   pods            │                                          │
   services        │                                          │ ── systemctl status ──
 NETWORK           │                                          │ * ssh.service - OpenBSD Secure Shell server
   sockets …       │                                          │      Loaded: loaded (…; enabled)
 SERVICE MAP       │                                          │
 CHECKS            │                                          │
 <enter> open  <j> journal  </> filter  <tab> tools  <:> cmd  <?> help
```

Three columns, always:

- **Tool rail (left).** Every tool and its views, visible. `tab` moves into it, `j`/`k` and `enter`
  open a view, `tab` or `esc` goes back. An entry nothing answers for (no cluster, no Salt) is faint.
- **List (middle).** The k9s-style table: `/` filter, `ctrl+f` fuzzy, sort, marks, `x` actions.
- **Live preview (right).** What `enter` would open for the selected row, loading as the cursor
  rests: a unit's status/journal/cat/deps, a host's overview, a pod's describe/YAML/events. `enter`
  takes it full width. `ctrl+p` hides the preview, `ctrl+n` the rail, `K` shows the knowledge panel.

Everything in the rail is also a `:` command, so nothing is only reachable by mouse-memory.

## The tools

| Tool | Views | Notes |
|---|---|---|
| **Fleet** | `:hosts`, `:targets` | Hosts from `ssh_config`, Salt, Chef, k8s nodes; load, memory, PSI, failed units. |
| **Systemd** | `:units`, `:timers`, `:journal`, `:process` | isd-style: filter the unit list, read status/journal/unit file/dependencies beside it. |
| **Kubernetes** | `:pods`, `:deployments`, `:services`, `:ingresses`, `:nodes`, `:event`, every CRD | kubectl names and short names (`:po`, `:svc`, `:deploy`); all namespaces unless the context or `:ns` says otherwise; `enter` = describe / YAML / events. |
| **Network** | `:sockets`, `:interfaces`, `:routes`, `:neigh`, `:probe firewall`, `:probe connections`, `:endpoints`, `:networkpolicies` | Plus two active checks, below. |
| **Service map** | `:map`, `:map k8s`, `:map hosts` | What talks to what; see below. |
| **Checks** | `:salt state` (dry run, grouped by identical diff), `:chef drift`, `:probe` | Configuration drift from the tools that own the configuration. |

### Network checks

```
:reach db.example.internal:5432 [--tls] [--http]
```
From every target host (or the whole inventory when no targets are set): resolve the name, ask the
kernel for the route, open one TCP connection, optionally read the TLS certificate dates and the
HTTP status. Results are grouped by identical output, so "nine hosts can, one cannot" is one screen.

```
:trace web-01 db-01 5432
```
One path, both ends, and a verdict. The far end reports what listens on the port (and under which
address), which firewall rules name it, and its real address — an inventory name is often an SSH
alias DNS has never heard of. Then the near end resolves, routes and connects. The last line says
where it breaks: name resolution, routing, nothing listening, **bound to loopback only**, or
filtered in between.

### Service map

`:map` draws one tree per entry point, and every edge says how it is known:

```
Ingress shop/shop  shop.example.internal
└─▶ Service shop/web  :80   [declared · ingress rule · shop.example.internal/]
    └─▶ Deployment shop/web  2/2 pods ready  :8080/TCP   [declared · selector · app=web]
        └─▶ Deployment shop/cart  :8080/TCP   [observed · hubble · 124 forwarded, 2 DROPPED]

Unit cart.service on web-01
└─▶ Unit postgresql@16-main.service on db-01  :5432/TCP   [observed · socket · 2 forwarded]
```

- **With Cilium:** jumpr asks each Cilium agent pod for its recent Hubble flows through the pod exec
  API (`hubble observe`), and draws workload → workload edges with forwarded and dropped counts.
- **Without Cilium:** the same map from what every cluster has — Ingress rules, Service selectors,
  and containers whose environment or arguments name another Service. These edges are marked
  *declared*: configuration that says traffic should flow, not traffic that was seen.
- **Hosts:** each host lists its sockets once. A listening socket names the systemd unit that owns
  it; an established connection to another host's listener is an edge between the two units.

Nothing is installed anywhere and nothing is written; the map is as fresh as the moment you ask.

## Words

jumpr uses each system's own word and never borrows one for another:

| Word | Means | Never means |
|---|---|---|
| **host** | a machine jumpr reaches over SSH | a Kubernetes node (though `h` pivots from a node to its host) |
| **node** | a Kubernetes node (`:nodes`); Chef's are `:chef-nodes` | an SSH host |
| **unit** | a systemd unit (`nginx.service`, `ssh.socket`) | a Kubernetes anything |
| **service** | a Kubernetes Service object (`:services`) | a systemd `.service` — that is a unit |
| **pod / container** | a pod, and a container inside it | a process on a host |
| **process** | a process on a host; in the map, only when its unit is unknown | |
| **socket** | a listening or connected socket on a host | a systemd `.socket` unit — that is a unit |
| **minion / job** | Salt's: `:minions`, `:salt-jobs` | a Kubernetes Job (`:jobs`) |

Kubernetes owns the bare kubectl names. Salt and Chef kinds are prefixed (`:salt-jobs`,
`:chef-nodes`, `:chef-roles`, `:chef-envs`) so a CRD or a core kind can never be shadowed.

## Quick start

```sh
make build && install -m 0755 jumpr ~/.local/bin/jumpr
jumpr --demo                 # the built-in fake fleet: the interface, no configuration
jumpr init                   # ~/.config/jumpr/{config,connections,policy,keymap}.yaml
$EDITOR ~/.config/jumpr/config.yaml   # ssh.config_files, kubernetes.kubeconfig, salt.master …
jumpr doctor                 # checks config, SSH, agent, bastions, backends
jumpr                        # tab → pick a tool; or :pods, :units, :map, :trace a b 443
```

`C-a` is the tmux-style prefix (panes, windows, detach with `C-a d`), `?` is help.

## State of this build (2026-11-16)

Verified: `make check` green (gofmt, vet, 44 packages of tests); the shell, Kubernetes, units,
`:map`, `:trace` and `:reach` exercised by hand against the OrbStack lab (10 hosts, Salt, Chef, k3s).

Known gaps, honestly:

- **Hubble edges are untested against a live Cilium cluster** — the lab's k3s runs flannel. The flow
  parser and the graph are unit-tested with recorded `hubble observe -o jsonpb` lines; the exec
  path to the agent pod has not met a real agent.
- The service map is a text tree, not a drawn graph, and it is a snapshot (no live refresh).
- `:reach` and `:trace` are commands with typed arguments, not forms.
- Kubernetes lists show a stray SSH error row for an unreachable inventory host.
- The docs below this section predate the rail and still describe the `:`-only interface.

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
:hosts [selector]      :units :proc :sockets :journal :alerts :minions :keys :salt-jobs
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
./jumpr --config-dir lab/jumpr         # M4 demo: :minions → :salt-jobs → :hosts salt.role=web
                                         #          → :salt state → x apply → :run --via salt 'uptime'
make lab-test                            # lab-only suite; skips when the lab is down
go run ./cmd/@@ down                     # stop the lab
```

`lab/jumpr/config.yaml` points jumpr at `lab/ssh_config` (written by the lab controller), the lab's Salt
master and the lab's k3s kubeconfig (`lab/.keys/kubeconfig`, context `@@`); the Salt source adds the grains
labels (`role`, `dc`) the demo selectors use. Run jumpr from the repository root so the relative paths
resolve. Everything else — `go test ./...`, `make check` — runs without the lab, the network or an SSH agent.
