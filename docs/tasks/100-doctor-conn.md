# Task 100 — `jumper doctor conn`

- **Milestone:** M8
- **Depends on:** 098
- **Parallel group:** M8-A (with 099)
- **Status:** done

## Goal
The main tool for adapting jumper to an environment (SPEC §7.2 last bullet).

## Files
- modify `internal/app/doctor.go`, `internal/app/doctor_test.go`, `cmd/jumper/main.go`

## Requirements
`jumper doctor conn <profile> <url>` prints step by step with `ok`/`FAIL` and timings: profile found and valid;
proxy type and address (credentials redacted); DNS/connect through the proxy; TLS: negotiated version, peer
certificate subject, issuer, expiry, whether it chained to the system pool or the configured CA; auth type; for
helper profiles the helper's exit status, which header and cookie **names** it returned and `expires_at` (values
redacted); the request result: status code, `Content-Type`, redirect target if any, first 200 bytes of the body
(redacted). Exit code 0 only when the final status is 2xx. `--verbose` adds response headers (redacted).

## Acceptance
```
go test ./internal/app/ -run DoctorConn -v     # httptest TLS server with a test CA; helper profile; a 302-to-login
                                               # case reports "redirected to login: auth not accepted"
```

## Pitfalls
- This command prints to its writer from `internal/app` only; nothing sensitive may reach the terminal.
