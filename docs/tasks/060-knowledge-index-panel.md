# Task 060 — Knowledge index and side panel

- **Milestone:** M3
- **Depends on:** 059
- **Parallel group:** M3-C (with 061)
- **Status:** todo

## Goal
"Have I seen this before?" — notes and past investigations matching the current host, unit or alert appear in a
side panel.

## Context
SPEC §5 W6, §10 (knowledge resurfacing). Mockup knowledge panel (path, why it matched, snippet).

## Files
- create `internal/investigation/knowledge.go`, `internal/investigation/knowledge_test.go`,
  `internal/investigation/testdata/kb/…` (a few notes)
- create `internal/tui/sidepanel.go`, `internal/tui/sidepanel_test.go`
- modify `internal/tui/viewpane.go` (panel on the right, `K` toggles), `internal/tui/commands_inv.go`
  (`:kb new <type> <name>`, `:kb reindex`)

## Requirements
1. Index: `func BuildIndex(root string) (*Index, error)` walks `<root>/knowledge/**.md` and
   `<root>/investigations/*.md`. Per document: frontmatter lists `hosts`, `services`, `alerts`, `tags` (all
   optional), the title (first `# ` heading), a snippet (first non-empty paragraph, 160 chars), status for
   investigations, and a lower-cased token set of the body. Path conventions add implicit keys:
   `knowledge/hosts/<h>.md` → host `<h>`; `services/<s>.md` → service; `alerts/<a>.md` → alert.
2. `func (ix *Index) Match(s Subject) []Hit` where `Subject { Hosts []string; Unit, Alert string; Tags []string }`
   (a local type; the TUI converts) and `Hit { Path, Title, Why, Snippet string; Score int; Investigation bool;
   Status string }`. Scoring: alert name match 100; unit/service match 80 (unit `nginx.service` matches service
   `nginx`); host match 60 (FQDN or short name); tag match 30; body token match 10 each (max 30). `Why` is the
   strongest reason (`alert name`, `unit nginx.service`, `host`, `tag api`, `text`). Sorted by score, then newest
   file first; at most 20.
3. `func (ix *Index) Update(path string) error` re-reads one file (called after appends and resolves).
4. Side panel: 34 cells wide, shown in a view pane when the pane interior is at least 90 wide and `K` is on
   (default on); title `Knowledge (N)`; each hit: path (accent), why (dim), snippet wrapped to 2 lines. `tab`
   moves focus into the panel, `enter` opens the note in a `Text` view, `esc`/`tab` returns.
5. Matching runs in a `tea.Cmd` whenever the top view's `Subject()` changes (compare with the previous subject;
   debounce 150 ms).
6. `:kb new host|service|alert|runbook <name>` creates a note file from a template with frontmatter and opens it
   in a text view (editing is done in the user's editor outside sitrep for now; flash the path).
7. On `:inv resolve`, offer (y/N dialog) to append the root-cause line to `knowledge/alerts/<alert>.md` or
   `knowledge/services/<unit>.md` when the investigation has one.

## Out of scope
Full-text search UI. Embeddings. Editing inside sitrep.

## Acceptance
```
go test ./internal/investigation/ -run 'Index|Match|Knowledge' -v
# TestMatchAlert, TestMatchUnitToService, TestMatchHostShortName, TestPathConventions, TestRanking, TestUpdate
go test -race ./internal/tui/ -run 'SidePanel|Knowledge' -v
# TestPanelShowsHitsForSubject, TestPanelHiddenWhenNarrow, TestToggleK
```

## Pitfalls
- Building the index must not delay the first frame: build it in a `tea.Cmd` at startup.
- A broken frontmatter block in one note must not break the index: skip the frontmatter, still index the body.
