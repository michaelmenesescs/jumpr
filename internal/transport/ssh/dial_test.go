package ssh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"jumpr/internal/errs"
	"jumpr/internal/transport/ssh/sshtest"

	"jumpr/internal/testload"
)

// dialSpecFor returns a spec that dials srv with the given identity file and a
// known_hosts entry for its host key.
func dialSpecFor(t *testing.T, srv *sshtest.Server, keyFile string) HostSpec {
	t.Helper()
	return HostSpec{
		Alias:                 "target",
		HostName:              "127.0.0.1",
		User:                  "tester",
		Port:                  srv.Port(),
		IdentityFiles:         []string{keyFile},
		KnownHostsFiles:       []string{writeKnownHosts(t, "127.0.0.1", srv.Port(), srv.HostKey())},
		StrictHostKeyChecking: "yes",
	}
}

// startDialServer starts a server that accepts pub and echoes "uptime".
func startDialServer(t *testing.T, o sshtest.Options, pub gossh.PublicKey) *sshtest.Server {
	t.Helper()
	if o.AuthorizedKeys == nil {
		o.AuthorizedKeys = []gossh.PublicKey{pub}
	}
	if o.Handler == nil {
		o.Handler = sshtest.CommandTable(map[string]string{"uptime": "ok\n"})
	}
	return sshtest.Start(t, o)
}

// assertClientWorks proves the client is authenticated and can run a command.
func assertClientWorks(t *testing.T, c *gossh.Client) {
	t.Helper()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()
	out, err := sess.Output("uptime")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if string(out) != "ok\n" {
		t.Fatalf("output = %q, want %q", out, "ok\n")
	}
}

// closedPort returns a loopback host and a port nothing is listening on.
func closedPort(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return "127.0.0.1", n
}

