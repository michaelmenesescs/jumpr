# jumpr — MVP re-sequence: pods + VMs, personal use, with AI

Status: **proposal, not committed to the task index.** Nothing here is executed yet.
Written 2026-11-12 against `docs/SPEC.md` v0.2, `docs/BUILD-PLAN.md`, `docs/tasks/README.md` (001–014 done).

---

## Goal

Ship a **personal** daily-driver that troubleshoots **both pods and VMs through one grammar**, and make the AI layer a
real milestone instead of a §17 footnote. Same code base, re-sequenced — no fork, no borrowed code.

## What the current plan gets right (do not touch)

- The contracts (BUILD-PLAN §4, §5, §6.2, §7, §11) already model exactly this: `Backend` + `Query` + `Resource` +
  `Watcher` + `LinkProvider`, an action bus every write goes through, investigations as Markdown.
- Zero-footprint-on-nodes and client-side-only are the right constraints for VM troubleshooting without an agent.
- 127 tasks with dependency and parallel groups is a real plan, and M0 is 14/16 done with green tests.

## The three mismatches with the new direction

| # | Now | What you asked for | Cost of ignoring it |
|---|---|---|---|
| 1 | Kubernetes is **M9, tasks 105–112** — after Salt (M4, 11 tasks), session server (M5), systemd depth (M6), Salt depth (M7) | Pods are **half the MVP** | You build 40+ tasks of Salt/systemd before you can look at a pod |
| 2 | AI is a **non-goal for v1** (§3) and "designed for" (§17): MCP server later | AI **integrated** | The bus and evidence model get built without the consumer that justifies them; retrofit later is a rewrite |
| 3 | Milestone order mirrors **your work stack** (Salt, Chef, Grafana, Elasticsearch first) | **Personal** use: your own k8s + your own VMs | Weeks of work on integrations you will not run at home |

Note the shape of mismatch 1: the ordering was written for the employer's stack, where Salt is the daily driver. That is
also why M4 is labelled "first fully usable release (v0.5)" — for personal use that label is wrong. Nothing about the
code changes; only the order of adoption.

---

## Proposed sequence

```
M0 skeleton           001–016   (015, 016 left)
M1 mux in-process     017–025
M2 SSH + Linux core   026–053        ← VMs, deepest backend
M3 actions + invest.  054–065        ← the bus + the record; AI's substrate
M9 Kubernetes         105–112        ← moved up from 9th
M13 AI (new)          128–134        ← new milestone, after the data exists
M8 HTTP + Prom/AM     098–104        ← alerts → investigation (W1) complete
--- v0.1 gate here ---
M4 Salt core           66–076
M5 session server      77–083
M6 systemd depth       84–091
M7 Salt depth          92–097
M10 Chef + plugins    113–118
M11 Grafana + ES      119–122
M12 hardening         123–127
```

Why this order and not "pods first":

- **Momentum.** 14 tasks and 20 commits are already through the spine; the frozen contracts assume it. Re-ordering work
  already done to chase a faster demo costs more than it saves.
- **M2 before M9 is not a detour.** It proves the backend seam against something that is *not* an API client, which is
  the exact risk in "one interface for any infrastructure". k8s is the easy backend (typed client, informers); SSH is the
  hard one (parsers, bastions, timeouts). Learn the seam on the hard one and k8s becomes 8 tasks.
- **M3 before M9 keeps the safety property.** K8s actions (task 109) are defined as bus actions; building the pod views
  before the bus would either duplicate write paths or introduce one that bypasses policy.
- **AI after M9, not after M2.** An assistant with no pods to see is a toy. After 105–108 it can reason about a real
  incident: this alert, these hosts, those pods.

### The alternative, for completeness

`M0 → M9-reads (105–108, 111) → M1 → M2 → M3 → M9-actions (109, 110)`. Pros: a k9s-shaped tool exists ~10 tasks from
now, which is motivating and validates the backend seam cheaply. Cons: it requires relaxing task 105's declared
dependency on 065 ("M3 exit") in `docs/tasks/README.md`, and it front-loads the *easy* backend, so the seam is proven
by the least demanding case. **Recommendation: spine first.** Your call, though — if motivation matters more than
elegance this week, take the alternative and note the dependency relaxation in HANDOFF.

---

## v0.1 gate — "personal daily driver"

Definition of done, stated as behaviour on your own infrastructure:

