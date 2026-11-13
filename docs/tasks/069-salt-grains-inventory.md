# Task 069 — Grains as an inventory source

- **Milestone:** M4
- **Depends on:** 066, 039
- **Parallel group:** M4-B (with 068, 071)
- **Status:** todo

## Goal
Hosts and labels (`salt.role`, `salt.dc`, `salt.os`, …) from the master's grains cache, without touching minions.

## Context
SPEC §8.6 (grains), §9 (sources, label namespacing).

## Files
- create `internal/backend/salt/grains.go`, `internal/backend/salt/grains_test.go`
- create `internal/backend/salt/source.go` implementing `inventory.Source` (the source lives in the salt package
  so that `inventory` never imports a backend)

## Requirements
1. `func (r *Runner) CachedGrains(ctx) (map[string]Grains, error)` — `salt-run cache.grains tgt='*'` (kwarg
   `tgt`); `type Grains map[string]json.RawMessage` with helpers `Str(key)`, `StrList(key)`, and dotted access for
   nested maps is not needed.
2. `func Labels(g Grains, extra []string) map[string]string` — always: `id`, `os`, `os_family`, `osrelease`,
   `osfinger`, `kernelrelease`, `saltversion`, `virtual`, `num_cpus`, `mem_total`; plus custom grains: every
   top-level grain whose value is a string, number or bool and whose key is not in a built-in denylist of noisy
   grains (`path`, `pythonpath`, `gpus`, `hwaddr_interfaces`, `ip_interfaces`, `ip4_interfaces`,
   `ip6_interfaces`, `dns`, `locale_info`, `systemd`, `disks`, `ssds`, `server_id`, `machine_id`, `uuid`,
   `serialnumber`, `saltpath`, `pythonexecutable`, `pythonversion`, `zmqversion`, `lsb_distrib_*`, `cpu_flags`,
   `cpu_model`, `biosversion`, `biosreleasedate`, `productname`, `manufacturer`, `master`, `shell`, `ps`,
   `groupname`, `username`, `gid`, `uid`, `pid`, `cwd`, `transactional`, `efi*`, `init`); list-valued custom
   grains (e.g. `roles: [web, api]`) become a comma-joined value. Values longer than 64 chars are dropped.
3. `Source`: `Name() "salt"`; one `inventory.Host` per minion in the grains cache: ID candidate = `fqdn` grain if
   it contains a dot and is not `localhost…`, else the minion id; aliases: minion id, `host` grain, `fqdn`,
   every address in `ipv4` except loopback; labels from `Labels`; `Addr` empty (ssh_config or the FQDN decides).
4. `func KnownGrainKeys(all map[string]Grains) map[string]bool` — keys present on at least one minion with a
   scalar or list-of-scalars value; used by the targeting compiler (070) to decide whether `role=web` can become
   `G@role:web`.
5. Empty grains cache (`{}`) → no hosts, no error (flash explains: `salt grains cache is empty; run
   saltutil.sync_grains or enable grains cache`).

## Out of scope
Per-minion `grains.items` (094). Pillar.

## Acceptance
```
go test ./internal/backend/salt/ -run 'Grains|Labels|Source|KnownGrain' -v
# TestLabelsBuiltinsAndCustom (role, dc present; path, ip_interfaces absent), TestListGrainJoined,
# TestSourceBuildsHosts (fqdn vs id, aliases include IPs, no loopback), TestEmptyCache
# plus inventory integration: ssh_config host "web-01.lab.example.internal" and minion "web-01" merge into one
# host that has both ssh.user and salt.role, and unprefixed role=web
```

## Pitfalls
- Grain values are arbitrary JSON; decode lazily per key and ignore anything that is not what you expect.
- Do not put secrets-looking grains into labels; the 64-char cap and denylist are the guard.
