# Task 038 — Probe batch compiler and section splitter

- **Milestone:** M2
- **Depends on:** 026, 027
- **Parallel group:** M2-B (with 029, 030, 036)
- **Status:** todo

## Goal
Compile several small read-only commands into one script (one round trip per host) and split the reply back into
per-section output, exit code and classified error.

## Context
SPEC §7.1 (batch exec, `@@SITREP` markers, `needs-privilege`). BUILD-PLAN §7.

## Files
- create `internal/probe/probe.go`, `internal/probe/probe_test.go`

## Requirements
1. `type Section struct { ID string; Cmd string; Sudo bool }` (`ID` matches `^[a-z0-9_.-]+$`).
2. `type Batch struct { Nonce string; Sections []Section }`; `func NewBatch(nonce string, s ...Section) (Batch,
   error)` (duplicate or invalid IDs → `errs.Internal`). The nonce is 8 hex characters supplied by the caller
   (random in production, fixed in tests).
3. `func (b Batch) Script() string` — POSIX `sh`, no bashisms:
   ```
   b() { printf '\n@@SITREP:<nonce>:%s:B@@\n' "$1"; printf '\n@@SITREP:<nonce>:%s:B@@\n' "$1" >&2; }
   e() { printf '\n@@SITREP:<nonce>:%s:E:%s@@\n' "$1" "$2"; printf '\n@@SITREP:<nonce>:%s:E:%s@@\n' "$1" "$2" >&2; }
   b loadavg; cat /proc/loadavg; e loadavg $?
   b sockets; sudo -n -- sh -c 'ss -H -tuanp'; e sockets $?
   ```
   A section's `Cmd` is emitted as written when `Sudo` is false and as `sudo -n -- sh -c <shq.Quote(Cmd)>` when
   true. The whole script is meant to run with `transport.Command{Script: b.Script(), Sudo: false}`.
4. `type SectionResult struct { ID string; Stdout, Stderr string; RC int; Err error; Missing bool }`.
5. `func (b Batch) Split(stdout, stderr []byte) map[string]SectionResult` — parse both streams by markers with the
   batch nonce only. For each section: text between its `B` and `E` markers, minus the single leading and
   trailing newline the markers add. `RC` from the `E` marker of stdout. `Err = shq.Classify(RC, Stderr,
   section.Sudo)`. A section with no `B` marker, or `B` without `E` (script died or output truncated) →
   `Missing = true`, `Err = errs.New(errs.Remote, "probe", "no result (truncated or aborted)")`. Every section of
   the batch has an entry in the map.
6. `func (b Batch) Command(timeout time.Duration) transport.Command`.

## Out of scope
Running batches (044). Which probes exist (039–043).

## Acceptance
```
go test ./internal/probe/ -v
# TestScriptShape (golden string for a 2-section batch with nonce "deadbeef"),
# TestSplitRoundTripLocal: run Script() locally with `sh` via os/exec IN THE TEST (sections: `echo a`,
#   `echo err >&2; exit 3`, `printf 'no newline'`, a missing command) and assert Split results, incl. rc 127 →
#   errs.NotAvailable, exact stdout without added newlines
# TestSplitTruncated (cut stdout mid-section → Missing), TestSplitIgnoresForeignMarkers (other nonce in output),
# TestSudoSectionClassified ("sudo: a password is required" in stderr → errs.Sudo)
```

## Pitfalls
- `$?` must be read immediately after the command; do not put anything between the command and `e id $?`.
- A command ending without a newline: the marker's leading `\n` guarantees the marker starts a line; strip
  exactly one newline on each side, not all whitespace.
- A section command must not contain an unterminated quote or heredoc; reject `Cmd` values containing `<<`.
