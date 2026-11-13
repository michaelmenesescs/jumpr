# Task 039 — Inventory: hosts, sources, merge, identity, cache

- **Milestone:** M2
- **Depends on:** 028
- **Parallel group:** M2-D (with 040, 041, 042, 043, 049)
- **Status:** done

## Goal
One merged host list from several sources, with stable identities and labels, cached on disk.

## Context
BUILD-PLAN §4.7. SPEC §9.

## Files
- create `internal/inventory/inventory.go` (types, `Store` implementing `Inventory`)
- create `internal/inventory/identity.go`, `internal/inventory/merge.go`, `internal/inventory/cache.go`
- create `internal/inventory/source_sshconfig.go`, `internal/inventory/source_static.go`
- create `internal/inventory/inventory_test.go`, `internal/inventory/testdata/hosts.yaml`

## Requirements
1. Types verbatim from BUILD-PLAN §4.7.
2. Identity: `type Rewrite struct { From *regexp.Regexp; To string }`;
   `func Canonical(name string, rewrites []Rewrite) resource.HostID` — lower-case, trim trailing dot, apply
   rewrites in order (`ReplaceAllString`), result is the ID.
3. Merge: hosts from different sources are the same machine when their canonical ID is equal **or** when one
   host's ID or alias equals another's ID or alias (after canonicalization), or the short name (before the first
   dot) of one equals the full ID of the other. Merged host: ID = the longest FQDN-looking name among the
   candidates (ties: lexical); `Addr` from the first source that provides one (ssh_config first); aliases =
   union; labels: every source label stored as `<source>.<key>`; an unprefixed `<key>` is also set when only one
   distinct value exists across sources; `host` label = ID; `Sources` sorted.
4. `Store`: `func New(sources []Source, rewrites []Rewrite, cachePath string, now func() time.Time) *Store`.
   - `Refresh`: load all sources concurrently (each with a 30 s timeout); failed sources keep their previous
     hosts (from the last successful load or the cache) and are reported in the returned map; then merge and
     swap atomically; write the cache.
   - `Hosts`, `Select`, `Resolve` (ID, alias or short name; ambiguous short name → not found), `Age`.
   - `LoadCache() error`: read `cachePath` (JSON: per-source host lists + timestamps); used at startup for an
     instant first frame. Missing file is fine.
5. ssh_config source: `Name() "ssh"`; one host per `Resolver.Hosts()` entry; `Addr` = alias; aliases include the
   `HostName` when different; labels: `user`, and `jump` (first hop) when present. Hosts listed in
   `ExcludePatterns` (globs, e.g. bastions) are skipped — option on the source.
6. static source: `Name() "static"`; YAML file:
   ```yaml
   hosts:
     - name: web-01.lab.example.internal
       addr: web-01.lab.example.internal    # optional
       aliases: [web-01]
       labels: {role: web, dc: lab1}
   ```
   Also accept a plain text file (one host per line, `#` comments) when the path does not end in `.yaml`/`.yml`.

## Out of scope
Salt, k8s, Prometheus, Chef sources (069, 111, 103, 113). Background refresh scheduling (052).

## Acceptance
```
go test -race ./internal/inventory/ -v
# TestCanonicalRewrites (node_exporter :9100, ip-10-1-2-3 → 10.1.2.3), TestMergeByAliasAndShortName,
# TestLabelsNamespacedAndShortcuts (conflicting values → no unprefixed key), TestSelect (role=web),
# TestResolveAmbiguousShortName, TestRefreshKeepsFailedSource, TestCacheRoundTrip,
# TestSSHConfigSource (with a testdata config), TestStaticSourceYAMLAndPlain
```

## Pitfalls
- Merging is transitive (A~B via alias, B~C via short name): use union-find or iterate to a fixed point.
- `Hosts()` returns copies; callers must not be able to mutate the store's maps.
