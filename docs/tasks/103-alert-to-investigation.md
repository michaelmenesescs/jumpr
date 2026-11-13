# Task 103 — Alert → investigation → hosts (SPEC W1 complete)

- **Milestone:** M8
- **Depends on:** 101, 102, 059
- **Parallel group:** — (serial)
- **Status:** todo

## Goal
`Enter` on a firing alert opens an investigation with the right window and hosts, and the host views gain metrics.

## Files
- create `internal/backend/prom/resolve.go`, `source.go` (inventory source `prom`), `_test.go` files
- modify `internal/tui/views/host.go` (metrics section), `internal/tui/root.go` or a new
  `internal/tui/commands_alert.go`, `internal/inventory/identity.go` if a rewrite helper is needed

## Requirements
1. `func HostsFromLabels(labels map[string]string, inv inventory.Inventory, keys []string) []resource.HostID` —
   try label keys in order (config `alerts.host_labels`, default `instance, host, hostname, node, nodename,
   exported_instance`): strip a `:port`, apply identity rewrites, `Resolve`. No direct host label → build a
   selector from the remaining labels that are also inventory label keys (`dc=ewr service=api` → `dc=ewr`) and
   select; if that yields more than 500 hosts or none, open with an empty target set and a flash explaining why.
2. `Enter` on an alert: `Log.Open(title = alertname + main labels, window = [startsAt − 30m, now], targets,
   alerts)`; set session window and targets; push `:hosts` for the targets; flash as in the mockup. Alerts whose
   labels carry `namespace`/`pod` open the pods view instead once the k8s backend exists (leave a hook:
   `AlertOpeners []func(alert) (tea.Cmd, bool)` in `Env`).
3. Host list and overview: when a Prometheus is configured, add columns/section from configurable queries
   (`metrics.host_columns: [{id, title, expr}]` with `$host`/`$instance` substitution; default: none, example in
   docs): a value column and a sparkline over the window; the alert's own expression (from `Rules`) is shown in
   the overview when the investigation was opened from an alert.
4. Inventory source `prom`: hosts from `up` targets (`instance` → host), labels `prom.job`, and any target labels.
5. Knowledge panel subject now includes the alert name.

## Acceptance
```
go test -race ./internal/backend/prom/ -run 'HostsFromLabels|Source' -v
go test -race ./internal/tui/ -run 'AlertOpen' -v     # teatest with fake AM/Prom: enter on alert → "inv:" in the
                                                      # status line, targets count, hosts view, timeline "open"
```

## Pitfalls
- Batch metric queries: one PromQL per column for all hosts (`expr{instance=~"a|b|c"}`), not one per host; chunk
  the regex at 200 hosts.
