# Task 074 — State returns: parse, normalize, group by identical diff (pure)

- **Milestone:** M4
- **Depends on:** 037, 066
- **Parallel group:** M4-A (with 070)
- **Status:** done

## Goal
Turn a `state.apply` return (real or `test=True`) into rows and into a stable text form so that hosts with the
same outcome group together.

## Context
SPEC §5 W4, §8.6 (states: "grouped by identical diff"). Golden files: `testdata/salt/state-*.json`.

## Files
- create `internal/backend/salt/state.go`, `internal/backend/salt/state_test.go`

## Requirements
1. A minion's state return is one of:
   - an object keyed by `module_|-id_|-name_|-function`, each value like
     `{"result": true|false|null, "changes": {...}, "comment": "…", "name": "/etc/nginx/nginx.conf",
     "__sls__": "nginx", "__run_num__": 3, "__id__": "nginx-config", "duration": 12.3, "start_time": "13:55:02.1"}`
   - a list of strings (render/compile errors)
   - a plain string (e.g. `ERROR: …`) or the no-return forms.
2. `type StateRow struct { Module, ID, Name, Function, SLS string; RunNum int; Result *bool; Changed bool;
   Changes json.RawMessage; Comment string; Duration float64 }` and
   `type StateRun struct { Rows []StateRow; Errors []string; Test bool }`.
   `func ParseStateReturn(raw json.RawMessage) (StateRun, error)` — rows sorted by `RunNum`; key split on `_|-`
   into exactly 4 parts (fall back to the `__id__`/`name` fields when the key is odd); `Changed` = `changes` is a
   non-empty object.
3. `func (r StateRun) Counts() (ok, changed, failed, pending int)` — with `test=True`, `result == null` means
   "would change" → `pending`; `result == false` → `failed`; `result == true` with changes → `changed`; else `ok`.
4. `func (r StateRun) Status() resource.Status` — errors or failed → Crit; pending or changed → Warn; else OK.
5. `func (r StateRun) Normalized() string` — the grouping text: for each row that is failed, pending or changed,
   in `RunNum` order:
   ```
   <result-word> <sls> :: <id> (<module>.<function>) <name>
     <comment, first 3 lines>
     <changes rendered>
   ```
   `result-word` = `FAILED|WOULD-CHANGE|CHANGED`. Changes rendering: if the object has a string field `diff`,
   use it verbatim; otherwise compact JSON with sorted keys. Clean runs normalize to `clean (<n> states)`.
   Errors normalize to `ERROR` + the lines. Durations, start times, PIDs and run numbers never appear.
6. `func (r StateRun) Summary() string` — `42 ok · 2 would change · 1 failed`.
7. `func RenderRows(r StateRun) [][]string` for a table: `sls`, `id`, `function`, `result`, `comment` (first
   line).

## Out of scope
Running states (075). The state tree (092).

## Acceptance
```
go test ./internal/backend/salt/ -run 'State' -v
# TestParseDict (inline sample with 3 states: ok, null+diff, false), TestParseErrorList, TestParseString,
# TestCountsTestMode, TestNormalizedStableAcrossDurations (same run with different duration/start_time → equal),
# TestNormalizedDiffVerbatim, TestCleanRun, TestGroupingTwoHostsSameDiff (group.Grouper → one group),
# plus golden: testdata/salt/state-test-clean.json and state-test-changes.json when present
```

## Pitfalls
- `result` is tri-state: use `*bool`, and remember JSON `null` → nil.
- State IDs and names may themselves contain `_|-`-like text; split with a limit and prefer the `__id__` field.
- Sort map keys before rendering any JSON; Go map order would break grouping.
