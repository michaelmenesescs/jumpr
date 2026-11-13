# Task 110 — Pod exec in a terminal pane; port-forward

- **Milestone:** M9
- **Depends on:** 106, 050
- **Parallel group:** M9-B (with 109)
- **Status:** done

## Goal
`s` on a pod opens a shell in a pane (never taking over the TTY); `shift+f` forwards a port.

## Files
- create `internal/backend/k8s/exec.go`, `portforward.go`, `_test.go` files
- modify `internal/mux/termpane/termpane.go` (accept any `transport.Shell` factory, not only SSH),
  `internal/tui/commands_k8s.go`

## Requirements
1. `func (b *Backend) OpenShell(ctx, ns, pod, container string, req transport.PTYRequest) (transport.Shell,
   error)` using `remotecommand.NewSPDYExecutor`/websocket fallback with `TTY: true`, a `TerminalSizeQueue` fed by
   `Resize`, and pipes for stdin/stdout. Command default: `sh -c 'command -v bash >/dev/null && exec bash || exec
   sh'`.
2. `termpane.New` takes `open func(ctx, transport.PTYRequest) (transport.Shell, error)` and a title; the SSH and
   pod variants are thin constructors. `ContentState` for pod shells: `Type "term"`, `Host` =
   `k8s:<ctx>/<ns>/<pod>/<container>`.
3. Opening a pod shell is recorded (`shell opened in <ns>/<pod>`), refused in read-only mode, and counts as a
   terminal pane for synchronized input.
4. Port-forward: `:pf <local>:<remote>` on the selected pod/service (service → pick a ready endpoint pod);
   listens on `127.0.0.1` only; listed in `:pf` (kind `portforward`: local, target, since, conns, bytes) with `x`
   to stop; belongs to the session and survives detach; recorded as an action-like `input` entry.

## Acceptance
```
go test -race ./internal/backend/k8s/ -run 'Exec|PortForward' -v      # exec against a fake stream server; resize
                                                                      # queue; port-forward lifecycle with a fake
go test -race ./internal/mux/termpane/ -v                             # still green after the constructor change
```

## Pitfalls
- Never bind a port-forward to `0.0.0.0`.
- The exec stream ends without EOF on some API servers when the shell exits; treat stream error after exit as a
  normal close.
