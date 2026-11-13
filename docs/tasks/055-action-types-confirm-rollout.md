# Task 055 — Action types, confirm level, rollout plan (pure)

- **Milestone:** M3
- **Depends on:** 005
- **Parallel group:** M3-A (with 054)
- **Status:** done

## Goal
The types of `internal/action` and the pure decisions of the bus: how much confirmation, and how the targets are
cut into canary and batches.

## Context
BUILD-PLAN §4.5. SPEC §11 (confirm scale, canary, rolling batches). Mockup plan dialog (canary 1, batch 10 %, max
50, typed count over 20).

## Files
- create `internal/action/action.go`
- create `internal/bus/types.go` (ConfirmLevel, Rollout, Proposal, Denial, Confirmation, Phase, Event, Summary,
  Run, Recorder, Bus interface)
- create `internal/bus/rollout.go`, `internal/bus/rollout_test.go`

## Requirements
1. `action.go` and `types.go`: verbatim from BUILD-PLAN §4.5.
2. `type RolloutConfig struct { Canary int; BatchPercent int; BatchMax int; FailureThresholdPercent float64;
   TypedConfirmOver int }` (mirrors `config.Rollout`).
3. `func ConfirmFor(targets int, anyProtected bool, risk action.Risk, c RolloutConfig) ConfirmLevel`:
   `targets > TypedConfirmOver || anyProtected` → `ConfirmTypedCount`; `targets >= 2` → `ConfirmList`; else
   `ConfirmYesNo`. `RiskDestructive` raises `ConfirmYesNo` to `ConfirmList` (shows the single target explicitly).
4. `func PlanRollout(targets int, c RolloutConfig) Rollout`:
   - 1 target: `Canary 0`, one batch of 1.
   - otherwise canary = `min(c.Canary, targets−1)` (at least 1 when `c.Canary >= 1`); remaining = targets −
     canary; batch size = `clamp(ceil(remaining × BatchPercent / 100), 1, BatchMax)`; batches =
     `ceil(remaining / size)`.
   - `FailureThreshold = FailureThresholdPercent / 100`.
5. `func Batches(steps []action.Step, r Rollout) (canary []action.Step, batches [][]action.Step)` — preserves
   order.
6. `func CheckConfirmation(p Proposal, c Confirmation) error` — `Denied != nil` → `errs.Policy`; wrong
   `ProposalID` → `errs.Internal`; `ConfirmTypedCount` requires `strings.TrimSpace(c.Typed)` to equal the decimal
   target count (no separators) else `errs.Policy` "typed count does not match".
7. `func ShouldStop(failedAfterCanary, doneAfterCanary, totalAfterCanary int, threshold float64) bool` — true when
   `failed / total > threshold` (so with the default 5 % and 20 hosts, the second failure stops it; with ≤ 20
   hosts and threshold 0.05 one failure is exactly 5 % and does not stop — document this in the test).

## Out of scope
Policy (056), execution (057).

## Acceptance
```
go test ./internal/bus/ -run 'Confirm|Rollout|Batches|ShouldStop' -v
# PlanRollout table: 1, 2, 12, 20, 21, 100, 1512 targets with defaults (1512 → canary 1, size 50, 31 batches)
# ConfirmFor table incl. protected and destructive; CheckConfirmation table
```

## Pitfalls
- Integer maths with `ceil`: `(a*b + 99) / 100`, not floats.
- `Batches` must not reorder steps; the canary is the first `Canary` steps.
