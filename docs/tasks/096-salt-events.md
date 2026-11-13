# Task 096 — Salt event stream

- **Milestone:** M7
- **Depends on:** 094, 045
- **Parallel group:** — (serial)
- **Status:** todo

## Goal
`:events` — the master's event bus live in a pane (SPEC §8.6 "Events").

## Files
- create `internal/backend/salt/events.go`, `internal/backend/salt/events_test.go`
- create `internal/tui/views/saltevents.go`, `internal/tui/views/saltevents_test.go`
- modify `internal/tui/commands_salt.go`

## Requirements
1. `func (b *Backend) Events(ctx) (<-chan Event, error)` using the `Streamer` on the master:
   `salt-run state.event pretty=False` (sudo). Each line is `<tag>\t<json>`; split at the first tab; tolerate a
   single-space separator. `Event{Tag string; Time time.Time (from data "_stamp"); Kind string; Minion, JID, Fun
   string; Data json.RawMessage}`. Kind from the tag: `salt/job/<jid>/new` → `job-new`;
   `salt/job/<jid>/ret/<minion>` → `job-ret`; `salt/auth` → `auth`; `minion_start`/`salt/minion/<id>/start` →
   `minion-start`; `salt/beacon/<minion>/<name>/…` → `beacon`; `salt/key` → `key`; `salt/run/…` → `runner`;
   else `other`.
2. View: follow list (time, kind coloured, minion, summary: `state.apply ret ok` / `rc=1` / `auth pend`),
   filter by text, `1`–`6` toggle kinds, sitrep's own polling runners hidden by default (`h` toggles), pause with
   `space` (buffer keeps filling up to 10,000), `enter` on a job event → job detail (072), on a minion → host.
   Returns of `pillar.*` are hidden as in task 093.
3. The stream is tied to the pane context; it survives detach; if it ends (master restart) the view shows
   `stream ended — r to reconnect`.

## Acceptance
```
go test -race ./internal/backend/salt/ -run Events -v     # golden testdata/salt/event-sample.txt; odd lines skipped
go test -race ./internal/tui/views/ -run SaltEvents -v
```

## Pitfalls
- `state.event` holds one of the master connection's four session slots for as long as the pane is open; allow
  one event stream per session and reuse it for several panes.
