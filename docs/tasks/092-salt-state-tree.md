# Task 092 — Salt state tree browser

- **Milestone:** M7
- **Depends on:** 076
- **Parallel group:** — (serial)
- **Status:** done

## Goal
`:states` — environments, top file, sls files, and what a minion would get (SPEC §8.6 "States").

## Files
- create `internal/backend/salt/tree.go`, `internal/backend/salt/tree_test.go`
- create `internal/tui/views/salttree.go`, `internal/tui/views/salttree_test.go`
- modify `internal/backend/salt/kinds.go`, `internal/tui/commands_salt.go`

## Requirements
1. Runner calls and parsers (golden: `testdata/salt/fileserver-*.json`, `show-top.json`, `show-sls-nginx.json`,
   `show-highstate.json`): `salt-run fileserver.envs` → `[]string`; `salt-run fileserver.file_list saltenv=<env>`
   → paths; derive sls names from `*.sls` paths (`nginx/init.sls` → `nginx`, `nginx/conf.sls` → `nginx.conf`);
   `salt <minion> state.show_top` → `{minion: {env: [sls…]}}`; `state.show_sls <sls>` and `state.show_highstate`
   → `{minion: {state_id: {module: [args…], "__sls__": …, "__env__": …}}}` flattened to rows (`id`, `module.fun`,
   `sls`, `name`, requisites summarized).
2. Kind `salt-sls` (aliases `states`, `sls`; global): `sls`, `env`, `path`, `assigned` (number of targeted
   minions whose top includes it; computed lazily for the session targets with one `state.show_top` job).
3. View: left = sls list (filterable); `enter` → rendered `show_sls` rows for a chosen minion (first target, or
   prompt); `t` → the top-file assignment for the session targets grouped by identical sls list (fan-out style:
   "20 identical · 2 differ"); `c` → raw sls source via `salt <master's own minion> cp.get_file_str salt://<path>` (a job on one minion);
   when that fails show "source not readable".
4. From an sls: `d` → `:salt state <sls>` dry run on the session targets (075).

## Acceptance
```
go test ./internal/backend/salt/ -run 'Tree|Sls|ShowTop|ShowHighstate' -v
go test -race ./internal/tui/views/ -run SaltTree -v
```

## Pitfalls
- `show_sls`/`show_highstate` return a **list of error strings** on render errors; show them, do not fail.
- Rendered state data can include pillar-derived secrets (passwords in `file.managed` contents): pass every
  displayed string through `redact`, and never record it.