1. `jumpr` opens on a kube context; `:pods`, `:deploy`, `:nodes`, `:events` list and stream.
2. Enter on a pod → describe, YAML, logs (follow, filter, time window).
3. `:ssh web-01` opens a pane; `:hosts` lists the fleet; `:units`, `:processes`, `:sockets`, `:journal` work.
4. Enter on a pod **in a pane** keeps the pod table visible next to it — the mux thesis, not a tab bar.
5. Pivot: pod → node → the node's host overview, and back.
6. Any write (restart, delete, kill, systemctl verb) goes through the bus: plan → confirm → execute, recorded.
7. `:investigate` records the window + targets and writes a Markdown file.
8. AI: select rows, ask "why is this failing", get a suggestion that is either an explanation or a `:` command you can
   run with one keystroke — never an automatic write.

Tasks: **001–065 (spine) + 105–111 (k8s, minus 112 CRDs/edit) + 128–131 (AI read-only)**. 14 done, **~66 remaining**.
That is the honest number; it is more than a weekend and less than the 127 on the board.

---

## New milestone M13 — AI (tasks 128–134)

Design constraints that come from this repo, not from AI fashion:

- **No new Go dependencies.** `net/http` + `encoding/json` against an OpenAI-compatible endpoint is enough. Your local
  Qwen3.6-35B-A3B on `:8084` is exactly that shape. The recipe is `ai.endpoint` in config; nothing in §10 changes.
- **The model speaks the command bar.** Its output is a `:kind selector` line, an explanation, or a shell command for
  `:run`. It does *not* get a private tool-calling protocol in v0.1: local 35B models are unreliable function-callers,
  and the command bar is already the validated surface (§1: "the command bar is the primary interface"). Validation is
  deterministic: parse the suggestion with `resource.ParseSelector` + `Commands.Lookup`, reject what does not parse.
- **Evidence in, nothing out.** The prompt is an evidence pack built from what is on screen: the query, the selected
  rows, the visible log window, the bus's last N records. Everything passes `internal/redact` (task 054) first.
- **Reads are free, writes are not.** v0.1 exposes zero write tools. Task 132 (proposals as bus actions) is explicitly
  after v0.1, and even then a proposal is a *pending* action in the TUI, per the interview's policy.

| ID | Task | Files | Acceptance |
|---|---|---|---|
| 128 | AI client: OpenAI-compatible chat completions, streaming, timeouts, bounded prompt | `internal/ai/client.go`, `internal/config` (`ai.*` keys) | `httptest` server returns SSE chunks; client emits ordered deltas; context cancel mid-stream returns `errs.KindTimeout`; a 90 s read timeout is enforced |
| 129 | Evidence pack builder | `internal/ai/context.go` | given a query + rows + lines + bus records, output is stable, bounded by `ai.max_context_bytes`, secrets redacted, and contains no `Resource` fields not in the allowlist |
| 130 | Suggestion parser + validator | `internal/ai/suggest.go` | `:` lines extracted from free text; unparseable selectors rejected with `errs.Config`; a suggestion naming an unknown kind is dropped, not offered |
| 131 | AI pane | `internal/tui/views/ai.go` | streaming through `tui.NextBatch`; a suggestion line is offered as a runnable command (`enter` = run, `c` = copy); `esc` returns focus; 80 ms/frame budget holds with a 4,000-line transcript |
| 132 | Proposals as bus actions (post-v0.1) | `internal/ai/tools.go`, `internal/bus` | a proposal creates a `pending` action with `actor=ai`; nothing executes without a confirmation dialog; policy denials surface in the TUI |
| 133 | `jumpr doctor ai` | `internal/app`, `cmd/jumpr` | reports endpoint, model list, round-trip latency, and a one-line failure reason |
| 134 | Recorded-fixture tests | `internal/ai/testdata/*.json`, `//go:build ai` live test | `go test ./...` needs no network; the `ai`-tagged test hits the local endpoint when it is up and skips cleanly when it is not |

**Contract amendments needed before 128** (two, both small, both must be deliberate rather than discovered mid-task):

1. `docs/BUILD-PLAN.md` §11 — add an `ai:` config section (`endpoint`, `model`, `timeout`, `max_context_bytes`,
   `enabled: false` by default).
2. `docs/BUILD-PLAN.md` §4.1 — add one error kind, `AI`, so failures do not masquerade as `Internal`. Alternatives:
   reuse `Config`/`Internal` (cheaper, worse diagnostics). Recommend the new kind.

Do these as a single `docs:` commit before 128, so the freeze rule keeps meaning something.

---

## Rename: jumpr → jumpr

You named the tool jumpr; the repo, module, binary, config dir, cache dir, crash dir and env prefix all say jumpr.
Doing it now is mechanical and cheap; doing it after 60 more tasks is a bigger mechanical change and a docs rewrite.

