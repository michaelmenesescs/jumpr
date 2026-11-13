# Task 004 — Selector and time-window parsers

- **Milestone:** M0
- **Depends on:** 002
- **Parallel group:** M0-A (with 003, 008, 009)
- **Status:** done

## Goal
`resource.Selector` and `resource.TimeWindow` with parsers, exactly as in BUILD-PLAN §4.2.

## Context
BUILD-PLAN §4.2 (Selector, TimeWindow). SPEC §9 (selectors), §13 (time).

## Files
- create `internal/resource/selector.go`, `internal/resource/selector_test.go`
- create `internal/resource/timewindow.go`, `internal/resource/timewindow_test.go`

## Requirements
Selector:
1. Tokenize on spaces; double quotes group (`name="a b"`), backslash escapes a quote inside quotes.
2. Token forms, checked in this order: `@name` → set; `!key` → `OpNotExists`; `key!=v` → `OpNotEq`;
   `key!~re` → `OpNotMatch`; `key=v` → `OpEq`; `key~re` → `OpMatch`; bare `key` → `OpExists`.
3. Keys match `^[A-Za-z_][A-Za-z0-9_.\-/]*$`. Regex values must compile (RE2); compile once at parse time and
   keep the compiled regexp in an unexported field.
4. `Matches`: all terms must hold. A missing key fails `OpEq`/`OpMatch`/`OpExists` and satisfies
   `OpNotEq`/`OpNotMatch`/`OpNotExists`. Regex is unanchored.
5. `String()` produces canonical text (terms in input order, then sets); values containing a space or a quote are
   quoted. `ParseSelector(s.String())` equals `s`.
6. Empty or all-space input → zero Selector, no error. Errors are `errs.Config` naming the bad token.

TimeWindow:
7. `ParseTimeWindow(s, now)`: relative `-<n><unit>` with units `s m h d w` → `From = now − d`, `ToNow = true`;
   range `A..B` where each side is `now`, a relative offset, RFC 3339, `2006-01-02 15:04`, `2006-01-02T15:04`,
   or `15:04` (today, in `now`'s location). `Label` is the trimmed input. `From` must be before `To`.
8. `Bounds(now)` returns `(From, now)` when `ToNow`, else `(From, To)`.
9. `func (w TimeWindow) IsZero() bool`, and `func (w TimeWindow) String() string` → `Label` or `-` when zero.

## Out of scope
Expanding `@sets` (task 046). The `jq:` filter prefix. Host-range syntax like `web-0[01-24]`.

## Acceptance
```
go test ./internal/resource/ -run 'Selector|TimeWindow' -v
# table tests must include: `role=web env=prod dc~ewr|lax !maint host~^db @saved @investigation`,
# quoted values, each operator against present/missing keys, bad regex, bad key, round-trip;
# windows: -30m, -2d, "2026-11-12 13:00..14:00", "13:00..now", "-2h..-1h", reversed range (error), garbage (error)
make lint
```

## Pitfalls
- `dc~ewr|lax` — the `|` is part of the regex, not a separator.
- Check `!=` and `!~` before `=` and `~`, or `a!=b` parses as key `a!`.
- Do not use `time.Now()`; use the `now` parameter.
