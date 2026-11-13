package ssh

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skeema/knownhosts"
	gossh "golang.org/x/crypto/ssh"

	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/transport"
	"jumper/internal/transport/ssh/sshtest"

	"jumper/internal/testload"
)

// Aliases used by every fixture; nothing here is a real host.
const (
	xfHostAlias    = "web-01.example.internal"
	xfBastionAlias = "bastion.example.internal"
)

// xfOptions configures one loopback fixture.
type xfOptions struct {
	handler     func(cmd string, stdin io.Reader, stdout, stderr io.Writer) int
	maxSessions int
	bastion     bool
	proxy       bool
	tr          Options
}

// xfKey is one known_hosts entry.
type xfKey struct {
	port int
	key  gossh.PublicKey
}

// xfHostSpec is one host as the generated ssh_config describes it.
type xfHostSpec struct {
	alias string
	port  int
	jump  string
}

// xfFixture is a pool plus a Transport talking to one loopback SSH server,
// optionally reached through a bastion and through a cuttable TCP proxy.
type xfFixture struct {
	pool    *Pool
	tr      *Transport
	host    resource.HostID
	server  *sshtest.Server
	bastion *sshtest.Server
	proxy   *tcpProxy
}

// newXFFixture starts the servers, writes the ssh_config and the known_hosts
// file, and returns the fixture. Everything is closed at t.Cleanup.
func newXFFixture(t *testing.T, o xfOptions) *xfFixture {
	t.Helper()
	priv, pub := newEd25519(t)
	keyFile := writePrivateKey(t, priv, "")
	keys := []gossh.PublicKey{pub}

	f := &xfFixture{host: xfHostAlias}
	var hosts []xfHostSpec
	var known []xfKey

	if o.bastion {
		f.bastion = sshtest.Start(t, sshtest.Options{AllowForward: true, AuthorizedKeys: keys})
		hosts = append(hosts, xfHostSpec{alias: xfBastionAlias, port: f.bastion.Port()})
		known = append(known, xfKey{port: f.bastion.Port(), key: f.bastion.HostKey()})
	}
	f.server = sshtest.Start(t, sshtest.Options{
		AuthorizedKeys: keys,
		Handler:        o.handler,
		MaxSessions:    o.maxSessions,
	})
	port := f.server.Port()
	if o.proxy {
		f.proxy = newTCPProxy(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		port = f.proxy.Port()
	}
	spec := xfHostSpec{alias: xfHostAlias, port: port}
	if o.bastion {
		spec.jump = xfBastionAlias
	}
	hosts = append(hosts, spec)
	known = append(known, xfKey{port: port, key: f.server.HostKey()})

	cfgPath := xfWriteConfig(t, keyFile, xfWriteKnownHosts(t, known), hosts)
	res, err := NewResolver([]string{cfgPath}, t.TempDir(), "tester")
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	f.pool = NewPool(PoolOptions{Resolver: res, Dial: DialOptions{ConnectTimeout: 5 * time.Second}})
	t.Cleanup(func() { _ = f.pool.Close() })
	f.tr = New(f.pool, o.tr)
	return f
}

// xfWriteKnownHosts writes one known_hosts line per entry.
func xfWriteKnownHosts(t *testing.T, keys []xfKey) string {
	t.Helper()
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(knownhosts.Line([]string{hostAddr("127.0.0.1", k.port)}, k.key))
		b.WriteByte('\n')
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// xfWriteConfig writes an ssh_config for the given hosts.
func xfWriteConfig(t *testing.T, keyFile, knownFile string, hosts []xfHostSpec) string {
	t.Helper()
	var b strings.Builder
	for _, h := range hosts {
		fmt.Fprintf(&b, "Host %s\n  HostName 127.0.0.1\n  Port %d\n  User tester\n", h.alias, h.port)
		if h.jump != "" {
			fmt.Fprintf(&b, "  ProxyJump %s\n", h.jump)
		}
		fmt.Fprintf(&b, "  IdentityFile %s\n  UserKnownHostsFile %s\n"+
			"  StrictHostKeyChecking yes\n  ConnectTimeout 5\n\n", keyFile, knownFile)
	}
	path := filepath.Join(t.TempDir(), "ssh_config")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// tcpProxy forwards loopback connections to one server and can cut the ones it
// holds. That is how a connection that dies in the middle of a command is
// simulated without killing the server: the listener keeps accepting, so the
// next dial is a genuine reconnect.
type tcpProxy struct {
	ln       net.Listener
	upstream string

	mu     sync.Mutex
	conns  []net.Conn
	closed bool
}

func newTCPProxy(t *testing.T, upstream string) *tcpProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &tcpProxy{ln: ln, upstream: upstream}
	go p.serve()
	t.Cleanup(p.Close)
	return p
}

func (p *tcpProxy) Port() int {
	_, port, err := net.SplitHostPort(p.ln.Addr().String())
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return n
}

func (p *tcpProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		u, err := net.Dial("tcp", p.upstream)
		if err != nil {
			_ = c.Close()
			continue
		}
		if !p.track(c, u) {
			continue
		}
		go p.pipe(c, u)
		go p.pipe(u, c)
	}
}

func (p *tcpProxy) pipe(a, b net.Conn) {
	_, _ = io.Copy(a, b)
	_ = a.Close()
	_ = b.Close()
}

func (p *tcpProxy) track(conns ...net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		for _, c := range conns {
			_ = c.Close()
		}
		return false
	}
	p.conns = append(p.conns, conns...)
	return true
}

