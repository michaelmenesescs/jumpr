# jumpr — Specification v0.2

> Working name. An agentless, client-side **shell for SREs**: k9s + isd + tmux + Salt/Chef and Linux tooling in one
> terminal, with nothing required on the servers.
> Status: DRAFT for build · 2026-11-12 · Owner: Michael
> How it gets built: `docs/BUILD-PLAN.md` (contracts, milestones) and `docs/tasks/` (one file per task).

## Changelog

**v0.2 (2026-11-12)** — after the mockup review (see `INTERVIEW.md`, "Follow-up"):

- New framing: a shell tailored for an SRE that plugs into whatever (§1, §2).
- New **multiplexer and session model**, tmux-style: sessions → windows → panes, prefix key, terminal panes,
  guarded synchronized input, copy mode, detach/reattach through a client-side session server (§21). The v0.1
  non-goal "no long-running daemons" now means "nothing long-running **on servers**" (§3).
- **Salt** is much deeper and earlier: jobs, targeting compiler, state browser, pillar, mine, keys, events, Salt as
  an execution transport, salt-ssh awareness (§8.6).
- **systemd** parity with isd is spelled out (§8.1.1). `list-units` is parsed from plain text so one parser works
  on every supported distro.
- Kubernetes keeps k9s parity; exec opens in a terminal pane instead of taking over the TTY (§8.2).
- Testing is built around a **local lab** (§22). All helper tooling is Go (§18).
- Backend interface, package layout and dependency list moved to `BUILD-PLAN.md` (single source of truth).
- Phase plan revised: multiplexer second, Salt fourth (§19).
- Sections §1–§20 keep their v0.1 numbers; §21 and §22 are new.

**v0.1 (2026-11-12)** — first draft from the interview.

---

## 1. Purpose

**The selling point: it feels like k9s + isd-tui + tmux + the Salt and Chef CLIs + everyday Linux tools, and it
needs nothing on the servers.** One central interface that you control, learn once, and operate any
infrastructure through.

One terminal tool you can rely on to debug infrastructure end to end: from an alert, to the hosts behind it, to the
units, processes, sockets, logs and metrics on those hosts, to the pods on them, to the config management that put
them there, and then to fix it. Each step keeps the full context, and everything you did is written down.

It replaces juggling k9s, isd, tmux layouts full of SSH sessions, Grafana tabs, Kibana tabs, `salt`/`knife` shells
and ad-hoc `ssh` loops. It also remembers what you learned for next time.

It is a **shell for SREs**, not a dashboard: the command bar (`:kind selector`, `:run`, `:salt …`) is the primary
interface, panes hold whatever you need side by side (a table, a journal, a live shell on a node), and anything
you can reach with SSH, a kubeconfig or an HTTP API can be plugged in as a backend or a YAML plugin.

### Problems it solves (from the interview)

1. **Alerts are chased through many apps with no shared context.** Fix: an **Investigation** carries the time
   window and the targets across every view.
2. **AI or scripts run commands with no visibility.** Fix: every action, human or not, goes through one **action
   bus** that is visible in the TUI and logged. AI is not in v1, but the bus is built for it (§17).
3. **Nothing builds up long-term, personal learning.** Fix: plain-Markdown **investigation logs and knowledge notes**
   that resurface automatically on related targets.

## 2. Design principles

1. **Zero footprint on nodes.** Nothing is installed, copied, or left behind. Only SSH plus binaries already present
   on RHEL 8+ / Ubuntu 20.04+. No temp files are written on the remote side.
2. **Client-side everything.** All parsing, joining, diffing and rendering happen on the laptop.
3. **Reliable before clever.** Every remote call has a timeout. The UI never blocks. Partial results are shown,
   labelled, and still useful. When something fails, it says exactly what and where.
4. **Tool-agnostic core.** Backends plug into a common resource model. Adding a backend never requires touching
   the UI.
5. **Generic transport, enterprise-specific config.** The code knows nothing about any particular company. Auth and
   proxies are configured or delegated to user-provided helper commands.
6. **Plain text is the record.** Investigations, knowledge and audit are Markdown files you own.
7. **Powerful writes, guarded blast radius.** Full read/write like k9s, with built-in safeguards proportional to the
   target count.
8. **Works like tmux.** Sessions, windows, split panes and a prefix key. A session survives closing the terminal.
   The only background process is on the laptop (§21).
9. **Plugs into whatever.** Backends and plugins are first-class: a new integration adds kinds, pivots and
   actions without touching the UI, the multiplexer or the safety layer.
10. **Borrow ergonomics people already know.** k9s for navigation, isd for systemd, tmux for panes and keys, the
    Salt and knife CLIs for vocabulary. Do not invent a new habit where a good one exists.

## 3. Non-goals (v1)

- Windows client support. Non-systemd or legacy (EL7 and older) nodes.
- Being a monitoring system. It reads Prometheus, Elasticsearch and Grafana. It does not store time series.
- Anything long-running or installed **on servers**, a central server component, or a web UI. (A per-user session
  server on the laptop is in scope since v0.2; see §21.)
- Being a general-purpose terminal multiplexer. Panes exist to debug infrastructure; local shell panes are a
  convenience, not a tmux replacement.
- Multiple clients mirroring one session, and remote (network) attach.
- Built-in LLM chat (deferred; see §17).
- Replacing Salt or Chef. It drives their existing CLIs; it does not reimplement state logic.

## 4. Constraints

| Constraint | Consequence |
|---|---|
| Client is the macOS laptop only (arm64, amd64 nice-to-have) | Single static Go binary, `CGO_ENABLED=0` |
| No downloads on nodes | Probes are shell one-liners over SSH, using `-j`/JSON output where the distro supports it |
| Access via bastions (ProxyJump) with keys/agent | Native SSH that honours `~/.ssh/config`; bastion connection multiplexing |
| Passwordless sudo usually available | `sudo -n` for privileged reads and writes; graceful degradation if it is denied |
| 100s–low 1000s hosts per session | Bounded concurrency, per-bastion limits, streaming results, grouped views |
| HTTP backends behind SSO / custom proxy | Pluggable **Connection Profiles** + **auth helper** exec protocol (§7.2) |
| Kubernetes via local kubeconfig | Use `client-go` with standard kubeconfig loading rules, including exec credential plugins |
| Salt/Chef reachable via SSH to master/workstation | Backends run `salt … --out=json` / `knife … -F json` over SSH |
| Work use; neutral | No employer names, hosts, or conventions in the repo. Examples use `example.internal` |
| Sessions must survive closing the terminal | A per-user session server on the laptop over a unix socket (§21). Client side only |
| One toolchain | Every helper and dev tool in the repo is written in Go (lab controller, fault injection, mock SSO proxy, example auth helpers, capture/sanitize, release). The Makefile only calls `go` |

## 5. Core workflows (acceptance scenarios)

**W1 — Alert to root cause (primary).**
`:alerts`, select a firing alert, then `Enter`. jumpr opens an Investigation with window = [alert start − 30m, now]
and targets = hosts resolved from the alert labels. The host view shows units with failed/restarting state, the
journal for the window, top processes, sockets, disk/mem pressure, and a Prometheus sparkline of the alert's
expression. Pivot to `salt` drift for those hosts and the pods on those nodes. Restart the unit with a confirmation.
Every step is appended to `investigations/<date>-<slug>.md`, and related knowledge notes are shown in a side panel.

