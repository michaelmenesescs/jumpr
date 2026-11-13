# Task 108 — Describe and YAML views

- **Milestone:** M9
- **Depends on:** 106
- **Parallel group:** M9-A (with 107, 111)
- **Status:** done

## Goal
`d` describe-equivalent and `y` YAML for any object (SPEC §8.2).

## Files
- create `internal/backend/k8s/describe.go`, `describe_test.go`, `internal/tui/views/k8sobject.go`

## Requirements
1. YAML: fetch through the dynamic client, drop `metadata.managedFields`, marshal with `sigs.k8s.io/yaml`.
   Secrets: `data`/`stringData` values replaced by `‹redacted N bytes›` unless revealed with the same explicit
   confirm-and-do-not-record flow as pillar values (task 093).
2. Describe: hand-written summaries for pod (containers, images, state, last state with exit code and reason,
   limits/requests, probes, node, QoS, conditions), deployment/sts/ds (replicas, strategy, conditions), node
   (conditions, capacity/allocatable, taints, pods count), service (type, ports, selector, endpoints) followed by
   the object's events (`involvedObject.uid`), newest last. Other kinds: metadata + conditions + events.
3. Tabs: `describe`, `yaml`, `events`. `r` refresh.

## Acceptance
```
go test ./internal/backend/k8s/ -run 'Describe|YAML' -v     # golden text for a crash-looping pod fixture; secret
                                                            # values absent from YAML
```

## Pitfalls
- Do not import `k8s.io/kubectl/pkg/describe`.
