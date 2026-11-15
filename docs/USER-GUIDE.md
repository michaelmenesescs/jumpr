# jumpr — user guide

jumpr is one terminal tool for operating infrastructure from your laptop: from an alert to the
hosts behind it, to the units, processes, sockets, logs and metrics on those hosts, to the pods on
them, to the config management that put them there — and then to the fix. It keeps the context at
every step and writes down what you did.

**Nothing is installed on the servers.** jumpr only uses SSH and commands that already exist on
RHEL 8+/Ubuntu 20.04+, plus your kubeconfig or an HTTP API when you configure one. All parsing,
joining, diffing and rendering happen on your machine.

Every example below is neutral: `example.internal` is the domain, `bastion1.example.internal` is
the bastion, `salt.example.internal` is the Salt master.

---

## 1. Install

jumpr is a single static binary for macOS (arm64 and amd64). No runtime, no cgo, no helper
daemon to install.

```
tar xzf jumpr-v1.2.3-darwin-arm64.tar.gz
xattr -d com.apple.quarantine ./jumpr   # only if you downloaded it with a browser
install -m 0755 jumpr ~/bin/jumpr      # anywhere on your PATH
jumpr version
```

The archive also carries this guide (`docs/USER-GUIDE.md`), the key reference
(`docs/KEYS.md`) and the example configuration and plugins (`examples/`), which are the same
files `jumpr init` writes — copy them by hand only if you want to start from them.

You need:

- an SSH agent or a key jumpr can read (`~/.ssh/id_*`), and `ssh_config` entries for your
  hosts, or a Salt master (see §2);
- a terminal of at least 100x30 for comfort (the UI works smaller, it just shows fewer columns).

`jumpr` runs a small session server in the background (§5), so a session survives closing the
laptop lid. It uses one unix socket under `~/.cache/jumpr/`.

---

## 2. First run: `jumpr init`, SSH and Salt

`jumpr init` writes the configuration skeleton and never overwrites an existing file:

```
jumpr init                 # ~/.config/jumpr/
jumpr init --config-dir /tmp/jumpr-demo   # try it somewhere else
```

It creates:

```
~/.config/jumpr/
  config.yaml         every key, at its default, with comments
  connections.yaml    HTTP connection profiles (§8)
  policy.yaml         the action policy (§6)
  keymap.yaml         key overrides (§3)
  plugins/            empty; your YAML plugins go here (§9)
```

Configuration lives in `~/.config/jumpr`, or wherever `--config-dir` / `$JUMPR_CONFIG_DIR`
points. Everything is optional: a missing file means defaults. An unknown key is an error naming
the file and line, so a typo never silently does nothing.

### Pointing jumpr at your hosts

Hosts come from your `ssh_config`. jumpr reads the files in `ssh.config_files`, resolves `Host`
aliases, `HostName`, `User`, `Port`, `IdentityFile`, `ProxyJump` and bastions, and uses the same
`known_hosts` your `ssh` does:

```yaml
ssh:
  config_files: [~/.ssh/config]
  known_hosts: []          # empty = whatever ssh_config says, else ~/.ssh/known_hosts
  concurrency: 64
  connect_timeout: 10s
  exec_timeout: 30s
```

Add a second source if your inventory lives in a file:

```yaml
inventory:
  sources:
    - type: ssh_config
    - type: static
      path: ~/.config/jumpr/hosts.yaml
```

`hosts.yaml` is a list of `{id, labels, hostname?, user?, port?}`; labels are what `:hosts
role=web` selects on. `:inventory refresh` re-reads every source.

### Pointing jumpr at a Salt master

jumpr drives the Salt CLI **over SSH** on the existing master. Nothing is installed there and no
API is required; it uses `salt`, `salt-run` and `salt-key` as they are.

```yaml
salt:
  master: salt.example.internal   # an inventory ID or an ssh_config alias
  sudo: true                      # run the CLI as root when the master needs it
  timeout: 60s                    # one job
  poll_interval: 2s
```

Once `master` is set, the `salt` backend appears (status line shows a green dot), and:

- `:minions` lists minions with grains as labels; `:keys` lists the key state;
- `:grains <minion>` shows a minion's grains, `:pillar <minion>` its pillar (values are redacted
  in records and logs);
- `:jobs` lists running and recent jobs, and opening one groups the returns per minion;
- grains can also feed the inventory: add `- type: salt` under `inventory.sources`.

Check it before trusting it:

```
jumpr doctor          # configuration, SSH, agent, bastions, backends, knowledge root, terminal
```

---

## 3. Keys

`docs/KEYS.md` is the full reference. It is generated from the built-in bindings by a test, so it
cannot drift, and `:keys` shows the map jumpr is *currently* running with (after your overrides).

The essentials:

