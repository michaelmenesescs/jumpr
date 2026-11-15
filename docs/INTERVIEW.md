# Requirements interview — 2026-11-12

This is the raw record of what Michael asked for and answered. SPEC.md is built from it. If the two disagree, this
file reflects what the user actually said; update the SPEC to match.

## Original ask

> A new TUI to improve on tools like k9s and isd-tui, and implement other tools alongside it like Grafana and
> Prometheus integrations, Chef and SaltStack integration and deep Linux integration. Basically comprehensive
> debugging for infrastructure that is tool agnostic and plugs into whatever. Use Go since it's lightweight and has
> TUI support. **Main requirement: it must work in enterprise environments where I can't download anything on the
> server nodes. It needs to run fully client side and be able to query everything without downloading dependencies.**

Process requested: interview → brainstorm → spec → build.

## Answers

| Topic | Answer |
|---|---|
| Node access | SSH through bastion/jump hosts (ProxyJump), keys/agent |
| v1 integrations | **All**: deep Linux over SSH, Kubernetes, Prometheus/Grafana, Chef/Salt |
| Write capability | **Full read/write like k9s** (guards added in spec as a recommendation, not a restriction) |
| Fleet size per session | Hundreds to low thousands (fan-out) |
| Inventory | Multiple sources: ~/.ssh/config, Salt/Chef, k8s nodes, Prometheus targets, static file |
| Privilege | Usually passwordless sudo |
| Client platform | **Only macOS laptop** |
| Linux mix | Modern systemd-only (RHEL 8+ / Ubuntu 20.04+) |
| Extensibility | Declarative YAML plugins + Go interfaces for first-class backends |
| #1 pain | "Tracking down alerts through multiple applications with no context. Need to have AI run commands and I have no visibility or long-term personal learning context." |
| Central logs | Elasticsearch remote clusters (available via MCP today); use the ES API directly in the app |
| Ownership | **For use at work; keep it neutral, no employer specifics** |
| AI integration | "None for now" (design for it later) |
| AI policy (future) | Agent can read freely; every write needs approval in the TUI |
| Long-term memory store | **Markdown files only, plain-text first** |
| Salt/Chef access | Via SSH to Salt master / Chef workstation using existing CLIs (`salt --out=json`, `knife -F json`) |
| Prom/Grafana/ES auth | All behind SSO / corp proxy. Follow-up: "Custom proxy, just build it out as generic as possible and I will customize it for my own enterprise environment" |
| Kubernetes | Works via kubeconfig |
| Grafana | Query it directly if possible |
| Overall | "Just need a tool that I can rely on and is built towards what I need in a debugging tool" (reliability > breadth) |

## Decisions the assistant made (user did not object; revisit if wrong)

- Working name `jumpr`.
- Knowledge root default `~/jumpr/`.
- Elasticsearch is read-only in v1.
- Generic auth = connection profiles + an "auth helper" exec protocol (JSON headers/cookies on stdout).
- Blast-radius safety: canary + rolling batches; typed confirmation for more than 20 targets.

## Follow-up — 2026-11-12 (after reviewing the clickable mockup)

User approved moving forward ("ok let's try it"). New and sharpened requirements, in the user's words where possible:

- "I want it to feel like **k9s + isd-tui + tools for Chef and Salt and general linux tools** with **no server
  dependencies required**. That is the selling point."
- "**It should work like tmux.**" → treat as a first-class requirement: windows, split panes, a prefix key, and
  sessions that survive detach/reattach (client-side only; still nothing on the nodes).
- "I'm basically **writing a shell tailored for an SRE**. It should be able to **plug into whatever**." One central
  interface the user controls, learns, and operates any infra through, instead of relying on multiple tools.
- "I feel like I can **leverage Salt a lot more**." → Salt gets deeper integration than the v0.1 spec (beyond
  drift/grains), and should come earlier in the build order.
- The tools named (k9s, isd-tui, tmux, Salt, Chef CLIs) are tools the user likes; borrow their ergonomics.
- Process: a stronger model scopes the end-to-end build; a fast agent (Hermes running DeepSeek V4.1 Flash) does the
  implementation; a **mock k8s cluster and dedicated lab infra** are set up locally so the tool can be tested and
  refined against something real.

## Follow-up — 2026-11-14 (after consolidating on the laptop and renaming to jumpr)

New direction, in the user's words where possible:

- "I want jumpr to be my **cybersecurity tool** and also **agentic debugging tool** which integrate k8s and other
  services to have **one system**. Instead of jumping to different windows you should see everything via jumpr."
- "+ the **application level request flow** for applications when connected to k8s clusters or linux clusters,
  **systemd tree**, let's think outside of the box."
- "Can we see the **systemctl units in the tree**." → the systemd tree is the first visible deliverable of the graph
  milestone (task 163).

Answers to the four decisions the assistant asked for:

| Question | Answer |
|---|---|
| Zero footprint absolute, or eBPF allowed? | **eBPF opt-in.** Only a `bpftrace` already on the node, never installed, off by default, consent per host. |
| Defensive only, or active checks? | **Defensive only.** Read-only posture and hunting from evidence jumpr already reads. No scanning, no exploitation. |
| Agent model | **Local by default** (OpenAI-compatible endpoint on the laptop: LM Studio / llama.cpp). Remote models only by explicit opt-in. |
| Neutral repo | **Keep neutral.** `example.internal`, no employer names, hosts or conventions. |

Unifying idea the user accepted: one **live system graph** (request → LB → ingress → service → pod/process → unit →
cgroup → sockets → upstreams → node → identity). Debugging (request flow, systemd tree) and security (exposure,
identity, blast radius, hunting) are lenses on the same graph; the agent and `jumpr mcp` walk it with the same
read-only tools.
