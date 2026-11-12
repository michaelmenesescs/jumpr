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

- Working name `sitrep`.
- Knowledge root default `~/sitrep/`.
- Elasticsearch is read-only in v1.
- Generic auth = connection profiles + an "auth helper" exec protocol (JSON headers/cookies on stdout).
- Blast-radius safety: canary + rolling batches; typed confirmation for more than 20 targets.
