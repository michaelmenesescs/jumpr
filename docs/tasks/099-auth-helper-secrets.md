# Task 099 — Auth helper protocol, secrets, example helper (Go)

- **Milestone:** M8
- **Depends on:** 098
- **Parallel group:** M8-A (with 100)
- **Status:** todo

## Goal
The enterprise extension point of SPEC §7.2: run a user command that returns headers/cookies; resolve
`{{ secret "name" }}`.

## Files
- create `internal/transport/httpx/helper.go`, `secrets.go`, `_test.go` files
- create `examples/auth-helper-cookie/main.go` (Go; reads a cookie value from the macOS Keychain or an env var and
  prints the helper JSON), `examples/auth-helper-cookie/README.md`
- modify `internal/app/rules_test.go` (allow `os/exec` in `internal/transport/httpx`)

## Requirements
1. Helper: `exec.CommandContext` with the configured argv (templated `{{ .URL }}`, `{{ .Profile }}`), no shell,
   60 s default timeout (the helper may open a browser), stdout = JSON `{headers, cookies[], expires_at}` per
   SPEC §7.2; stderr is shown to the user in the "waiting for auth helper…" dialog, never stored. Result cached
   in memory until `expires_at` or `cache_ttl`; concurrent requests share one helper run (`singleflight`).
2. Refresh: on 401/403, or a redirect whose `Location` matches `refresh_on` login patterns (the client does not
   follow redirects for helper profiles), invalidate, run the helper **once**, retry the request once, then
   return `errs.Auth` naming the profile.
3. While detached, a helper that needs interaction simply runs (it can open a browser on the laptop); its
   timeout applies.
4. Secrets: `{{ secret "name" }}` in profile strings resolves, in order: `Deps.Env["SITREP_SECRET_<NAME>"]`,
   macOS Keychain (`/usr/bin/security find-generic-password -s sitrep -a <name> -w`), a configured command
   (`secrets: {name: {command: [...]}}`). A literal credential in `connections.yaml` (`password:`/`token:` with a
   non-template value) produces a `:health` warning.
5. The example helper is a real Go program with its own `main`, documented in three lines, and built by
   `go build ./examples/...` in `make check`.

## Acceptance
```
go test -race ./internal/transport/httpx/ -run 'Helper|Secret' -v
# a helper binary is built in the test from a tiny Go file in testdata (go build) — or the test re-executes
# itself (TestMain with an env switch) to act as the helper; cases: helper JSON applied, cached, refreshed on 401
# exactly once, bad JSON → errs.Auth with profile name, timeout, cookies scoped by domain
go build ./examples/...
```

## Pitfalls
- No shell: the helper argv is executed directly. `~` in argv[0] is expanded by sitrep.
- The helper's stdout is a credential: do not log it, do not include it in errors.