**W2 — Fleet sweep.** `:hosts role=web` (≈1,500 hosts), then `:probe failed-units`. Results are grouped by
identical output: "1,481 identical · 19 differ". Open the 19 and compare them.

**W3 — Kubernetes debugging.** A k9s-equivalent for pods, deployments, nodes, events and logs. A pod's node can be
opened as a host: `Enter` on the node gives the SSH host view of the same machine.

**W4 — Config drift.** `:salt state` against the target set: `state.apply test=True` (dry run) grouped by
identical diff ("22 clean · 2 would change"), or `knife node show` / last-run status per host. Apply from the same
view through the action bus.

**W5 — Ad-hoc command.** `:run 'ss -s'` on the current target set. Output is grouped, logged, and can be saved as a
YAML plugin in one keystroke.

**W6 — "Have I seen this before?"** Opening any host, service, or alert shows matching knowledge notes and past
investigations, ranked by relevance.

**W7 — Work like tmux.** Split the window: hosts table on the left, a followed journal top right, an interactive
shell on the bad node bottom right. Open a second window for the Salt job you started. Zoom a pane, copy a line
out of the journal, detach, close the laptop lid, reattach later: the follow and the job are still there.

**W8 — "What did Salt just do?"** `:jobs` shows running and recent jobs. Open one: returns per minion grouped as
"41 identical · 2 differ · 1 no return". Pivot from a differing minion to its host view and journal for the same
time window.

**W9 — Same thing on many shells, safely.** With four shell panes open on four web hosts, compose one line and
broadcast it: the line goes through the action bus (plan, confirm, canary pane first), and is recorded.

## 6. Architecture

The diagram shows the logical layers. All of it runs in one process on the laptop: standalone, or inside the
per-user session server with a thin client attached (§21). Package layout, Go interfaces and the dependency list
are in `BUILD-PLAN.md` §3, §4 and §10.

```
┌──────────────── Multiplexer: sessions · windows · panes · prefix key (§21) ─────┐
│  each pane = a view stack (below)  or  a terminal (SSH shell / pod exec / local) │
└───────────────▲─────────────────────────────────────────────────────────────────┘
┌───────────────┴─────────────── TUI (Bubble Tea) ───────────────────────────────┐
│  views · command bar · filters · confirm dialogs · investigation side panel     │
└───────────────▲─────────────────────────────────────────────────▲──────────────┘
                │ messages (async, never blocks)                  │
┌───────────────┴──────────────┐                    ┌─────────────┴──────────────┐
│        Action Bus             │◀── future: MCP ───│     Investigation / KB      │
│ queue · policy · audit · undo │                    │ markdown log · notes · search│
└───────────────▲──────────────┘                    └────────────────────────────┘
                │
┌───────────────┴────────────────── Resource Layer ──────────────────────────────┐
│ Kinds · Resources · Links (pivots) · Selectors · TimeWindow · Fan-out grouping  │
└──▲────────▲────────▲─────────▲─────────▲──────────▲──────────▲─────────▲───────┘
   │linux   │k8s     │prom/am  │grafana  │elastic   │salt      │chef     │yaml plugins
┌──┴────────┴────────┴─────────┴─────────┴──────────┴──────────┴─────────┴───────┐
│                               Transports                                       │
│  SSH pool (bastion mux, sudo, batch exec, PTY shells) · Salt (as executor) ·    │
│  HTTP (Connection Profiles, auth)                                               │
└────────────────────────────────────────────────────────────────────────────────┘
```

### Package layout and dependencies

Moved to `BUILD-PLAN.md` §3 and §10 so there is one source of truth. Summary of the dependency policy: Bubble Tea
v1 line, `golang.org/x/crypto/ssh`, `kevinburke/ssh_config`, `skeema/knownhosts`, `charmbracelet/x/vt` (terminal
panes), `k8s.io/client-go` (largest; accepted), `itchyny/gojq`, `gopkg.in/yaml.v3`. Prometheus, Grafana,
Elasticsearch and Alertmanager use plain `net/http` with small typed clients, not vendor SDKs. Nothing is added
outside the list without asking.

## 7. Transports

### 7.1 SSH

- Reads `~/.ssh/config`: `Host`, `HostName`, `User`, `Port`, `IdentityFile`, `ProxyJump` (chains), `IdentitiesOnly`,
  `UserKnownHostsFile`, `StrictHostKeyChecking`, `ConnectTimeout`, `ServerAliveInterval`.
- Auth: ssh-agent (`SSH_AUTH_SOCK`), then key files (passphrase prompt inside the TUI), then keyboard-interactive.
  SSH certificates are supported through the agent.
- **Bastion multiplexing:** one SSH client per bastion. Target hosts are dialled as `direct-tcpip` channels through
  it. Settings: `max_conns_per_bastion` (default 4 parallel bastion clients) and `max_channels_per_conn` (default
  8). sshd's `MaxSessions` (default 10) counts session channels, so it mainly bounds concurrent commands on one
  **target** connection (jumpr uses at most 4); forwarded channels through a bastion are bounded by jumpr's own
  limit to keep one TCP connection from becoming the bottleneck. `MaxStartups` on targets and bastions is
  respected by ramping connects through the per-bastion semaphore. Both limits are tuned against the lab bastion.
- **Interactive shells:** a terminal pane opens a session channel with a PTY (`xterm-256color`) on the pooled
  connection to the host; resize follows the pane. Nothing is started on the node except the user's login shell.
- **Global concurrency:** default 64 in-flight hosts, configurable. Each host has its own connect, exec and total
  timeouts.
- **Connection reuse:** host connections stay idle-alive for 5 minutes (configurable). Keepalives are sent.
- **Batch exec:** a probe set is compiled into **one** `sh -c` script that prints delimited sections:
  `printf '\n@@SITREP:<id>:<rc>@@\n'`. One round trip per host per refresh. Output size is capped per section
  (default 2 MiB) with a truncation marker.
- **sudo:** `sudo -n` only (never prompts). If it fails with "a password is required", the section is marked
  `needs-privilege` and the rest still render.
- **Fallback transport:** `transport: system-ssh` per host pattern. It shells out to `/usr/bin/ssh` using
  `ControlMaster=auto` for configs the native client cannot handle (e.g. GSSAPI).
- **Host keys:** honour `known_hosts`. If a key is unknown, show the fingerprint and ask to accept (once or
  permanently). A key mismatch is a hard fail with no override inside the TUI.
- **Guarantee:** remote commands never write files. Writes happen only through explicit Actions (§11).

### 7.2 HTTP — Connection Profiles (generic proxy/SSO layer)

Every HTTP backend instance references a named **connection profile**. Profiles are designed for you to customize
for your environment without code changes.

