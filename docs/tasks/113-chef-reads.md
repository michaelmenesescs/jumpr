# Task 113 — Chef backend: knife runner, nodes, status, inventory

- **Milestone:** M10
- **Depends on:** 065
- **Parallel group:** M10-A (with 115)
- **Status:** done

## Goal
SPEC §8.7 reads, via `knife … -F json` on the workstation over SSH.

## Context
Lab: `chef.lab.example.internal` runs chef-zero with knife configured for user `sre`; web-01..03 and db-01 are
registered; db-01's last run fails on purpose.

## Files
- create `internal/backend/chef/runner.go`, `backend.go`, `kinds.go`, `parse.go`, `source.go`, `_test.go` files
- extend `cmd/@@` capture with `capture chef` → `testdata/chef/*.json`
- modify `internal/config/` (`chef: {workstation, knife: "knife", sudo: false, timeout}`), `internal/app/core.go`

## Requirements
1. Runner: builds `knife <args…> -F json` with `shq.Join` (no sudo by default), decodes with the tolerant
   `FirstJSON` approach (copy the 20 lines; do not import the salt package), maps errors (`knife: command not
   found` → NotAvailable; `401`/`Failed to authenticate` → Auth; connection refused to the Chef server →
   Unreachable).
2. Kinds: `chef-node` (name, environment, platform, roles, run_list short, `ohai_time` age → `last check-in`,
   status), `chef-role`, `chef-env`. Sources: `knife status -F json` (name, ohai_time, run_list, platform…),
   `knife node show <n> -l -F json` for detail (tabs: summary, run-list, attributes by precedence — attributes
   are redacted with `redact` and capped), `knife search node '<q>' -a <attr>`.
3. Stale check-in (default > 2 h) → Warn; > 24 h → Crit.
4. Inventory source `chef`: hosts from `knife status` with labels `chef.environment`, `chef.roles`
   (comma-joined), `chef.platform`.
5. Health dot `chef`: `knife node list` round trip.

## Acceptance
```
go test -race ./internal/backend/chef/ -v
go run ./cmd/@@ capture chef --config-dir lab/jumper      # lab up
```

## Pitfalls
- `knife search` query strings are Solr syntax typed by the user: pass as one quoted argument, never interpolate.
