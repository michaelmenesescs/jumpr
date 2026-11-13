package sshtest_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"jumper/internal/transport/ssh/sshtest"

	gossh "golang.org/x/crypto/ssh"
)

const testTimeout = 5 * time.Second

func newSigner(t testing.TB) gossh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("client signer: %v", err)
	}
	return signer
}

func dial(t testing.TB, addr string, hostKey gossh.PublicKey, signer gossh.Signer) *gossh.Client {
	t.Helper()
	client, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "tester",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.FixedHostKey(hostKey),
		Timeout:         testTimeout,
	})
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// sink collects everything read from a session so a test can wait for a
// substring without racing a second reader.
type sink struct {
	mu      sync.Mutex
	buf     []byte
	changed chan struct{}
}

func watch(r io.Reader) *sink {
	s := &sink{changed: make(chan struct{})}
	go func() {
		b := make([]byte, 1024)
		for {
			n, err := r.Read(b)
			if n > 0 {
				s.mu.Lock()
				s.buf = append(s.buf, b[:n]...)
				close(s.changed)
				s.changed = make(chan struct{})
				s.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return s
}

func (s *sink) waitContains(t testing.TB, want string) string {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		s.mu.Lock()
		got := string(s.buf)
		ch := s.changed
		s.mu.Unlock()
		if strings.Contains(got, want) {
			return got
		}
		select {
		case <-ch:
		case <-time.After(time.Until(deadline)):
			t.Fatalf("timeout waiting for %q in output %q", want, got)
		}
	}
}

func TestExecRunsHandler(t *testing.T) {
	signer := newSigner(t)
	var mu sync.Mutex
	var gotCmd, gotStdin string

	srv := sshtest.Start(t, sshtest.Options{
		AuthorizedKeys: []gossh.PublicKey{signer.PublicKey()},
		Handler: func(cmd string, stdin io.Reader, stdout, stderr io.Writer) int {
			b, _ := io.ReadAll(stdin)
			mu.Lock()
			gotCmd, gotStdin = cmd, string(b)
			mu.Unlock()
			io.WriteString(stdout, "out:"+cmd)
			io.WriteString(stderr, "err:text")
			return 0
		},
	})
	client := dial(t, srv.Addr(), srv.HostKey(), signer)

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()
	sess.Stdin = strings.NewReader("payload")

	out, err := sess.Output(`echo 'a b'  "$x" && uptime`)
	if err != nil {
		t.Fatalf("output: %v", err)
	}
	if want := `out:echo 'a b'  "$x" && uptime`; string(out) != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotCmd != `echo 'a b'  "$x" && uptime` {
		t.Fatalf("handler command = %q", gotCmd)
	}
	if gotStdin != "payload" {
		t.Fatalf("handler stdin = %q, want payload", gotStdin)
	}
	if srv.Sessions() != 1 {
		t.Fatalf("Sessions() = %d, want 1", srv.Sessions())
	}
	if srv.Conns() != 1 {
		t.Fatalf("Conns() = %d, want 1", srv.Conns())
	}
}

func TestExitStatus(t *testing.T) {
	signer := newSigner(t)
	for _, want := range []int{0, 1, 42, 127} {
		srv := sshtest.Start(t, sshtest.Options{
			Handler: func(string, io.Reader, io.Writer, io.Writer) int { return want },
		})
		client := dial(t, srv.Addr(), srv.HostKey(), signer)
		sess, err := client.NewSession()
		if err != nil {
			t.Fatalf("new session: %v", err)
		}
		err = sess.Run("anything")
		_ = sess.Close()
		if want == 0 {
			if err != nil {
				t.Fatalf("status 0: %v", err)
			}
			continue
		}
		exitErr, ok := err.(*gossh.ExitError)
		if !ok {
			t.Fatalf("status %d: err = %v (%T), want *ssh.ExitError", want, err, err)
		}
		if got := exitErr.ExitStatus(); got != want {
			t.Fatalf("exit status = %d, want %d", got, want)
		}
	}
}

func TestMaxSessionsRejects(t *testing.T) {
	signer := newSigner(t)
	srv := sshtest.Start(t, sshtest.Options{
		MaxSessions: 1,
		Handler:     func(string, io.Reader, io.Writer, io.Writer) int { return 0 },
	})
	client := dial(t, srv.Addr(), srv.HostKey(), signer)

	first, err := client.NewSession()
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	if _, err := client.NewSession(); err == nil {
		t.Fatalf("second session was accepted, want rejection")
	} else if !strings.Contains(err.Error(), "max sessions") {
		t.Fatalf("second session error = %v, want it to mention max sessions", err)
	}
	if got := srv.MaxConcurrentSessions(); got != 1 {
		t.Fatalf("MaxConcurrentSessions() = %d, want 1", got)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	deadline := time.Now().Add(testTimeout)
	for {
		sess, err := client.NewSession()
		if err == nil {
			sess.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session never became available again: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestForwardToSecondServer(t *testing.T) {
	signer := newSigner(t)

	target := sshtest.Start(t, sshtest.Options{
		Handler: sshtest.CommandTable(map[string]string{"uptime": "10:00 up 3 days"}),
	})
	bastion := sshtest.Start(t, sshtest.Options{AllowForward: true})

	client := dial(t, bastion.Addr(), bastion.HostKey(), signer)

	conn, err := client.Dial("tcp", target.Addr())
	if err != nil {
		t.Fatalf("dial through bastion: %v", err)
	}
	defer conn.Close()

	cc, chans, reqs, err := gossh.NewClientConn(conn, target.Addr(), &gossh.ClientConfig{
		User:            "tester",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.FixedHostKey(target.HostKey()),
		Timeout:         testTimeout,
	})
	if err != nil {
		t.Fatalf("handshake with target through bastion: %v", err)
	}
	inner := gossh.NewClient(cc, chans, reqs)
	defer inner.Close()

	sess, err := inner.NewSession()
	if err != nil {
		t.Fatalf("inner session: %v", err)
	}
	out, err := sess.Output("uptime")
	if err != nil {
		t.Fatalf("inner output: %v", err)
	}
	if string(out) != "10:00 up 3 days" {
		t.Fatalf("through-bastion output = %q", out)
	}
	if got := bastion.MaxConcurrentChannels(); got != 1 {
		t.Fatalf("MaxConcurrentChannels() = %d, want 1", got)
	}
	if got := bastion.Sessions(); got != 0 {
		t.Fatalf("bastion Sessions() = %d, want 0 (no session channel on the bastion)", got)
	}
}

func TestShellEchoAndResize(t *testing.T) {
	signer := newSigner(t)
	srv := sshtest.Start(t, sshtest.Options{
		Handler: func(string, io.Reader, io.Writer, io.Writer) int { return 0 },
	})
	client := dial(t, srv.Addr(), srv.HostKey(), signer)

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()

	if err := sess.RequestPty("xterm-256color", 24, 80, gossh.TerminalModes{}); err != nil {
		t.Fatalf("request pty: %v", err)
	}
	// Pipes must be taken before the shell request starts the process.
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	got := watch(stdout)

	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	// Only window-change is reported; the initial PTY size is not a resize.
	if err := sess.WindowChange(40, 120); err != nil {
		t.Fatalf("window change: %v", err)
	}

	if _, err := stdin.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got.waitContains(t, "hello")
	got.waitContains(t, "resize 120x40")

	if _, err := stdin.Write([]byte("exit\n")); err != nil {
		t.Fatalf("write exit: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shell wait: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatalf("shell did not exit")
	}
}

func TestKillDropsConnections(t *testing.T) {
	signer := newSigner(t)
	srv := sshtest.Start(t, sshtest.Options{
		Handler: sshtest.CommandTable(map[string]string{"ping": "pong"}),
	})
	client := dial(t, srv.Addr(), srv.HostKey(), signer)

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	out, err := sess.Output("ping")
	if err != nil {
		t.Fatalf("output: %v", err)
	}
	if string(out) != "pong" {
		t.Fatalf("output = %q", out)
	}
	_ = sess.Close()

	srv.Kill()
	srv.Kill() // idempotent: t.Cleanup calls it again

	if _, err := client.NewSession(); err == nil {
		t.Fatalf("session on a killed server was accepted")
	}

	deadline := time.Now().Add(testTimeout)
	for {
		conn, err := net.DialTimeout("tcp", srv.Addr(), 200*time.Millisecond)
		if err != nil {
			break
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatalf("server still accepting connections after Kill")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCommandTable(t *testing.T) {
	table := sshtest.CommandTable(map[string]string{
		"up":     "short",
		"uptime": "long",
		"df -h":  "disk",
	})

	tests := []struct {
		cmd        string
		wantOut    string
		wantErr    string
		wantStatus int
	}{
		{cmd: "uptime", wantOut: "long"},
		{cmd: "sh -c uptime", wantOut: "long"},              // longest substring wins
		{cmd: "cat /proc/uptime -- df -h", wantOut: "long"}, // longest key wins
		{cmd: "df -h", wantOut: "disk"},
		{cmd: "up", wantOut: "short"},
		{cmd: "true; up", wantOut: "short"},
		{cmd: "nope", wantErr: "not found", wantStatus: 127},
		{cmd: "", wantErr: "not found", wantStatus: 127},
	}
	for _, tc := range tests {
		var stdout, stderr bytes.Buffer
		status := table(tc.cmd, strings.NewReader(""), &stdout, &stderr)
		if status != tc.wantStatus {
			t.Fatalf("%q: status = %d, want %d", tc.cmd, status, tc.wantStatus)
		}
		if stdout.String() != tc.wantOut {
			t.Fatalf("%q: stdout = %q, want %q", tc.cmd, stdout.String(), tc.wantOut)
		}
		if !strings.Contains(stderr.String(), tc.wantErr) {
			t.Fatalf("%q: stderr = %q, want %q", tc.cmd, stderr.String(), tc.wantErr)
		}
	}
}

func TestAuthorizedKeys(t *testing.T) {
	allowed := newSigner(t)
	other := newSigner(t)

	srv := sshtest.Start(t, sshtest.Options{
		AuthorizedKeys: []gossh.PublicKey{allowed.PublicKey()},
		Handler:        func(string, io.Reader, io.Writer, io.Writer) int { return 0 },
	})

	if _, err := gossh.Dial("tcp", srv.Addr(), &gossh.ClientConfig{
		User:            "tester",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(other)},
		HostKeyCallback: gossh.FixedHostKey(srv.HostKey()),
		Timeout:         testTimeout,
	}); err == nil {
		t.Fatalf("unauthorized key was accepted")
	}

	// An empty AuthorizedKeys list accepts any key.
	open := sshtest.Start(t, sshtest.Options{
		Handler: func(string, io.Reader, io.Writer, io.Writer) int { return 0 },
	})
	client := dial(t, open.Addr(), open.HostKey(), other)
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("session with any key: %v", err)
	}
	if err := sess.Run("anything"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.HasPrefix(open.KnownHostsLine(), "[127.0.0.1]:") {
		t.Fatalf("KnownHostsLine() = %q", open.KnownHostsLine())
	}
	if open.Port() == 0 {
		t.Fatalf("Port() = 0")
	}
}

func TestConnDelay(t *testing.T) {
	signer := newSigner(t)
	srv := sshtest.Start(t, sshtest.Options{
		ConnDelay: 150 * time.Millisecond,
		Handler:   func(string, io.Reader, io.Writer, io.Writer) int { return 0 },
	})

	start := time.Now()
	client := dial(t, srv.Addr(), srv.HostKey(), signer)
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("handshake took %v, want at least the 150ms ConnDelay", elapsed)
	}
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if err := sess.Run("x"); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestHostKeyType(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{HostKeyType: "rsa"})
	if got := srv.HostKey().Type(); got != gossh.KeyAlgoRSA {
		t.Fatalf("host key type = %q, want %q", got, gossh.KeyAlgoRSA)
	}

	def := sshtest.Start(t, sshtest.Options{})
	if got := def.HostKey().Type(); got != gossh.KeyAlgoED25519 {
		t.Fatalf("default host key type = %q, want %q", got, gossh.KeyAlgoED25519)
	}
}