```yaml
connections:
  corp-sso:
    proxy:
      # one of: none | env | http | socks5 | ssh-socks
      type: ssh-socks          # built-in dynamic SOCKS over an SSH host (like ssh -D)
      via: bastion1.example.internal
      # type: http
      # url: http://proxy.example.internal:3128
      # headers: { Proxy-Authorization: "{{ secret \"proxy-auth\" }}" }
    tls:
      ca_file: ~/.config/jumpr/ca/corp-root.pem
      client_cert: ""          # optional mTLS
      client_key: ""
      insecure_skip_verify: false
      server_name: ""          # SNI override
    auth:
      # one of: none | basic | bearer | headers | cookie | helper
      type: helper
      helper:
        command: ["~/.config/jumpr/helpers/sso-login.sh", "{{ .URL }}"]
        cache_ttl: 50m
        refresh_on: [401, 403, 302-to-login]
    headers:                   # static extra headers, templated
      X-Requested-By: jumpr
    timeout: 15s
    retries: 2                 # idempotent GETs only
```

**Auth helper protocol.** This is the enterprise extension point. jumpr runs the helper and expects JSON on
stdout:

```json
{ "headers": {"Authorization": "Bearer …"},
  "cookies": [{"name": "_oauth2_proxy", "value": "…", "domain": ".example.internal"}],
  "expires_at": "2026-11-12T18:00:00Z" }
```

- The helper can do anything: read a browser cookie DB, run `curl --negotiate`, call an internal CLI, or open a
  browser and wait for a login. jumpr pauses the request and shows "waiting for auth helper…".
- Credentials are kept **in memory only**. They are never written to logs or investigations, and they are redacted
  from errors.
- Auth failures are detected (401/403, or a redirect to a configured login URL pattern). jumpr re-runs the helper
  once, then surfaces a clear error naming the profile.
- `jumpr doctor conn <profile> <url>` shows the resolved proxy, TLS chain, helper output (redacted), and response.
  This is the main tool for customizing your environment.

**Secrets.** Values like `{{ secret "name" }}` resolve from: an env var, the macOS Keychain (`security
find-generic-password`), or a command. Plaintext secrets in the config produce a warning.

## 8. Backends

All backends implement the `Backend` interface (streaming `List`, `Get`, `Links` for pivots, `Health`), optionally
`Watcher` for live updates, and `action.Planner` if they can write. The exact Go signatures are the contract in
`BUILD-PLAN.md` §4.4–§4.5. In short: kinds and columns are declared as data, lists stream partial results per
host, pivots are queries, and writes are never executed by a backend directly; a backend only **plans** steps that
the action bus runs.

A backend reaches the world through a **transport**: SSH exec, an SSH PTY shell, Salt (as an executor), or an HTTP
connection profile. Backends are compiled in; YAML plugins (§12) cover everything else.

### 8.1 Linux (SSH) — the deepest backend

Probes run over SSH. JSON output is used only where every supported distro has it (RHEL 8+ / Ubuntu 20.04+);
otherwise one plain-text parser serves all.

| Kind | Source |
|---|---|
| `host` (overview) | `/proc/loadavg`, `/proc/meminfo`, `/proc/pressure/*` (PSI), `/proc/stat`, `uptime`, `/etc/os-release`, `uname -r`, `systemctl is-system-running` |
| `unit` | `systemctl list-units --all --plain --no-legend --no-pager` (JSON output needs systemd ≥ 246, which RHEL 8 and Ubuntu 20.04 lack), `systemctl show <u>` (props), `systemctl list-timers` |
| `journal` | `journalctl -o json --since/--until -u … -p …` (window from Investigation), with follow mode |
| `process` | `ps -eo pid,ppid,user,stat,pcpu,pmem,rss,etimes,comm,args --no-headers` + `/proc/<pid>/{status,limits,cgroup,fd}` on drill-in |
| `socket` | `ss -tanpiOH` / `ss -uanpH` (incl. process, queues, rtt) |
| `interface` / `route` | `ip -j -s link`, `ip -j addr`, `ip -j route`, `ip -j neigh` |
| `disk` / `mount` | `lsblk -J -b -o …`, `findmnt -J`, `df -PTk`, `/proc/diskstats` (rate via two samples) |
| `cgroup` | `systemd-cgls --no-pager`, `systemd-cgtop -b -n 2` |
| `kernel` | `dmesg -T --level=err,warn` (sudo), `/proc/sys` keys on demand |
| `pkg` | `rpm -qa --qf …` / `dpkg-query -W -f …` (on demand; slow) |
| `file` | `sudo cat`, `stat`, `tail -n` (read-only viewer, size-capped) |

Actions: `systemctl start|stop|restart|reload|enable|disable|mask|unmask`, `kill -SIGNAL`, `journalctl --vacuum-*`,
`systemctl daemon-reload`, and the generic `:run` (arbitrary command, always treated as a write).

#### 8.1.1 systemd — parity with isd

| isd feature | jumpr |
|---|---|
| Unit list with fuzzy search | `:units [host]` — `/` filter; quick filters by state (`failed`, `active`, `inactive`) and type (`service`, `timer`, `socket`, `mount`, …); failed units sort first |
| Status preview | Unit detail, tab **status**: `systemctl status <u> --no-pager -n 0` plus key properties (`ActiveState`, `SubState`, `Result`, `NRestarts`, `ExecMainStatus`, `MemoryCurrent`, `TasksCurrent`, `ActiveEnterTimestamp`) |
| `show` | Tab **show**: all `systemctl show` properties, filterable |
| `cat` | Tab **cat**: `systemctl cat <u>` with drop-ins marked |
| Journal preview / follow | Tab **journal**, and `j`: `journalctl -u <u> -o json` for the time window; `w` follows |
| Dependencies | Tab **deps**: `systemctl list-dependencies <u> --plain` and `--reverse` |
| Timers | `:timers [host]`: `systemctl list-timers --all` with next/last run and the unit they trigger |
| start/stop/restart/reload/enable/disable/mask | `x` actions, through the action bus |
| Edit (drop-in override) | **Preview only in v1**: edit a drop-in locally, see the diff against `systemctl cat`, and get the exact commands that would apply it. Applying needs a remote file write, which v1 does not do (§20 open question) |
| — (not in isd) | **Unit-file diff across hosts**: `:unitdiff <unit>` runs `systemctl cat` on the target set and groups identical files; `d` diffs a group against the majority |
| — (not in isd) | Fleet view: the same unit across N hosts in one table (state, since, restarts) |

### 8.2 Kubernetes

- `client-go` with the standard loading rules: `$KUBECONFIG` and `~/.kube/config`, contexts, exec auth plugins.
  The context switcher is `:ctx`.
- Kinds: pods, deployments, statefulsets, daemonsets, jobs, cronjobs, nodes, services, endpoints/endpointslices,
  ingresses, configmaps (secrets show keys only by default), events, namespaces, PVC/PV, plus generic CRDs via
  discovery.
- Informer-backed lists with live updates. Logs (follow, previous, all containers). Describe-equivalent views.
  YAML view.
- Actions: delete, scale, rollout restart, cordon/uncordon/drain, exec shell (opens a **terminal pane** attached to
  the pod through the API, the equivalent of `kubectl exec -it`; it never takes over the whole TTY, so it works
  when detached), port-forward, edit (`$EDITOR` in a local terminal pane).
- **Pivot:** `node` → `host` (by node name, InternalIP, or a configurable mapping), and `host` → pods on that node.

### 8.3 Prometheus + Alertmanager

