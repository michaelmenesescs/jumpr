# Connection map and built-in tracing — scoping plan

_Status: scoping only. Written 2026-11-12. Nothing here is implemented, and no file outside this one was touched._
_Repo state at time of writing: 79/132 tasks merged, M0–M8 mostly done, M9 (k8s) not started._

## 0. What this is

A TUI map of who-talks-to-whom inside a Kubernetes cluster: every pod, service,
node and external endpoint as a node, every observed connection as an edge, and a
way to walk a single request from ingress to the last hop — without leaving the
terminal and without `kubectl`, `hubble`, `cilium`, `kubectl-trace` or `kubectl
debug` doing the work for us.

The point is debugging: "why is checkout timing out" should be a two-key
question ("which pods is `checkout` talking to, what is failing, and what is
denying it") rather than a chain of five tools with five output formats.

## 1. Constraints this must respect

These are the project's existing rules (`AGENTS.md`, `docs/BUILD-PLAN.md` §10),
not new ones:

- **Zero footprint on nodes.** Nothing installed, no file written, no daemon
  started. Only pre-existing binaries and APIs, reached through transport jumper
  already has (SSH exec/stream) or over the API server.
- **Our own clients.** We query APIs ourselves (client-go, HTTP, gRPC over the
  transport we own). Shelling out to `kubectl`/`cilium`/`hubble` on the
  workstation is explicitly *not* the design — those binaries are neither
  guaranteed present nor a stable interface. Where a node-local binary is the
  only source of a fact (e.g. `cilium-dbg` inside the agent pod), it runs on the
  node through our executor, and its output is parsed by us.
- **No new dependency unless a task says so** (existing rule 3). gRPC/protobuf
  for Hubble is a real decision — see §10.
- **Read-only by default.** Anything that changes cluster state (enabling L7
  visibility, creating a port-forward as a standing resource) goes through the
  action bus with a recorded, confirmed action — the same rule as kubectl-apply
  style writes today.
- **The UI never blocks.** Streaming flows arrive as `Event`s through
  `backend.Watcher` (`Watch(ctx, query) <-chan Event`), batched into the view via
  `tui.NextBatch` exactly like journals and job output.

## 2. Data sources, ranked

| # | Source | What it gives | Reachable how | Cost |
|---|--------|---------------|---------------|------|
| 1 | **Cilium agent local API** (`/var/run/cilium/cilium.sock`, per-node) | endpoint→identity map, ipcache, service list, policy verdicts, `monitor` event stream (L3/L4 + drops), `bpf ct list` (conntrack), `bpf nat list` | through the k8s API's pod-exec (we own a SPDY exec via client-go, task 105 already brings client-go) into the `cilium` DaemonSet pod, or over SSH to the node if it runs the agent binary on the host | one exec per node, long-lived stream for monitor |
| 2 | **Hubble relay** (`hubble-peer`/`hubble-relay` gRPC :4244) | cluster-wide flow records with workload labels, verdicts, L7 (HTTP/DNS) when visible | gRPC to the relay Service; requires either a port-forward we create and own, or direct reachability | one stream for the whole cluster; **needs a gRPC client + protobufs** |
| 3 | **Kubernetes API** (client-go, task 105) | pods, EndpointSlices, Services, NetworkPolicies, CiliumNetworkPolicy (dynamic client) | the API server we already talk to | small, cacheable |
| 4 | **Node-local netfilter/conntrack** (`conntrack -L`, `ss -tnp`, `/proc/net/tcp`) over SSH | ground truth for non-Cilium clusters and for host-network traffic | existing SSH transport | one exec per node per poll (cheap, but poll-only — no stream) |
| 5 | **Cilium policy objects** (CNP/CCNP) | intended allow/deny graph, i.e. "who *may* talk to whom" | dynamic client | small |

Honest limits, stated up front:

- **L4 is always answerable; L7 is not.** Method/path/status need L7 visibility
  (Cilium L7 policy or `--enable-l7-proxy` with explicit policy), which is a
  cluster-side change. The map must render useful L4 truth and *say* when L7 is
  unavailable rather than pretend.
- **Sampling and aggregation are mandatory.** A busy cluster emits thousands of
  flows/second; the map is an aggregate over a window, not a packet capture.
- **Conntrack polling is a fallback**, not a substitute: it gives no policy
  verdict and no drop reason.

### 2.1 Facts to verify before writing code (not assumed)

1. On a Cilium DaemonSet pod, `cilium-dbg bpf ipcache list -o json`,
   `cilium-dbg endpoint list -o json`, `cilium-dbg monitor -t drop -o json` —
   which of these subcommands actually support `-o json`, and their exact shapes.
   Verify on the lab cluster; never parse a format we have not captured.
2. Whether the relay is reachable directly or only through a port-forward, and
   whether jumper can create one via the API's `pods/portforward` subresource
   (SPDY) without extra dependencies.
3. Whether `hubble.api.v1.Observer/GetFlows` proto can be vendored from the
   Cilium repo at a pinned version and compiled with what client-go already
   brings (grpc + protobuf will still be new modules — decision §10).
4. Behaviour of `cilium-dbg monitor` when the agent is busy: does it drop events
   (it reports a "lost events" counter? verify), and what its backpressure looks
   like when our SSH channel is slow.

## 3. The model

Two layers, deliberately separate:

**Intended graph** — from API objects: workloads → services → endpoints, plus
policy allow rules. Static, cheap, answers "what should be possible".

**Observed graph** — from flows: directed edges with `(src identity, dst
identity, port, protocol, verdict, bytes, packets, count, first, last)`. Answers
"what actually happened".

The view shows observed by default, with intended drawn as a faint overlay and
disagreements highlighted (observed but not allowed → **denied** edge, red;
allowed but never observed → dashed grey).

```go
// internal/topology — pure, no I/O, unit-testable without a cluster.
type Node struct {
    ID     string      // "pod/checkout-7d9f", "svc/checkout", "ext/example.com:443"
    Kind   NodeKind    // Pod, Service, Node, External, CIDR
    Label  string      // display label
    NS     string
    Labels map[string]string
    Health NodeHealth  // from k8s status / agent endpoint state
}

type Edge struct {
    Src, Dst   string
    Proto      string   // tcp/udp/icmp
    Port       uint16   // 0 for icmp
    Verdict    Verdict  // Allowed, Denied, Unknown
    Bytes, Packets, Flows uint64
    First, Last time.Time
    Reasons    []string // drop reasons, policy names
    L7         *L7Stats // nil when not visible
}

type Graph struct { /* nodes, edges, window, sampled bool, lost uint64 */ }
```

`Graph` is built from a bounded flow store (ring buffer keyed by edge identity,
default 60 s / 50k edges) and is what both the renderer and the drill-in views
consume. Everything about it is deterministic given the same flow records, which
is what makes golden-file tests possible (§7).

## 4. The TUI

New view kind `netmap` (plus `:netmap` command), rendered by
`internal/tui/views/netmap.go` on top of a pure layout package
(`internal/topology/layout.go`).

Layout, top to bottom, is a **layered DAG** so the picture is stable between
frames (nothing is more useless than a graph that reshuffles every second):

```
ext/0.0.0.0/0            ingress-nginx  ─┐
                                          ├─► svc/checkout ─► pod/checkout-7d9f ─┬─► svc/redis ─► pod/redis-0
svc/checkout (ClusterIP) ────────────────┘                                      └─► ext/payments.example:443
```

- One row per layer (external → ingress → service → workload → datastore/egress).
- Edges are orthogonal bundles; a bundle carries a port badge (`:8080`) and a
  traffic sparkline; width/colour by rate and verdict.
- Node glyphs: pod (`●` running, `◐` pending, `✕` not-ready), service (`◇`),
  external (`○`), node (`▣`). ASCII glyph set honoured via `render.GlyphSet`
  (task 142 already did this plumbing).
- Budget: 100×30 frame with 400 nodes / 2,000 edges must render in **< 8 ms**
  (task 012's table budget is 5 ms for 1,512 rows; the map is allowed a little
  more, and it gets measured, not guessed).

Keys: `enter` focus a node, `esc` back, `f` cycles focus depth (1/2/all hops),
`v` toggles observed/intended/both, `p` port filter, `d` "denied only", `t`
timeline scrub (last 1m/5m/1h), `s` sort by bytes/denied/count, `/` filter by
name/label, `o` open the trace view for the selected edge.

### 4.1 Focus mode (the actual debugging tool)

Selecting a pod collapses the graph to that pod's neighbourhood: inbound edges
grouped by source (who calls me), outbound grouped by destination (who I call),
each row with rate, error/deny count and p99 latency when L7 exists. This is the
screen that answers "what is checkout talking to that is failing".

### 4.2 Edge detail

`enter` on an edge: both identities (including Cilium numeric identities and
labels), ports, verdict distribution, drop reasons, first/last seen, a
per-second sparkline, the policy objects that permit it, and the raw flow
records behind the aggregate (the last 50, redacted).

## 5. Tracing

"Tracing" here has to be two honest levels, because jumper cannot inject context
into an application without instrumentation:

**Level 1 — flow trace (always available).** Given a selected edge or pod, walk
related flows in a time window and stitch a causal path: `client → ingress →
service → pod A → pod B → external`, showing per-hop gap and latency. Correlation
is by (time, addresses, port) with a bounded lookahead; it is a *reconstruction*,
not a propagated trace, and the view says so. This is what makes "where did it
stall" answerable in a cluster with no tracing instrumentation.

**Level 2 — L7 request trace (when L7 visibility exists).** If Hubble reports L7
flows for the edge, the trace view shows method, path, status, protocol and
latency per hop, and can aggregate by status class. When it does not, the view
shows one dim line: "L7 not visible for this edge — enable L7 policy/visibility
for namespace X (records a cluster change)".

Explicitly out of scope: OTLP/Jaeger ingestion and trace-context propagation.
That needs app instrumentation or a collector, which is a different product.

## 6. Where it plugs into jumper

| Piece | Where | Notes |
|---|---|---|
| Cilium flows backend | `internal/backend/cilium/` — kinds `flow`, `endpoint`, `identity`; implements `Backend` + `Watcher` | `Watch` streams `Event`s; `List` answers windowed queries |
| Conntrack fallback | `internal/backend/netflow/` (poll-based, SSH) | no `Watcher`; refresh interval from config |
| Topology builder | `internal/topology/` (pure) | graph, aggregation, layout, trace stitching |
| Views | `internal/tui/views/netmap.go`, `trace.go`, `edge.go` | existing `View`/`Factory` contract |
| Commands | `internal/tui/commands_netmap.go` — `:netmap`, `:flows`, `:trace <pod>`, `:denied` | command registry from task 014 |
| Config | new `cilium: {agent_socket, relay, l7: auto|on|off}`, `netmap: {window, max_edges, sample}` | `internal/config` strict decode |
| Lab | `lab/` gains a Cilium-installed k3s profile and a `fault net-partition` for tests | lab track owns it |

Nothing in the frozen contract (`BUILD-PLAN` §4–§7, §11) changes: new kinds,
new backend, new view and new config keys are all additive, which is exactly how
M8–M11 were added.

## 7. Tests that do not need a cluster

This is the part that keeps the feature trustworthy:

1. **Golden flow fixtures.** `testdata/cilium/` holds captured, sanitised
   NDJSON flow records (agent monitor output and Hubble `GetFlows` output), the
   same way `testdata/linux/` holds journal/ps/ss captures. Parsers are tested
   against them; a parser never sees a shape we did not capture.
2. **Deterministic graphs.** `Graph` built from fixtures → golden text render at
   fixed sizes (100×30, 40×15), including the ASCII glyph set.
3. **Layout invariants.** Node ordering stable across shuffles of input order;
   no overlapping boxes; every edge attached to a drawn node; ≤ N crossings for a
   fixed fixture (property test, 500 random graphs).
4. **Trace stitching.** Synthetic flow sets with a known causal chain → expected
   path; missing-hop and simultaneous-flow cases.
5. **Replay.** `:netmap --replay file.ndjson` renders without any cluster — the
   demo mode for screenshots and for debugging a reported issue offline.
6. **Live lab test** (`//go:build lab`, `TestLab…`): against the lab's Cilium
   profile, assert we observe a known client→server flow (a request the lab test
   itself generates), that a denied flow appears as denied, and that the agent
   stream survives an agent restart.

## 8. Performance and scale budget

| Metric | Target |
|---|---|
| Flow ingest | 5,000 events/s sustained, bounded memory (ring buffer, `max_edges` 50k) |
| Frame render | < 8 ms at 100×30 with 400 nodes / 2,000 edges |
| First paint | < 1.5 s after `:netmap` (parallel: API list + agent snapshot) |
| Memory | < 150 MB at 50k edges |
| Drops | The view shows the agent's lost-event counter when > 0 (never silently) |

## 9. Task breakdown (proposed, 145–153)

Written as task files only after the decisions in §10 are made; ≤127 is the
auto-started range, so these will never be picked up by tonight's scheduler run.

| Task | Goal | Depends on |
|---|---|---|
| 145 | Cilium agent transport: SPDY exec into the agent pod, JSON snapshot of endpoints/ipcache, golden fixtures | 105 |
| 146 | `flow` kind + Hubble/agent stream as `Watcher` events, ring-buffer store, replay file | 145 |
| 147 | `netflow` fallback backend: conntrack/ss over SSH, poll refresh | 105 |
| 148 | `internal/topology`: graph model, aggregation windows, deterministic layout | 146 |
| 149 | `netmap` view: layered DAG, glyphs, sparkline edges, focus mode, 8 ms budget | 148, 142 |
| 150 | Edge detail + `:denied` view: verdicts, drop reasons, policy attribution | 149 |
| 151 | Trace view: L1 flow-trace stitching, L2 L7 when visible, timeline scrub | 150 |
| 152 | Policy overlay: CNP/NetworkPolicy intended graph, disagreement highlighting | 148 |
| 153 | Lab profile: Cilium k3s + `fault net-partition`, `TestLab` end-to-end | 149 |

Estimate: tasks 145–149 are the core (roughly 5 focused days at the observed
pace of this project's tasks); 150–153 add 4 more. So ~9 focused days for the
full feature, or ~5 days for a useful v1 that stops after 150.

## 10. Decisions needed from you

1. **Hubble gRPC: vendor the protos or stay agent-only for v1?**
   Agent-only means per-node exec streams (N streams, one per node) but zero new
   dependencies. The relay means one stream for everything but adds gRPC +
   protobuf modules. Recommendation: **agent-only v1**, add relay once the graph
   is proven.
2. **L7 visibility:** may jumper enable L7 policy/visibility on a namespace (a
   recorded, confirmed cluster write, per rule 9), or must L7 stay something you
   enable yourself? Recommendation: yes, but only as an explicit, logged action
   with the policy text shown first.
3. **Port-forward:** creating one per session (and tearing it down on exit) is a
   real cluster-write-ish action. Allowed? Recommendation: yes, ephemeral only.
4. **Scope of "external":** include internet traffic (CIDR nodes, very noisy
   but useful) or only in-cluster plus named externals? Recommendation: CIDR
   nodes off by default, `x` to show.
5. **Non-Cilium clusters:** is the conntrack/ss fallback worth building now, or
   is "Cilium-only with a clear error" acceptable? Recommendation: build it, but
   after L1 tracing works — it is the safety net for your homelab.

## 11. Risks

- **Format drift.** `cilium-dbg` JSON is not a stable public API; it changes
  between Cilium minors. Mitigation: pin the version in a lab test, parse
  defensively, and fail with the captured line instead of a panic (rule 11).
- **False confidence.** A reconstructed flow trace is not a distributed trace.
  The UI must never look like it knows causality it only inferred; every L1 path
  carries a one-line caveat and the hop gaps.
- **Load.** Watching flows on a big cluster means real bytes through the SSH/exec
  channel. Sampling and aggregation are not optional.
- **Binary size.** client-go already pushes the binary near the 80 MB check in
  task 105; adding gRPC makes that worse. Another argument for agent-only v1.
