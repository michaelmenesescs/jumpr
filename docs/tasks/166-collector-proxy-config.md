# Task 166 — Proxy config collector: nginx and haproxy routes

- **Milestone:** M15
- **Depends on:** 164
- **Parallel group:** —
- **Status:** todo

## Context
SPEC §23.2 (proxy config row), §24.1 step 2 (routes are used by `:flow`). BUILD-PLAN §14.3.

## Files
- create `internal/graph/collect/proxy.go`, `internal/graph/collect/parse_nginx.go`, `internal/graph/collect/parse_haproxy.go` and tests
- add `testdata/proxy/{nginx,haproxy}/…`

## Requirements
1. Only on hosts where the graph has an active `nginx.service` / `haproxy.service`. Reads `sudo -n nginx -T` and,
   for haproxy, the `-f` path from `systemctl show haproxy -p ExecStart` read with `sudo -n cat`. Never runs a
   reload or a config test that writes anything.
2. Parse client side: nginx `server` blocks (`listen`, `server_name`), `location` prefixes, `proxy_pass` /
   `upstream` members; haproxy `frontend`/`backend`/`server`/`use_backend` with simple ACLs (`hdr(host)`,
   `path_beg`). Anything unparsed is kept as an attr `unparsed=<n> lines`, never an error.
3. Edges `routes` from the listener to upstream addresses (resolved to hosts/listeners by the fleet join), attrs
   `server_name`, `path`, `backend`; confidence Configured.
4. Without sudo, the collector reports `HostErr` kind `Permission` once per host and the view says "proxy routes
   need sudo".

## Acceptance
```
go test ./internal/graph/collect/ -run 'Proxy|Nginx|Haproxy' -v
make check
```
