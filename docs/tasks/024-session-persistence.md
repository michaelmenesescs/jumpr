# Task 024 — Session persistence (layout survives restart)

- **Milestone:** M1
- **Depends on:** 023
- **Parallel group:** — (serial; touches the root or `app`)
- **Status:** todo

## Goal
Quit and restart: the same windows, splits and views come back.

## Context
SPEC §21.4 ("what survives a server restart"). BUILD-PLAN §6 (persistence), §4.9 (`ContentState`).

## Files
- create `internal/mux/persist.go`, `internal/mux/persist_test.go`
- modify `internal/tui/root.go` (autosave tick, restore), `internal/app/app.go` (load on start)

## Requirements
1. `type SessionFile struct { Version int; Name string; Active int; Windows []WindowFile; History []string;
   SavedAt time.Time }`, `WindowFile { Name string; Layout *Layout; Focus PaneID; Panes map[PaneID]ContentState }`
   with JSON tags (lower snake case). `Version` is 1.
2. `func Snapshot(m *Model, history []string, now time.Time) SessionFile`.
3. `func Save(path string, f SessionFile) error` — write to `path + ".tmp"` then `os.Rename`; file mode 0600;
   create the directory with 0700.
4. `func Load(path string) (SessionFile, error)` — missing file → `os.ErrNotExist` passthrough; wrong version or
   corrupt JSON → `errs.Config` (the caller starts fresh and flashes a warning; it must not crash).
5. `func Restore(f SessionFile, factory Factory, prefixKey string, styles Styles) (*Model, tea.Cmd, error)` —
   rebuilds the session; pane IDs are kept; contents are created through the factory with their saved
   `ContentState`; counters continue after the highest restored ID. A pane whose content cannot be created gets
   the default content and the error is collected into a returned warning list.
6. Root: after any mux op or stack navigation set a dirty flag; a 1 s `tea.Tick` saves when dirty (never save
   synchronously in `Update`: do it in a `tea.Cmd`). Path: `<cache>/sessions/<session name>.json`. Save once more
   on quit.
7. `app.Run`: on start, load the session file for `main`; if present restore, else create a fresh session.
   `--fresh` flag skips restoring.
8. The view pane factory restores `ContentState.Commands` by running them through `Commands.Exec` in order
   (first one becomes the root view).

## Out of scope
Terminal pane state (added in 050). Multiple sessions (M5).

## Acceptance
```
go test -race ./internal/mux/ -run 'Persist|Snapshot|Restore' -v
# TestSaveLoadRoundTrip, TestRestoreKeepsLayoutAndFocus, TestCorruptFileIsConfigError, TestSaveIsAtomic
go test -race ./internal/tui/ -run 'Autosave|Restore' -v
# TestAutosaveAfterSplit (file appears in t.TempDir() within 2s), TestRestoreRebuildsViews
```

## Pitfalls
- Tests use `t.TempDir()` as the cache dir; never the real `~/.cache`.
- JSON map keys must be strings: `map[PaneID]ContentState` needs `PaneID` to marshal as a string key; implement
  `encoding.TextMarshaler`/`TextUnmarshaler` on `PaneID` or convert to `map[string]…` in the file type.
