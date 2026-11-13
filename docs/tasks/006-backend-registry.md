# Task 006 — Backend interface and registry

- **Milestone:** M0
- **Depends on:** 005
- **Parallel group:** — (serial)
- **Status:** done

## Goal
`internal/backend` exactly as in BUILD-PLAN §4.4.

## Context
BUILD-PLAN §4.4.

## Files
- create `internal/backend/backend.go` (interfaces and types)
- create `internal/backend/registry.go`
- create `internal/backend/registry_test.go`

## Requirements
1. Types and interfaces verbatim.
2. `Registry.Register`: rejects (`errs.Config`) an empty name, a duplicate backend name, and any kind name or
   alias already taken by a kind name or alias (case-insensitive).
3. `Lookup` is case-insensitive and matches name or alias. `Kinds()` is sorted by `Name`. `Backends()` keeps
   registration order. All methods safe for concurrent use (`sync.RWMutex`).
4. `func (r *Registry) Get(name string) (Backend, bool)` by backend name.
5. `func (s HealthState) String() string` → `unconfigured|ok|degraded|down`.

## Out of scope
Any real backend. Health polling (task 015).

## Acceptance
```
go test -race ./internal/backend/ -v      # uses a tiny stub Backend defined in the test file
```

## Pitfalls
- Do not add `Actions` to `Backend`; writes go through `action.Planner` (M3).
- Do not use `init()` self-registration.
