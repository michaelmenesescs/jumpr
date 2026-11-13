# Task 121 — Elasticsearch: log sources and search

- **Milestone:** M11
- **Depends on:** 119
- **Parallel group:** — (serial)
- **Status:** todo

## Goal
`:logs` — search central logs for the investigation's hosts and window (SPEC §8.5). Read-only.

## Files
- create `internal/backend/elastic/client.go`, `backend.go`, `query.go`, `_test.go` files, `testdata/elastic/*.json`
- create `internal/tui/views/eslogs.go`, `eslogs_test.go`
- modify `internal/config/` (`elasticsearch: [{name, url, connection, sources: [{name, index, time_field,
  host_field, message_field, level_field}]}]`), `internal/app/core.go`

## Requirements
1. Client: `_search` with `query.bool.filter` = time range + `terms` on the host field (session targets, both FQDN
   and short names; chunked at 1,000) + optional `query_string` typed by the user; sort by time asc with a
   tiebreaker; paging with `search_after`; `size` 500; `_source` limited to the configured fields plus user-added
   columns; `track_total_hits: false`. Cross-cluster index patterns pass through unchanged. `_field_caps` for
   field completion; `_resolve/index` for `:logs sources`.
2. Only GET/POST to `_search`, `_field_caps`, `_resolve`, `_cat/indices`, `_pit` — the client refuses any other
   path (read-only guarantee, tested).
3. View like the journal view: time, level (coloured), host, message; `/` local filter; `q` edits the
   `query_string`; `c` adds a column; `enter` shows the full document (redacted); `F` finding; banner with
   source, window, query, count (`500+`).
4. Health dot per cluster (`GET /` → version).

## Acceptance
```
go test -race ./internal/backend/elastic/ -v     # request bodies asserted as JSON; paging; error envelope
                                                 # (`error.root_cause`); refused paths
go test -race ./internal/tui/views/ -run ESLogs -v
```

## Pitfalls
- User `query_string` text goes into JSON via `encoding/json`, never string concatenation.