| Keys | Action |
|---|---|
| `:kind [selector]` | open a view, k9s-style: `:hosts role=web`, `:units`, `:alerts` |
| `j k g G`, `enter`, `esc` | move, open, back |
| `/` | filter the view (substring, `~regexp`) |
| `?` | help for the current view |
| `x` | actions menu for the selection |
| `C-a %` / `C-a "` | split the pane right / below |
| `C-a d` | detach (§5) |

### Rebinding

Every binding in `keymap.yaml` is overridable by name, and so is every prefix binding:

```yaml
# ~/.config/jumpr/keymap.yaml
open: [o, enter]          # replaces the default list: name every key you want to keep
quit: [Q]
prefix:
  split-h: ["|"]          # "%" no longer splits
  focus-left: [h, left]
```

- A list replaces the default completely.
- Unknown names, and a key bound to two actions, are errors — jumprs tells you the file and line
  and refuses to start with the file loaded.
- The prefix key itself is not a binding: it is `mux.prefix` in `config.yaml`
  (`C-a` by default, `C-b` when jumpr runs inside tmux).
- `:keys` lists the effective map; restart jumpr after editing the file.

---

## 4. The nine workflows

Each one is a real path through the interface. `enter` drills in, `esc` goes back, and the
breadcrumb at the top of the pane always says where you are.

**W1 — alert to root cause.** `:alerts` lists firing alerts from Alertmanager. Select one and
press `enter`: jumpr opens an **investigation** (a Markdown record, §7) with the window set to
[alert start − 30m, now] and the targets resolved from the alert's labels. The host view shows
failed and restarting units, the journal for that window, top processes, sockets, disk and memory
pressure, and a sparkline of the alert's expression. `u` opens a unit, `j` its journal, `p`
processes, `s` sockets, `m` metrics. From a unit, `x` offers the actions for it — a restart is
planned, confirmed and executed (§6). Every step lands in the timeline.

**W2 — fleet sweep.** `:hosts role=web` over a large fleet, then `:probe failed-units`. Results
stream in per host and are grouped by identical output: "1,481 identical · 19 differ". `enter` on
a group lists its members, and from there `enter` opens one host. `:probe` with no name lists the
probes; `:probe pkg <name>` is the single-host form. Any probe result can be saved as a plugin
with `s`.

**W3 — Kubernetes debugging.** With a kubeconfig — `kubernetes.kubeconfig` in `config.yaml`, or
`$KUBECONFIG`, or `~/.kube/config` — `:pods`, `:deployments`, `:nodes`, `:events` work like k9s, and so does every
CRD: generic resources are discovered once and appear by plural name with the server's own
printer columns. `d` describes, `y` shows YAML. `enter` on a pod's node opens that node as an SSH
host — the same machine, the other half of the story. `:ns <name>` scopes, `:ctx <name>` switches
context, `:pf` forwards a port.

**W4 — config drift.** `:salt state` runs `state.apply test=True` against the current targets and
groups the returns by identical diff: "22 clean · 2 would change". Open a changing group to see
the diff, and apply from the same view (`x`) — the apply goes through the action bus with the
usual confirmation. For Chef, `:chef drift` shows hosts whose last run failed and `:chef whyrun`
shows what a run would change.

**W5 — ad-hoc command.** `:run 'ss -s'` runs on the current target set, grouped by identical
output, recorded in the timeline. `--sudo` wraps it with `sudo -n`; `--via salt` sends it through
the Salt transport instead of SSH. `--no-rollout` skips batching for a single host.

**W6 — "have I seen this before?"** Open any host, service or alert: the knowledge panel shows
your notes and past investigations about it, strongest match first. `K` toggles it, `enter` opens
the note, `:kb new host <name>` writes a new one.

**W7 — work like tmux.** Split the window: hosts table on the left, a followed journal top right,
an interactive shell on the bad node bottom right (`C-a S` or `:ssh`, then `w` to follow).
`C-a c` opens a second window for the Salt job you started; `C-a n`/`p`/`0`–`9` move between
windows; `C-a z` zooms a pane and `C-a H J K L` resizes it; `C-a [` is copy mode (vi keys, `y`
yanks to the macOS clipboard). `C-a d` detaches; reattach later and the follow and the job are
still there (§5).

**W8 — "what did Salt just do?"** `:jobs` lists running and recent jobs; `enter` opens one and
groups the returns per minion ("41 identical · 2 differ · 1 no return"). `enter` on a differing
minion pivots to its host, and `t` sets the journal to the job's window, so you see what the
minion printed while it failed.

**W9 — the same thing on many shells, safely.** Open four shell panes on four web hosts, then
`C-a B` (`:sync raw`) in the window: the line you compose goes to every pane **through the action
bus** — one plan, one confirmation, the canary pane first — and it is recorded in the
investigation. `policy.yaml` can set `sync.raw: deny` to turn this off entirely;
`max_sync_panes` caps how many panes may take part.

