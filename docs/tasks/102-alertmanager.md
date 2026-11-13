# Task 102 — Alertmanager: alerts view and silences

- **Milestone:** M8
- **Depends on:** 101, 057
- **Parallel group:** — (serial)
- **Status:** done

## Goal
`:alerts` as in the mockup; silences as bus actions (SPEC §8.3).

## Files
- create `internal/backend/prom/alertmanager.go`, `am_actions.go`, `_test.go` files, `testdata/alertmanager/*.json`
- modify `internal/backend/prom/kinds.go`, `internal/config/`, `internal/app/core.go`

## Requirements
1. Alertmanager API v2: `GET /api/v2/alerts` (active, silenced, inhibited), `GET /api/v2/silences`,
   `POST /api/v2/silences`, `DELETE /api/v2/silence/{id}`.
2. Kind `alert` (aliases `alerts`, `al`; global): `sev` (from label `severity`), `alert`, `labels` (compact,
   without `alertname`/`severity`), `source` (`alertmanager:<name>`), `start`, `state`
   (`firing|silenced|inhibited`). Sort: firing first, by severity (`critical`, `warning`, other), then start.
   This kind name is shared: the fake backend's `alert` kind is only registered in `--demo`.
3. Kind `silence`: `id` (short), `matchers`, `creator`, `comment`, `starts`, `ends`, `state`.
4. Planner `alertmanager`: `alertmanager.silence.create` (params `duration` default `2h`, `comment` required;
   matchers from the selected alert's labels; creator = local username passed in from `app`),
   `alertmanager.silence.expire` (destructive: no, but confirm), `alertmanager.silence.extend`. Display = the
   HTTP call (`POST <url>/api/v2/silences {matchers…, endsAt…}`). Recorded like every action.
5. Health dot `am`.

## Acceptance
```
go test -race ./internal/backend/prom/ -run 'Alertmanager|Silence' -v
```

## Pitfalls
- Silence times are RFC 3339 in UTC; the comment is free text typed by the user — JSON-encode, never concatenate.
