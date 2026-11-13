# Task 029 — SSH authentication and host key verification

- **Milestone:** M2
- **Depends on:** 028
- **Parallel group:** M2-B (with 030, 036, 038)
- **Status:** todo

## Goal
Build `ssh.ClientConfig` for a `HostSpec`: agent first, then key files; host keys checked against known_hosts with
an interactive accept for unknown keys.

## Context
SPEC §7.1 (auth order, host keys). BUILD-PLAN §4.8.1 (`interact`), §12 (risk: known_hosts key types).

## Files
- create `internal/interact/interact.go` (verbatim from BUILD-PLAN §4.8.1) and `internal/interact/static.go`
  (`Static(answers ...Answer) Prompter` for tests; `None()` which always returns `errs.Interaction`)
- create `internal/transport/ssh/auth.go`, `internal/transport/ssh/hostkeys.go`
- create `internal/transport/ssh/auth_test.go`, `internal/transport/ssh/hostkeys_test.go`

## Requirements
Auth (`auth.go`):
1. `type AuthOptions struct { AgentSocket func() string; Prompter interact.Prompter; ReadFile func(string)
   ([]byte, error) }` (`AgentSocket` is a function because the socket path changes on reattach).
2. `func AuthMethods(spec HostSpec, o AuthOptions) ([]ssh.AuthMethod, func())` — returns methods and a cleanup:
   - agent: unless `IdentitiesOnly`, if the socket path is non-empty and dialable (`net.Dial("unix", …)`, 2 s),
     `ssh.PublicKeysCallback(agent.NewClient(conn).Signers)`.
   - key files: `ssh.PublicKeysCallback` that lazily loads each `IdentityFiles` entry on first use;
     `*ssh.PassphraseMissingError` → ask the prompter (`interact.Passphrase`, title `Passphrase for <file>`), up
     to 3 tries; cache decrypted signers in memory per file for the process lifetime (a small `sync.Map` in an
     `Auth` struct is fine); unreadable or missing files are skipped.
   - with `IdentitiesOnly` and an agent: offer only agent keys whose public key matches a listed identity file's
     `.pub` (if the `.pub` is readable); otherwise key files only.

Host keys (`hostkeys.go`):
3. `type HostKeyOptions struct { Files []string; Strict string; Prompter interact.Prompter; Append func(file,
   line string) error }`.
4. `func HostKeyConfig(spec HostSpec, o HostKeyOptions) (cb ssh.HostKeyCallback, algos []string, err error)` using
   `github.com/skeema/knownhosts`: `kh, _ := knownhosts.NewDB(files...)`; `algos = kh.HostKeyAlgorithms(addr)`;
   callback behaviour:
   - known and matching → ok.
   - mismatch (`knownhosts.IsHostKeyChanged`) → `errs.HostKey`, message
     `host key for <host> changed (known_hosts line N); refusing`. Never overridable.
   - unknown (`knownhosts.IsHostUnknown`): `Strict == "yes"` → `errs.HostKey`; `"no"` or `"accept-new"` → accept
     and append; otherwise ask the prompter (`interact.HostKeyUnknown`, body with host, key type and
     `ssh.FingerprintSHA256`): `once` → accept in memory for this process; `always` → append a line
     (`knownhosts.Line`) to the first file via `Append`; `reject`/cancel → `errs.HostKey`.
   - missing known_hosts files are treated as empty.
5. The address used for lookup is `knownhosts.Normalize(net.JoinHostPort(HostName, port))`.

## Out of scope
Keyboard-interactive and password auth. Certificates beyond what the agent provides. The TUI dialog (051).

## Acceptance
```
go test -race ./internal/transport/ssh/ -run 'Auth|HostKey' -v
# auth: unencrypted ed25519 key file works (generate keys in the test with crypto/ed25519 + ssh.MarshalPrivateKey);
#   encrypted key asks the prompter and caches; wrong passphrase 3 times → no signer; in-process agent
#   (agent.NewKeyring served on a temp unix socket) is used first; IdentitiesOnly filters agent keys
# hostkeys: known ok; changed → errs.HostKey; unknown+strict yes → error; unknown+prompt "always" appends a line
#   that a second DB load accepts; unknown+no prompter → errs.Interaction mapped to errs.HostKey;
#   ed25519-only known_hosts entry → algos contains only ed25519 types
```

## Pitfalls
- Generate test keys at test time; never commit private keys, not even test ones.
- With plain `x/crypto/ssh/knownhosts` a server offering RSA first fails as "mismatch" when only ed25519 is
  known. Returning `algos` and setting `ClientConfig.HostKeyAlgorithms` (done in task 031) is the fix; do not
  skip it.
- Unix socket paths in tests: use `os.MkdirTemp("/tmp", …)`; `t.TempDir()` paths can exceed the 103-byte limit on
  macOS.
