//go:build e2e

// End-to-end detach tests: the real binary, a real pseudo-terminal and the
// session server, with every byte the server paints fed through the in-process
// VT emulator from task 049. Run with `make e2e` (or
// `go test -tags e2e -run TestE2E ./internal/app/ -count=1`); they are excluded
// from `go test ./...` because they build a binary and need PTYs.
package app

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"jumpr/internal/mux/vt"
)

// e2eDeadline is how long a screen assertion may take. Every wait polls the
// emulator; none of them sleeps for a fixed time and then asserts.
const e2eDeadline = 15 * time.Second

// e2eClient is one attached terminal: the child process, the PTY master, the
// raw byte log and the VT emulator the bytes are painted into.
type e2eClient struct {
	cmd   *exec.Cmd
	f     *os.File
	emu   vt.Emulator
	waitc chan error

	mu  sync.Mutex
	raw bytes.Buffer
}

// buildJumpr builds the real binary once per test. `go build` is cached, so
// this is quick after the first test.
func buildJumpr(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the binary and needs a PTY")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(shortDir(t), "jumpr")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/jumpr")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// e2eEnv is the environment the client and the server see: a private HOME, no
// inherited socket, no nested-session marker and no tmux.
func e2eEnv(t *testing.T, home string) []string {
	t.Helper()
	env := replaceEnv(os.Environ(), "HOME", home)
	env = replaceEnv(env, "JUMPR_SOCKET", "")
	env = replaceEnv(env, "SITREP", "")
	env = replaceEnv(env, "TMUX", "")
	return replaceEnv(env, "TERM", "xterm-256color")
}

// startE2E starts `jumpr -S sock --demo-server --config-dir cfg` on a fresh PTY
// of cols×rows and continuously drains it into the emulator.
func startE2E(t *testing.T, bin, home, sock, cfg string, cols, rows int) *e2eClient {
	t.Helper()
	cmd := exec.Command(bin, "-S", sock, "--demo-server", "--config-dir", cfg)
	cmd.Env = e2eEnv(t, home)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		t.Fatalf("start pty: %v", err)
	}
	emu := vt.New(cols, rows, 4000)
	c := &e2eClient{cmd: cmd, f: f, emu: emu, waitc: make(chan error, 1)}
	go func() { c.waitc <- cmd.Wait() }()
	// The emulator's reply stream must be drained or Write blocks.
	go io.Copy(io.Discard, emu.Responses()) //nolint:errcheck // test plumbing
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := f.Read(buf)
			if n > 0 {
				c.mu.Lock()
				c.raw.Write(buf[:n])
				c.mu.Unlock()
				_, _ = emu.Write(buf[:n])
			}
			if rerr != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = f.Close()
		_ = emu.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	return c
}

// screen is the visible screen as plain text, exactly rows lines of cols cells.
func (c *e2eClient) screen() string { return ansi.Strip(c.emu.Render()) }