- Prometheus HTTP API: `query`, `query_range`, `series`, `labels`, `targets`, `rules`, `alerts`.
- Alertmanager API v2: alerts (firing/silenced/inhibited), silences. Actions: create, expire, and extend silences.
- Rendering: braille sparklines in tables, a full-pane line chart for a selected series, and PromQL entry with
  history.
- Every query uses the Investigation's TimeWindow.

### 8.4 Grafana (query directly)

- `GET /api/search`: dashboards and folders. `GET /api/dashboards/uid/<uid>`: panel definitions.
- Panels are rendered **inside the TUI** by running their queries through `POST /api/ds/query` (Grafana's unified
  query API, which uses Grafana's own datasource auth). Supported in v1: timeseries/stat/gauge panels backed by
  Prometheus or Elasticsearch. Other panel types show the title plus "open in browser" (`o`).
- Dashboard template variables become TUI selectors. A `$host`/`$instance` variable is filled from the current
  target.
- Grafana alerting (`/api/prometheus/grafana/api/v1/alerts`) feeds into `:alerts` alongside Alertmanager.
- Annotations: read in v1. Writing an annotation ("jumpr: restarted nginx on 12 hosts") is an optional Action.

### 8.5 Elasticsearch

- Plain REST: `_search` (Query DSL + `query_string`), `_field_caps`, `_cat/indices`, `_resolve/index`, plus
  point-in-time/search_after for paging. Cross-cluster patterns (`remote:index-*`) work as-is.
- Saved "log sources": index pattern, time field, host field, message field, and level field. Host and time
  filters come from the Investigation.
- Live tail = polling `search_after` on the time field.
- Read-only in v1.

### 8.6 Salt (over SSH to the master)

Salt is the second-deepest backend after Linux. jumpr drives the **existing** `salt`, `salt-run` and `salt-key`
CLIs on the master over one SSH connection (`salt.master: saltmaster.example.internal`, `sudo -n` if configured).
It does not use salt-api, does not install anything, and does not reimplement state logic.

**Job engine (everything sits on this).** Commands are submitted with `salt --async … --out=json`, which returns a
job id (jid) immediately. jumpr then polls `salt-run jobs.list_job <jid> --out=json` (expected minions + returns
so far) until every expected minion has returned or the timeout passes. Minions that never answer are reported as
`no return`, never silently dropped. Small, fast reads (`test.ping`, `grains.get`) may use the synchronous form
with `--static -t <timeout>`.

| Area | Kinds / commands in jumpr | Salt CLI used |
|---|---|---|
| **Minions and keys** | `:minions` (id, up/down, key state, os, role, salt version, last seen), `:keys` (accepted / pending / rejected / denied with fingerprints) | `salt-run manage.status`, `salt-run manage.versions`, `salt-key -L --out=json`, `salt-key -F --out=json` |
| **Key management** | accept / reject / delete as actions; accept shows the fingerprint; delete needs a typed confirmation | `salt-key -a|-r|-d <id> -y` |
| **Grains** | inventory source (labels `salt.role`, `salt.dc`, `salt.os`, …) read from the master's cache without touching minions; grains explorer per minion | `salt-run cache.grains tgt='*' --out=json`, `salt <tgt> grains.items` |
| **Targeting compiler** | jumpr selector → Salt target and back. `role=web dc~ewr|lax !maint` compiles to `-C 'G@role:web and G@dc:ewr\|lax and not G@maint:*'` when every key is a known grain; otherwise the selector is resolved on the client and sent as a chunked `-L` list. Pillar (`I@`), nodegroups (`N@name`, also usable as `@salt:name`), PCRE (`E@`) and globs are understood. A native expression can be typed directly (`:salt -C '…'`); `--preview-target` resolves it to hosts so it becomes a normal jumpr target set. The compiled target is always shown in the plan | `salt -C '…' --preview-target`, `salt-run config.get nodegroups` |
| **Jobs browser** | `:jobs` — running (`jobs.active`) and history (window-bound). Job detail groups returns per minion like a fan-out: "41 identical · 2 differ · 1 no return"; pivot from a minion to its host | `salt-run jobs.active`, `salt-run jobs.list_jobs start_time=… end_time=…`, `salt-run jobs.list_job <jid>`, `salt-run jobs.kill_job`-equivalent (`saltutil.kill_job`) as an action |
| **States** | `:salt state [sls]` — `state.apply test=True` on the target set, normalized and **grouped by identical diff**; per-state rows (id, function, result, changes, comment); `:states` — state tree browser (envs, top file, sls files, rendered `show_sls`/`show_highstate` for a minion) | `state.apply test=True`, `state.show_top`, `state.show_sls`, `state.show_highstate`, `salt-run fileserver.envs`, `salt-run fileserver.file_list` |
| **Pillar** | `:pillar <minion>` — keys and structure visible, **values redacted by default**; reveal one value on demand; pillar values are never written to investigations or logs | `pillar.items`, `pillar.get` |
| **Mine** | `:mine` — mine functions and cached values per minion | `salt-run cache.mine tgt='*'`, `mine.get` |
| **Events** | `:events` — live event bus (job new/return, minion start, auth, beacons) streamed over the same SSH connection, filterable by tag | `salt-run state.event pretty=False` (long-running; cancelled when the pane closes) |
| **Beacons and schedules** | per-minion lists | `beacons.list`, `schedule.list` (`return_yaml=False`) |
| **Actions** | `state.apply` / `state.sls` (dry-run preview is mandatory in the plan), `service.restart`, `cmd.run`, `saltutil.refresh_pillar`, `saltutil.sync_all`, kill job | through the action bus; jumpr's canary and rolling batches are implemented by submitting one job per batch (Salt's own `-b` is not used, so abort and thresholds behave like every other action) |

**Salt as an execution transport.** For fleets, `:run --via salt` and probes can execute through the master
(`cmd.run_all`) instead of SSH fan-out. It implements the same `Executor` contract, so grouping, the bus and
recording are identical.

| | SSH fan-out | Salt transport |
|---|---|---|
| Connections | one per host (through bastions) | one, to the master |
| Speed at 1,000+ hosts | bounded by concurrency (64) | one publish; returns in seconds |
| Works when | SSH reaches the node | the minion is healthy and connected |
| Runs as | your user, `sudo -n` when asked | **root, always** (shown in the plan; policy can forbid it) |
| Good for debugging Salt itself | yes | no (it depends on what is broken) |
| Side effects | none | command and output are kept in the **master's job cache** like any Salt job (shown as a warning on first use) |
| Streaming output | yes | no (result arrives when the minion returns) |

Default is SSH. jumpr suggests Salt when the target set is larger than `salt.suggest_over` (default 200) and the
master is healthy. Scripts are passed base64-encoded so no quoting layer can alter them.

**salt-ssh awareness.** If the master has a roster, it can be read as an inventory source. jumpr does **not**
execute through `salt-ssh` unless `salt.allow_salt_ssh: true`, because salt-ssh copies a thin runtime to the
target (`/var/tmp`), which breaks the zero-footprint rule; when enabled, the plan says so.

**`salt-call` local mode.** When the master is unreachable or restricted, a single host's view can run
`sudo -n salt-call --local grains.items` / `salt-call state.apply test=True --out=json` over SSH on the minion
itself.

