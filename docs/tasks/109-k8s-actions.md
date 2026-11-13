# Task 109 — Kubernetes actions through the bus

- **Milestone:** M9
- **Depends on:** 106, 061
- **Parallel group:** M9-B (with 110)
- **Status:** todo

## Goal
delete, scale, rollout restart, cordon/uncordon/drain (SPEC §8.2) with plans and previews.

## Files
- create `internal/backend/k8s/actions.go`, `actions_test.go`

## Requirements
1. Specs `k8s.delete` (destructive), `k8s.scale` (param `replicas`), `k8s.rollout.restart`, `k8s.node.cordon`,
   `k8s.node.uncordon`, `k8s.node.drain` (destructive; params `grace`, `delete-emptydir-data yes|no`,
   `ignore-daemonsets yes`).
2. `Display` = the equivalent kubectl line (`kubectl --context c -n ns delete pod x`), `Via` = `k8s:<context>`.
   Targets carry labels `k8s.context`, `k8s.namespace` so policy selectors can protect namespaces
   (`protected: ["k8s.namespace=kube-system"]`).
3. Previews: delete → owner (`controlled by ReplicaSet x — it will be recreated` or `bare pod: it will be gone`);
   scale → current vs new replicas; rollout restart → pods affected and strategy; drain → the pods that would be
   evicted, those skipped (daemonsets, mirror pods) and those blocking (PDB, local storage) computed with a
   server-side **dry-run eviction** per pod.
4. Drain implementation: cordon, then evictions via the Eviction API with retries on 429 (PDB) up to the step
   timeout (10 min), honouring context cancel; never force-deletes.
5. A "target" for rollout purposes is one object; many pods selected → the bus's canary/batches apply.

## Acceptance
```
go test -race ./internal/backend/k8s/ -run 'Action|Plan|Drain' -v     # fake clientset with reactors: eviction
                                                                      # 429 then success; PDB-blocked preview
```

## Pitfalls
- `rollout restart` = patch `spec.template.metadata.annotations["kubectl.kubernetes.io/restartedAt"]`.
- Dry-run requests must set `DryRun: ["All"]`; verify in tests that non-dry-run calls happen only in `Step.Run`.