// cut closes every connection the proxy holds, leaving the listener up.
func (p *tcpProxy) cut() {
	p.mu.Lock()
	conns := p.conns
	p.conns = nil
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// Close stops the proxy and drops its connections.
func (p *tcpProxy) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	conns := p.conns
	p.conns = nil
	p.mu.Unlock()
	_ = p.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
}

func TestCapWriterKeepsFirstBytes(t *testing.T) {
	w := newCapWriter(8)
	if n, err := w.Write([]byte("abcdef")); n != 6 || err != nil {
		t.Fatalf("Write = %d, %v; want 6, nil", n, err)
	}
	if w.Truncated() {
		t.Fatal("Truncated before the cap was reached")
	}
	if n, err := w.Write([]byte("ghijkl")); n != 6 || err != nil {
		t.Fatalf("Write past the cap = %d, %v; want 6, nil", n, err)
	}
	if got := string(w.Bytes()); got != "abcdefgh" {
		t.Fatalf("Bytes = %q, want %q", got, "abcdefgh")
	}
	if !w.Truncated() {
		t.Fatal("Truncated is false after dropping output")
	}
	if w.Len() != 8 {
		t.Fatalf("Len = %d, want 8", w.Len())
	}
	if n, err := w.Write(nil); n != 0 || err != nil {
		t.Fatalf("Write(nil) = %d, %v; want 0, nil", n, err)
	}
	empty := newCapWriter(4)
	if n, err := empty.Write([]byte("toolong")); n != 7 || err != nil {
		t.Fatalf("Write = %d, %v; want 7, nil", n, err)
	}
	if got := string(empty.Bytes()); got != "tool" {
		t.Fatalf("Bytes = %q, want %q", got, "tool")
	}
}

