# Task 120 — Render Grafana panels in the TUI via `/api/ds/query`

- **Milestone:** M11
- **Depends on:** 119, 101
- **Parallel group:** — (serial)
- **Status:** done

## Goal
timeseries / stat / gauge panels backed by Prometheus or Elasticsearch render as charts (SPEC §8.4).

## Files
- create `internal/backend/grafana/query.go`, `frames.go`, `_test.go` files, `internal/tui/views/dashboard.go`,
  `dashboard_test.go`

## Requirements
1. `POST /api/ds/query` with `{queries: [target + datasource + refId + intervalMs + maxDataPoints], from, to}`
   (epoch ms strings) for the session window; variable interpolation (`$var`, `${var}`, `[[var]]`,
   `$__interval`, `$__rate_interval`, `$__range`) done client-side before sending; multi-value variables joined
   with `|` for regex matchers.
2. Decode data frames (`results.<refId>.frames[].schema.fields` + `data.values`) into series (time + number
   fields; labels from field config/labels).
3. Dashboard view: panels laid out in a 1–3 column grid depending on width; timeseries → `render.Chart`, stat →
   big number + sparkline, gauge → bar; unsupported types show title + `open in browser (o)`. Panels load
   concurrently (max 6 in flight), each with its own spinner and error kind. `enter` zooms a panel to the full
   pane; `r` reloads; reload on window change.
4. `F` on a panel records a finding with the panel title, the queries and min/max/last values.

## Acceptance
```
go test -race ./internal/backend/grafana/ -run 'Query|Frames|Interpolate' -v
go test -race ./internal/tui/views/ -run Dashboard -v
```

## Pitfalls
- `maxDataPoints` should equal the panel's cell width × 2 (braille); asking for thousands of points is slow.
