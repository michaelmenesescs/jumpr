# Task 002 — Error taxonomy (`internal/errs`)

- **Milestone:** M0
- **Depends on:** 001
- **Parallel group:** — (serial; everything imports it)
- **Status:** todo

## Goal
The `errs` package exactly as written in BUILD-PLAN §4.1.

## Context
BUILD-PLAN §4.1, §7. SPEC §15 (failure isolation).

## Files
- create `internal/errs/errs.go`
- create `internal/errs/errs_test.go`

## Requirements
1. Copy the `Kind` constants, `Error` struct and function signatures from BUILD-PLAN §4.1 verbatim.
2. `(*Error).Error()` format: `<op>: <host>: <msg>: <cause>`; parts that are empty are skipped (and their
   colon). If `Msg` is empty and `Err` is nil, use the kind as the message. Example:
   `ssh.dial: web-01: connection refused`.
3. `New` and `Wrap` return `*Error`. `Wrap(kind, op, nil)` returns a non-nil `*Error` with `Err == nil`
   (callers only wrap real errors; do not add nil magic).
4. `WithHost` returns a **copy**; it must not mutate the receiver.
5. `KindOf`: uses `errors.As` for `*Error`; else `errors.Is(err, context.DeadlineExceeded)` → `Timeout`;
   `errors.Is(err, context.Canceled)` → `Canceled`; else `Internal`; `nil` → `""`.
6. Add `func Is(err error, kind Kind) bool { return KindOf(err) == kind }`.

## Out of scope
Classifying SSH or remote stderr text (tasks 033, 027).

## Acceptance
```
go test ./internal/errs/ -run . -v     # tests cover: Error() formats, Unwrap via errors.Is, WithHost copy,
                                       # KindOf for *Error / wrapped *Error (fmt.Errorf %w) / ctx errors / nil
make lint
```

## Pitfalls
- A wrapped `*Error` inside `fmt.Errorf("x: %w", e)` must still be found by `KindOf` (use `errors.As`).
- Do not define sentinel error variables; kinds are the only classification.
