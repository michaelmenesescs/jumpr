# Task 101 — Prometheus backend: targets, rules, PromQL, sparklines, chart

- **Milestone:** M8
- **Depends on:** 099
- **Parallel group:** — (serial)
- **Status:** todo

## Goal
SPEC §8.3 for Prometheus.

## Files
- create `internal/backend/prom/client.go`, `backend.go`, `kinds.go`, `_test.go` files, `testdata/prom/*.json`
- create `internal/render/chart.go`, `internal/render/chart_test.go`
- create `internal/tui/views/promql.go`, `internal/tui/views/promql_test.go`
- modify `internal/config/` (`prometheus: [{name, url, connection}]`), `internal/app/core.go`

## Requirements
1. Typed client over `httpx.Client`: `Query(ctx, expr, ts)`, `QueryRange(ctx, expr, from, to, step)`,
   `Targets`, `Rules`, `Alerts`, `Series`, `LabelValues`; decodes the standard envelope (`status`, `data`,
   `errorType`, `error`, `warnings`); `status != success` → `errs.Remote` with the Prometheus message; step
   chosen so a range returns at most 300 points.
2. Kinds: `target` (job, instance, health, last scrape, error), `rule` (group, name, state, health), `series`
   result table for instant queries.
3. `:promql <expr>` view: instant result as a table (labels, value) with a per-row sparkline from a range query
   over the session window; `enter` → full-pane braille line chart (`render.Chart(series, w, h)` with y-axis
   labels and min/max/last); `e` edits the expression; history of expressions.
4. `render.Chart`: braille 2×4 dot plotting, exact size, handles NaN gaps, one or more series (up to 4 colours).
5. Health dot per configured Prometheus (`/-/healthy` or a trivial query), named by `name`.
6. Every query uses the session `TimeWindow` and reloads on `WindowChangedMsg`.

## Acceptance
```
go test -race ./internal/backend/prom/ -v        # httptest with recorded JSON: vector, matrix, error envelope,
                                                 # warnings; step calculation table
go test ./internal/render/ -run Chart -v         # exact size; NaN gap; flat line; single point
go test -race ./internal/tui/views/ -run PromQL -v
```

## Pitfalls
- Sample values arrive as strings (`"1.5"`, `"NaN"`, `"+Inf"`); parse with `strconv.ParseFloat`.
