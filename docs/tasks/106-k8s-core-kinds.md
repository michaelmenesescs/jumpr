# Task 106 — Kubernetes core kinds with informers

- **Milestone:** M9
- **Depends on:** 105
- **Parallel group:** — (serial)
- **Status:** todo

## Goal
k9s-equivalent live lists for the kinds in SPEC §8.2.

## Files
- create `internal/backend/k8s/informers.go`, `kinds.go`, `rows.go`, `rows_test.go`, `informers_test.go`

## Requirements
1. Kinds with aliases as in kubectl (`po`, `deploy`, `sts`, `ds`, `job`, `cj`, `no`, `svc`, `ep`, `ing`, `cm`,
   `secret`, `ev`, `ns`, `pvc`, `pv`): columns matching `kubectl get -o wide` essentials. Pods: `namespace`,
   `name`, `ready`, `status` (computed like kubectl: `CrashLoopBackOff`, `OOMKilled`, `Init:0/1`,
   `Terminating`, …), `restarts`, `age`, `ip`, `node`. Secrets: names, type and key names only; values are never
   read into `Fields` (use metadata-only informers: `metadatainformer`).
2. Shared informer factory per context, started lazily per kind on first `List`, stopped after 5 minutes without
   a subscriber. `List` returns the cache snapshot as one `Result`; `Watch` (implements `backend.Watcher`) emits
   add/modify/delete events; the table view applies them (add `Watch` support to `views.Table`: when the backend
   is a `Watcher` and the kind is `Watchable`, subscribe after the initial list; batch events with `NextBatch`).
3. Filters: session namespace, `Query.Selector` terms as label selectors where possible (`=`, `!=`, exists) and
   client-side for regex terms; `Args["node"]` → field `spec.nodeName`.
4. Status mapping for row colour: pods not Ready/Running → Warn; `CrashLoopBackOff`/`Error`/`OOMKilled`/
   `ImagePullBackOff` → Crit; nodes `NotReady` → Crit; events type `Warning` → Warn.
5. RBAC: a forbidden list for one kind shows `auth` for that kind only.

## Acceptance
```
go test -race ./internal/backend/k8s/ -run 'Rows|Informer|PodStatus' -v
# fake clientset (k8s.io/client-go/kubernetes/fake): pod status table (12 cases copied from kubectl's printer
# logic), watch events reach the channel, namespace filter, secrets expose no data
go test -race ./internal/tui/views/ -run 'TableWatch' -v
```

## Pitfalls
- Pod status computation is subtle (init containers, waiting reasons, deletion timestamp); port kubectl's logic
  carefully and test each case.
- Never block `List` waiting for cache sync longer than 10 s; return what is there with `Stale`.
