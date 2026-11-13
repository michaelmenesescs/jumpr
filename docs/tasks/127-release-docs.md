# Task 127 — Keymap file, release build (Go tool), user documentation

- **Milestone:** M12
- **Depends on:** 124, 126
- **Parallel group:** — (serial; last task)
- **Status:** done

## Goal
Daily-driver ready: rebindable keys, a reproducible release, and docs a new user can follow.

## Files
- create `internal/tui/keymap/load.go`, `load_test.go`
- modify `cmd/@@/main.go`; create `internal/devtools/release.go`, `release_test.go`
- create `docs/USER-GUIDE.md`, `docs/KEYS.md`; modify `README.md`, `Makefile` (`dist`)

## Requirements
1. `keymap.yaml`: override any binding of `keymap.Map` and the mux key table by name; unknown names and
   conflicting keys are reported with line numbers; `:keys` shows the effective map; `docs/KEYS.md` is generated
   by a test with `-update` from the defaults (so it cannot drift).
2. `go run ./cmd/@@ release --version vX.Y.Z` (Go, no scripts): checks a clean tree and passing
   `make check`; builds `darwin/arm64` and `darwin/amd64` with `CGO_ENABLED=0 -trimpath -ldflags "-s -w -X
   main.version=…"`; writes `dist/jumper-<ver>-darwin-<arch>.tar.gz` with the binary, `README.md`, the example
   configs and plugins; writes `dist/SHA256SUMS`; prints sizes. `--sign <identity>` runs `codesign` when given
   (open question: signing identity). No upload, no tag push.
3. `docs/USER-GUIDE.md`: install, `jumper init`, pointing at ssh_config and a Salt master, the nine workflows of
   SPEC §5 as short walkthroughs, sessions and detach, safety model (bus, policy, read-only), investigations and
   knowledge files, connection profiles with the example helper, writing a plugin, troubleshooting with
   `doctor`. Neutral examples only.
4. `README.md`: what it is (the selling point sentence from SPEC §1), a 10-line quick start, links.

## Acceptance
```
go test ./internal/tui/keymap/ ./internal/devtools/ -v
go run ./cmd/@@ release --version v0.0.0-test --skip-checks && ls dist/ && shasum -a 256 -c dist/SHA256SUMS
make check
```

## Pitfalls
- The release tool must refuse to run with uncommitted changes unless `--skip-checks` is given (test-only flag).
- No employer names or real hosts in the docs; `example.internal` only.