// waitFor polls the screen until cond is true.
func (c *e2eClient) waitFor(t *testing.T, what string, cond func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(e2eDeadline)
	for time.Now().Before(deadline) {
		if cond(c.screen()) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("screen never showed %s; last screen:\n%s", what, c.screen())
}

// waitText waits for a substring of the visible screen.
func (c *e2eClient) waitText(t *testing.T, want string) {
	t.Helper()
	c.waitFor(t, want, func(s string) bool { return strings.Contains(s, want) })
}

// paneCount is how many pane titles the screen shows.
func (c *e2eClient) paneCount() int {
	n := 0
	for _, line := range strings.Split(c.screen(), "\n") {
		n += len(paneTitleRE.FindAllString(line, -1))
	}
	return n
}

// typeSlow writes one byte at a time, giving Bubble Tea a separate read per
// key. A single write of ":alerts\r" can arrive as one unparseable key.
func (c *e2eClient) typeSlow(t *testing.T, s string) {
	t.Helper()
	for _, b := range []byte(s) {
		if _, err := c.f.Write([]byte{b}); err != nil {
			t.Fatalf("write %q: %v", string(b), err)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// rawLen is the current length of the byte log, for frame-relative assertions.
func (c *e2eClient) rawLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.raw.Len()
}

// rawFrom returns a copy of the bytes written after offset off.
func (c *e2eClient) rawFrom(off int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if off > c.raw.Len() {
		return ""
	}
	return string(c.raw.Bytes()[off:])
}

// waitFrame polls the frames painted after offset off for one that satisfies
// cond. Frames are the renderer's deltas, split the way pipe_test.go does it.
func (c *e2eClient) waitFrame(t *testing.T, off int, what string, cond func(lines []string) bool) {
	t.Helper()
	deadline := time.Now().Add(e2eDeadline)
	for time.Now().Before(deadline) {
		for _, lines := range frameLines(c.rawFrom(off)) {
			if cond(lines) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no frame after byte %d showed %s", off, what)
}

// waitExit waits for the child to end and returns its exit code.
func (c *e2eClient) waitExit(t *testing.T) int {
	t.Helper()
	select {
	case err := <-c.waitc:
		if err == nil {
			return 0
		}
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			return ee.ExitCode()
		}
		t.Fatalf("wait: %v", err)
	case <-time.After(e2eDeadline):
		t.Fatal("client did not exit")
	}
	return -1
}

// asExitError is errors.As without importing errors for one call site.
func asExitError(err error, target **exec.ExitError) bool {
	for err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			*target = ee
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// lastLine is the last non-empty line the client wrote to its terminal.
func (c *e2eClient) lastLine() string {
	lines := strings.Split(ansi.Strip(c.rawFrom(0)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(strings.TrimRight(lines[i], "\r")); s != "" {
			return s
		}
	}
	return ""
}

// runJumpr runs a control command with the test's private environment.
func runJumpr(t *testing.T, bin, home, sock string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"-S", sock}, args...)...)
	cmd.Env = e2eEnv(t, home)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var ee *exec.ExitError
	if asExitError(err, &ee) {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("run jumpr %v: %v", args, err)
	return "", -1
}

// killServer is the cleanup path: it stops whatever server the test left.
func killServer(t *testing.T, bin, home, sock string) {
	t.Helper()
	cmd := exec.Command(bin, "-S", sock, "kill-server")
	cmd.Env = e2eEnv(t, home)
	_ = cmd.Run()
}

// e2eSetup builds the binary and a private home, config dir and socket.
func e2eSetup(t *testing.T) (bin, home, cfg, sock string) {
	t.Helper()
	bin = buildJumpr(t)
	dir := shortDir(t)
	home = filepath.Join(dir, "home")
	cfg = filepath.Join(dir, "config")
	for _, d := range []string{home, cfg} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sock = filepath.Join(dir, "s.sock")
	t.Cleanup(func() { killServer(t, bin, home, sock) })
	return bin, home, cfg, sock
}

// openDemoHost opens one known demo host. The host list is sorted by load, so
// web-ewr-001 is not on the first screen; opening it explicitly is how the pipe
// test pins the same host.
func openDemoHost(t *testing.T, c *e2eClient) {
	t.Helper()
	c.waitText(t, "1,512 hosts")
	c.typeSlow(t, ":host host=web-ewr-001\r")
	c.waitFor(t, "the one-host table", func(s string) bool {
		return strings.Contains(s, "web-ewr-001") && strings.Contains(s, "1 hosts")
	})
}

// TestE2EDetachReattach is requirement 1: attach, type a command, split, detach
// with prefix d, get the terminal back, attach a second client at a different
// size, list the session and end the server.
func TestE2EDetachReattach(t *testing.T) {
	bin, home, cfg, sock := e2eSetup(t)

	c := startE2E(t, bin, home, sock, cfg, 120, 36)
	openDemoHost(t, c)

	c.typeSlow(t, ":alerts\r")
	c.waitText(t, "HighErrorRate")

	// Prefix C-a then % splits the window in two.
	c.typeSlow(t, "\x01%")
	c.waitFor(t, "two panes", func(string) bool { return c.paneCount() >= 2 })

	// Prefix d detaches: the client exits 0 and says where it left from.
	c.typeSlow(t, "\x01d")
	if code := c.waitExit(t); code != 0 {
		t.Fatalf("detach exit code = %d, want 0", code)
	}
	if got, want := c.lastLine(), "[detached (from session main)]"; got != want {
		t.Fatalf("last line = %q, want %q", got, want)
	}

	// The terminal is back in canonical mode with echo on.
	tio, err := unix.IoctlGetTermios(int(c.f.Fd()), unix.TIOCGETA)
	if err != nil {
		t.Fatalf("termios: %v", err)
	}
	if tio.Lflag&unix.ICANON == 0 || tio.Lflag&unix.ECHO == 0 {
		t.Fatalf("terminal was not restored: lflag = %#x, want ICANON and ECHO set", tio.Lflag)
	}

	// A second client, different size: both panes and the alert view are
	// still there, laid out for 100×30.
	c2 := startE2E(t, bin, home, sock, cfg, 100, 30)
	c2.waitText(t, "HighErrorRate")
	c2.waitFor(t, "two panes at 100×30", func(s string) bool {
		return c2.paneCount() >= 2 && len(strings.Split(s, "\n")) == 30
	})
	c2.waitFrame(t, 0, "a 100-column status line", func(lines []string) bool {
		for _, l := range lines {
			if strings.HasPrefix(l, "jumpr") && ansi.StringWidth(l) == 100 {
				return true
			}
		}
		return false
	})

	// ls reports exactly one session, and it is attached.
	out, code := runJumpr(t, bin, home, sock, "ls")
	if code != 0 {
		t.Fatalf("ls exit code = %d: %s", code, out)
	}
	lines := nonEmptyLines(out)
	if len(lines) != 1 {
		t.Fatalf("ls printed %d lines, want 1:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], "main") || !strings.Contains(lines[0], "(attached)") {
		t.Fatalf("ls line = %q, want main and attached", lines[0])
	}

	// kill-server ends the client with the server's exit message.
	if out, code := runJumpr(t, bin, home, sock, "kill-server"); code != 0 {
		t.Fatalf("kill-server exit code = %d: %s", code, out)
	}
	if code := c2.waitExit(t); code != 0 {
		t.Fatalf("client exit after kill-server = %d, want 0", code)
	}
	if got := c2.lastLine(); !strings.HasSuffix(got, "[server exited]") {
		t.Fatalf("last line after kill-server = %q, want it to end with %q", got, "[server exited]")
	}
}

// TestE2EClientKilled is requirement 2: killing the client with SIGKILL leaves
// the session whole; a new client attaches to the same layout.
func TestE2EClientKilled(t *testing.T) {
	bin, home, cfg, sock := e2eSetup(t)

	c := startE2E(t, bin, home, sock, cfg, 120, 36)
	openDemoHost(t, c)
	c.typeSlow(t, ":alerts\r")
	c.waitText(t, "HighErrorRate")

	if err := c.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill client: %v", err)
	}
	c.waitExit(t)

	c2 := startE2E(t, bin, home, sock, cfg, 120, 36)
	c2.waitText(t, "HighErrorRate")
}

// TestE2EResize is requirement 3: a PTY resize reaches the server and the next
// frames are drawn at the new width.
func TestE2EResize(t *testing.T) {
	bin, home, cfg, sock := e2eSetup(t)

	c := startE2E(t, bin, home, sock, cfg, 120, 36)
	openDemoHost(t, c)
	c.waitFrame(t, 0, "a 120-column status line", func(lines []string) bool {
		for _, l := range lines {
			if strings.HasPrefix(l, "jumpr") && ansi.StringWidth(l) == 120 {
				return true
			}
		}
		return false
	})

	off := c.rawLen()
	if err := pty.Setsize(c.f, &pty.Winsize{Cols: 90, Rows: 25}); err != nil {
		t.Fatalf("setsize: %v", err)
	}
	c.emu.Resize(90, 25)
	c.waitFrame(t, off, "a 90-column status line", func(lines []string) bool {
		for _, l := range lines {
			if strings.HasPrefix(l, "jumpr") && ansi.StringWidth(l) == 90 {
				return true
			}
		}
		return false
	})
}

// nonEmptyLines returns the trimmed non-empty lines of s.
func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
