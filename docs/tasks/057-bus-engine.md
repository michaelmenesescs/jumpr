# Task 057 — Action bus engine: propose, execute, canary, rolling, record

- **Milestone:** M3
- **Depends on:** 054, 056
- **Parallel group:** — (serial)
- **Status:** todo

## Goal
The implementation of `bus.Bus`: the only code path that performs writes.

## Context
BUILD-PLAN §4.5, §8. SPEC §11 (plan → confirm → execute → record), §21.4 (a paused rollout stays paused).

## Files
- create `internal/bus/bus.go`, `internal/bus/run.go`, `internal/bus/bus_test.go`,
  `internal/bus/bustest/planner.go` (scripted fake planner for tests in other packages)

## Requirements
1. `func New(o Options) Bus` with `Options { Policy *Policy; Rollout RolloutConfig; Recorder Recorder; Redact
   *redact.Redactor; ReadOnly func() bool; Concurrency int; Now func() time.Time; Log *slog.Logger }`.
2. `Register(name, planner)`: remembers the planner; spec ownership is resolved lazily: `Propose` asks each
   planner via an added optional interface `interface{ Owns(specID string) bool }`; if a planner does not
   implement it, ownership is by prefix `name + "."` (e.g. planner `linux` owns `linux.systemctl.restart`).
3. `Propose(ctx, req)`: validate (`Targets` non-empty, de-duplicate by host+ref); find the planner (none →
   `errs.Config`); `planner.Plan` (30 s timeout); attach a new `ID` (time-ordered, e.g. `20261112T140512-3f9a`);
   `Policy.Check` → `Denied`; `Confirm` via `ConfirmFor` (protected = any target `Policy.Protected`);
   `Rollout` via `PlanRollout`. A denied proposal is recorded with `Recorder.RecordDenied` immediately.
4. `Execute(ctx, p, c)`: `CheckConfirmation`; re-check policy and read-only (state may have changed); start the
   run goroutine with a context derived from **the bus's own root context** (passed in `Options.Context`), not
   from the caller's ctx, so closing a dialog or detaching does not kill a rollout; return the `Run`.
5. Run loop:
   - canary steps (sequential) → emit each `StepResult`; any canary failure → `PhaseStopped`, remaining steps
     counted as `Skipped`.
   - if there was a canary and more steps remain: emit `PhasePaused` and wait for `Continue()` or `Abort()`
     (no timeout; survives detach).
   - batches: steps of a batch run concurrently (bounded by `Concurrency`, default 16); after each batch evaluate
     `ShouldStop`; `Abort()` takes effect between batches.
   - a step fails when `Err != nil` or `RC != 0`.
   - each step runs with a per-step timeout (`Options.StepTimeout`, default 5 min) and panic recovery.
   - final event carries `Summary`; then `Recorder.RecordAction(p, summary, results)` (outputs passed through
     `Redact.Cap(output, 64 KiB)`), then the events channel is closed.
6. Events channel is buffered (256) and the run never blocks on a slow consumer: if the buffer is full, coalesce
   by dropping `Result`-only events from the channel (results are still kept for the recorder and the summary).
7. `Run` also exposes `Results() []action.StepResult` (snapshot) and `Proposal() Proposal` so a UI can reattach
   to a running rollout. `func (b *bus) Active() []Run` lists unfinished runs.

## Out of scope
Dialogs (061). Real planners (062, 063, 075).

## Acceptance
```
go test -race ./internal/bus/ -run 'Bus|Run' -v
# with bustest planner (scripted per-host results, optional delay):
# TestSingleTargetNoCanary, TestCanaryFailureStopsAndSkipsRest (mockup scenario: canary fails → 0 others touched),
# TestCanaryPauseThenContinue, TestAbortAfterCanary, TestAbortBetweenBatches, TestThresholdStops,
# TestDeniedCannotExecute (errs.Policy) and is recorded, TestTypedCountRequired (21 targets),
# TestReadOnlyFlippedAfterPropose, TestRecorderGetsRedactedOutput, TestRunSurvivesCallerCancel,
# TestSlowConsumerDoesNotBlock, TestStepPanicIsFailure
```

## Pitfalls
- `Continue()`/`Abort()` may be called several times or at the wrong phase: make them idempotent and non-blocking.
- The only place that calls `step.Run` is `run.go`. Add a test that greps the repo for `.Run(ctx)` on
  `action.Step` outside `internal/bus` if you can; at minimum keep it true.
- Do not hold a lock while running steps.
