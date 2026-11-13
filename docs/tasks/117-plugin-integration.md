# Task 117 — Plugins as kinds, links and actions; save `:run` as a plugin

- **Milestone:** M10
- **Depends on:** 116, 063
- **Parallel group:** M10-B (with 118)
- **Status:** todo

## Goal
A loaded plugin behaves like a built-in kind (SPEC §12, W5 "saved as a YAML plugin in one keystroke").

## Files
- create `internal/plugin/backend.go`, `actions.go`, `save.go`, `_test.go` files
- create `internal/tui/commands_plugin.go`; create `examples/plugins/*.yaml` (5 examples: `nginx-status`,
  `cert-expiry`, `ntp-offset`, `pg-replication`, `lb-pool-members`)
- modify `internal/app/core.go`, `internal/tui/views/fanout.go` (`S` save)

## Requirements
1. Backend `plugin`: each host-target definition is a kind named after the plugin (ScopeHost): runs the command
   through the fan-out on the targets; `view.group_by_output: true` opens the fan-out view, otherwise a table
   with a `host` column + declared columns. Links map to queries with templated args. `emits_hosts` rows become
   pivotable (enter → host overview after inventory resolve).
2. Plugin actions → planner `plugin`: spec `plugin.<name>.<action>`, actor `plugin:<name>` recorded, always via
   the bus.
3. Reload on change: kinds are re-registered (extend `Registry` with `Replace(b Backend) error` — additive
   method, allowed); problems are listed in `:plugins` with file and line; a broken edit keeps the last good
   version loaded.
4. `S` in a `:run` result view: prompt for a name → writes `plugins/<name>.yaml` (`target: host`, the command,
   `parse: raw`, `group_by_output: true`, `privileged` from `--sudo`) → flash the path. Never overwrites.
5. `:plugins` lists plugins (name, target, description, state) and opens the file path.

## Acceptance
```
go test -race ./internal/plugin/ -run 'Backend|Action|Save|Reload' -v
go test -race ./internal/tui/ -run 'Plugin' -v
```

## Pitfalls
- Plugin **reads** (`command`) run without confirmation, like built-in probes: the plugin author vouches for
  them. The loader still rejects read commands containing obvious writes (`>`, `tee`, `rm `,
  `systemctl (start|stop|restart)`); those belong in `actions`, which always go through the bus.
