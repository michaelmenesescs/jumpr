# Task 054 — Secret redaction

- **Milestone:** M3
- **Depends on:** 002
- **Parallel group:** M3-A (with 055)
- **Status:** todo

## Goal
One function every record, log line and error message passes through before it is stored or shown.

## Context
SPEC §10 (redaction), §16. BUILD-PLAN §7.

## Files
- create `internal/redact/redact.go`, `internal/redact/redact_test.go`

## Requirements
1. `type Redactor struct`; `func New(extra []string) (*Redactor, error)` (extra user regexps; a bad pattern →
   `errs.Config`); `func (r *Redactor) String(s string) string`; `func (r *Redactor) Bytes(b []byte) []byte`.
2. Built-in patterns, each replaced by `‹redacted:<name>›` (keep the key, redact the value where there is one):
   - `bearer`: `(?i)(authorization:\s*bearer\s+)\S+` and `(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`
   - `basic`: `(?i)(authorization:\s*basic\s+)\S+`
   - `aws-key`: `\b(AKIA|ASIA)[0-9A-Z]{16}\b`; `aws-secret`: `(?i)(aws_secret_access_key\s*[=:]\s*)\S+`
   - `private-key`: everything from `-----BEGIN [A-Z ]*PRIVATE KEY-----` to the matching `-----END …-----`
   - `password`: `(?i)\b(pass(word|wd)?|secret|token|api[_-]?key)(\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)` → key and
     separator kept
   - `url-cred`: `://[^/\s:@]+:[^/\s@]+@` → `://‹redacted:url-cred›@`
   - `jwt`: `\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`
   - `cookie`: `(?i)((set-)?cookie:\s*)[^\r\n]+`
3. Idempotent: `String(String(s)) == String(s)`. Safe for concurrent use.
4. `func (r *Redactor) Cap(s string, maxBytes int) string` — redact, then cut on a line boundary and append
   `… (N more lines)`.

## Out of scope
Pillar redaction (093 has its own rule: redact every value).

## Acceptance
```
go test ./internal/redact/ -v     # one positive and one negative case per pattern; idempotence; Cap; a 1 MiB
                                  # input finishes in < 200 ms
```

## Pitfalls
- Go regexp has no backreferences or lookaheads; use capture groups and `ReplaceAllString` with `${1}`.
- `password=` inside a URL query and `PasswordAuthentication yes` (sshd config) are the classic false negative
  and false positive; include both in the tests (the second must **not** be redacted: require `[=:]`).
