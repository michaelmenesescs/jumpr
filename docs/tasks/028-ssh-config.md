# Task 028 — Resolve hosts from ssh_config

- **Milestone:** M2
- **Depends on:** 003
- **Parallel group:** M2-A (with 026, 027, 037)
- **Status:** done

## Goal
Given a host name, produce everything needed to dial it, including the ProxyJump chain, from one or more
ssh_config files.

## Context
SPEC §7.1 (supported keywords). The lab's `lab/ssh_config` uses `Host`, `HostName`, `User`, `Port`,
`IdentityFile`, `ProxyJump`, `UserKnownHostsFile`, `IdentitiesOnly`.

## Files
- create `internal/transport/ssh/sshconfig.go`, `internal/transport/ssh/sshconfig_test.go`
- create `internal/transport/ssh/testdata/config_basic`, `config_jump_chain`, `config_wildcards`

## Requirements
1. `type HostSpec struct { Alias, HostName, User string; Port int; IdentityFiles []string; IdentitiesOnly bool;
   KnownHostsFiles []string; StrictHostKeyChecking string; ConnectTimeout time.Duration; ServerAliveInterval
   time.Duration; Jumps []string; ProxyCommand string }`.
2. `type Resolver struct` built by `func NewResolver(files []string, home, defaultUser string) (*Resolver, error)`
   using `github.com/kevinburke/ssh_config` (`ssh_config.Decode` per file). Missing files are skipped; a file
   that fails to parse is an `errs.Config` error naming it.
3. `func (r *Resolver) Resolve(alias string) (HostSpec, error)`:
   - first file that has a value wins per keyword (like OpenSSH), falling back to defaults: `HostName` = alias,
     `User` = `defaultUser`, `Port` = 22, `IdentityFiles` = none, `StrictHostKeyChecking` = `ask`,
     `KnownHostsFiles` = `~/.ssh/known_hosts`.
   - expand `~` and `%h` (hostname), `%r` (user), `%p` (port), `%%` in `HostName`, `IdentityFile`,
     `UserKnownHostsFile`.
   - `ProxyJump`: comma-separated chain, each element `[user@]host[:port]`; `none` → empty. Stored in order
     (first hop first).
   - `ProxyCommand` set (and not `none`) → returned in the spec; the dialer rejects it later with a clear message.
4. `func (r *Resolver) Chain(alias string) ([]HostSpec, error)` — the specs to dial in order, ending with the
   target; each jump element is itself resolved through the config (so a jump can have its own jump). Detect
   loops and chains longer than 5 → `errs.Config`.
5. `func (r *Resolver) Hosts() []string` — all literal (non-wildcard, non-negated) `Host` patterns across files,
   de-duplicated, sorted. Used by the inventory.

## Out of scope
`Match` blocks (unsupported by the library; ignore). `Include` works through the library when paths are
resolvable. Certificates. Dialing.

## Acceptance
```
go test ./internal/transport/ssh/ -run 'Resolve|Chain|Hosts' -v
# TestResolveDefaults, TestResolveWildcardHostBlock ("Host *.lab.example.internal" with User sre),
# TestResolveIdentityTildeAndTokens, TestChainSingleJump, TestChainTwoJumps, TestChainLoopError,
# TestProxyJumpNone, TestHostsSkipsWildcards, TestFirstFileWins
```

## Pitfalls
- `ssh_config.Get` reads the real `~/.ssh/config`. Do not use the package-level functions; decode the given
  files and call methods on the returned `*ssh_config.Config`.
- The library returns `""` for unset keys and has built-in defaults for some (`Port` → `"22"`); use
  `cfg.Get(alias, key)` and treat empty as unset.
- `IdentityFile` can occur several times: use `GetAll`.
