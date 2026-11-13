# Task 070 — Targeting compiler: jumper selectors ↔ Salt targets (pure)

- **Milestone:** M4
- **Depends on:** 004, 066
- **Parallel group:** M4-A (with 074)
- **Status:** todo

## Goal
Turn a jumper selector or host list into the best Salt target, and parse a native Salt target typed by the user.

## Context
SPEC §8.6 (targeting compiler row). BUILD-PLAN §11 (`salt.list_chunk`).

## Files
- create `internal/backend/salt/target.go`, `internal/backend/salt/target_test.go`

## Requirements
1. `type Compiled struct { Targets []Target; Native bool; Minions []string; Explain string }` — one or more
   targets to submit (several only for chunked lists); `Native` = compiled to grains/compound; `Minions` =
   expected minion IDs when known; `Explain` = one line for the plan dialog, e.g.
   `compound: G@role:web and G@dc:ewr` or `list of 312 minions in 2 chunks (selector uses non-grain key "prom.job")`.
2. `func CompileSelector(sel resource.Selector, grainKeys map[string]bool) (Target, bool)` — native compile
   succeeds only when the selector has **no** `@sets` and every term's key (after stripping a `salt.` prefix) is
   in `grainKeys`, or is `host`. Mapping, joined with ` and `:
   - `k=v` → `G@k:v`; `k!=v` → `not G@k:v`; `k` → `G@k:*`; `!k` → `not G@k:*`
   - `k~re` → `P@k:re`; `k!~re` → `not P@k:re`
   - `host=v` → `L@v`; `host~re` → `E@re`; `host!~re` → `not E@re`
   A value containing a space, a quote, or any of `()` makes native compile fail (fall back to list). A single
   positive term still returns type `compound` (uniform handling). Compound expressions never start with `not`:
   prefix `* and ` when the first term is negated.
3. `func CompileHosts(minions []string, chunk int) []Target` — sorted, de-duplicated `list` targets of at most
   `chunk` IDs each (`Expr` comma-joined). Reject IDs containing a comma or whitespace.
4. `func Compile(sel resource.Selector, resolved []string, grainKeys map[string]bool, chunk int) Compiled` —
   native when possible **and** `len(resolved) > 20` (small sets are clearer as lists); else chunked list.
   `Minions = resolved` in both cases (the client-side resolution is the expectation; a mismatch with Salt's own
   match is reported by the job engine as extra/missing minions).
5. `func ParseNative(s string) (Target, error)` — accepts what a user types after `:salt`:
   `-C '<expr>'`, `-G 'k:v'`, `-E '<re>'`, `-L 'a,b'`, `-N <group>`, `-I 'k:v'` (pillar → compound `I@k:v`), or a
   bare glob. Quotes are optional. Unknown flag → `errs.Config`.
6. `func (t Target) String() string` — CLI form for display: `-C 'G@role:web and G@dc:ewr'`.
7. `func ParsePreview(out []byte) ([]string, error)` — output of `--preview-target`: a JSON list, or text lines
   `- web-01`; sorted.

## Out of scope
Running `--preview-target` (076 wires `:salt -C …` → preview → target set). Nodegroup definitions (097).

## Acceptance
```
go test ./internal/backend/salt/ -run 'Compile|ParseNative|ParsePreview|TargetString' -v
# CompileSelector table (exact strings):
#   role=web dc~ewr|lax !maint        → G@role:web and P@dc:ewr|lax and not G@maint:*
#   !maint                            → * and not G@maint:*
#   salt.role=web host~^web-0         → G@role:web and E@^web-0
#   prom.job=node                     → not native
#   role=web @canary                  → not native
#   name="a b"                        → not native
# CompileHosts chunking (450 ids, chunk 200 → 3 targets), Compile small set prefers list, ParseNative table
```

## Pitfalls
- `|` inside `P@dc:ewr|lax` is regex alternation and is safe inside a compound expression only because the whole
  expression is passed as one shell word by `shq.Join`; never split or re-join it with spaces.
- Grain matching in Salt is glob-based for `G@`; a value containing `*` or `?` typed by the user is a glob, which
  is fine and intended.
