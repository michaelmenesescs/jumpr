# Task 059 — Investigation lifecycle, recorder, commands, timeline

- **Milestone:** M3
- **Depends on:** 058, 046, 055
- **Parallel group:** — (serial; touches the root)
- **Status:** todo

## Goal
Open an investigation, have views and actions append to it automatically, add notes and findings, see the
timeline, resolve it.

## Context
SPEC §10, §13 (`n`, `F`, `T`). BUILD-PLAN §4.10 (`Log`), §5 (`InvestigationChangedMsg`). Mockup timeline.

## Files
- create `internal/investigation/log.go`, `internal/investigation/log_test.go`
- create `internal/tui/commands_inv.go`, `internal/tui/views/timeline.go`, `internal/tui/inv_test.go`
- modify `internal/tui/env.go` (`Inv investigation.Log`, `SessionState.Investigation`), `internal/tui/msgs.go`,
  `internal/tui/root.go`, `internal/tui/views/table.go`, `internal/tui/views/journal.go`,
  `internal/tui/views/fanout.go` (auto entries, `F`)

## Requirements
1. `log.go`: `func NewLog(root string, red *redact.Redactor, now func() time.Time) *FileLog` implementing `Log`
   and `bus.Recorder`:
   - `Open(title string, window resource.TimeWindow, targets []resource.HostID, alerts []string) (Meta, error)` —
     creates the file, appends the `open` entry, becomes current.
   - `Resume(id string) (Meta, error)`, `Close()` (no current), `Resolve(rootCause string) error` (entry + status).
   - `Append`: to the current investigation; when none is open, only `action`, `denied` and `input` kinds are
     written, to the audit file; other kinds are dropped silently.
   - `RecordAction`: entry text `` `<plan title>` by **<actor>** → ok N/M`` plus failed count; output attachment =
     the plan's step display lines and per-host `rc` + first lines of output, grouped (use `group.Grouper`) so 500
     identical outputs take one block. `RecordDenied`: `denied` entry with the rule.
2. Commands: `:inv` (list; `enter` resumes), `:inv open <title>` (window = session window, targets = session
   targets), `:inv close`, `:inv resolve` (prompt for a one-line root cause), `:note <text>` and key `n` (prompt),
   `:timeline` and key `T` (pushes the timeline view).
3. Auto entries (only when an investigation is open; never for plain table navigation):
   - host overview opened → `view` `host overview <host>`
   - journal view loaded → `journal` `<unit or all units> on <host> (N lines in window)`
   - fan-out finished → `probe` `` `<title>` on N hosts → <headline> `` with the grouped output attached
4. `F` on a selected row in table, journal, fan-out and (via copy mode) any selection → `finding` entry with the
   row's text; flash `finding recorded`.
5. Timeline view: a `Text` view over the investigation file's content, reloaded when the file changes (after
   each append the root sends a private refresh message); `o` flashes the file path (opening `$EDITOR` comes with
   local panes); `R` = resolve. With no investigation: the mockup's hint text and the audit file path.
6. Root: `InvestigationChangedMsg` updates `SessionState`, status line `inv:<id>`; `@investigation` in selectors
   resolves to the investigation's targets.
7. All file writes happen in `tea.Cmd`s, never in `Update`.

## Out of scope
Alert-driven open (103). Knowledge notes on resolve (060).

## Acceptance
```
go test -race ./internal/investigation/ -run Log -v
# TestAppendWithoutInvestigationGoesToAuditOnlyForActions, TestRecordActionGroupsOutputs, TestResolve
go test -race ./internal/tui/... -run 'Inv|Timeline|Finding|Note' -v
# teatest with a temp knowledge root: ":inv open nginx 443" → status line has "inv:"; open a host → file has a
# **view** bullet; "n" + text → **note**; "F" on a journal row → under ## Findings; "T" shows the Markdown
```

## Pitfalls
- Do not record every keystroke or every table refresh; the timeline must stay readable. Only the events listed.
- The investigation root can be inside a git repo the user edits; never rewrite more of the file than needed.
