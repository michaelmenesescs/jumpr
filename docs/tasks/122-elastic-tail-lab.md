# Task 122 — Elasticsearch live tail; M11 lab tests

- **Milestone:** M11
- **Depends on:** 120, 121
- **Parallel group:** — (serial; M11 exit)
- **Status:** done

## Goal
`w` follows logs by polling `search_after`; dashboards and logs work in the lab behind the SSO proxy.

## Files
- modify `internal/backend/elastic/backend.go`, `internal/tui/views/eslogs.go`
- create `internal/backend/elastic/lab_test.go`, `internal/backend/grafana/lab_test.go` (lab tag)
- modify `docs/HANDOFF.md`, `README.md`

## Requirements
1. Tail: poll every 2 s (5 s after three empty polls) with `search_after` from the last hit; cap 20,000 lines;
   stops on view close; shows lag (`now − last event time`).
2. Host overview gains `l` → logs for that host; unit detail → logs filtered by unit name when the source has a
   configurable `unit_field`.
3. Lab tests (skip when the observability stack is absent): a dashboard from the lab Grafana renders at least
   one timeseries panel through `/api/ds/query`; ES search returns nginx log lines for a web host inside the
   fault window; both through the `lab-sso` profile.

## Acceptance
```
go test -race ./internal/backend/elastic/ -run Tail -v
make check && make lab-test
```

## Pitfalls
- Events with identical timestamps: without the tiebreaker sort field, `search_after` skips or repeats lines.
