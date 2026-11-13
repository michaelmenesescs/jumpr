# Task 071 — Salt job engine: async submit, poll, per-minion results

- **Milestone:** M4
- **Depends on:** 066
- **Parallel group:** M4-B (with 068, 069)
- **Status:** done

## Goal
Run any Salt function on a target as an async job and stream per-minion results, with explicit "no return".

## Context
SPEC §8.6 (job engine). BUILD-PLAN D9, §8 (timeouts), §12.

## Files
- create `internal/backend/salt/jobs.go`, `internal/backend/salt/jobs_parse.go`,
  `internal/backend/salt/jobs_test.go`

## Requirements
1. Parsing `salt-run jobs.list_job <jid>` (sample; verify against `testdata/salt/list-job-*.json`):
   ```json
   {"jid": "20261112135501123456", "Function": "cmd.run_all", "Arguments": ["…"], "Target": ["web-01","web-02"],
    "Target-type": "list", "User": "sudo_sre", "StartTime": "2026, Nov 12 13:55:01.123456",
    "Minions": ["web-01", "web-02"],
    "Result": {"web-01": {"return": {"pid": 1, "retcode": 0, "stdout": "ok", "stderr": ""}, "retcode": 0,
                          "success": true}}}
   ```
   → `JobInfo{JID, Function string; Arguments []json.RawMessage; Target json.RawMessage; TargetType, User string;
   Start time.Time; Minions []string; Result map[string]MinionReturn}` and
   `MinionReturn{Return json.RawMessage; Retcode int; Success bool}`. `Target` may be a string or a list.
   `StartTime` layout `2006, Jan 02 15:04:05.000000` (master local time; keep as naive local).
   An unknown jid returns `{}` or an object with `"Error"`: → `errs.NotAvailable`.
2. `type Submit struct { Targets []Target; Fun string; Args []string; Kwargs map[string]string; Expect []string;
   Timeout time.Duration }` (`Expect` = minion IDs the caller believes will match; may be empty).
3. `type MinionResult struct { Minion string; Return json.RawMessage; Retcode int; Success bool; NoReturn bool;
   Unexpected bool; JID string }`.
4. `func (r *Runner) Start(ctx context.Context, s Submit) (*Job, error)`:
   - for each target: `salt --async <target> <fun> …` → `ParseJID`. An error on any chunk cancels nothing already
     submitted but is returned in `Job.SubmitErrors`; if every chunk failed → return the error.
   - `Job` exposes `JIDs() []string`, `Results() <-chan MinionResult` (closed when finished), `Expected()
     []string` (updated from `Minions` once known), `Cancel()`.
5. Poll loop per job (one goroutine): every `poll` (default 2 s; first poll after 500 ms) call `jobs.list_job`
   for each unfinished jid; emit each minion's result exactly once; expected set = union of `Minions` from the
   master and `Expect`. Finished when every expected minion has reported, or when `Timeout` (default 60 s)
   passes: then emit `NoReturn: true` for each missing minion and close. Minions that report but were not in
   `Expect` (when `Expect` was non-empty) get `Unexpected: true`.
   - `Minions` empty on the first polls (job cache lag) is normal: keep polling; do not finish early before 3 s.
   - a failing poll (transport error) is retried; three consecutive failures → emit `NoReturn` for the rest with
     the error attached to `Job.Err()` and close.
   - `ctx` cancel or `Cancel()` stops polling and closes the channel (the Salt job itself keeps running on the
     minions; killing it is an explicit action, task 075).
6. `func (r *Runner) Lookup(ctx, jid string) (JobInfo, error)`; `func (r *Runner) Active(ctx) (map[string]
   ActiveJob, error)` for `salt-run jobs.active` (`{jid: {"Function","Arguments","Target","Target-type","User",
   "Running": [{"web-01": 4321}], "Returned": ["web-02"]}}`); `func (r *Runner) ListJobs(ctx, w
   resource.TimeWindow, now time.Time) ([]JobSummary, error)` for `salt-run jobs.list_jobs` with kwargs
   `start_time`/`end_time` formatted `2006-01-02 15:04:05` (output `{jid: {"Function","Arguments","Target",
   "Target-type","User","StartTime"}}`); sorted newest first.
7. `func JIDTime(jid string) (time.Time, bool)` — a jid encodes `YYYYMMDDhhmmssffffff`.

## Out of scope
Kinds and views (072). Executor (073). State semantics (074).

## Acceptance
```
go test -race ./internal/backend/salt/ -run 'Job|JID|ListJobs|Active|Lookup' -v
# with a scripted fake master (transporttest OnFunc that returns growing list_job results per poll, injected
# poll interval 10ms):
# TestStartStreamsEachMinionOnce, TestNoReturnAfterTimeout, TestChunkedTargetsMerge (2 jids → one stream),
# TestUnexpectedMinionFlagged, TestCancelStopsPolling, TestPollErrorsRetriedThenGiveUp,
# TestEmptyMinionsDoesNotFinishEarly, TestParseListJobTargetStringOrList, TestListJobsWindowKwargs
```

## Pitfalls
- One `salt-run` per jid per poll; with 8 chunks that is 8 calls every 2 s on one SSH connection (4 sessions at a
  time) — acceptable; do not poll faster.
- Emit-once bookkeeping is per (jid, minion).
- Timeouts are measured from submit, with an injected clock/ticker in tests (no real 60 s waits).
