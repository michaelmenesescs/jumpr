# Task 046 — Target sets: current set, saved sets, selection

- **Milestone:** M2
- **Depends on:** 039, 022
- **Parallel group:** M2-E (with 045)
- **Status:** todo

## Goal
The session always knows "which hosts am I operating on": set from a selector, from marked rows, or from a saved
set.

## Context
BUILD-PLAN §4.7 (`targets`), §5 (`TargetsChangedMsg`). SPEC §9 (`@saved-set`, `@investigation`), §10 (`sets/`).

## Files
- create `internal/targets/targets.go`, `internal/targets/store.go`, `internal/targets/targets_test.go`
- create `internal/tui/commands_targets.go`, `internal/tui/commands_targets_test.go`
- modify `internal/tui/env.go` (add `Inventory`, `Sets *targets.Store`, `SessionState.Targets`),
  `internal/tui/msgs.go` (`TargetsChangedMsg`), `internal/tui/root.go` (handle it; status line count),
  `internal/tui/views/table.go` (row marking)

## Requirements
1. `targets.Resolve` as in BUILD-PLAN §4.7: hosts matching the selector terms, intersected with nothing else,
   **unioned** with the hosts of each `@set` when the selector has no terms, and **intersected** with them when it
   has terms (`role=web @canary` = web hosts that are in the canary set). Unknown set → `errs.Config`.
   `@investigation` uses `invHosts`. Result sorted and de-duplicated; `Label` = the selector text.
2. `Store`: `NewStore(root string)`; `List() []string`; `Load(name) ([]resource.HostID, error)`;
   `Save(name string, hosts []resource.HostID) error` (name `^[a-z0-9][a-z0-9_-]*$`; file
   `<root>/sets/<name>.txt`, one host per line, header comment with the date; atomic write);
   `Delete(name)`.
3. `func FromHosts(label string, hosts []resource.HostID) Set`.
4. Commands: `:targets` (no args: opens a text view listing the current set), `:targets <selector>` (resolve and
   set), `:targets clear`, `:targets save <name>`, `:targets load <name>`, `:targets marked` (use marked rows of
   the focused table). Each emits `TargetsChangedMsg` and a flash `targets: 24 hosts (role=web dc=ewr)`.
5. Table view: `space` toggles a mark on the current row (shown as `▸` in a 1-cell gutter), `*` marks all
   filtered rows, `U` clears all marks. Expose
   `func (t *Table) MarkedHosts() []resource.HostID`. When a list of a host-scoped kind is opened with no hosts in
   the query and no selector, it uses the session target set.
6. Opening a `host` list with a selector (`:hosts role=web`) **also** sets the session targets to the result
   (this is how the mockup's `targets: 24` gets set). The root does this when it handles `OpenQueryMsg` for kind
   `host`.

## Out of scope
Host range syntax. Investigation targets (059 passes them in).

## Acceptance
```
go test ./internal/targets/ -v
# TestResolveSelector, TestResolveSetUnion, TestResolveSetIntersect, TestUnknownSet, TestStoreRoundTrip,
# TestStoreRejectsBadName
go test -race ./internal/tui/... -run 'Targets|Mark' -v
# TestHostsCommandSetsTargets (status line shows "targets:504" after ":hosts dc=ewr"),
# TestMarkedRowsBecomeTargets, TestTargetsSaveLoad
```

## Pitfalls
- Do not bind `ctrl+a`: it is the prefix. Marks use `space`, `*` and a clear key only.
- The status line shows the count from `SessionState.Targets`, not from the focused table.
