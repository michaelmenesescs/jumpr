# jumper

Agentless, client-side infrastructure debugging TUI (Go).

- Start here: [docs/HANDOFF.md](docs/HANDOFF.md)
- Spec: [docs/SPEC.md](docs/SPEC.md)
- Build plan and contracts: [docs/BUILD-PLAN.md](docs/BUILD-PLAN.md)
- Tasks: [docs/tasks/README.md](docs/tasks/README.md)
- Test lab: [lab/README.md](lab/README.md)
- Requirements interview: [docs/INTERVIEW.md](docs/INTERVIEW.md)
- Agent rules: [AGENTS.md](AGENTS.md)

## Lab observability stack (M8)

The M8 tests (`internal/backend/prom/lab_test.go`, run by `make lab-test`) use
the lab's Prometheus and Alertmanager behind a mock SSO cookie proxy. The stack
is configured in `lab/jumper/config.yaml` (`prometheus:`, `alertmanager:`) and
`lab/jumper/connections.yaml` (the `lab-sso` connection profile).

```sh
go build -o lab/.keys/auth-helper ./examples/auth-helper-cookie
export JUMPER_AUTH_COOKIE=<the cookie the mock proxy handed out>
make lab-test
./jumper doctor conn lab-sso https://prom.lab.example.internal --config-dir lab/jumper
```

The proxy is a Go program owned by the lab track. The tests expect it to:

- require the `_oauth2_proxy` cookie, and answer `302` to a `Location`
  containing `login` when the cookie is missing or stale;
- answer `POST /__lab/sso/invalidate` (lab-only, no auth) with
  `{"cookie": "<renewed value>"}`, invalidating the current cookie — that is how
  the auth-helper refresh test gets a new value;
- fire an alert whose alertname (or another label/annotation) names `haproxy`
  for `web-01`/`web-02` within a few minutes of
  `go run ./cmd/sitrep-lab fault haproxy-443`.

When the stack, the proxy or the helper is absent, those tests skip with
`lab observability stack not running`.
