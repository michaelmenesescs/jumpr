package ssh

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"jumper/internal/transport"
)

// shellLog drains one Shell in the background so a test can wait for output
// without racing a second reader on the same PTY.
type shellLog struct {
	mu  sync.Mutex
	buf strings.Builder
	err error
}

// watchShell starts the only reader of sh.
func watchShell(sh transport.Shell) *shellLog {
	sl := &shellLog{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := sh.Read(buf)
			if n > 0 {
				sl.mu.Lock()
				sl.buf.Write(buf[:n])
				sl.mu.Unlock()
			}
			if err != nil {
				sl.mu.Lock()
				sl.err = err
				sl.mu.Unlock()
				return
			}
		}
	}()
	return sl
}

// text returns everything read so far.
func (s *shellLog) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// waitFor fails unless want turns up in the output within five seconds.
func (s *shellLog) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if strings.Contains(s.text(), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("shell output %q never contained %q", s.text(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// openTestShell opens a PTY shell on the fixture host.
func openTestShell(t *testing.T, f *xfFixture, req transport.PTYRequest) transport.Shell {
	t.Helper()
	sh, err := f.tr.OpenShell(context.Background(), f.host, req)
	if err != nil {
		t.Fatalf("OpenShell: %v", err)
	}
	t.Cleanup(func() { _ = sh.Close() })
	return sh
}

// TestShellEcho checks that keystrokes reach the remote login shell and its
// output comes back.
func TestShellEcho(t *testing.T) {
	f := newXFFixture(t, xfOptions{})
	sh := openTestShell(t, f, transport.PTYRequest{Term: "xterm-256color", Cols: 80, Rows: 24})
	log := watchShell(sh)

	if _, err := io.WriteString(sh, "hello\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	log.waitFor(t, "hello")
}

// TestShellResize checks that Resize reaches the remote PTY as a window change
// with the columns and rows the caller asked for.
func TestShellResize(t *testing.T) {
	f := newXFFixture(t, xfOptions{})
	sh := openTestShell(t, f, transport.PTYRequest{Term: "xterm-256color", Cols: 80, Rows: 24})
	log := watchShell(sh)

	if err := sh.Resize(100, 30); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	log.waitFor(t, "resize 100x30")
}

// TestShellCloseReleasesLease checks that Close hands the pool's session slot
// back exactly once, and that a second Close is harmless.
func TestShellCloseReleasesLease(t *testing.T) {
	f := newXFFixture(t, xfOptions{})
	sh := openTestShell(t, f, transport.PTYRequest{Term: "xterm-256color", Cols: 80, Rows: 24})
	watchShell(sh)

	if s := f.pool.Stats(); s.Leases != 1 {
		t.Fatalf("stats = %+v, want the shell's lease held", s)
	}
	if err := sh.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if s := f.pool.Stats(); s.Leases != 0 {
		t.Fatalf("stats = %+v, want the lease released by Close", s)
	}
	if err := sh.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if s := f.pool.Stats(); s.Leases != 0 {
		t.Fatalf("stats = %+v, want no lease after a second Close", s)
	}
}

// TestShellWaitReturnsOnExit checks that Wait returns when the remote shell
// exits, and that the exit code is not an error.
func TestShellWaitReturnsOnExit(t *testing.T) {
	f := newXFFixture(t, xfOptions{})
	sh := openTestShell(t, f, transport.PTYRequest{Term: "xterm-256color", Cols: 80, Rows: 24})
	watchShell(sh)

	if _, err := io.WriteString(sh, "exit\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- sh.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("Wait = %v, want nil after the shell exited", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after the remote shell exited")
	}

	if s := f.pool.Stats(); s.Leases != 0 {
		t.Fatalf("stats = %+v, want the lease released when the shell ended", s)
	}
}

// TestShellStartCommand checks that a non-empty Command starts that command on
// a PTY instead of a login shell.
func TestShellStartCommand(t *testing.T) {
	f := newXFFixture(t, xfOptions{handler: func(cmd string, _ io.Reader, stdout, _ io.Writer) int {
		_, _ = io.WriteString(stdout, "ran "+cmd+"\n")
		return 0
	}})
	sh := openTestShell(t, f, transport.PTYRequest{Term: "xterm-256color", Cols: 80, Rows: 24, Command: "uptime -p"})
	log := watchShell(sh)

	log.waitFor(t, "ran uptime -p")
	if err := sh.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}
