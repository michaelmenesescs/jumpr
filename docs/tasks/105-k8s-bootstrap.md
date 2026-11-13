# Task 105 — Kubernetes bootstrap: client-go, contexts, health, size check

- **Milestone:** M9
- **Depends on:** 065
- **Parallel group:** — (serial; adds dependencies)
- **Status:** todo

## Goal
A `k8s` backend that connects with the standard kubeconfig rules and can switch contexts (SPEC §8.2).

## Files
- modify `go.mod`/`go.sum`, `internal/deps/deps.go`
- create `internal/backend/k8s/client.go`, `backend.go`, `client_test.go`
- create `internal/tui/commands_k8s.go` (`:ctx`, `:ns`)
- modify `internal/config/` (`kubernetes: {kubeconfig: "", contexts_protected: []}`), `internal/app/core.go`

## Requirements
1. `go get k8s.io/client-go@v0.37.2 k8s.io/api@v0.37.2 k8s.io/apimachinery@v0.37.2`. Record `ls -l jumper` size
   before and after in `docs/HANDOFF.md`; fail the task (stop and report) if the binary exceeds 80 MB.
2. Loading: `clientcmd.NewNonInteractiveDeferredLoadingClientConfig` with explicit path (config) or
   `$KUBECONFIG` (value passed in from `app`, never read here) / `~/.kube/config`; exec credential plugins work
   as in kubectl. Timeouts: 10 s dial, 30 s request; QPS 50 / burst 100.
3. `:ctx` lists contexts (current marked) and switches; switching tears down informers (task 106) and rebuilds
   the client. `:ns <name>|all` sets the session namespace. Status line shows `ctx:<name>`. Contexts listed in
   `contexts_protected` set the session read-only on switch (SPEC §16).
4. `Health`: `GET /version` with a 5 s timeout; OK shows the server version; auth errors → Down with kind `auth`.
5. Exec-plugin prompts (e.g. SSO login in a browser) are outside jumper's control: if the plugin needs a TTY it
   fails; surface its stderr in `:health`.

## Acceptance
```
go test -race ./internal/backend/k8s/ -v      # kubeconfig with 2 contexts in testdata; httptest API server for
                                              # /version; context switch; missing kubeconfig → Unconfigured
make check
```

## Pitfalls
- Use only `client-go` typed and dynamic clients; do not import `kubectl` packages (huge).
- `klog` writes to stderr by default: redirect it to the slog logger at startup (`klog.SetLogger`).
