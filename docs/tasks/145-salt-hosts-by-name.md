# Task 145 — Salt hosts are dialled by an SSH-resolvable name, not a raw IP

- **Milestone:** M14
- **Depends on:** 069, 044
- **Parallel group:** M14-A (with 146, 147)
- **Status:** todo

## Goal
Every host that the Salt inventory source contributes is reachable over SSH exactly like the same host from
`ssh_config`. Today 5 lab tests fail with `ssh.dial: 192.168.139.104: ssh: handshake failed: … no supported methods
remain` (TestLabM7LocalState, TestLabSaltExecutorMatchesSSH, TestLabSaltDryRunGrouped, TestLabSaltApplyThroughBus,
and the grains inventory test logs `role=web matched 192.168.139.104`).

## Facts (measured 2026-11-14 against the lab)
- Lab minion grains: `id: web-02`, `fqdn: sr-web-02`, `host: sr-web-02` (no dot) → `hostID()` in
  `internal/backend/salt/source.go` picks `web-02`; `aliases()` adds `sr-web-02` and the `ipv4` addresses.
- `lab/ssh_config` names the host `web-02.lab.example.internal` (`HostName 192.168.139.104`) and gives its
  `IdentityFile` and `ProxyJump` only under `Host *.lab.example.internal`.
- The two inventory records therefore meet only through the IP alias, and the merged host ends up dialled by the
  IP, which no `Host` block matches, so no key and no ProxyJump apply.

## Files
- modify `internal/inventory/merge.go`, `internal/inventory/identity.go` (and their tests)
- modify `internal/backend/salt/source.go` only if the fix needs it
- add `internal/inventory/testdata/merge-salt-sshconfig/…`

## Requirements
1. Write the failing unit test first: an ssh_config source with `web-02.lab.example.internal → HostName
   192.0.2.104` and a Salt source with minion `web-02` (ipv4 `192.0.2.104`) must merge into **one** host whose ID is
   `web-02.lab.example.internal` and whose dial target is that ssh_config alias.
2. Merge rule: when records meet through any alias, the record from an `ssh_config` source supplies the ID and the
   dial alias (it is the only source ssh can resolve). Other sources contribute labels and aliases.
3. A Salt-only host (no ssh_config match) keeps its Salt ID, and `inventory.identity.rewrites` apply to it before the
   merge, so `{from: '^(web-\d+)$', to: '$1.lab.example.internal'}` makes it resolvable. Add that rewrite to
   `lab/jumpr/config.yaml` only if the merge rule alone does not fix the lab.
4. A raw IP is never chosen as an ID or a dial target when any non-IP alias exists.

## Acceptance
```
go test ./internal/inventory/ ./internal/backend/salt/ -v
make check
make lab-test   # TestLabSalt* and TestLabM7LocalState pass
```

## Pitfalls
- Do not special-case the lab domain in code. The rule is about source precedence, not names.
- Keep the inventory cache format readable by the previous version, or bump it the way task 044 versioned it.
