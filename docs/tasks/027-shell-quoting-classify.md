# Task 027 — Shell quoting, sudo wrapping, remote error classification

- **Milestone:** M2
- **Depends on:** 002
- **Parallel group:** M2-A (with 026, 028, 037)
- **Status:** done

## Goal
The single place where remote command lines are assembled and where remote failures are classified.

## Context
SPEC §7.1 (sudo -n), §16 (strict quoting). BUILD-PLAN §7 (pattern table), §12 (risk: quoting).

## Files
- create `internal/shq/shq.go`, `internal/shq/shq_test.go`
- create `internal/shq/classify.go`, `internal/shq/classify_test.go`

## Requirements
1. `func Quote(s string) string` — POSIX single-quote quoting: wrap in `'…'`, replace each `'` with `'\''`. Empty
   string → `''`. Strings matching `^[A-Za-z0-9_@%+=:,./-]+$` are returned unquoted.
2. `func Join(args ...string) string` — `Quote` each, join with spaces.
3. `func Wrap(script string, sudo bool) string` — the exact line sent to the remote:
   - `sudo == false`: `sh -c <Quote(script)>`
   - `sudo == true`: `sudo -n -- sh -c <Quote(script)>`
   Always prefix the script with `export LC_ALL=C LANG=C; ` so parsers see stable output.
4. `func Classify(rc int, stderr string, sudo bool) error` — returns nil when nothing matched; otherwise
   `*errs.Error` per BUILD-PLAN §7: `Sudo` patterns (only when `sudo` is true); rc 127 or
   `command not found` → `NotAvailable`; rc 126 → `NotAvailable` (`permission denied`). Match on lower-cased
   stderr. The message is the first matching stderr line, trimmed to 200 characters.
5. `func ValidUnitName(s string) bool` (`^[A-Za-z0-9@:._\\-]+$`, max 256) and `func ValidSignal(s string) bool`
   (`TERM|KILL|HUP|INT|USR1|USR2|QUIT|STOP|CONT` or 1–64) for callers that put user input into commands.

## Out of scope
SSH handshake error classification (033). Salt quoting (073).

## Acceptance
```
go test ./internal/shq/ -v
# Quote table: empty, spaces, single quote, double quote, $VAR, backticks, newline, semicolon, unicode, "a'b'c"
# TestQuoteRoundTripLocal: for 500 random strings (printable + quotes + newlines) run locally
#   `sh -c "printf %s <Quote(s)>"` with os/exec IN THE TEST and compare output to s
# TestWrapSudo exact strings; Classify table incl. sudo patterns ignored when sudo=false
```

## Pitfalls
- Do not use `strconv.Quote` or `%q`; Go quoting is not shell quoting.
- `os/exec` is allowed in `_test.go` files only (the rules test skips test files).
- A NUL byte cannot be passed in an argument; `Quote` strips NULs.