---

## 5. Sessions and detach

jumpr runs like tmux: a client attaches to a session hosted by a session server.

```
jumpr                      # attach to the most recent session, or create "main"
jumpr new -s incident      # create and attach a named session
jumpr new -s salt-run -d   # create it without attaching
jumpr ls                   # list sessions: name, windows, attached, created, activity
jumpr kill-session -t incident
jumpr kill-server          # end the server and every session
```

Inside jumpr, `C-a d` or `:detach` returns you to your shell with everything still running:
follows keep following, jobs keep polling, shells stay connected. `jumpr` attaches again.
A second client on the same session sees the same panes; `--standalone` skips the server
entirely for one process (debugging only — the session dies with the terminal).

The layout, the windows and each pane's view are restored after a restart (`--fresh` skips that).
`C-a s` (`:sessions`) switches between sessions.

---

## 6. Safety model

There are three layers, and they are deliberate.

**Read-only.** `jumpr --readonly` starts a session where every write is denied, and
`:readonly on|off` toggles it at runtime. Reads still work, shells still work if `policy.yaml`
allows them.

**The action bus.** Every write — a unit restart, a package change, a Salt state apply, a key
delete, a Kubernetes patch, a broadcast line — is planned by its backend, checked against the
policy, confirmed by you, executed, and recorded. There is no second path: nothing in jumpr
writes directly. A plan shows the exact commands, the hosts, the blast radius and the canary; a
dry run (`state.apply test=True`, a server-side dry run for a patch) is not a write. Rolling
batches, a canary first, and typed confirmation over a size threshold are all configurable:

```yaml
# ~/.config/jumpr/config.yaml
rollout:
  canary: 1
  batch_percent: 10
  batch_max: 50
  failure_threshold_percent: 5
  typed_confirm_over: 20
```

**Policy.** `policy.yaml` is the guard rail on top: `readonly`, `max_targets`, protected targets
that always need the typed count, deny rules by pattern or by action spec, and whether root
transports or shells are allowed. Built-in denials (stop sshd, `mkfs`, `rm -rf /`, reboot, `dd`
onto a block device) cannot be removed, only extended. `:policy` shows the effective policy.

Every action, denial and broadcast line is appended to the investigation, or to the daily audit
file when none is open. Credentials, passphrases, pillar values and tokens are redacted before
anything is written.

---

## 7. Investigations and knowledge files

Everything is plain Markdown under `knowledge.root` (`~/jumpr` by default):

```
~/jumpr/
  investigations/2026-11-12-api-5xx-ewr.md   one file per investigation
  audit/2026-11-12.md                        actions, denials and input when no investigation is open
  knowledge/hosts/web-01.example.internal.md
  knowledge/services/nginx.md
  knowledge/alerts/HighErrorRate.md
  knowledge/runbooks/rolling-restart.md
```

An investigation carries the window, the targets, the alerts and the timeline; `:inv` opens,
closes, resolves or resumes one (`:inv resume <id>`), `:timeline` shows it as a view, `:note
<text>` appends a note, `F` marks a selection as a finding, and `y` yanks. Files are ordinary
Markdown: edit them by hand, `git init` the root, review the diff. jumpr reads them back —
notes match by host, service, alert and tag, and closed investigations are searched too — which is
what makes W6 work.

---

## 8. HTTP connection profiles (with the example helper)

Prometheus, Alertmanager, Grafana, Elasticsearch and HTTP plugins all go through a named
**connection profile** in `connections.yaml`, so proxy, TLS and authentication are configured once:

```yaml
connections:
  corp-sso:
    proxy:
      type: ssh-socks                       # built-in SOCKS over an SSH host, like ssh -D
      via: bastion1.example.internal
    tls:
      ca_file: ~/.config/jumpr/ca/corp-root.pem
    auth:
      type: helper
      helper:
        command: ["~/go/bin/auth-helper-cookie", "-name", "_oauth2_proxy", "-domain", ".example.internal"]
        cache_ttl: 50m
        refresh_on: [401, 403, 302-to-login]
    headers:
      X-Requested-By: jumpr
    timeout: 15s
```

`auth.type` is one of `none`, `basic`, `bearer`, `headers`, `cookie` or `helper`. A helper is any
executable that prints this JSON on stdout:

```json
{"headers": {"Authorization": "Bearer …"},
 "cookies": [{"name": "_oauth2_proxy", "value": "…", "domain": ".example.internal"}],
 "expires_at": "2026-11-12T18:00:00Z"}
```

jumpr keeps the answer **in memory only**, caches it for `cache_ttl`, and on one of the
`refresh_on` conditions (a 401/403, or a redirect to a login page) drops the cache, runs the
helper once and retries once. `examples/auth-helper-cookie` is a working one: it reads the cookie
from `JUMPR_AUTH_COOKIE` or the macOS Keychain.

