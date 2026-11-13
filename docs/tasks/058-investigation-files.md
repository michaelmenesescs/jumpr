# Task 058 — Investigation and audit Markdown files

- **Milestone:** M3
- **Depends on:** 054
- **Parallel group:** M3-B (with 056)
- **Status:** todo

## Goal
Read and write the plain-Markdown records exactly in the format of SPEC §10.

## Context
SPEC §10 (file layout and example), §11 step 4 (record). BUILD-PLAN §4.10. Mockup timeline screen.

## Files
- create `internal/investigation/file.go`, `internal/investigation/entry.go`, `internal/investigation/audit.go`
- create `internal/investigation/file_test.go`, `internal/investigation/testdata/golden-investigation.md`,
  `internal/investigation/testdata/golden-audit.md`

## Requirements
1. Types `EntryKind`, `Entry`, `Meta` verbatim from BUILD-PLAN §4.10.
2. `func Slug(title string) string` — lower-case, non-alphanumerics → `-`, collapsed, max 48 chars.
   `func NewID(now time.Time, title string) string` → `2026-11-12-api-5xx-ewr`.
3. `func Create(root string, m Meta) (path string, err error)` — writes
   `<root>/investigations/<id>.md` with YAML frontmatter (`id`, `status`, `opened` RFC 3339, `window` as a
   two-element list `[from, to|now]`, `targets` (list; fold more than 30 hosts as the first 30 plus a count
   comment), `alerts`, `tags`), then `# <Title>`, `## Timeline`, `## Findings`, `## Resolution`. Refuses to
   overwrite; an existing ID gets a `-2`, `-3` suffix.
4. `func FormatEntry(e Entry, red *redact.Redactor) string` — one bullet:
   `- 14:02:40 **probe** `failed-units` on 24 hosts → 22 identical, 2 differ` (time `15:04:05` local, kind bold,
   then `Text`; when `Actor != human` append ` by **<actor>**`). With `Output`: a `<details>` block exactly as in
   SPEC §10 (summary `output (N lines)`, fenced code, indented two spaces), output redacted and capped at 16 KiB.
   Backtick fences inside the output are neutralized (use a fence longer than any run of backticks in it).
5. `func AppendEntry(path string, e Entry, red *redact.Redactor) error` — inserts the bullet at the end of the
   `## Timeline` section (before the next `## ` heading); for `EntryFinding` also appends `- <text>` under
   `## Findings`. Read-modify-write with an atomic rename; an `flock` on the file serializes writers. If the user
   edited the file by hand and the heading is missing, append a new `## Timeline` at the end rather than failing.
6. `func ReadMeta(path string) (Meta, error)`, `func SetStatus(path, status, resolution string) error` (updates
   frontmatter `status`, adds `resolved:` timestamp, writes the text under `## Resolution`).
7. `func List(root string) ([]Meta, error)` — newest first.
8. Audit: `func AppendAudit(root string, e Entry, red *redact.Redactor, now time.Time) error` →
   `<root>/audit/YYYY-MM-DD.md`, created with `# Audit <date>` on first use, same bullet format.

## Out of scope
The `Log` object and UI (059). Knowledge notes (060).

## Acceptance
```
go test ./internal/investigation/ -run 'Slug|Create|Format|Append|ReadMeta|SetStatus|List|Audit' -v
# golden comparison (fixed clock, fixed zone) for a file after: create, probe entry, journal entry with 38 lines
# of output, action entry, note, finding, resolve; TestAppendToHandEditedFile; TestOutputWithBackticks;
# TestRedactionApplied (a bearer token in output is not in the file); TestConcurrentAppends (20 goroutines →
# 20 bullets, file valid)
```

## Pitfalls
- Frontmatter is parsed with `yaml.v3` into a struct; everything after the second `---` line is opaque text that
  must be preserved byte for byte except for the inserted bullet.
- `syscall.Flock` works on macOS without cgo; always unlock/close in a defer.
- Tests set `time.Local` expectations explicitly by formatting with a fixed `*time.Location`.