**Health dot:** green when `salt-run manage.status` answers in time; half when some accepted minions are down;
red when the master or `sudo -n salt` fails.

### 8.7 Chef (over SSH to a workstation)

- Configured with `chef.workstation: chefws.example.internal`.
- Reads: `knife node list -F json`, `knife node show <n> -l -F json`, `knife search node '<q>' -F json -a …`,
  `knife status -F json` (last check-in), roles/environments/run-lists.
- Drift signals: ohai time staleness, last run failure (if a run-status attribute or report is available), and
  run-list differences across a group.
- Actions: `knife ssh '<q>' 'sudo chef-client'` is deliberately **not** used. Instead, jumpr runs `sudo chef-client`
  (or `--why-run` as a dry run) on targets via its own SSH fan-out, so safety and audit stay uniform.

### 8.8 YAML plugins

See §12.

## 9. Inventory & identity

**Sources** (all optional, merged):

- `~/.ssh/config` Host entries (non-wildcard)
- static files (YAML/CSV/plain list, with labels)
- Salt grains (`role`, `env`, `dc`, …) and Chef nodes (`roles`, `environment`, attributes via `-a`)
- Kubernetes nodes (labels)
- Prometheus targets (`up` series labels, e.g. `job`, `instance`)
- plugin-defined sources (any command or HTTP call that returns a list)

**Identity resolution:** each host is a canonical `HostID`. Aliases come from FQDN, short name, IPs, k8s node name,
Prometheus `instance` (port stripped), and Chef/Salt minion IDs. Configurable regex rewrites handle site-specific
naming:

```yaml
identity:
  rewrites:
    - from: '^(.*):9100$'            to: '$1'      # node_exporter instance -> host
    - from: '^ip-(\d+)-(\d+)-(\d+)-(\d+)\..*' to: '$1.$2.$3.$4'
  ssh_name: '{{ .FQDN }}'             # what to dial
```

Labels from all sources are merged under namespaced keys (`salt.role`, `k8s.zone`, `prom.job`) plus unprefixed
shortcuts if there is no conflict.

**Selectors** (one syntax everywhere): `role=web env=prod dc~ewr|lax !maint host~^db` plus `@saved-set` and
`@investigation`. Inventory is cached on disk (`~/.cache/jumpr/inventory.json`) and refreshed in the background.
Stale data is labelled.

## 10. Investigations & knowledge (plain Markdown)

Root: `~/jumpr/` (configurable; can point inside any git repo).

```
~/jumpr/
  investigations/2026-11-12-api-5xx-ewr.md
  knowledge/hosts/<host>.md
  knowledge/services/<unit-or-app>.md
  knowledge/alerts/<alertname>.md
  knowledge/runbooks/<slug>.md
  sets/<name>.txt                 # saved target sets
```

**Investigation file:**

```markdown
---
id: 2026-11-12-api-5xx-ewr
status: open            # open | resolved | abandoned
opened: 2026-11-12T14:02:11-05:00
window: [2026-11-12T13:30:00-05:00, now]
targets: [web-ewr-0[01-24].example.internal]
alerts: [HighErrorRate{service="api"}]
tags: [api, nginx]
---
# API 5xx in EWR

## Timeline
- 14:02:11 **open** from alert `HighErrorRate` (24 hosts)
- 14:02:40 **probe** `failed-units` on 24 hosts → 22 identical, 2 differ (`nginx.service failed`)
- 14:03:05 **journal** nginx on web-ewr-007 → `bind() to 0.0.0.0:443 failed (98: Address already in use)`
  <details><summary>output (38 lines)</summary>

  ```
  …
  ```
  </details>
- 14:05:12 **action** `systemctl restart nginx` on 2 hosts by **human** → ok 2/2
- 14:06:00 **note** stale haproxy bound :443 after salt run 20261112135501

## Findings
## Resolution
```

- Entries are appended automatically (actor: `human` | `plugin:<name>` | `agent:<name>`). Output is attached
  collapsed and size-capped. Secrets are redacted using config patterns plus built-in token regexes.
- `n` adds a free-text note, `F` marks the selected output as a finding. Resolving prompts for a one-line root cause
  and offers to create or append a knowledge note.
- **Knowledge resurfacing:** a local inverted index (built at startup, kept in memory) over frontmatter (`hosts`,
  `services`, `alerts`, `tags`) and the text. When a target, unit, or alert is opened, matching notes appear in a
  side panel (`K` toggles). No database; files are the source of truth.

## 11. Actions & safety

Every write, from any backend, plugin, or (future) agent, goes through the **Action Bus**:

1. **Plan:** the resolved target list, the exact commands or API calls, and a dry-run/diff where available
   (`state.apply test=True`, `chef-client --why-run`, `kubectl diff`-equivalent, `systemctl` current state).
2. **Confirm:** scaled to the blast radius.
   - 1 target: `y/N`
   - 2–20 targets: shows the list, `y/N`
   - more than 20, or the target set includes something labelled `protected` (configurable): **type the target count** to confirm
3. **Execute:** for many targets, use a **canary** (default 1 host, or N%) → pause and show results → **rolling
   batches** (default 10% / max 50) with `failure_threshold` (default: stop on first failure in canary, >5% after).
   `Esc` aborts remaining batches.
4. **Record:** results are appended to the Investigation (or `~/jumpr/audit/YYYY-MM-DD.md` if none is open),
   including actor, plan, per-host rc, and duration.

Policy (`policy.yaml`): deny patterns (e.g. never `systemctl stop sshd`), protected label selectors, a max targets
per action, and a read-only mode switch (`--readonly`, or per context such as `prod`).

### 11.1 Interactive shells and synchronized input

An interactive shell in a terminal pane is a deliberate escape hatch: what you type there is not planned or
confirmed, exactly like a plain `ssh` session. Opening one is recorded (`shell opened on web-01`), is refused in
read-only mode unless `policy.shell_in_readonly: true`, and the pane border shows the host and a `protected` mark.

Typing into several shells at once is where accidents happen, so it is guarded:

- **Broadcast line (default, `prefix B` or `:sync`).** A prompt collects one full line. On Enter it becomes an
  action on the bus: the plan lists the panes/hosts and the exact line, confirmation scales with the count, the
  line is sent to a **canary pane first**, then to the rest. Deny patterns apply to the line. The line is recorded.
- **Raw synchronized input (`:sync raw`, opt-in).** Every keystroke goes to all terminal panes in the window, like
  tmux `synchronize-panes`. Turning it on is itself a confirmed action; it is limited to `mux.max_sync_panes`
  (default 8), refused when any pane's host is `protected` or the session is read-only, shown as a red
  `SYNC ×N` badge, switched off automatically after 5 minutes idle, and each completed line is recorded
  best-effort. Deny patterns are checked on Enter and block the Enter key; this is a seat belt, not a security
  boundary (shell history and editors bypass it), and the UI says so.
- Target sets larger than the pane limit do not use shells at all: use `:run`, which is the same idea with proper
  grouping and rollout.

## 12. YAML plugins

Location: `~/.config/jumpr/plugins/*.yaml`. Hot-reloaded. Validated on load with clear errors.