func TestDialDirectOK(t *testing.T) {
	priv, pub := newEd25519(t)
	keyFile := writePrivateKey(t, priv, "")
	srv := startDialServer(t, sshtest.Options{}, pub)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := DialDirect(ctx, dialSpecFor(t, srv, keyFile), DialOptions{ConnectTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	assertClientWorks(t, c)
	if got := srv.Conns(); got != 1 {
		t.Fatalf("server connections = %d, want 1", got)
	}
}

func TestDialDirectAuthFails(t *testing.T) {
	priv, _ := newEd25519(t)
	keyFile := writePrivateKey(t, priv, "")
	_, otherPub := newEd25519(t)
	srv := startDialServer(t, sshtest.Options{AuthorizedKeys: []gossh.PublicKey{otherPub}}, otherPub)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := DialDirect(ctx, dialSpecFor(t, srv, keyFile), DialOptions{ConnectTimeout: 5 * time.Second})
	if err == nil {
		c.Close()
		t.Fatal("dial succeeded with a key the server does not accept")
	}
	if got := errs.KindOf(err); got != errs.Auth {
		t.Fatalf("kind = %s, want auth (%v)", got, err)
	}
}

func TestDialUnknownHostKeyStrict(t *testing.T) {
	priv, pub := newEd25519(t)
	keyFile := writePrivateKey(t, priv, "")
	srv := startDialServer(t, sshtest.Options{}, pub)

	spec := dialSpecFor(t, srv, keyFile)
	spec.KnownHostsFiles = []string{emptyKnownHosts(t)}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := DialDirect(ctx, spec, DialOptions{ConnectTimeout: 5 * time.Second})
	if err == nil {
		c.Close()
		t.Fatal("dial succeeded with an unknown host key under StrictHostKeyChecking yes")
	}
	if got := errs.KindOf(err); got != errs.HostKey {
		t.Fatalf("kind = %s, want hostkey (%v)", got, err)
	}
}

func TestDialRefused(t *testing.T) {
	priv, pub := newEd25519(t)
	keyFile := writePrivateKey(t, priv, "")
	host, port := closedPort(t)

	spec := HostSpec{
		Alias:                 "down",
		HostName:              host,
		User:                  "tester",
		Port:                  port,
		IdentityFiles:         []string{keyFile},
		KnownHostsFiles:       []string{writeKnownHosts(t, host, port, pub)},
		StrictHostKeyChecking: "yes",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := DialDirect(ctx, spec, DialOptions{ConnectTimeout: 5 * time.Second})
	if err == nil {
		c.Close()
		t.Fatal("dial to a closed port succeeded")
	}
	if got := errs.KindOf(err); got != errs.Unreachable {
		t.Fatalf("kind = %s, want unreachable (%v)", got, err)
	}
}

func TestDialTimeout(t *testing.T) {
	const timeout = 100 * time.Millisecond
	priv, pub := newEd25519(t)
	keyFile := writePrivateKey(t, priv, "")
	// The server stalls its handshake far past the client's connect timeout.
	srv := startDialServer(t, sshtest.Options{ConnDelay: 2 * time.Second}, pub)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	c, err := DialDirect(ctx, dialSpecFor(t, srv, keyFile), DialOptions{ConnectTimeout: timeout})
	elapsed := time.Since(start)
	if err == nil {
		c.Close()
		t.Fatal("dial succeeded against a server that never finishes the handshake")
	}
	if got := errs.KindOf(err); got != errs.Timeout {
		t.Fatalf("kind = %s, want timeout (%v)", got, err)
	}
	if limit := timeout + 500*time.Millisecond; elapsed > testload.Scale(limit) {
		t.Fatalf("dial returned after %v, want within %v", elapsed, limit)
	}
}

func TestDialViaBastion(t *testing.T) {
	priv, pub := newEd25519(t)
	keyFile := writePrivateKey(t, priv, "")

	bastion := startDialServer(t, sshtest.Options{AllowForward: true}, pub)
	target := startDialServer(t, sshtest.Options{}, pub)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bc, err := DialDirect(ctx, dialSpecFor(t, bastion, keyFile), DialOptions{ConnectTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("dial bastion: %v", err)
	}
	defer bc.Close()

	c, err := DialVia(ctx, bc, dialSpecFor(t, target, keyFile), DialOptions{ConnectTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("dial target via bastion: %v", err)
	}
	defer c.Close()

	assertClientWorks(t, c)
	if got := target.Conns(); got != 1 {
		t.Fatalf("target connections = %d, want 1", got)
	}
	if got := bastion.MaxConcurrentChannels(); got < 1 {
		t.Fatalf("bastion forwarded channels = %d, want at least 1", got)
	}
}

func TestDialViaBastionTargetDown(t *testing.T) {
	priv, pub := newEd25519(t)
	keyFile := writePrivateKey(t, priv, "")
	bastion := startDialServer(t, sshtest.Options{AllowForward: true}, pub)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bc, err := DialDirect(ctx, dialSpecFor(t, bastion, keyFile), DialOptions{ConnectTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("dial bastion: %v", err)
	}
	defer bc.Close()

	host, port := closedPort(t)
	spec := HostSpec{
		Alias:                 "down",
		HostName:              host,
		User:                  "tester",
		Port:                  port,
		IdentityFiles:         []string{keyFile},
		KnownHostsFiles:       []string{emptyKnownHosts(t)},
		StrictHostKeyChecking: "yes",
	}
	c, err := DialVia(ctx, bc, spec, DialOptions{ConnectTimeout: 5 * time.Second})
	if err == nil {
		c.Close()
		t.Fatal("dial through the bastion to a closed port succeeded")
	}
	if got := errs.KindOf(err); got != errs.Unreachable {
		t.Fatalf("kind = %s, want unreachable (%v)", got, err)
	}
}

func TestKeepaliveDetectsDeadPeer(t *testing.T) {
	const every = 200 * time.Millisecond
	priv, pub := newEd25519(t)
	keyFile := writePrivateKey(t, priv, "")
	srv := startDialServer(t, sshtest.Options{}, pub)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := DialDirect(ctx, dialSpecFor(t, srv, keyFile), DialOptions{ConnectTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	dead := make(chan struct{})
	startKeepalive(ctx, c, every, func() { close(dead) })

	srv.Kill()

	select {
	case <-dead:
	case <-time.After(3 * every):
		t.Fatal("onDead was not called within three keepalive intervals")
	}
}

func TestProxyCommandRejected(t *testing.T) {
	priv, _ := newEd25519(t)
	keyFile := writePrivateKey(t, priv, "")
	spec := HostSpec{
		Alias:         "proxied",
		HostName:      "example.internal",
		User:          "tester",
		Port:          22,
		IdentityFiles: []string{keyFile},
		ProxyCommand:  "nc %h %p",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := DialDirect(ctx, spec, DialOptions{ConnectTimeout: time.Second})
	if err == nil {
		c.Close()
		t.Fatal("dial succeeded with a ProxyCommand")
	}
	if got := errs.KindOf(err); got != errs.Config {
		t.Fatalf("kind = %s, want config (%v)", got, err)
	}
	want := "ProxyCommand is not supported by the native transport; use transport: system-ssh for proxied"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestClassifyDialPatterns(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want errs.Kind
	}{
		{"auth", errStr("ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain"), errs.Auth},
		{"hostkey", errStr("ssh: handshake failed: knownhosts: key is unknown"), errs.HostKey},
		{"refused", errStr("dial tcp 127.0.0.1:1: connect: connection refused"), errs.Unreachable},
		{"no route", errStr("dial tcp 10.0.0.1:22: connect: no route to host"), errs.Unreachable},
		{"no such host", errStr("dial tcp: lookup nope.example.internal: no such host"), errs.Unreachable},
		{"bastion prohibited", errStr(`ssh: rejected: administratively prohibited ("port forwarding is disabled")`), errs.Unreachable},
		{"bastion connect failed", errStr(`ssh: rejected: connect failed ("dial tcp 10.0.0.2:22: connect: connection refused")`), errs.Unreachable},
		{"i/o timeout", errStr("dial tcp 10.0.0.1:22: i/o timeout"), errs.Timeout},
		{"deadline", context.DeadlineExceeded, errs.Timeout},
		{"canceled", context.Canceled, errs.Canceled},
		{"unclassified", errStr("ssh: something odd"), errs.Internal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errs.KindOf(classifyDial(tt.err, "example")); got != tt.want {
				t.Fatalf("kind = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestClassifyDialPreservesErrsError(t *testing.T) {
	// ssh.NewClientConn wraps the callback's error, so the kind must survive
	// the handshake wrapping rather than be re-derived from the text.
	inner := errs.New(errs.Interaction, "ssh.hostkey", "host key for example is unknown and nobody can be asked")
	err := classifyDial(fmt.Errorf("ssh: handshake failed: %w", inner), "example")
	if got := errs.KindOf(err); got != errs.Interaction {
		t.Fatalf("kind = %s, want needs-interaction (%v)", got, err)
	}
	if !errors.Is(err, inner) {
		t.Fatalf("cause lost: %v", err)
	}
}

// errStr is a bare error with the given text, so the classifier sees exactly
// what crypto/ssh writes.
type plainErr string

func (e plainErr) Error() string { return string(e) }

func errStr(s string) error { return plainErr(s) }
