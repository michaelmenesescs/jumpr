# Task 053 — `@@ capture`: golden files from the lab (Go tool)

- **Milestone:** M2
- **Depends on:** 035
- **Parallel group:** M2-F (with 051)
- **Status:** done

## Goal
A Go command that records real command output from the lab's three distros into `testdata/`, sanitized, so parser
tests run against reality.

## Context
BUILD-PLAN D13 (all helper tooling is Go), §9 (parsers). SPEC §18, §22.

## Files
- create `cmd/@@/main.go` (subcommand dispatch: `capture`, `sanitize`)
- create `internal/devtools/capture.go`, `internal/devtools/sanitize.go`, `internal/devtools/sanitize_test.go`
- create `internal/backend/linux/golden_test.go`
- add files under `testdata/linux/<probe>/<distro>/…` (generated)

## Requirements
1. `go run ./cmd/@@ capture linux [--config-dir lab/jumper] [--out testdata/linux]`: for one host per
   distro (`web-01` → `rocky9`, `db-01` → `ubuntu2204`, `web-03` → `ubuntu2404`; mapping in a table at the top of
   `capture.go`) run each raw command of the Linux probes (take them from `linux` package exports: add
   `func RawCommands() map[string]linux.Section` to `probes.go`) through the SSH transport and write stdout to
   `testdata/linux/<probe>/<distro>/<name>.txt`. Print one line per file written. Exit non-zero if any host
   fails.
2. `sanitize.go`: `func Sanitize(b []byte, hostMap map[string]string) []byte` — replaces real lab machine names
   (`sr-web-01` etc. and their OrbStack domains) with the `*.lab.example.internal` aliases, MAC addresses with
   `02:00:00:00:00:NN`, IPv4 addresses outside documentation ranges with stable `192.0.2.N` mappings (same input
   IP → same output IP within one run), machine IDs / boot IDs / UUIDs with zeros, SSH key fingerprints with
   `SHA256:REDACTED`, and the local macOS username with `sre`. Deterministic output for the same input.
   `capture` always sanitizes before writing. `go run ./cmd/@@ sanitize <file…>` rewrites files in place.
3. `golden_test.go`: for every directory under `testdata/linux/<probe>/*/`, run the matching parser and assert
   invariants that hold on any healthy host (units list contains `sshd.service` or `ssh.service`; meminfo total
   > 0; ps has PID 1; ss has a listener on 22; os-release has an ID; journal lines parse with ≤ 1 % skipped).
   The test passes (with `t.Skip`) when a distro directory does not exist yet.
4. Run the capture against the lab and commit the generated files in this task. If the lab is not available,
   commit the tool and tests only and note it in `docs/HANDOFF.md`.

## Out of scope
Salt captures (067). Updating expected values in existing parser tests.

## Acceptance
```
go test ./internal/devtools/ -v           # sanitize table: IPs stable-mapped, MACs, UUIDs, hostnames, idempotent
go build -o /dev/null ./cmd/@@
go run ./cmd/@@ capture linux --config-dir lab/jumper     # lab up: writes files, exit 0
go test ./internal/backend/linux/ -run Golden -v
git grep -nE '([0-9a-f]{2}:){5}[0-9a-f]{2}' testdata/ | grep -v '02:00:00:00:00' ; test $? -eq 1   # no real MACs
```

## Pitfalls
- This is a Go program, not a shell script. No `.sh`, no Python.
- `cmd/@@` may use `os.Stdout`/`os.Args` (it is under `cmd/`); `internal/devtools` takes writers as
  parameters like everything else under `internal/`.
- Review the diff of `testdata/` before committing: no usernames, no real domains, nothing from outside the lab.
