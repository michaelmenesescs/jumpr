# Task 146 — `IdentityFile` accumulates across ssh_config files, like OpenSSH

- **Milestone:** M14
- **Depends on:** 028, 031
- **Parallel group:** M14-A
- **Status:** todo

## Goal
With `ssh.config_files: [~/.ssh/config, lab/ssh_config]`, the lab hosts failed authentication: `~/.ssh/config` has
`Host *` → `IdentityFile ~/.ssh/id_ed25519`, and `Resolver.getAll` (`internal/transport/ssh/sshconfig.go:72`)
returns the values of the **first** file that has any, so the lab key was never offered. OpenSSH merges
`IdentityFile` (and `CertificateFile`) from every matching block in every file, in order. The workaround in the
user's config (lab file first) must become unnecessary.

## Files
- modify `internal/transport/ssh/sshconfig.go`, `internal/transport/ssh/sshconfig_test.go`
- modify `internal/transport/ssh/auth.go`, `internal/transport/ssh/auth_test.go` only for requirement 3
- add `internal/transport/ssh/testdata/sshconfig/two-files/…`

## Requirements
1. `IdentityFile` and `CertificateFile` values are collected from every config file in `ssh.config_files` order,
   de-duplicated after token and `~` expansion, order kept. All other keys stay first-match-wins.
2. Test: two files, the first with `Host *` + key A, the second with `Host *.lab.example.internal` + key B →
   `IdentityFiles == [A, B]` for `web-01.lab.example.internal`, `[A]` for `other.example.internal`.
3. Check `auth.go` against OpenSSH semantics: without `IdentitiesOnly yes`, agent keys that match no
   `IdentityFile` are still offered (after the listed ones). Make it match, with a test, if it does not.
4. `jumpr doctor ssh <host> --verbose` lists the identity files in the order they will be tried.

## Acceptance
```
go test ./internal/transport/ssh/ -run 'SSHConfig|Auth' -v
make check
```
Manual: swap `ssh.config_files` back to `[~/.ssh/config, lab/ssh_config]` → `jumpr doctor` shows salt and chef ok.

## Pitfalls
- `kevinburke/ssh_config` `GetAll` is per file; the merge belongs in `Resolver`, not in a fork of the library.
- The OpenSSH limit on offered keys is enforced by servers (`MaxAuthTries`, default 6). Keep the order sane: listed
  files first, then agent keys.
