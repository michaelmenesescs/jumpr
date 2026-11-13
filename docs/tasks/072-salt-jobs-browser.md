# Task 072 — Jobs browser: live and historical jobs, grouped job detail

- **Milestone:** M4
- **Depends on:** 068, 071, 048
- **Parallel group:** M4-C (with 073)
- **Status:** todo

## Goal
SPEC W8: `:jobs` → open a job → "41 identical · 2 differ · 1 no return" → pivot to a minion's host.

## Context
SPEC §5 W8, §8.6 (jobs browser).

## Files
- create `internal/backend/salt/kind_jobs.go`, `internal/backend/salt/kind_jobs_test.go`
- create `internal/tui/views/saltjob.go`, `internal/tui/views/saltjob_test.go`
- modify `internal/backend/salt/kinds.go`, `internal/tui/views/factories.go`

## Requirements
1. Kind `salt-job` (aliases `jobs`, `job`; global): columns `jid`, `start` (time), `state` (`running|done`),
   `function`, `target` (compact: list → `web-01,web-02 (+10)`), `user`, `returned` (`10/12` for running jobs,
   blank for history). `List` merges `Active` (always) with `ListJobs` for the query window (default: session
   window, else last 1 h). Running first, then newest first. `Args["minion"]` filters to jobs whose target list
   or returns include that minion (history filter is best effort on `Target`). `Args["fun"]` filters by function
   prefix. Hide sitrep's own polling noise: drop jobs whose function is `runner.jobs.*` or `saltutil.find_job`.
2. Refresh: the table view's normal refresh; add an optional `AutoRefresh() time.Duration` method on views that
   the root honours with a `tea.Tick` (5 s for this kind while any job is running).
3. Job detail view `views.NewSaltJob(env, saltBackend, jid)`:
   - loads `Lookup(jid)`; while the job is still active, re-polls every 2 s.
   - header: function + arguments (redacted through `env.Redact` — add `Redact *redact.Redactor` to `Env`),
     target, user, start, duration so far.
   - body: results grouped with `group.Grouper` over each minion's **rendered return**: for
     `cmd.run`/`cmd.run_all` the stdout (+ stderr, + `rc=N` line when non-zero); for state functions the
     normalized state summary from task 074 when available (until then the compact JSON); otherwise compact JSON
     with sorted keys. Missing minions (`Minions` − `Result` keys, once the job is no longer active) are shown as
     the group `no return`.
   - banner: `41 identical · 2 differ · 1 no return` (reuse `Summary.Headline()`; map "no return" into the
     errors bucket with kind `timeout` and render that bucket's label as `no return` in this view).
   - keys as in the fan-out view (`enter` members, `d` diff, `o` full output, `space` mark), plus on a member:
     `enter` → host overview via `MinionHost`; `j` → that host's journal with the window set to
     `[job start − 1m, job start + duration + 5m]`.
4. `enter` on a job row opens the detail view; factory registered for kind `salt-job-detail` with
   `Args["jid"]`; `CommandLine()`: `salt-job-detail --arg jid=<jid>`.
5. Record: opening a job detail during an investigation appends a `view` entry `salt job <jid> <function>`.

## Out of scope
Killing jobs (075). Event stream (096).

## Acceptance
```
go test -race ./internal/backend/salt/ -run 'KindJobs' -v
# TestJobsRunningFirst, TestJobsWindowed, TestJobsMinionFilter, TestJobsHideInternal
go test -race ./internal/tui/views/ -run SaltJob -v
# TestJobDetailGroups (44 minions: 41 same stdout, 2 different, 1 missing → headline text),
# TestJobDetailRepollsWhileActive, TestJobDetailPivotToHost, TestJobDetailRedactsArguments
```

## Pitfalls
- `jobs.list_jobs` on a busy master is large; always pass the time window kwargs.
- Arguments can contain secrets (`cmd.run 'curl -H "Authorization: …"'`); always redact before display or record.