```
go build -o ~/go/bin/auth-helper-cookie ./examples/auth-helper-cookie
jumpr doctor conn corp-sso https://prom.example.internal/api/v1/query?query=up
```

`doctor conn` prints the resolved proxy, the TLS chain, the helper's output (redacted) and the
response, which is the fastest way to find out why a profile does not work. Reference the profile
by name from a backend (`prometheus.connection: corp-sso`) or a plugin (`connection: corp-sso`).

---

## 9. Writing a plugin

A plugin is one YAML file in `~/.config/jumpr/plugins/`. It is hot-reloaded: save it, then
`:plugins reload`. This one sums up nginx's workers and established connections on a host:

```yaml
apiVersion: jumpr/v1
kind: Plugin
name: nginx-status
description: nginx worker + connection summary
target: host                 # host | k8s | http | inventory
privileged: true             # run it with sudo -n
command: |
  nginx -T 2>/dev/null | grep -E '^\s*(worker_processes|listen)' ;
  ss -Htanp state established '( sport = :443 )' | wc -l
parse:
  type: regex                # json | jq | regex | columns | kv | lines | raw
  pattern: '^\s*(?P<directive>\w+)\s+(?P<value>[^;]+);'
view:
  columns: [directive, value]
  group_by_output: true
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

- `target: host` runs the command on each target host over SSH; `target: http` sends a request
  through a connection profile; `target: inventory` reads the inventory instead.
- `parse` turns the output into rows. `json`/`jq` decode, `regex` uses named groups as fields,
  `columns` splits fixed columns, `kv` and `lines` are the simple cases, `raw` shows text.
- `emits_hosts: host` makes each row pivotable to a host.
- `actions` are real writes: they go through the action bus like everything else.
- A malformed plugin is reported with the file, line and field, and jumpr keeps running without
  it. `:plugins` lists what loaded.

`S` on a fan-out view (a `:run` or `:probe` result) saves the command behind those results as a
plugin, so the fastest way to write one is to run it once and save the view.

---

## 10. Troubleshooting

Start with `doctor`; it checks each layer and says what to do next.

```
jumpr doctor                       # config, SSH, agent, bastions, samples, backends, knowledge root
jumpr doctor ssh web-01.example.internal    # resolve, dial, authenticate, run read-only probes
jumpr doctor conn corp-sso https://prom.example.internal/   # one HTTP profile
jumpr --debug                      # structured debug log in ~/.cache/jumpr/jumpr.log
```

| Symptom | What to look at |
|---|---|
| "no backends configured; try --demo" | `config.yaml` has no source with hosts: set `ssh.config_files` or `salt.master` |
| A host cell shows `timeout` | reachability or bastion saturation: `doctor ssh <host>`, `ssh.max_conns_per_bastion` |
| A cell shows `sudo` | the account needs a password for `sudo -n`; jumpr never prompts remote sudo |
| A cell shows `not-available` | the command is not on that host (rc 127); the probe needs another tool |
| `auth` / `hostkey` | an unknown or changed host key, or a failed SSH auth — `doctor ssh <host>` prints both |
| An HTTP backend shows `auth` | the connection profile: `doctor conn <profile> <url>` |
| Keys do nothing, or jumpr refuses to start | the error names the `keymap.yaml` line; `:keys` shows the effective map |
| A plugin is missing from `:plugins` | the load error names file, line and field |
| The UI looks wrong after a panic | jumpr restored the terminal and wrote a crash report to `~/.cache/jumpr/crash/`; attach it to a bug report (it is redacted) |
| A session is stale | `jumpr ls`, `jumpr kill-session -t <name>`, or `jumpr kill-server` |

`--demo` runs a built-in fake fleet of 1,512 hosts with no configuration at all: the quickest way
to see the interface, and a way to tell a UI problem from a connection problem.

---

## 11. Where things live

| Path | What |
|---|---|
| `~/.config/jumpr/config.yaml` | backends, inventory, SSH, Salt, rollout limits |
| `~/.config/jumpr/connections.yaml` | HTTP connection profiles |
| `~/.config/jumpr/policy.yaml` | action policy |
| `~/.config/jumpr/keymap.yaml` | key overrides |
| `~/.config/jumpr/plugins/*.yaml` | YAML plugins |
| `~/jumpr/` | investigations, audit files and knowledge notes |
| `~/.cache/jumpr/jumpr.log` | debug log (`--debug`) |
| `~/.cache/jumpr/crash/` | crash reports |
| `~/.cache/jumpr/jumpr.sock` | session-server socket |

`JUMPR_CONFIG_DIR`, `JUMPR_SOCKET` and `JUMPR_GLYPHS` override the configuration directory, the
socket and the glyph set (`auto|unicode|nerd|ascii`).