Suggested single task, `000 — rename`, before 015 lands (015/016 add more user-facing strings):

- `go.mod`: `module jumpr` → `module github.com/<you>/jumpr`, then rewrite every import path.
- `cmd/jumpr` → `cmd/jumpr`; `Makefile` targets and `build` output; `.gitignore`.
- Config/cache/crash paths: `~/.config/jumpr` → `~/.config/jumpr`, `~/.cache/jumpr` → `~/.cache/jumpr`,
  env prefix `JUMPR_` → `JUMPR_`.
- Docs: SPEC, BUILD-PLAN, AGENTS.md, HANDOFF, all 127 task files (mostly the word "jumpr" in prose and paths).
- Keep the repo **directory** name as-is until you decide where it lives; a directory is not an API.

Two things the rename decision forces, both yours:

- **Repo home.** HANDOFF says "no remote yet — ask before creating one (work project; may need a work Git host)". Personal
  use changes that. A private personal GitHub repo is now the sane default; the neutral-repo rule (no employer names)
  stays, because it costs nothing and the tool may still be used at work.
- **The `@@` and `@@` binaries** belong to the lab track; renaming them touches `cmd/@@/` and
  `lab/`, which the implementation rules say not to edit. That is a lab-track request, or a rename that leaves them
  alone for now (they are dev-only). Recommend: rename `jumpr` only, leave the lab binaries until the lab track moves.

---

## Cybersecurity direction — where it actually plugs in (not MVP)

Do not add scanning/detection to v0.1. But the current design already contains the enforcement points a security tool
needs, which is worth knowing before they get built:

- **Policy (task 056)** — deny rules, protected targets, read-only contexts. That is a security control, already planned.
- **Audit (task 058)** — every bus action recorded to Markdown, with `actor`. That is the evidence trail.
- **Redaction (task 054)** — the reason an incident record is shareable.
- **Later, additive:** a `finding` kind + a source for scanner output (Trivy/OpenSCAP/lynis), and a `posture` view. That
  is one backend and one view — no core changes, which is the payoff of the `Resource` model.

Keep it a **non-goal in SPEC §3** for v0.1 with this sentence, so the intent is on record: "Security tooling (scanning,
detection, posture) is deferred; the policy engine, audit record and redaction are built to serve it later."

---

## Risks

| Risk | Mitigation |
|---|---|
| **Dilution** — the survey's real finding: every tool that acts is single-domain, and breadth is what killed nothing but dulled many | Ship the 66-task v0.1 and dogfood it for two weeks before touching M4. If pods and VMs are not enough, nothing is |
| AI becomes the project | 128–131 are read-only and 7 tasks. No agent framework, no embeddings, no vector store, no MCP until the suggest loop earns it |
| Resequencing desyncs the agent workflow | M9's move is an edit to `docs/tasks/README.md` + HANDOFF in one commit; task 105's dependency on 065 stays satisfied (M3 precedes it either way) |
| Contract drift via AI config | One amendment commit for §4.1 + §11 before 128, exactly as the freeze rule intends |
| Local model quality | Suggestion-only + deterministic validation. No test depends on model output; fixtures are recorded |
| Salt deferred breaks the "v0.5 fully usable" claim | Redefine: v0.1 = personal driver (pods + VMs), v0.5 = the work-stack release (Salt/Chef/Grafana/ES). Say so in SPEC §19 |

## Open questions (need your call, none of them block 015/016)

1. **Rename now (task 000) or after v0.1?** Recommend now.
2. **Spine-first (M2 before M9) or pods-first (relax 105's dependency)?** Recommend spine-first.
3. **Are exec and port-forward (task 110) in v0.1?** Recommend yes — debugging a pod without a shell is half a tool.
4. **Alerts in v0.1 or v0.2?** Recommend v0.2: Prometheus/Alertmanager (M8) after AI, so W1 lands whole.
5. **AI: suggest-only (recommended) or tool-calling from day one?** Suggest-only is the version a 35B local model can
   actually do well.

## Validation for this plan itself

- `make check` stays green after the doc-only commits; the task index still has 127 rows plus 7 new AI rows.
- After the M9 move, `docs/tasks/README.md` dependency check still passes: 105 needs 065 (M3 exit) — satisfied by the
  new order; 106→105; 110 needs 050 (M2) and 106; 109 needs 061 (M3) and 106. No cycle.
- v0.1 is "done" only when all eight behaviours in the gate above are demonstrated against your own cluster and one VM,
  not against the fake backend.
