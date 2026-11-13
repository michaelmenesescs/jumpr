# Task 124 — `jumper init` and full `jumper doctor`

- **Milestone:** M12
- **Depends on:** 076
- **Parallel group:** M12-A (with 123, 125)
- **Status:** done

## Goal
First-run experience (SPEC §14).

## Files
- create `internal/app/init.go`, `init_test.go`, `internal/app/templates/*.yaml` (embedded with `go:embed`)
- modify `internal/app/doctor.go`, `cmd/jumper/main.go`

## Requirements
1. `jumper init [--config-dir d]` writes commented `config.yaml`, `connections.yaml`, `policy.yaml`,
   `keymap.yaml` and an empty `plugins/`; never overwrites (prints what it skipped); every key in BUILD-PLAN §11
   appears, commented with its default. A test loads the generated files with the real loaders.
2. `jumper doctor` (no args): config validity; ssh config readable; agent reachable and key count; each bastion
   found in the inventory: connect; one sample host per inventory source: echo + `sudo -n true`; each backend's
   `Health`; each connection profile referenced by a backend: reachability; knowledge root writable; session
   socket directory permissions; terminal: `TERM`, colour depth, inside tmux or not. Each line `ok|warn|FAIL` with
   a one-line fix hint. Exit 1 on any FAIL.

## Acceptance
```
go test ./internal/app/ -run 'Init|DoctorAll' -v
```

## Pitfalls
- Templates must stay in sync with `config.Default()`: the test compares the keys.
