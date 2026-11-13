# Task 112 — Generic resources (CRDs), edit, and M9 lab tests

- **Milestone:** M9
- **Depends on:** 107, 108, 109, 110, 111
- **Parallel group:** — (serial; M9 exit)
- **Status:** todo

## Goal
Any API resource is browsable; the lab proves W3.

## Files
- create `internal/backend/k8s/dynamic.go`, `dynamic_test.go`, `lab_test.go` (`//go:build lab`)
- modify `internal/app/core.go`, `lab/sitrep/config.yaml` (`kubernetes.kubeconfig: lab/.keys/kubeconfig`),
  `docs/HANDOFF.md`, `README.md`

## Requirements
1. Discovery (cached 10 min): every listable resource becomes a kind (name = plural, aliases = short names and
   singular; conflicts with built-in kinds are skipped). Columns from the server-side Table API
   (`Accept: application/json;as=Table;v=v1;g=meta.k8s.io`) so CRD printer columns work. Describe/YAML via 108.
   `:kinds` groups them by API group.
2. Edit (`e`): opens the YAML in a text editor pane only if local panes exist; otherwise v1 offers
   `k8s.apply-patch`: the user pastes or types a JSON merge patch in a text prompt; the plan shows a
   server-side dry-run diff; apply through the bus.
3. Lab tests (kubeconfig context `sitrep-lab`): nodes Ready = 2; fault `oom-pod` → a pod reaches
   `CrashLoopBackOff`/`OOMKilled` status in the pods kind; previous logs non-empty; node of that pod resolves to
   an inventory host and its journal (kernel) contains an OOM line; rollout restart of a test deployment through
   the bus; exec `echo ok` in a pod shell; cordon/uncordon `k3s-2`.

## Acceptance
```
go test -race ./internal/backend/k8s/ -run Dynamic -v
make check && make lab-test
```

## Pitfalls
- The Table API returns cells as mixed JSON types; render with `fmt.Sprint` and right-align numbers.
