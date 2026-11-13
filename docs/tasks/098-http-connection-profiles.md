# Task 098 — HTTP connection profiles: proxy, TLS, static auth

- **Milestone:** M8
- **Depends on:** 003, 054
- **Parallel group:** — (serial; adds a dependency)
- **Status:** done

## Goal
`internal/transport/httpx`: build an `*http.Client` from a named profile (SPEC §7.2).

## Files
- create `internal/transport/httpx/profile.go`, `client.go`, `proxy.go`, and `_test.go` files
- modify `go.mod`/`go.sum` (`go get golang.org/x/net@v0.62.0`), `internal/deps/deps.go`,
  `internal/config/` (load `connections.yaml`)

## Requirements
1. Structs for the YAML in SPEC §7.2 (`proxy`, `tls`, `auth`, `headers`, `timeout`, `retries`); strict decoding;
   validation errors name the profile and field.
2. `func (p *Profile) Client(deps Deps) (*Client, error)` where `Client.Do(ctx, *http.Request) (*http.Response,
   error)` applies: proxy (`none`, `env` using values passed in `Deps.Env`, `http` with optional headers,
   `socks5` via `golang.org/x/net/proxy`, `ssh-socks` = dial through an SSH host using a `Deps.SSHDial
   func(ctx, host, network, addr) (net.Conn, error)` supplied by the SSH pool: no local listener needed), TLS
   (CA file, client cert, SNI, `insecure_skip_verify` with a loud warning in `:health`), static auth (`none`,
   `basic`, `bearer`, `headers`, `cookie`), extra headers, timeout, retries with backoff for idempotent methods
   on connection errors and 502/503/504.
3. Errors → `errs`: 401/403 → `Auth`; 404 → `NotAvailable`; timeouts → `Timeout`; dial failures → `Unreachable`;
   bodies are capped (16 MiB) and error messages are redacted.
4. `func (c *Client) GetJSON(ctx, url string, out any) error`, `PostJSON(ctx, url string, in, out any) error`.
5. Add `func (p *Pool) DialContext(ctx, host resource.HostID, network, addr string) (net.Conn, error)` to the SSH
   pool for `ssh-socks`.

## Acceptance
```
go test -race ./internal/transport/httpx/ -v
# httptest servers: basic/bearer/header/cookie auth applied; custom CA accepted, unknown CA rejected; HTTP CONNECT
# proxy used (a tiny proxy in the test); SOCKS5 used; ssh-socks through an sshtest server; retries on 503 for GET
# and not for POST; 401 → errs.Auth; secrets never appear in error strings
```

## Pitfalls
- Never log request headers. Never put credentials in URLs.
- One `http.Transport` per profile, reused; do not create a client per request.
