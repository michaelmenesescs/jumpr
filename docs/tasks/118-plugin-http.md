# Task 118 — HTTP plugins and plugin inventory sources

- **Milestone:** M10
- **Depends on:** 116, 099
- **Parallel group:** M10-B (with 117)
- **Status:** todo

## Goal
`target: http` and `target: inventory` plugins (SPEC §12 second example, §9 "plugin-defined sources").

## Files
- create `internal/plugin/http.go`, `inventory.go`, `_test.go` files

## Requirements
1. HTTP plugin: request (method, url template, headers, body) through the named connection profile; GET/HEAD
   only for reads; other methods only inside `actions` (through the bus; display = method + URL). URL template
   values are path/query-escaped.
2. Inventory plugin: a host command on a configured host, or an HTTP request, whose parsed rows provide `name`
   plus label columns → an `inventory.Source` named `plugin.<name>`.
3. Timeouts 15 s default; response cap 16 MiB; errors classified by `httpx`.

## Acceptance
```
go test -race ./internal/plugin/ -run 'HTTP|Inventory' -v     # httptest: lb-pool-members example end to end
```

## Pitfalls
- Never allow a plugin read to use POST "because the API needs it" without the bus; offer `method: POST` only
  with `safe: true` declared explicitly in the plugin (documented as the author's assertion) and show it in
  `:plugins`.
