# Task 079 — Session server: socket, lock, session registry, control commands

- **Milestone:** M5
- **Depends on:** 077, 078
- **Parallel group:** M5-B (with 080)
- **Status:** todo

## Goal
`jumper server`: a per-user process that listens on a unix socket and connects clients to sessions.

## Context
BUILD-PLAN §6 (server rules), §6.2 (protocol). SPEC §21.4 (security, limits).

## Files
- create `internal/server/server.go`, `internal/server/conn.go`, `internal/server/lock.go`
- create `internal/server/server_test.go`

## Requirements
1. `type Options struct { SocketPath, LockPath string; NewSession func(name string) (SessionHandle, error);
   Version string; Log *slog.Logger; IdleExit time.Duration }` and a small interface so this package does not
   import `app`:
   ```go
   type SessionHandle interface {
       Attach(in io.Reader, out io.Writer, cols, rows int, env map[string]string, onDetach func(reason string)) error
       Detach(reason string)
       Resize(cols, rows int)
       Done() <-chan struct{}
       Kill()
       Info() SessionInfo
   }
   ```
   (`app.Session` gets a thin adapter in task 081.)
2. `func Serve(ctx context.Context, o Options) error`:
   - socket path longer than 103 bytes → `errs.Config`.
   - create the directory `0700`; take an exclusive non-blocking `flock` on `LockPath` (already held →
     `errs.Config` "server already running"); remove a stale socket file; `net.Listen("unix", …)`; `chmod 0600`.
   - accept loop; on `ctx` done: detach all clients with reason `shutdown`, kill sessions, close, remove the
     socket file, release the lock.
3. Per connection: read `HELLO` within 5 s (anything else → close). Version mismatch → `EXIT{error}` with the
   message from BUILD-PLAN §6.2. Then by `Hello.Command`:
   - `attach`: session = named, or the most recently active, or (none exist) create `main`. Unknown name →
     `EXIT{error, "no such session"}`. Send `WELCOME`, then `Attach` with `in` = a reader fed by `STDIN` frames
     and `out` = `ChunkWriter(STDOUT)`. Handle `RESIZE` and `DETACH` frames. When the session calls
     `onDetach(reason)` or ends (`Done`): send `EXIT{reason}` (`detached`, `replaced`, `ended`, `shutdown`) and
     close. Client connection drops → `Detach("hangup")` (session keeps running).
   - `new`: create (name required unless empty → `main`, `main-2`, …; duplicate → error); attach unless
     `Detached`.
   - `ls` → `RESULT{Sessions}`; `kill-session` → kill, `RESULT`; `kill-server` → `RESULT` then shut down.
4. Sessions that end are removed from the registry. With `IdleExit > 0`, the server exits when it has had no
   sessions for that long (default from the CLI: 0 = never while sessions exist, 10 s after the last one ends).
5. Peer check: after accept, verify the peer's uid equals the server's (`unix.GetsockoptXucred` with
   `SOL_LOCAL`/`LOCAL_PEERCRED` on darwin; build-tagged file `peer_darwin.go`, and `peer_other.go` returning ok).
6. All writes to one connection go through one mutex.

## Out of scope
The real client (080), daemonizing (081).

## Acceptance
```
go test -race ./internal/server/ -run 'Serve|Server' -v
# with a fake SessionHandle that echoes input to output and a test client speaking raw frames over a socket in
# os.MkdirTemp("/tmp", …):
# TestAttachCreatesMain, TestStdinStdoutFlow, TestResizeForwarded, TestDetachFrame, TestClientHangupDetaches,
# TestSecondClientReplacesFirst (first gets EXIT replaced), TestNewDuplicateName, TestLs, TestKillSession,
# TestKillServerRemovesSocket, TestVersionMismatch, TestSecondServerFailsOnLock, TestStaleSocketRemoved,
# TestSocketPermissions (0600, dir 0700), TestLongSocketPathRejected
```

## Pitfalls
- macOS `sun_path` is 104 bytes including the terminator; test sockets must live under a short `/tmp/…` path.
- Take the lock **before** deleting a stale socket, or two racing servers delete each other's socket.
- Never let a slow or dead client block a session: the `STDOUT` writer has a 5 s write deadline; on timeout the
  client is detached.