```yaml
apiVersion: jumpr/v1
kind: Plugin
name: nginx-status
description: nginx worker + connection summary
target: host                    # host | k8s | http | inventory
privileged: true                # run with sudo -n
command: |
  nginx -T 2>/dev/null | grep -E '^\s*(worker_processes|listen)' ;
  ss -Htanp state established '( sport = :443 )' | wc -l
parse:
  type: regex                   # json | jq | regex | columns | kv | lines | raw
  pattern: '^\s*(?P<directive>\w+)\s+(?P<value>[^;]+);'
view:
  columns: [directive, value]
  group_by_output: true         # fan-out grouping
links:
  - label: journal
    to: journal
    args: { unit: nginx.service }
actions:
  - name: reload
    command: systemctl reload nginx
    privileged: true
    confirm: true
```

HTTP plugin example (uses a connection profile):

```yaml
apiVersion: jumpr/v1
kind: Plugin
name: lb-pool-members
target: http
connection: corp-sso
request:
  method: GET
  url: https://lb-api.example.internal/v1/pools/{{ .Param "pool" }}/members
parse:
  type: jq
  expr: '.members[] | {host: .address, state: .state, conns: .active}'
view: { columns: [host, state, conns] }
emits_hosts: host               # rows become pivotable hosts
```

Go backends are compiled in, using the `Backend` interface (§8). No Go `plugin` package (fragile on macOS).

## 13. TUI / UX

```
┌ jumpr ─ ctx:prod-ewr ─ inv:api-5xx-ewr ─ window: -30m→now ─ targets: 24 ─ ●salt ●prom ●es ◐grafana ┐
│ :hosts role=web dc=ewr                                                         /filter: nginx      │
├──────────────────────────────────────────────────────┬────────────────────────────────────────────┤
│ HOST            LOAD  MEM%  PSI-io  FAILED  UNITS     │ Knowledge (3)                              │
│ web-ewr-007     4.12   71    3.2%   1 ⚠     nginx     │  · alerts/HighErrorRate.md                 │
│ web-ewr-012     3.98   69    2.9%   1 ⚠     nginx     │  · services/nginx.md  "port 443 conflict"  │
│ web-ewr-001 …   0.80   44    0.1%   0       —         │  · inv 2026-09-03-api-5xx (resolved)       │
├──────────────────────────────────────────────────────┴────────────────────────────────────────────┤
│ <enter> open  <u> units  <j> journal  <p> procs  <s> sockets  <m> metrics  <l> logs(es)  <:> cmd  │
└───────────────────────────────────────────────────────────────────────────────────────────────────┘
```

The frame above is one pane filling the window. With splits, the status line (top) and the command/hint bar
(bottom) stay global; each pane gets a border whose title carries its breadcrumbs and filter; the focused pane's
border is highlighted. See §21 for windows, panes and the prefix key.

- **Navigation:** k9s-style `:kind [selector]` with fuzzy autocomplete, `/` filter (substring, regex, or
  `jq:` prefix), `Enter` drill-in, `Esc` back, a breadcrumb stack per pane, and `?` for help on every view. Every
  multiplexer function is also a `:` command (`:split`, `:vsplit`, `:window`, `:zoom`, `:detach`).
- **Status line:** a health dot per backend (green ok, half degraded, red down, grey unconfigured).
- **Time:** `t` sets the window (`-1h`, `2026-11-12 13:00..14:00`, `alert`), and every pane respects it.
- **Fan-out view:** groups identical outputs, then `Enter` on a group shows its members, and `d` diffs two groups.
- **Shortcuts:** `x` actions menu, `r` refresh, `w` watch/follow, `y` yank to clipboard, `o` open in browser,
  `n` note, `F` finding, `K` knowledge panel, `T` timeline.
- **Theme:** dark, high contrast, works in 256-colour and truecolor terminals. All keys are rebindable
  (`keymap.yaml`).
- **Responsiveness:** input → frame < 16 ms. Every fetch is async with a spinner and its elapsed time. Results
  stream in per host.

## 14. Configuration

```
~/.config/jumpr/
  config.yaml          # backends, inventory, identity, defaults
  connections.yaml     # connection profiles (§7.2)
  policy.yaml          # safety (§11)
  keymap.yaml
  plugins/*.yaml
  helpers/             # user auth helpers (not shipped)
```

- `jumpr init` writes a commented example config. `jumpr doctor` checks every backend, connection, bastion, and
  sudo on a sample host, and reports actionable failures.
- Multiple named **environments** (`--env prod`) switch whole backend sets.

## 15. Reliability & performance targets

| Area | Target |
|---|---|
| Startup to first frame | < 300 ms (inventory from cache) |
| Single-host overview | < 1.5 s via warm bastion |
| 1,000-host probe | first results < 3 s, complete < 60 s at 64 concurrency, UI responsive throughout |
| Memory | < 300 MB at 2,000 hosts with overview data |
| Failure isolation | one host/backend failure never fails the view; errors are listed per host with a reason (`timeout`, `auth`, `hostkey`, `sudo`, `parse`) |
| Parsers | tolerant: unknown fields ignored, a missing command produces a `not-available` cell, never a crash |
| Crash safety | panic recovery restores the terminal and writes a crash report to `~/.cache/jumpr/crash/` |
| Logs | structured debug log `~/.cache/jumpr/jumpr.log` (`--debug`), with secrets redacted |

## 16. Security

- No credentials are persisted by jumpr. The Keychain and helpers own them.
- Investigations and audit files are redacted before writing (built-in patterns: bearer tokens, AWS keys, private
  keys, `password=`; plus user patterns).
- Remote commands are built with strict shell quoting (`shellescape`). User input is never interpolated unquoted.
- Host key checking is on by default and cannot be silently disabled.
- Defaults to read-only in contexts marked `protected: true`.

## 17. AI-readiness (not in v1, designed for)

- The Action Bus already records an `actor`. A future `jumpr mcp` subcommand exposes a stdio MCP server whose tools
  map 1:1 to bus operations (list, get, probe, query, search, propose_action).
- Policy from the interview: **agents read freely; every write requires approval in the TUI**. Agent writes appear as
  pending actions in a TUI queue, using the same plan → confirm → execute flow.
- The agent receives the Investigation file plus matched knowledge notes as context, which is the long-term learning
  loop.

## 18. Testing

- **Parser golden tests:** real outputs captured from RHEL 8/9, Rocky 9, Ubuntu 20.04/22.04/24.04 (sanitized) in
  `testdata/`, with a test for each probe.
- **SSH integration:** an in-process test SSH server (`gliderlabs/ssh`) with a fake command table, plus bastion
  chain and channel-limit tests.
- **HTTP backends:** `httptest` servers with recorded responses. Connection profile tests cover proxy (HTTP CONNECT,
  SOCKS), helper refresh on 401, and TLS CA.
- **TUI:** `teatest` snapshot tests for key views.
- **Scale test:** a fake transport simulating 2,000 hosts with latency and failure injection (CI benchmark).
- **Session server:** client and server in one test process over a temporary socket, plus one end-to-end test that
  drives a real client through a local PTY.
- **Lab:** the OrbStack lab in §22 is the end-to-end target. Lab tests carry the build tag `lab`
  (`make lab-test`); `go test ./...` never needs it. Golden parser inputs are captured from the lab's three
  distros with the Go dev tool.
