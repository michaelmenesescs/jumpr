# Task 032 — SSH connection pool with bastion multiplexing and limits

- **Milestone:** M2
- **Depends on:** 031
- **Parallel group:** — (serial)
- **Status:** done

## Goal
`Pool.Client(ctx, host)` returns a ready, reused connection to any host, creating bastion connections as needed
and never exceeding the configured limits.

## Context
SPEC §7.1 (bastion multiplexing, reuse, limits). BUILD-PLAN §8 (limits), §12 (risk 1).

## Files
- create `internal/transport/ssh/pool.go`, `internal/transport/ssh/pool_test.go`

## Requirements
1. `type PoolOptions struct { Resolver *Resolver; Dial DialOptions; MaxConnsPerBastion, MaxChannelsPerConn,
   MaxSessionsPerHost int; IdleTimeout time.Duration; Now func() time.Time }` (defaults 4, 8, 4, 5 min).
2. `func NewPool(o PoolOptions) *Pool`, `func (p *Pool) Close() error` (closes everything, stops goroutines).
3. `func (p *Pool) Client(ctx context.Context, host resource.HostID) (*Lease, error)`:
   - resolve the chain; for a direct host: one pooled client per host.
   - for a jumped host: obtain the first hop from the **bastion set** (below), then dial each further hop through
     the previous one; the final client is pooled per target host and remembers which bastion connection carries
     it.
   - concurrent requests for the same host share one dial (`singleflight.Group`); a failed dial is not cached.
   - `Lease` has `Client() *ssh.Client`, `Via() string`, and `Release()`. A lease also holds one of the host's
     `MaxSessionsPerHost` session slots (semaphore acquired with ctx); `Release` frees it.
4. Bastion set (per first-hop alias): up to `MaxConnsPerBastion` connections; each carries at most
   `MaxChannelsPerConn` **target connections**. Pick the connection with the fewest targets; open a new one only
   when all existing are full; when all are full and the cap is reached, wait (ctx-aware) for a slot. A target
   connection frees its slot when it is closed.
5. Health: when a client dies (keepalive `onDead`, or `client.Wait()` returns) remove it; if it was a bastion
   connection remove every target that rode on it. The next `Client` call redials.
6. Idle reaper: every 30 s (ticker; skip in tests by calling `p.reap()` directly) close target connections with
   no active lease for longer than `IdleTimeout`, then bastion connections with no targets.
7. `func (p *Pool) Stats() Stats { Bastions, Targets, Leases int }` for `:health` and tests.
8. `func (p *Pool) Forget(host resource.HostID)` drops a host's connection (used after errors).

## Out of scope
Running commands (033). Per-host transport override.

## Acceptance
```
go test -race ./internal/transport/ssh/ -run Pool -v
# with sshtest servers (one bastion with AllowForward, N targets):
# TestPoolReusesTargetConn (2 sequential leases → server saw 1 connection),
# TestPoolSingleflight (20 concurrent Client calls → 1 connection),
# TestPoolBastionChannelLimit (MaxConnsPerBastion=2, MaxChannelsPerConn=3, 6 targets → bastion saw exactly 2
#   connections, max 3 concurrent channels each; a 7th target blocks until one is closed/forgotten),
# TestPoolSessionSlots (MaxSessionsPerHost=2 → third lease waits; ctx cancel returns errs.Canceled),
# TestPoolBastionDeathDropsTargets (bastion.Kill → next Client redials), TestPoolIdleReap,
# TestPoolCloseStopsGoroutines
```

## Pitfalls
- Never hold the pool mutex while dialing; mark an entry "dialing" and release the lock.
- Do not confuse the two limits: `MaxChannelsPerConn` counts target connections tunnelled through one bastion
  connection; `MaxSessionsPerHost` counts concurrent commands on one target connection.
- Waiting for a slot must `select` on `ctx.Done()`.
