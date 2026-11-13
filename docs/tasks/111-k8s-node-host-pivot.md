# Task 111 — Node ↔ host pivot and the k8s inventory source

- **Milestone:** M9
- **Depends on:** 106
- **Parallel group:** M9-A (with 107, 108)
- **Status:** done

## Goal
SPEC W3: from a pod to its node as an SSH host and back.

## Files
- create `internal/backend/k8s/source.go`, `pivot.go`, `_test.go` files
- modify `internal/tui/commands_alert.go` (alerts with `namespace`/`pod` open the pods view)

## Requirements
1. Inventory source `k8s`: one host per node: aliases = node name, every `InternalIP`/`Hostname`/`ExternalIP`
   address, `kubernetes.io/hostname` label; labels `k8s.<label>` for a configurable allowlist (default: zone,
   region, instance-type, node-role keys, arch) plus `k8s.context`, `k8s.node`. Merging by alias joins it with the
   SSH and Salt records of the same machine.
2. `backend.LinkProvider` (from task 076): pod/node rows → `host` overview of the node (key `h`), units on it
   (kubelet/k3s/containerd), journal; host rows that are nodes → `pods on this node` (key `P`), node describe.
   When a node has no inventory match, the link still opens `:ssh`-able host by InternalIP only if
   `kubernetes.node_ssh_by_ip: true`; otherwise flash `node <n> is not in the inventory`.
3. Host overview gains a `KUBERNETES` line for nodes: ready state, pods count, not-ready pods.
4. Alert opener: alerts with `namespace` (+ `pod`) labels open the pods view filtered to them and still create
   the investigation; the investigation targets = the nodes of those pods.

## Acceptance
```
go test -race ./internal/backend/k8s/ -run 'Source|Pivot' -v
# inventory merge test: ssh host k3s-1.lab.example.internal + salt minion k3s-1 + k8s node k3s-1 (InternalIP)
# → one host with labels from all three
```

## Pitfalls
- Node names are often short names or cloud instance IDs; rely on the alias merge, not on string equality.
