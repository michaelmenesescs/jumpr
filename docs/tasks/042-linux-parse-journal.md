# Task 042 — Linux parser: journal (JSON lines)

- **Milestone:** M2
- **Depends on:** 005
- **Parallel group:** M2-D (with 039, 040, 041, 043, 049)
- **Status:** done

## Goal
Parse `journalctl -o json` output line by line, tolerant of everything journald can emit.

## Context
SPEC §8.1 (`journal`).

## Files
- create `internal/backend/linux/parse_journal.go`, `internal/backend/linux/parse_journal_test.go`
- create `testdata/linux/journal/rocky9/nginx-fail.jsonl`

## Requirements
1. `type Entry struct { Time time.Time; Priority int; Source string; PID string; Unit string; Message string;
   Cursor string }`.
2. `func ParseJournalLine(line []byte) (Entry, error)`:
   - `__REALTIME_TIMESTAMP`: decimal microseconds since the epoch, as a string.
   - `PRIORITY`: string `"0"`…`"7"`; missing → 6.
   - `Source`: `SYSLOG_IDENTIFIER`, else `_COMM`, else `kernel` when `_TRANSPORT == "kernel"`, else `?`.
   - `Unit`: `_SYSTEMD_UNIT`, else `UNIT`.
   - `MESSAGE` can be a string, `null`, or an array of byte values (non-UTF-8 payload): decode arrays to bytes
     and replace invalid UTF-8 with `�`. Any field may also be an array of strings (repeated field): take the
     first.
   - Strip trailing newlines from the message; multi-line messages keep inner newlines.
3. `func ParseJournal(out []byte) ([]Entry, int)` — all parseable lines in order plus the number of skipped
   (unparseable) lines. Empty lines are ignored silently.
4. `func PriorityName(p int) string` → `emerg alert crit err warn notice info debug`;
   `func PriorityStatus(p int) resource.Status` (0–2 Crit… precisely: ≤3 → Crit, 4 → Warn, else OK).
5. `func JournalCommand(o JournalOptions) string` building the command with `shq.Join`:
   `journalctl -o json --no-pager` + `-u <unit>` (if set) + `-p <maxPriority>` (if < 7) +
   `--since @<epoch>` `--until @<epoch>` (from a `resource.TimeWindow` and `now`) + `-n <limit>` (default 500) +
   `-f` when `Follow` (then no `--until` and `-n 50`) + `-k` when `Kernel`. `JournalOptions.Sudo` is carried by
   the caller, not in the string.

## Out of scope
Running it, the view, follow plumbing (045).

## Acceptance
```
go test ./internal/backend/linux/ -run 'Journal|Priority' -v
# sample line must parse:
# {"__REALTIME_TIMESTAMP":"1794512467000000","PRIORITY":"3","SYSLOG_IDENTIFIER":"nginx","_PID":"2290",
#  "_SYSTEMD_UNIT":"nginx.service","MESSAGE":"bind() to 0.0.0.0:443 failed (98: Address already in use)"}
# plus: MESSAGE as byte array, MESSAGE null, missing PRIORITY, truncated last line (counted as skipped),
# JournalCommand table (unit+window, follow, kernel) with exact expected strings
```

## Pitfalls
- Decode into `map[string]json.RawMessage` and handle each field's shape; decoding into a struct with string
  fields fails on arrays.
- `-f` never ends: it is only valid with the Streamer, never with `Exec`.
