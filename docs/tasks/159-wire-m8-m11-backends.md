# Task 159 — wire the M8–M11 backends into the running app

- **Milestone:** M12 (integration gap found by end-to-end verification on 2026-11-13)
- **Depends on:** 065, 098–104, 105–112, 113–122
- **Status:** todo

## Why

Everything from M8 to M11 is implemented, unit-tested and merged — and **unreachable from the
binary**. `internal/app/core.go` defines the builders

    PrometheusBackend, Alertmanager, GrafanaBackend, ElasticBackend, ChefBackend,
    KubernetesBackend, KubernetesDynamicBackend, KubernetesPatchPlanner, PluginManager, PluginDir

but nothing calls them: `buildDemo` registers `fake`, and `buildReal` registers `linux` and `salt`
only (`core.go:684`, `:773`, `:780`). Found on 2026-11-13 by running the real binary against a real
k3s cluster with a real kubeconfig: `jumpr doctor` reported `inventory 0 hosts` and
`sample k8s: no hosts from this source`, because the k8s backend was never built.

Each builder's own doc comment says "the task that owns app.go calls this function" — no task owned
it. This is that task.

## Files

- modify `internal/app/core.go` (a `wireExtraBackends` method) and `internal/app/session.go`
  (factories and commands for the new kinds, if they are not in `views.Factories()`)
- modify `internal/app/build_test.go` — add the wiring test below
- **do not** change `internal/backend/**`: those packages are done and tested

## Requirements

1. Load the connection profiles once per Core (`connections.yaml`), and build + register each
   backend whose config section is non-empty: `prometheus`, `alertmanager`, `grafana`, `elastic`,
   `chef`, `kubernetes` (+ the dynamic backend and its patch planner). A backend whose section is
   empty is simply not registered — never an error.
2. Register the YAML plugin manager (`PluginDir` + `PluginManager`), its backends, its factory and
   its `plugin` planner, when the directory exists and has valid plugins. A plugin `Problem` is a
   startup warning, never a failed start.
3. Add the `k8s` inventory source to the source list when `kubernetes.kubeconfig` (or the env
   default) resolves; nodes become hosts with the `host` kind so `:hosts` and `:pods` share one
   inventory.
4. A backend that fails to build (bad URL, unreachable master, malformed kubeconfig) must be a
   **startup warning naming the config section**, and the app must still start with the rest.
5. `jumpr doctor` must report one `backend` line per registered backend, with its health.

## Acceptance

```
go test ./internal/app/ -run 'Wire|Backends' -v      # with a temp config dir per test:
                                                     # empty sections register nothing;
                                                     # a prometheus section registers prom + alertmanager;
                                                     # a kubeconfig with two contexts registers the
                                                     # k8s backend and its inventory source;
                                                     # a bad URL warns and does not stop the start
make check
```

And by hand, against a real cluster (this is how the gap was found):

```
kubectl config view --raw > /tmp/k3s.yaml    # from the k3s host, rewrites to its IP
printf 'kubernetes:\n  kubeconfig: "/tmp/k3s.yaml"\ninventory:\n  sources:\n    - type: k8s\n' \
  > /tmp/cfg/config.yaml
jumpr doctor --config-dir /tmp/cfg          # must show the k8s backend ok + 1 host (the node)
jumpr --config-dir /tmp/cfg                 # :nodes and :pods must list the cluster
```

## Pitfalls

- The k8s client is per-process but the context/namespace is per-session (see task 155); do not make
  this wiring the reason that bug gets harder to fix — build one client, switch contexts per session.
- `PrometheusBackend` and friends take `httpx.Deps` and `map[string]*httpx.Profile`; the profile map
  must be loaded once, from `connections.yaml` in the config dir.
- Do not register two backends claiming the same kind: the registry rejects it and the app must
  report which two, not panic.
