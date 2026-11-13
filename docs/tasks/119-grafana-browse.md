# Task 119 — Grafana: search and dashboards

- **Milestone:** M11
- **Depends on:** 103
- **Parallel group:** — (serial)
- **Status:** todo

## Goal
`:dash` browse and open dashboards; variables become selectors (SPEC §8.4).

## Files
- create `internal/backend/grafana/client.go`, `backend.go`, `model.go`, `_test.go` files, `testdata/grafana/*.json`
- modify `internal/config/` (`grafana: [{name, url, connection}]`), `internal/app/core.go`

## Requirements
1. `GET /api/search?type=dash-db&query=`, `GET /api/dashboards/uid/<uid>`; model: panels (incl. rows/nested),
   `type`, `title`, `targets[]` (datasource ref, expr/query, refId), `templating.list[]` (name, type, current,
   options, query), time defaults.
2. Kind `dashboard` (aliases `dash`): title, folder, tags, uid. `enter` → dashboard view: a grid list of panels
   (title, type, supported yes/no). `o` copies/opens the browser URL (through an `app` hook; default = copy to
   clipboard).
3. Variables: shown as a header row; `v` edits one (options from the dashboard JSON; query-type variables
   resolved through `/api/ds/query` when simple label_values, else free text). `$host`/`$instance`/`$node`
   variables are auto-filled from the session target (first host) and marked `auto`.
4. Grafana alerting: `GET /api/prometheus/grafana/api/v1/alerts` rows merged into kind `alert` with source
   `grafana:<name>` (SPEC §8.4).
5. Health dot `grafana` (`/api/health`).

## Acceptance
```
go test -race ./internal/backend/grafana/ -v     # recorded JSON: search, a dashboard with rows + variables, alerts
```

## Pitfalls
- Dashboard JSON schema varies by version (`datasource` as string or object; `targets` absent on row panels);
  decode defensively.
