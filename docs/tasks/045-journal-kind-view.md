# Task 045 — Journal: windowed query, follow, view

- **Milestone:** M2
- **Depends on:** 042, 044, 013
- **Parallel group:** M2-E (with 046)
- **Status:** done

## Goal
`j` on a host or unit shows the journal for the session time window; `w` follows it live.

## Context
SPEC §8.1 (`journal`), §8.1.1, §13 (`t`, `w`). Mockup journal screen.

## Files
- create `internal/backend/linux/journal.go`, `internal/backend/linux/journal_test.go`
- create `internal/tui/views/journal.go`, `internal/tui/views/journal_test.go`
- modify `internal/backend/linux/backend.go` (add the `journal` kind and links), `internal/backend/linux/kinds.go`

## Requirements
1. Backend: kind `journal` (aliases `j`, `logs`; ScopeHost), columns `time`, `pri`, `source`, `message`.
   `Args`: `unit`, `priority` (0–7), `limit`, `kernel=1`. `List` runs `JournalCommand` (sudo true) per host via
   fan-out and emits entries as resources (`Status` from priority). For more than one host add a `host` column
   and merge-sort by time in the view, not in the backend.
2. `func (b *Backend) FollowJournal(ctx context.Context, host resource.HostID, o JournalOptions)
   (<-chan Entry, error)` — requires `Options.Stream transport.Streamer` (add the field); parses each line; closes
   on ctx cancel or stream end; unparseable lines are skipped.
3. Links: host → journal (key `j`); unit → journal with `Args["unit"]` (key `enter` / `j`).
4. View `views.NewJournal(env, backend, query)`: a table-like list (time dim, priority coloured, source accent,
   message; errors in Crit) reusing the table rendering helpers. Initially loads the window; newest at the
   bottom; cursor starts at the last line.
   - `w` toggles follow: starts `FollowJournal`, appends entries via the `NextBatch` pattern, keeps the cursor
     pinned to the end while it is on the last line; a `following` badge in the banner; toggling off cancels the
     stream. Follow is only offered for a single host.
   - `/` filter (substring or `~regex`) applies to source + message; `1`–`7` set the maximum priority and reload.
   - Reloads on `WindowChangedMsg`. Banner: `412 lines · -1h → now · -u nginx.service · p≤4`.
   - Keeps at most 20,000 entries in follow mode.
   - `CommandLine()`: `journal --host <h> --arg unit=<u>`.
5. Register the view as the factory for kind `journal` (add `func Factories() map[resource.KindName]tui.Factory`
   in `internal/tui/views/factories.go`; `app` merges it into `Env.Factories`).

## Out of scope
Elasticsearch logs. Marking findings (059).

## Acceptance
```
go test -race ./internal/backend/linux/ -run 'Journal' -v
go test -race ./internal/tui/views/ -run Journal -v
# backend: TestJournalListWindow (command contains --since @… --until @… and -u), TestFollowDeliversAndStops
#   (fake Streamer lines; cancel closes the channel)
# view: TestJournalRendersPriorities, TestJournalFollowAppends, TestJournalFilter, TestJournalReloadsOnWindow
```

## Pitfalls
- Cancel the follow stream when the view is popped or the pane closes: the view needs a `Close()`; make `Stack`
  call `Close()` on views implementing `io.Closer` when they are popped or replaced (small change in
  `stack.go`, with a test).
- Entries arrive oldest-first from `journalctl`; do not re-sort a single host's stream.