- **Tooling rule:** every helper is Go — the lab controller (`cmd/@@`), fault injection, the mock SSO
  proxy, the example auth helper, capture/sanitize (`cmd/@@`), golden updates (`go test -update`) and
  release. No bash or Python helper scripts; the Makefile only calls `go`.

## 19. Build plan (phases)

Detailed milestones, demo scripts and the task list are in `BUILD-PLAN.md` §2 and `docs/tasks/README.md`.

| Milestone | Scope | Exit criteria |
|---|---|---|
| **M0 Skeleton** | repo, Makefile, config loader, error taxonomy, resource model, selectors, fake backend, TUI shell (command bar, view stack, status line with health dots, help) | `jumpr --demo` navigates fake data; `make check` green |
| **M1 Multiplexer** | layout tree, panes, windows, prefix key, zoom/resize, copy mode, layout persistence; the app proven to run over a plain byte stream (no TTY) | W7 minus shells and detach, on fake data |
| **M2 SSH + Linux core** | ssh_config, agent, known_hosts, bastion pool, exec, batch probes, `sudo -n`; inventory (ssh_config + static); host, unit, journal, process, socket views; read-only fleet probes with grouping; SSH shell in a terminal pane | W2 against the lab; shell pane on a lab host |
| **M3 Actions + investigations** | action bus, policy, confirm levels, canary/rolling, audit; investigation Markdown, notes, findings, knowledge panel; time window; `:run`; guarded synchronized input | W1 minus metrics, W5, W6, W9; every write recorded |
| **M4 Salt core** | master runner, minions/keys, grains inventory, targeting compiler, job engine + jobs browser, state dry-run grouped by diff, Salt transport, Salt actions | W4 (Salt) + W8. **First fully usable release (v0.5)** |
| **M5 Session server** | protocol, server, client, auto-start, detach/reattach, env refresh, autosave/restore, behaviour inside tmux | W7 complete |
| **M6 systemd + Linux depth** | isd parity (§8.1.1), disks, network, cgroups, kernel log, files, packages, `/proc` drill-in | isd no longer needed day to day |
| **M7 Salt depth** | state tree browser, pillar (redacted), mine, key management, events, nodegroups, beacons/schedules, salt-ssh awareness, `salt-call` local | §8.6 complete |
| **M8 HTTP + Prometheus/Alertmanager** | connection profiles, proxies, TLS, auth helpers, `doctor conn`; alerts, PromQL, sparklines, silences; alert → hosts | W1 complete with metrics |
| **M9 Kubernetes** | client-go, core kinds, logs, describe/YAML, actions, exec in a pane, port-forward, node↔host pivot | W3 parity with daily k9s use |
| **M10 Chef + plugins** | knife over SSH, drift, `chef-client --why-run` through the bus; YAML plugin engine + 5 example plugins | W4 (Chef); custom plugin authored in < 10 min |
| **M11 Grafana + Elasticsearch** | dashboard browse + in-TUI panels via `/api/ds/query`; ES log sources, search, tail | dashboards and ES logs inside investigations |
| **M12 Hardening** | 2k-host scale test, crash safety, `jumpr init/doctor`, system-ssh fallback, release build, user docs | daily-driver ready |

Each milestone ends with a usable tool and a demo script against the lab, not a half-built layer.

## 20. Decisions & open questions

**Decided**
- Go, single static binary, macOS client only, zero node footprint.
- Native SSH by default with a system-ssh fallback; bastion multiplexing.
- HTTP auth/proxy via generic connection profiles + auth helper exec protocol; the user customizes for their
  environment.
- Grafana is queried directly (`/api/ds/query`), with link-out only for unsupported panels.
- Kubernetes via the local kubeconfig.
- Plain Markdown for investigations, knowledge, and audit.
- Full read/write with blast-radius guards.
- (v0.2) tmux-style multiplexer; the session process renders, the client is a byte pump; detach via a per-user
  session server on the laptop (§21). Bubble Tea v1 line.
- (v0.2) Terminal panes use an in-process VT emulator; nothing interactive ever takes over the whole TTY.
- (v0.2) Salt runs as async jobs polled by jid; Salt can be an execution transport; salt-ssh execution is off by
  default.
- (v0.2) `systemctl list-units` is parsed from plain text on all distros.
- (v0.2) Module path `jumpr` for now (renameable). All helper tooling in Go.

**Open**
1. Final name (working: `jumpr`).
2. Default knowledge root: `~/jumpr/`, or inside an existing notes repo?
3. Is a Chef run-status source available (reporting / run-status attribute), or only ohai time?
4. Do any Salt masters restrict `sudo salt` for your user (it affects whether `salt-call`-style local reads are needed)?
5. Is Elasticsearch read-only, or do you want saved searches written back?
6. **Prefix key.** Proposed: `C-a`, and `C-b` automatically when jumpr runs inside tmux (your tmux prefix is
   `C-a`, so it would never reach jumpr). Alternative: one fixed key that never collides (`C-b` everywhere, or
   `C-g`).
7. **Drop-in edits.** v1 previews a systemd drop-in but does not write it, because that is a file write on the
   node. Allow it as an explicit, recorded action (written with `sudo tee` + `daemon-reload`)?
8. **Salt transport and the job cache.** Running `:run --via salt` leaves the command and its output in the
   master's job cache. Acceptable as a labelled trade-off, or should the Salt transport be limited to read-only
   probes?
9. **Raw synchronized input.** Keep the opt-in raw mode (§11.1), or ship only the broadcast-line mode?
10. **Local shell panes.** Should a pane be allowed to run a local shell on the laptop (handy for `git`,
    `kubectl`), or keep panes strictly for jumpr views and remote shells?

## 21. Multiplexer and sessions ("it should work like tmux")

### 21.1 Model

```
server (one per user, on the laptop)
└─ session "main"                 named; owns targets, time window, investigation, read-only flag
   ├─ window 0 "hosts"            a full-screen layout; shown in the status line like tmux: 0:hosts* 1:salt-
   │  ├─ pane %1  view stack      :hosts role=web › web-01 › units
   │  ├─ pane %2  view stack      journal -u nginx (following)
   │  └─ pane %3  terminal        ssh web-01   (PTY on the node, emulated locally)
   └─ window 1 "salt"
      └─ pane %4  view stack      :jobs › 20261112135501
```

- A **pane** hosts exactly one of:
  - a **view stack** — everything in §13 (tables, journal, fan-out, timeline), with its own breadcrumbs and filter;
  - a **terminal** — an interactive PTY: an SSH shell on a host (M2), a pod exec (M9), or a local shell (open
    question 10). The remote byte stream is fed to an in-process terminal emulator and drawn inside the pane, so
    full-screen programs (`top`, `less`, `vim`, `journalctl` pager) work inside a split.
- Targets, the time window and the open investigation belong to the **session**, so every pane agrees on "which
  hosts, which time range, which incident". A pane can pin its own host (a host drill-in).
- Layout is a binary split tree, as in tmux. Minimum pane size 10×3.

### 21.2 Keys

Prefix: `C-a` by default; `C-b` when jumpr is started inside tmux (`mux.prefix`, `mux.prefix_in_tmux`). The status
line shows the active prefix. Pressing the prefix twice sends it through to the focused terminal pane. All
bindings are rebindable in `keymap.yaml`, and each has a `:` command.

