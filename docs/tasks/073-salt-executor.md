# Task 073 — Salt as an execution transport

- **Milestone:** M4
- **Depends on:** 071, 036
- **Parallel group:** M4-C (with 072)
- **Status:** todo

## Goal
`transport.Executor` + `transport.BatchExecutor` implemented with `cmd.run_all` through the job engine, so
probes, `:run` and rollouts can go through the master.

## Context
SPEC §8.6 (Salt as an execution transport: trade-off table). BUILD-PLAN §4.3 (`BatchExecutor`), §12 (risk: Salt
argument parsing).

## Files
- create `internal/backend/salt/executor.go`, `internal/backend/salt/executor_test.go`
- modify `internal/fanout/fanout.go`, `internal/fanout/fanout_test.go` (use `BatchExecutor` when available)

## Requirements
1. `func NewExecutor(r *Runner, b *Backend, o ExecOptions) *Executor`; `Name()` = `"salt"`;
   `ExecOptions { Chunk int; Timeout time.Duration; BatchWindow time.Duration (default 100 ms); BatchMax int
   (default 200) }`.
2. Script transport: the remote line is always
   `cmd.run_all cmd='echo <BASE64> | base64 -d | sh' python_shell=True ignore_retcode=True timeout=<seconds>`
   where `<BASE64>` is the standard base64 (no line wraps) of `export LC_ALL=C LANG=C; <script>`. `cmd.Sudo` is
   ignored (Salt runs as root); `cmd.Stdin` non-empty → `errs.Config` (unsupported via Salt).
3. Result mapping from each minion's return `{"pid", "retcode", "stdout", "stderr"}` → `ExecResult{RC, Stdout,
   Stderr, Via: "salt:<master>"}`. A return that is a string (Salt-level error such as `'cmd.run_all' is not
   available`) → `errs.Remote` with that text. `NoReturn` → `errs.Timeout` "no return from minion".
   A host with no known minion ID → `errs.Unreachable` "not a salt minion" without submitting.
4. `ExecBatch(ctx, hosts, cmd) <-chan transport.HostResult` — maps hosts → minion IDs, `CompileHosts` chunks, one
   `Start`, results streamed as they arrive; every host yields exactly one result.
5. `Exec(ctx, host, cmd)` — **micro-batching**: requests are queued; a single dispatcher goroutine collects
   requests with the same `(Script, Timeout)` for up to `BatchWindow` or `BatchMax` hosts, submits one job via
   the same path as `ExecBatch`, and delivers each result to its caller. A caller whose `ctx` ends gets
   `errs.Canceled` immediately (its minion's late result is discarded). `Close()` stops the dispatcher.
6. `fanout.Run`: if `ex` implements `transport.BatchExecutor`, delegate to `ExecBatch` (still de-duplicating
   hosts, still guaranteeing one result per host and channel close).
7. Size limit: scripts whose base64 form exceeds 96 KiB → `errs.Config` "script too large for the Salt
   transport".

## Out of scope
UI for choosing the transport (076). Streaming (not possible via Salt; `Streamer` is not implemented).

## Acceptance
```
go test -race ./internal/backend/salt/ -run 'Executor' -v
go test -race ./internal/fanout/ -v
# TestScriptIsBase64 (decode the kwarg from the recorded command line → original script with the LC_ALL prefix),
# TestHostileScriptsRoundTrip (quotes, $(…), newlines, "a=b c=d", unicode: decoded text identical),
# TestResultMapping (rc 3 with stdout/stderr), TestStringReturnIsRemoteError, TestNoReturnIsTimeout,
# TestExecBatchOneJobForManyHosts (150 hosts → 1 submit), TestMicroBatching (50 concurrent Exec → 1 submit),
# TestExecCanceledCaller, TestNonMinionHost, TestFanoutUsesBatchExecutor
```

## Pitfalls
- Base64 output contains `=` padding and `+` `/`; inside the single-quoted `cmd='…'` kwarg that is safe. Do not
  URL-encode.
- `ignore_retcode=True` only silences Salt's error logging; the real exit code is still in `retcode`.
- The dispatcher must never block forever on a caller that went away: results are sent on buffered (1) channels.
