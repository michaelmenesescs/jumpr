# Task 165 — Kubernetes collector: ingress → service → endpoints → pods → nodes → hosts

- **Milestone:** M15
- **Depends on:** 161, 106, 111
- **Parallel group:** M15-A (with 162)
- **Status:** todo

## Context
SPEC §23.2 (kubernetes row). BUILD-PLAN §14.3. The k8s backend's list/informer access and the task-111 node ↔ host
pivot.

## Files
- create `internal/graph/collect/k8s.go`, `internal/graph/collect/k8s_test.go` (fake clientset)
- modify `internal/app/graph.go` (register `k8s`)

## Requirements
1. Reads Ingress, Service, EndpointSlice, Pod, Node, ServiceAccount through the k8s backend (no second client).
2. Edges: ingress `routes` service (attrs host, path, port), service `selects` endpoint/pod (from EndpointSlices,
   Observed; from selectors when slices are missing, Configured), pod `runs-on` node, node ↔ host (`managed-by`
   pivot already defined in 111), pod `as` serviceaccount, container nodes under pods.
3. Scope by namespace; cluster-scoped objects once. Respect the session's context and namespace.
4. Pod status from phase/conditions/restarts (CrashLoopBackOff → Crit).

## Acceptance
```
go test ./internal/graph/collect/ -run 'K8s' -v
make check
make lab-test   # TestLabK8sGraph: lab demo app ingress → service → 2 pods → nodes k3s-1/k3s-2 → hosts
```
