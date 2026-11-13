# Task 031 — SSH dialer: direct and through a ProxyJump chain

- **Milestone:** M2
- **Depends on:** 029, 030
- **Parallel group:** — (serial)
- **Status:** todo

## Goal
One function that returns a connected `*ssh.Client` for a resolved chain, with timeouts, keepalives and classified
errors.

## Context
SPEC §7.1. BUILD-PLAN §7 (error table), §8 (timeouts).

## Files
- create `internal/transport/ssh/dial.go`, `internal/transport/ssh/dial_test.go`

## Requirements
1. `type DialOptions struct { Auth AuthOptions; HostKeys HostKeyOptions; ConnectTimeout, Keepalive time.Duration;
   Log *slog.Logger }`.
2. `func clientConfig(spec HostSpec, o DialOptions) (*ssh.ClientConfig, func(), error)` — user, auth methods,
   host key callback, `HostKeyAlgorithms` from task 029, `Timeout`.
3. `func DialDirect(ctx context.Context, spec HostSpec, o DialOptions) (*ssh.Client, error)` — `net.Dialer` with
   context; then `ssh.NewClientConn` guarded by a deadline on the `net.Conn` (handshake must respect the timeout);
   clear the deadline after the handshake.
4. `func DialVia(ctx context.Context, via *ssh.Client, spec HostSpec, o DialOptions) (*ssh.Client, error)` — open
   `via.Dial("tcp", host:port)` in a goroutine so `ctx` can abandon it (close the result if it arrives late),
   then handshake as above.
5. Reject specs with `ProxyCommand` → `errs.Config`: `ProxyCommand is not supported by the native transport; use
   transport: system-ssh for <host>`.
6. `func classifyDial(err error, host string) *errs.Error` per BUILD-PLAN §7 (auth, hostkey, timeout,
   unreachable; `ssh: rejected: administratively prohibited` and `connect failed` from a bastion →
   `Unreachable`). Preserve `*errs.Error` values raised by the host key callback (unwrap them from the handshake
   error with `errors.As`).
7. `func startKeepalive(ctx context.Context, c *ssh.Client, every time.Duration, onDead func())` — sends
   `keepalive@openssh.com` global requests; two consecutive failures or a 10 s reply timeout → `c.Close()` and
   `onDead()`.

## Out of scope
Pooling and limits (032). The whole-chain orchestration lives in the pool.

## Acceptance
```
go test -race ./internal/transport/ssh/ -run Dial -v
# TestDialDirectOK, TestDialDirectAuthFails (errs.Auth), TestDialUnknownHostKeyStrict (errs.HostKey),
# TestDialRefused (errs.Unreachable, closed port), TestDialTimeout (server with ConnDelay > timeout → errs.Timeout
#   within timeout+500ms), TestDialViaBastion, TestDialViaBastionTargetDown (errs.Unreachable),
# TestKeepaliveDetectsDeadPeer (server.Kill → onDead within 3 intervals), TestProxyCommandRejected
```

## Pitfalls
- `ssh.Dial` ignores context; do not use it. Dial TCP yourself, set `conn.SetDeadline`, then `ssh.NewClientConn`.
- The handshake error for a failed host key callback is wrapped as `ssh: handshake failed: <your error>`; use
  `errors.As` to recover the kind.
- Always close the `net.Conn` on any error path after it was opened.