func TestExecStdoutStderrRC(t *testing.T) {
	f := newXFFixture(t, xfOptions{handler: func(_ string, _ io.Reader, stdout, stderr io.Writer) int {
		_, _ = io.WriteString(stdout, "hello\n")
		_, _ = io.WriteString(stderr, "warn\n")
		return 3
	}})

	res, err := f.tr.Exec(context.Background(), f.host, transport.Command{Script: "echo hello"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if string(res.Stdout) != "hello\n" {
		t.Fatalf("Stdout = %q", res.Stdout)
	}
	if string(res.Stderr) != "warn\n" {
		t.Fatalf("Stderr = %q", res.Stderr)
	}
	if res.RC != 3 {
		t.Fatalf("RC = %d, want 3", res.RC)
	}
	if res.Host != f.host {
		t.Fatalf("Host = %q, want %q", res.Host, f.host)
	}
	if res.Via != "ssh" {
		t.Fatalf("Via = %q, want %q", res.Via, "ssh")
	}
	if res.Duration <= 0 {
		t.Fatalf("Duration = %v, want > 0", res.Duration)
	}
	if res.Truncated {
		t.Fatal("Truncated is true for a small output")
	}
	if f.tr.Name() != "ssh" {
		t.Fatalf("Name = %q, want %q", f.tr.Name(), "ssh")
	}
}

func TestExecNonZeroIsNotError(t *testing.T) {
	f := newXFFixture(t, xfOptions{handler: func(_ string, _ io.Reader, _, stderr io.Writer) int {
		_, _ = io.WriteString(stderr, "boom\n")
		return 1
	}})

	res, err := f.tr.Exec(context.Background(), f.host, transport.Command{Script: "false"})
	if err != nil {
		t.Fatalf("Exec of a failing command: %v (a non-zero rc is not an error)", err)
	}
	if res.RC != 1 {
		t.Fatalf("RC = %d, want 1", res.RC)
	}
	if string(res.Stderr) != "boom\n" {
		t.Fatalf("Stderr = %q", res.Stderr)
	}
}

func TestExecSudoWrapsCommand(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	f := newXFFixture(t, xfOptions{handler: func(cmd string, _ io.Reader, stdout, _ io.Writer) int {
		mu.Lock()
		seen = append(seen, cmd)
		mu.Unlock()
		_, _ = io.WriteString(stdout, "ok\n")
		return 0
	}})

	for _, sudo := range []bool{false, true} {
		if _, err := f.tr.Exec(context.Background(), f.host, transport.Command{Script: "id -un", Sudo: sudo}); err != nil {
			t.Fatalf("Exec(sudo=%v): %v", sudo, err)
		}
	}

	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("handler saw %d commands, want 2", len(got))
	}
	if !strings.HasPrefix(got[0], "sh -c ") || !strings.Contains(got[0], "id -un") {
		t.Fatalf("plain command = %q, want a `sh -c` line", got[0])
	}
	if !strings.HasPrefix(got[1], "sudo -n -- sh -c ") || !strings.Contains(got[1], "id -un") {
		t.Fatalf("sudo command = %q, want a `sudo -n -- sh -c` line", got[1])
	}
}

func TestExecSudoPasswordRequired(t *testing.T) {
	f := newXFFixture(t, xfOptions{handler: func(_ string, _ io.Reader, _, stderr io.Writer) int {
		_, _ = io.WriteString(stderr, "sudo: a password is required\n")
		return 1
	}})

	res, err := f.tr.Exec(context.Background(), f.host, transport.Command{Script: "id -un", Sudo: true})
	if !errs.Is(err, errs.Sudo) {
		t.Fatalf("Err = %v (kind %q), want kind %q", err, errs.KindOf(err), errs.Sudo)
	}
	if !strings.Contains(string(res.Stderr), "a password is required") {
		t.Fatalf("Stderr = %q, want the sudo message", res.Stderr)
	}
	if res.RC != 1 {
		t.Fatalf("RC = %d, want 1", res.RC)
	}

	// Without Sudo the same stderr is just data: rc 1 is not an error.
	res, err = f.tr.Exec(context.Background(), f.host, transport.Command{Script: "id -un"})
	if err != nil {
		t.Fatalf("Exec without sudo: %v", err)
	}
	if res.RC != 1 {
		t.Fatalf("RC = %d, want 1", res.RC)
	}
}

func TestExecTimeout(t *testing.T) {
	f := newXFFixture(t, xfOptions{
		handler: func(string, io.Reader, io.Writer, io.Writer) int {
			time.Sleep(2 * time.Second)
			return 0
		},
		tr: Options{ExecTimeout: 150 * time.Millisecond},
	})

	start := time.Now()
	res, err := f.tr.Exec(context.Background(), f.host, transport.Command{Script: "sleep 2"})
	elapsed := time.Since(start)
	if !errs.Is(err, errs.Timeout) {
		t.Fatalf("Err = %v (kind %q), want kind %q", err, errs.KindOf(err), errs.Timeout)
	}
	if elapsed > testload.Scale(1500*time.Millisecond) {
		t.Fatalf("Exec took %v, want it back near the 150ms timeout", elapsed)
	}
	if res.RC != -1 {
		t.Fatalf("RC = %d, want -1", res.RC)
	}
	if res.Host != f.host || res.Via != "ssh" {
		t.Fatalf("result = %+v, want host and via filled", res)
	}
}

func TestExecCancel(t *testing.T) {
	f := newXFFixture(t, xfOptions{handler: func(string, io.Reader, io.Writer, io.Writer) int {
		time.Sleep(2 * time.Second)
		return 0
	}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	res, err := f.tr.Exec(ctx, f.host, transport.Command{Script: "sleep 2"})
	elapsed := time.Since(start)
	if !errs.Is(err, errs.Canceled) {
		t.Fatalf("Err = %v (kind %q), want kind %q", err, errs.KindOf(err), errs.Canceled)
	}
	if elapsed > testload.Scale(time.Second) {
		t.Fatalf("Exec took %v after a cancel, want it back at once", elapsed)
	}
	if res.RC != -1 {
		t.Fatalf("RC = %d, want -1", res.RC)
	}
}

func TestExecTruncates(t *testing.T) {
	const total = 5 << 20
	const limit = 1 << 20
	f := newXFFixture(t, xfOptions{
		handler: func(_ string, _ io.Reader, stdout, _ io.Writer) int {
			_, _ = io.WriteString(stdout, strings.Repeat("a", total))
			return 0
		},
		tr: Options{MaxOutput: limit},
	})

	res, err := f.tr.Exec(context.Background(), f.host, transport.Command{Script: "cat big"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.RC != 0 {
		t.Fatalf("RC = %d, want 0: the command must still exit cleanly", res.RC)
	}
	if !res.Truncated {
		t.Fatal("Truncated is false for 5 MiB against a 1 MiB cap")
	}
	if len(res.Stdout) != limit {
		t.Fatalf("Stdout is %d bytes, want the %d byte cap", len(res.Stdout), limit)
	}
	if strings.Trim(string(res.Stdout), "a") != "" {
		t.Fatal("Stdout holds something other than the first bytes")
	}
}

func TestExecThroughBastion(t *testing.T) {
	f := newXFFixture(t, xfOptions{
		bastion: true,
		handler: func(_ string, _ io.Reader, stdout, _ io.Writer) int {
			_, _ = io.WriteString(stdout, "ok\n")
			return 0
		},
	})

	res, err := f.tr.Exec(context.Background(), f.host, transport.Command{Script: "uptime"})
	if err != nil {
		t.Fatalf("Exec through the bastion: %v", err)
	}
	if string(res.Stdout) != "ok\n" {
		t.Fatalf("Stdout = %q", res.Stdout)
	}
	if want := "ssh via " + xfBastionAlias; res.Via != want {
		t.Fatalf("Via = %q, want %q", res.Via, want)
	}
}

func TestExecServerDiesMidCommand(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	f := newXFFixture(t, xfOptions{
		proxy: true,
		handler: func(cmd string, _ io.Reader, stdout, _ io.Writer) int {
			if strings.Contains(cmd, "sleep") {
				once.Do(func() { close(started) })
				time.Sleep(2 * time.Second)
				return 1
			}
			_, _ = io.WriteString(stdout, "hi\n")
			return 0
		},
	})

	go func() {
		<-started
		time.Sleep(50 * time.Millisecond)
		f.proxy.cut()
	}()

	res, err := f.tr.Exec(context.Background(), f.host, transport.Command{Script: "sleep 2"})
	if !errs.Is(err, errs.Unreachable) {
		t.Fatalf("Err = %v (kind %q), want kind %q", err, errs.KindOf(err), errs.Unreachable)
	}
	if res.RC != -1 {
		t.Fatalf("RC = %d, want -1: the command never reported a status", res.RC)
	}

	// The dead connection was dropped, so the next Exec dials again.
	res, err = f.tr.Exec(context.Background(), f.host, transport.Command{Script: "echo hi"})
	if err != nil {
		t.Fatalf("Exec after the reconnect: %v", err)
	}
	if string(res.Stdout) != "hi\n" || res.RC != 0 {
		t.Fatalf("result after reconnect = %+v", res)
	}
	if st := f.pool.Stats(); st.Targets != 1 || st.Leases != 0 {
		t.Fatalf("stats = %+v, want one pooled target and no lease left behind", st)
	}
}

func TestExecSessionLimitRejected(t *testing.T) {
	f := newXFFixture(t, xfOptions{
		maxSessions: 1,
		handler:     sshtest.CommandTable(map[string]string{"uptime": "ok\n"}),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := f.pool.Client(ctx, f.host)
	if err != nil {
		t.Fatalf("hold a session: %v", err)
	}
	defer lease.Release()
	held, err := lease.Client().NewSession()
	if err != nil {
		t.Fatalf("hold the only session slot: %v", err)
	}
	defer func() { _ = held.Close() }()

	_, err = f.tr.Exec(ctx, f.host, transport.Command{Script: "uptime"})
	if !errs.Is(err, errs.Unreachable) {
		t.Fatalf("Err = %v (kind %q), want kind %q", err, errs.KindOf(err), errs.Unreachable)
	}
	if !strings.Contains(err.Error(), "MaxSessions") {
		t.Fatalf("Err = %q, want the MaxSessions hint", err)
	}
}

func TestExecSessionRetry(t *testing.T) {
	f := newXFFixture(t, xfOptions{
		maxSessions: 1,
		handler:     sshtest.CommandTable(map[string]string{"uptime": "ok\n"}),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := f.pool.Client(ctx, f.host)
	if err != nil {
		t.Fatalf("hold a session: %v", err)
	}
	held, err := lease.Client().NewSession()
	if err != nil {
		lease.Release()
		t.Fatalf("hold the only session slot: %v", err)
	}
	// The slot frees up well inside the 200 ms retry delay.
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = held.Close()
		lease.Release()
	}()

	res, err := f.tr.Exec(ctx, f.host, transport.Command{Script: "uptime"})
	if err != nil {
		t.Fatalf("Exec after the session slot freed up: %v", err)
	}
	if string(res.Stdout) != "ok\n" || res.RC != 0 {
		t.Fatalf("result = %+v", res)
	}
}
