# Task 005 — Resource model types

- **Milestone:** M0
- **Depends on:** 004
- **Parallel group:** — (serial)
- **Status:** done

## Goal
The remaining types of `internal/resource` from BUILD-PLAN §4.2, plus small helpers for reading fields.

## Context
BUILD-PLAN §4.2.

## Files
- create `internal/resource/resource.go` (HostID, KindName, Ref, Status, Resource, Column*, Align, Scope, KindSpec,
  Query, Link)
- create `internal/resource/fields.go`
- create `internal/resource/resource_test.go`

## Requirements
1. Types verbatim from BUILD-PLAN §4.2.
2. `Ref.String()` as documented. Add `func ParseRef(s string) (Ref, error)` (inverse; exactly 5 `/`-separated
   parts).
3. `fields.go`: `func (r Resource) Str(id string) string`, `Int(id) (int64, bool)`, `Float(id) (float64, bool)`,
   `Time(id) (time.Time, bool)`. `Str` formats any allowed type deterministically (`float64` with
   `strconv.FormatFloat(v, 'f', -1, 64)`, `nil` or missing → `""`).
4. `func Compare(a, b any) int` ordering two field values of the same type (numbers numerically, strings
   lexically, times chronologically, `nil` last, mismatched types by `Str`). Used for sorting tables.
5. `func NormalizeHostID(s string) HostID` lower-cases and trims a trailing dot.
6. `func (k KindSpec) Column(id string) (Column, bool)`.

## Out of scope
Rendering (task 008). Registry (006).

## Acceptance
```
go test ./internal/resource/ -v
go vet ./internal/resource/
```

## Pitfalls
- `Resource.Fields` values use `int64` and `float64` only — never `int` or `float32`. The helpers should still
  accept `int` defensively.
- No methods with pointer receivers on `Resource`; it is passed by value everywhere.
