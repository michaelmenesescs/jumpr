//go:build homelab

// Homelab suite: the SSH path against a real Ubuntu 24.04 node reached through
// the user's own ~/.ssh/config. Every test here is read-only. Nothing in this
// file compiles into `go test ./...`; it only runs under `make homelab-test`
// with JUMPER_HOMELAB_UBUNTU set to an ssh_config alias.

package ssh_test

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/shq"
	"jumper/internal/transport"
	sshx "jumper/internal/transport/ssh"
)

const (
	// homelabDialTimeout is the budget for deciding whether the node is
	// reachable at all. A laptop away from the tailnet must skip, not fail.
	homelabDialTimeout = 3 * time.Second
	// homelabCallTimeout bounds one remote call in these tests.
	homelabCallTimeout = 10 * time.Second
	// homelabSetupTimeout bounds a whole test, dial and cleanup included.
	homelabSetupTimeout = 4 * homelabCallTimeout
)

// homelabHost returns the ssh_config alias under test. Every test in the
// homelab suite starts here, so an unset variable skips instead of failing.
func homelabHost(t *testing.T) string {
	t.Helper()
	host := os.Getenv("JUMPER_HOMELAB_UBUNTU")
	if host == "" {
		t.Skip("JUMPER_HOMELAB_UBUNTU not set")
	}
	return host
}

// homelabHome is the real home directory, where ~/.ssh/config lives.
func homelabHome(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	return home
}

// homelabResolver decodes the real ~/.ssh/config. No fixture: this task is
// about the config the user actually has.
func homelabResolver(t *testing.T) *sshx.Resolver {
	t.Helper()
	home := homelabHome(t)
	r, err := sshx.NewResolver([]string{home + "/.ssh/config"}, home, os.Getenv("USER"))
	if err != nil {
		t.Fatalf("read ~/.ssh/config: %v", err)
	}
	return r
}

// homelabDialOpts is what the client uses for a real node: the user's agent,
// the known_hosts from ssh_config, and no prompter (a test cannot answer one).
func homelabDialOpts() sshx.DialOptions {
	return sshx.DialOptions{
		Auth:           sshx.AuthOptions{AgentSocket: func() string { return os.Getenv("SSH_AUTH_SOCK") }},
		ConnectTimeout: homelabCallTimeout,
	}
}

// homelabResolve resolves host and skips when its TCP port does not answer
// within homelabDialTimeout.
func homelabResolve(t *testing.T, host string) sshx.HostSpec {
	t.Helper()
	spec, err := homelabResolver(t).Resolve(host)
	if err != nil {
		t.Fatalf("resolve %s: %v", host, err)
	}
	addr := net.JoinHostPort(spec.HostName, strconv.Itoa(spec.Port))
	conn, derr := net.DialTimeout("tcp", addr, homelabDialTimeout)
	if derr != nil {
		t.Skipf("%s (%s) unreachable within %v: %v", host, addr, homelabDialTimeout, derr)
	}
	_ = conn.Close()
	return spec
}

// homelabTransport returns an SSH executor for host over a pooled connection,
// plus the HostID its Ref would carry.
func homelabTransport(t *testing.T, host string) (*sshx.Transport, resource.HostID) {
	t.Helper()
	homelabResolve(t, host)
	pool := sshx.NewPool(sshx.PoolOptions{Resolver: homelabResolver(t), Dial: homelabDialOpts()})
	t.Cleanup(func() { _ = pool.Close() })
	return sshx.New(pool, sshx.Options{ExecTimeout: homelabCallTimeout}), resource.HostID(host)
}

// homelabCtx is the context for one homelab test.
func homelabCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), homelabSetupTimeout)
	t.Cleanup(cancel)
	return ctx
}

// homelabSkipIfSilent turns "the node never answered" into a skip: a laptop
// away from the tailnet, or a node that is down, must not fail CI. Everything
// after the first answer is a real failure.
func homelabSkipIfSilent(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	switch errs.KindOf(err) {
	case errs.Timeout, errs.Unreachable:
		t.Skipf("%s: node did not answer: %v", what, err)
	}
}

// TestHomelabSSHConfigResolve proves the alias resolves from the real
// ~/.ssh/config to something the dialer can use: a user, a host and port 22.
func TestHomelabSSHConfigResolve(t *testing.T) {
	host := homelabHost(t)

	spec, err := homelabResolver(t).Resolve(host)
	if err != nil {
		t.Fatalf("resolve %s from ~/.ssh/config: %v", host, err)
	}
	if strings.TrimSpace(spec.User) == "" {
		t.Fatalf("%s resolved without a user: %+v", host, spec)
	}
	if strings.TrimSpace(spec.HostName) == "" {
		t.Fatalf("%s resolved without a host name: %+v", host, spec)
	}
	if spec.Port != 22 {
		t.Fatalf("%s resolved to port %d, want 22", host, spec.Port)
	}
	if len(spec.Jumps) != 0 {
		t.Logf("%s goes through jump hosts %v", host, spec.Jumps)
	}
	t.Logf("%s resolves to %s@%s:%d (known_hosts %v)", host, spec.User, spec.HostName, spec.Port, spec.KnownHostsFiles)
}

// TestHomelabExecRoundTrip runs one real command through the pooled transport
// and one that is not available on the far side.
func TestHomelabExecRoundTrip(t *testing.T) {
	host := homelabHost(t)
	spec := homelabResolve(t, host)
	tr, id := homelabTransport(t, host)
	ctx := homelabCtx(t)

	start := time.Now()
	res, err := tr.Exec(ctx, id, transport.Command{Script: "id -un", Timeout: homelabCallTimeout})
	took := time.Since(start)
	homelabSkipIfSilent(t, "id -un", err)
	if err != nil {
		t.Fatalf("id -un: %v", err)
	}
	out := string(res.Stdout)
	switch {
	case res.RC != 0:
		t.Fatalf("id -un: rc %d, stderr %q", res.RC, strings.TrimSpace(string(res.Stderr)))
	case strings.TrimSpace(out) == "":
		t.Fatalf("id -un returned no output")
	case strings.ContainsAny(out, "\x1b\x00\r"):
		t.Fatalf("id -un output carries control bytes: %q", out)
	case strings.TrimSpace(out) != spec.User:
		t.Fatalf("id -un = %q, want the ssh_config user %q", strings.TrimSpace(out), spec.User)
	case !strings.HasSuffix(out, "\n"):
		t.Fatalf("id -un output is not newline terminated: %q", out)
	}
	t.Logf("exec id -un: %q in %v (via %s)", strings.TrimSpace(out), took.Round(time.Millisecond), res.Via)

	// A command the far side does not have must be classified, not failed at
	// the transport layer: a non-zero rc is not an error there.
	res, err = tr.Exec(ctx, id, transport.Command{Script: "sh -c 'exit 127'", Timeout: homelabCallTimeout})
	if err != nil {
		t.Fatalf("failing command returned a transport error: %v", err)
	}
	if res.RC != 127 {
		t.Fatalf("failing command: rc %d, want 127", res.RC)
	}
	if kind := errs.KindOf(shq.Classify(res.RC, string(res.Stderr), false)); kind != errs.NotAvailable {
		t.Fatalf("rc 127 classified as %q, want %q", kind, errs.NotAvailable)
	}
}
