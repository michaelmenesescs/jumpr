# Task 037 — Grouping identical outputs, summary headline, unified diff

- **Milestone:** M2
- **Depends on:** 005
- **Parallel group:** M2-A (with 026, 027, 028)
- **Status:** todo

## Goal
"1,481 identical · 19 differ · 4 errors" — the core of every fleet view.

## Context
BUILD-PLAN §4.6 (`group`). SPEC §5 W2, §13 (fan-out view). Mockup fan-out screen.

## Files
- create `internal/group/group.go`, `internal/group/diff.go`, `internal/group/group_test.go`,
  `internal/group/diff_test.go`

## Requirements
1. Types and functions verbatim from BUILD-PLAN §4.6. This package imports `resource`, `errs` only (it does not
   import `fanout`; callers pass host, output and rc).
2. Key = hex SHA-256 of `normalize(output)` + `"\x00"` + rc. Default normalization when `normalize == nil`: trim
   trailing whitespace on each line and trailing blank lines.
3. `Summary()`: groups sorted by host count descending, ties by `Key`; hosts inside a group sorted; `Errors`
   grouped by `errs.KindOf(err)`; `Done = hosts in groups + hosts in errors`.
4. `Headline()` per the doc comment. Examples that must hold:
   - 24 hosts, all same → `24 identical`
   - 1,512 total: majority group 1,489, other groups 19 hosts in total, 4 errors →
     `1,489 identical · 19 differ · 4 errors` (identical = size of the majority group; differ = hosts in all
     other groups)
   - in progress → `812/1,512 · 800 identical · 12 differ`
   - nothing yet → `0/1,512`
5. `func (s Summary) Majority() (Group, bool)`; `func (s Summary) Differing() []Group`.
6. Built-in normalizers: `func StripTimestamps(s string) string` (ISO-8601 and `Mon DD HH:MM:SS` → `<ts>`),
   `func StripPIDs(s string) string` (`pid=123`/`[123]` → `pid=<n>`/`[<n>]`), `func Chain(fs ...func(string)
   string) func(string) string`.
7. `UnifiedDiff` with `difflib.UnifiedDiff{A, B: difflib.SplitLines(...), FromFile, ToFile, Context: 3}`.

## Out of scope
Rendering (048). Colouring diffs (048).

## Acceptance
```
go test ./internal/group/ -v
# TestGroupingBasic, TestHeadlineTable (the four examples above, exact strings), TestErrorsByKind,
# TestNormalizeDefault (trailing spaces do not split groups), TestStripTimestamps, TestStripPIDs,
# TestUnifiedDiffEqualIsEmpty, TestUnifiedDiffShape, TestDeterministicOrder (shuffle input 20 times)
```

## Pitfalls
- Thousands separators: do not import `render` (this package stays a leaf); write a 5-line local helper.
- Map iteration order is random; sort before returning anything.
