# Task 107 — Pod logs

- **Milestone:** M9
- **Depends on:** 106
- **Parallel group:** M9-A (with 108, 111)
- **Status:** done

## Goal
`l` / `Enter` on a pod: logs with follow, previous, container choice (SPEC §8.2; mockup pod logs).

## Files
- create `internal/backend/k8s/logs.go`, `logs_test.go`, `internal/tui/views/podlogs.go`, `podlogs_test.go`

## Requirements
1. `func (b *Backend) Logs(ctx, ns, pod string, o LogOptions) (<-chan LogLine, error)` using
   `CoreV1().Pods(ns).GetLogs(...).Stream`; options: container (default: first, or all containers merged with a
   `[container]` prefix), `Previous`, `Follow`, `SinceTime` from the session window, `TailLines` (default 500),
   `Timestamps: true` (parse the RFC 3339 prefix into `LogLine.Time`).
2. View (built on the text view): `w` follow, `p` toggle previous (auto-selected when the pod is in
   `CrashLoopBackOff`), `c` choose container, `/` filter, level colouring by a simple regex
   (`ERROR|FATAL|WARN|INFO|DEBUG`), `F` finding. Lines up to 64 KiB.
3. Multi-pod logs from a deployment/statefulset/daemonset row (`l`): up to 20 pods merged, prefixed by pod name.
4. Investigation entry `journal`-style: `logs <ns>/<pod> (N lines)`.

## Acceptance
```
go test -race ./internal/backend/k8s/ -run Logs -v      # httptest stream; follow cancel; previous flag set
go test -race ./internal/tui/views/ -run PodLogs -v
```

## Pitfalls
- The log stream must be closed on view close; a leaked follow holds an API connection forever.