| Keys (after prefix) | Action | Command |
|---|---|---|
| `%` / `"` (also `\|` / `-`) | split right / split below | `:vsplit`, `:split` |
| arrows or `h j k l` | focus pane in that direction | `:focus left\|right\|up\|down` |
| `o` / `;` | next pane / last pane | |
| `H J K L` (repeatable) | resize by 2 cells | `:resize <dir> <n>` |
| `z` | zoom / unzoom the pane | `:zoom` |
| `x` | close pane (confirm if it holds a live shell or a running rollout) | `:kill-pane` |
| `{` / `}` / `Space` | swap panes / next preset layout (even-h, even-v, main-left) | `:layout <name>` |
| `c` / `&` | new window / close window | `:window new\|kill` |
| `n` / `p` / `0`–`9` / `w` | next / previous / by number / window list | `:window <n>` |
| `,` / `$` | rename window / session | `:rename …` |
| `s` | session list (switch) | `:sessions` |
| `S` | open an SSH shell pane to a host (prompt with completion; defaults to the selected host) | `:ssh <host>` |
| `[` | copy mode | `:copy` |
| `B` | broadcast a line to the terminal panes of this window (§11.1) | `:sync` |
| `:` | command bar (works from a terminal pane too) | |
| `d` | detach | `:detach` |
| `?` | key help | `:help` |

Inside a view pane, k9s-style keys work without the prefix (§13). Inside a terminal pane every key goes to the
remote side except the prefix.

### 21.3 Copy mode

`prefix [` freezes the pane and shows its text with scrollback (terminal panes keep `mux.scrollback` lines,
default 10,000; view panes expose their full content, not only the visible rows). vi keys: `h j k l`, `g`/`G`,
`C-u`/`C-d`, `/` search, `v` select, `V` line select, `y` yank, `q`/`Esc` leave. Yank goes to the macOS clipboard
(`pbcopy`) and, as a fallback and when nested in tmux or SSH, via OSC 52. `F` in copy mode marks the selection as
a finding in the open investigation.

### 21.4 Sessions, detach and reattach

```
$ jumpr                    # attach to the last session, or create "main"; starts the server if needed
$ jumpr new -s db-incident # new named session
$ jumpr attach -t main     # reattach
$ jumpr ls                 # sessions, windows, attached?, last activity
$ jumpr kill-session -t x  ·  jumpr kill-server
$ jumpr --standalone       # no server: one process, exits when you quit (also the only mode before M5)
```

- **Architecture.** One per-user **session server** on the laptop, started on demand, listening on a unix socket
  (`~/.cache/jumpr/jumpr.sock`, mode 0600 in a 0700 directory). The server owns sessions, SSH connections,
  backends, the action bus and the investigation log, and it **renders**: each session is one long-lived TUI
  program that writes terminal bytes. The client only puts the local terminal in raw mode and pumps bytes and
  resize events over the socket. This is tmux's own design; it means there is one code path for standalone and
  server mode, no state to synchronize, and a client small enough to be obviously correct. Wire protocol:
  `BUILD-PLAN.md` §6.
- **What survives a detach:** everything in the session — followed journals, running fan-outs, Salt job polling,
  open shells, a rollout paused at its canary (it stays paused; it never proceeds without a human).
- **What survives a server restart or reboot:** the layout and each pane's command history (autosaved to
  `~/.cache/jumpr/sessions/<name>.json`), the investigation files, and the audit log. Live connections do not;
  terminal panes come back as "disconnected — press r to reconnect".
- **Prompts** (unknown host key, key passphrase) are shown in the TUI. If nobody is attached, the operation waits
  briefly and then fails with `needs-interaction`.
- **ssh-agent:** each attach refreshes `SSH_AUTH_SOCK` from the attaching client, as tmux's `update-environment`
  does.
- **Inside tmux:** supported. The prefix switches to `C-b`, OSC 52 is used for the clipboard, and the terminal
  title is set to `jumpr: <session>`. Running jumpr's own client inside a jumpr terminal pane is refused
  (`$SITREP` is set in local panes).
- **Security:** the socket is reachable only by the same user. The server holds no credentials beyond what the
  process would hold standalone (agent access, in-memory auth-helper tokens). `jumpr kill-server` drops
  everything. Nothing is listening on the network, and nothing runs on any node.
- **Limits (v1):** one client per session at a time (a second attach takes over); one colour profile per server.

### 21.5 Build order

The multiplexer is built in-process first (M1) and is fully usable standalone. From the first commit the code
follows the detach-safety rules (`BUILD-PLAN.md` §6.1: no direct stdin/stdout, no TTY takeover, sizes only from
messages), and an early test runs the whole app over an in-memory pipe. M5 then adds only the socket, the client
and process management.

## 22. Test lab

A small, real environment on the laptop (OrbStack machines, arm64) so every workflow in §5 can be exercised end to
end. It is controlled by a Go command, `cmd/@@` (`up | down | status | fault list | fault <name> [--undo]`);
provisioning snippets that run inside lab machines are embedded in that binary. jumpr itself is used against the
lab exactly as against production: over SSH through the bastion, nothing installed by jumpr.

| Host (`*.lab.example.internal`) | OS | Role |
|---|---|---|
| `bastion` | Ubuntu 24.04 | sshd only; every other host is reached with ProxyJump through it |
| `salt` | Rocky 9 | salt-master and its own minion; state tree (`common`, `nginx`, `haproxy`, `postgres`), pillar including a fake secret, nodegroups `webs` and `k8s` |
| `chef` | Ubuntu 22.04 | chef-zero / cinc with a knife workstation |
| `web-01`, `web-02` | Rocky 9 | salt minions (grains `role`, `dc`), nginx, node_exporter |
| `web-03` | Ubuntu 24.04 | same role on a second distro |
| `db-01` | Ubuntu 22.04 | postgres, third distro |
| `k3s-1`, `k3s-2` | Ubuntu 24.04 | k3s server and agent; also SSH hosts and salt minions (node ↔ host pivot); kubeconfig context `@@` |
| observability (containers, later) | — | Prometheus, Alertmanager, Grafana, Elasticsearch behind a mock SSO proxy that requires a cookie (exercises connection profiles and auth helpers) |

- User `sre` with passwordless sudo on every machine; a lab-only SSH key; `lab/ssh_config` is generated.
- **Fault injection** (`@@ fault <name>`, undone with `--undo`) reproduces the mockup's scenarios: nginx
  cannot bind :443 because haproxy took it, `/var` filling up, a crash-looping OOM pod, a stopped minion, a pending
  minion key, a failing state.
- **How tests use it:** lab tests are ordinary Go tests behind the build tag `lab`. They read `lab/ssh_config`
  (and `lab/.keys/kubeconfig`), skip with a clear message when the lab is down, inject a fault, drive jumpr's packages
  (not the TUI) against real hosts, assert, and reset in cleanup. Each milestone has one demo script (manual,
  `BUILD-PLAN.md` §2) and at least one `TestLab…` test. Parser goldens are captured from the lab's three distros.
- The lab is a development aid only. Nothing in the product depends on it, and no lab hostnames or keys are
  secrets (the key is generated locally and git-ignored).
