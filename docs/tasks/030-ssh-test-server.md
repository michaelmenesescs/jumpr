# Task 030 — In-process SSH test server

- **Milestone:** M2
- **Depends on:** 026
- **Parallel group:** M2-B (with 029, 036, 038)
- **Status:** todo

## Goal
A test-only SSH server that behaves enough like sshd to test dialing, bastions, limits, exec, streams and PTYs
without any network beyond loopback.

## Context
BUILD-PLAN §9, §12 (risk: bastion limits). Uses `github.com/gliderlabs/ssh` (tests only).

## Files
- create `internal/transport/ssh/sshtest/server.go`, `internal/transport/ssh/sshtest/server_test.go`

## Requirements
1. `type Options struct { Name string; AuthorizedKeys []gossh.PublicKey; Handler func(cmd string, stdin
   io.Reader, stdout, stderr io.Writer) int; MaxSessions int; AllowForward bool; ConnDelay time.Duration;
   HostKeyType string }`.
2. `func Start(t testing.TB, o Options) *Server` — listens on `127.0.0.1:0`, generates a host key
   (`ed25519` default, `rsa` optional), stops in `t.Cleanup`. `Server` exposes `Addr() string`, `Port() int`,
   `HostKey() gossh.PublicKey`, `KnownHostsLine() string`, and counters: `Sessions() int` (total),
   `MaxConcurrentSessions() int`, `Conns() int`, `MaxConcurrentChannels() int` (direct-tcpip).
3. Exec requests run `Handler` with the raw command string; the return value is the exit status. A session
   without a command (shell) with a PTY request echoes input back and writes `resize <cols>x<rows>\n` on window
   change; `exit\n` ends it with status 0.
4. `MaxSessions > 0`: a session channel opened while that many are active on the same connection is rejected
   (as sshd does: channel open failure).
5. `AllowForward`: accept `direct-tcpip` channels and proxy them to the requested address (so a server can act as
   a bastion for another test server). Count concurrent forwarded channels.
6. Public-key auth only: accept keys in `AuthorizedKeys`; empty list accepts any key.
7. `func CommandTable(m map[string]string) func(…) int` helper: longest key that is a substring of the command
   wins; writes the value to stdout, exit 0; no match → stderr `not found`, exit 127.
8. `func (s *Server) Kill()` closes the listener and all connections (to test reconnects).

## Out of scope
sftp, agent forwarding, password auth.

## Acceptance
```
go test -race ./internal/transport/ssh/sshtest/ -v
# using a plain golang.org/x/crypto/ssh client in the test:
# TestExecRunsHandler, TestExitStatus, TestMaxSessionsRejects, TestForwardToSecondServer (bastion → target),
# TestShellEchoAndResize, TestKillDropsConnections
```

## Pitfalls
- gliderlabs: forwarding needs `ChannelHandlers["direct-tcpip"] = ssh.DirectTCPIPHandler` and a
  `LocalPortForwardingCallback` returning true; wrap the handler to count channels.
- gliderlabs `Session.RawCommand()` gives the command string unsplit; use it, not `Command()`.
- Enforcing `MaxSessions` needs a custom `"session"` channel handler that counts per connection and calls
  `newChan.Reject(gossh.Prohibited, "max sessions")` before delegating to `ssh.DefaultSessionHandler`.
