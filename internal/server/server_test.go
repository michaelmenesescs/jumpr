package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jumper/internal/errs"
)

// fakeSession is a SessionHandle that echoes its input to its output, records
// what the server told it, and runs until it is killed or the test ends.
type fakeSession struct {
	name string

	mu       sync.Mutex
	attached bool
	cols     int
	rows     int
	env      map[string]string
	attachN  int
	detaches []string
	resizes  [][2]int
	killed   bool
	onDetach func(string)

	done chan struct{}
}

func newFakeSession(name string) *fakeSession {
	return &fakeSession{name: name, done: make(chan struct{})}
}

func (f *fakeSession) Attach(in io.Reader, out io.Writer, cols, rows int, env map[string]string, onDetach func(string)) error {
	f.mu.Lock()
	f.attached = true
	f.cols, f.rows = cols, rows
	f.env = env
	f.attachN++
	f.onDetach = onDetach
	f.mu.Unlock()

	go func() { _, _ = io.Copy(out, in) }()
	<-f.done
	return nil
}

func (f *fakeSession) Detach(reason string) {
	f.mu.Lock()
	f.detaches = append(f.detaches, reason)
	f.attached = false
	cb := f.onDetach
	f.mu.Unlock()
	if cb != nil {
		cb(reason)
	}
}

func (f *fakeSession) Resize(cols, rows int) {
	f.mu.Lock()
	f.resizes = append(f.resizes, [2]int{cols, rows})
	f.mu.Unlock()
}

func (f *fakeSession) Done() <-chan struct{} { return f.done }

func (f *fakeSession) Kill() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = true
	select {
	case <-f.done:
	default:
		close(f.done)
	}
}

func (f *fakeSession) Info() SessionInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return SessionInfo{Name: f.name, Windows: 1, Attached: f.attached}
}

func (f *fakeSession) attachCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attachN
}

func (f *fakeSession) detachReasons() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.detaches...)
}

func (f *fakeSession) resizesSeen() [][2]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]int(nil), f.resizes...)
}

func (f *fakeSession) size() (cols, rows int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cols, f.rows
}

func (f *fakeSession) envSeen() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.env
}

func (f *fakeSession) isKilled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.killed
}

// harness is one server in a short /tmp directory.
type harness struct {
	t    *testing.T
	dir  string
	run  string
	sock string
	lock string

	cancel context.CancelFunc
	done   chan error
	exited bool

	mu       sync.Mutex
	sessions map[string]*fakeSession
}

func newHarness(t *testing.T, idle time.Duration) *harness {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "jumper-srv-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return &harness{
		t:        t,
		dir:      dir,
		run:      filepath.Join(dir, "run"),
		sock:     filepath.Join(dir, "run", "s.sock"),
		lock:     filepath.Join(dir, "run", "s.lock"),
		sessions: make(map[string]*fakeSession),
	}
}

func (h *harness) options(idle time.Duration) Options {
	return Options{
		SocketPath: h.sock,
		LockPath:   h.lock,
		NewSession: func(name string) (SessionHandle, error) {
			if !ValidSessionName(name) {
				return nil, errs.New(errs.Config, "test.factory", "invalid name")
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			f := newFakeSession(name)
			h.sessions[name] = f
			return f, nil
		},
		Version:  "test",
		IdleExit: idle,
	}
}

func (h *harness) start(idle time.Duration) {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)
	o := h.options(idle)
	go func() { h.done <- Serve(ctx, o) }()
	waitFor(h.t, "socket", func() bool {
		fi, err := os.Stat(h.sock)
		return err == nil && fi.Mode()&os.ModeSocket != 0
	})
}

func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	h.mu.Lock()
	exited := h.exited
	h.mu.Unlock()
	if exited {
		return
	}
	select {
	case err := <-h.done:
		h.mu.Lock()
		h.exited = true
		h.mu.Unlock()
		if err != nil {
			h.t.Errorf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		h.t.Errorf("server did not stop")
	}
}

// waitExit waits for Serve to return and remembers that it did.
func (h *harness) waitExit() error {
	select {
	case err := <-h.done:
		h.mu.Lock()
		h.exited = true
		h.mu.Unlock()
		return err
	case <-time.After(5 * time.Second):
		h.t.Fatalf("server did not exit")
		return nil
	}
}

func (h *harness) fake(name string) *fakeSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[name]
}

func (h *harness) fakeNames() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	names := make([]string, 0, len(h.sessions))
	for n := range h.sessions {
		names = append(names, n)
	}
	return names
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (h *harness) dial() net.Conn {
	h.t.Helper()
	nc, err := net.DialTimeout("unix", h.sock, 2*time.Second)
	if err != nil {
		h.t.Fatalf("dial: %v", err)
	}
	h.t.Cleanup(func() { _ = nc.Close() })
	return nc
}

func (h *harness) hello(nc net.Conn, hello Hello) {
	h.t.Helper()
	if hello.Protocol == 0 {
		hello.Protocol = ProtocolVersion
	}
	if hello.Cols == 0 && hello.Rows == 0 {
		hello.Cols, hello.Rows = 120, 40
	}
	if err := WriteJSON(nc, FrameHello, hello); err != nil {
		h.t.Fatalf("write HELLO: %v", err)
	}
}

func readFrame(t *testing.T, nc net.Conn) (byte, []byte) {
	t.Helper()
	_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	typ, payload, err := ReadFrame(nc)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return typ, payload
}

func decodeWelcome(t *testing.T, payload []byte) Welcome {
	t.Helper()
	var w Welcome
	if err := json.Unmarshal(payload, &w); err != nil {
		t.Fatalf("decode WELCOME: %v", err)
	}
	return w
}

func decodeExitFrame(t *testing.T, payload []byte) Exit {
	t.Helper()
	var e Exit
	if err := json.Unmarshal(payload, &e); err != nil {
		t.Fatalf("decode EXIT: %v", err)
	}
	return e
}

func decodeResult(t *testing.T, payload []byte) Result {
	t.Helper()
	var r Result
	if err := json.Unmarshal(payload, &r); err != nil {
		t.Fatalf("decode RESULT: %v", err)
	}
	return r
}

// expectExit reads frames until EXIT arrives (STDOUT may come first).
func expectExit(t *testing.T, nc net.Conn) Exit {
	t.Helper()
	for {
		typ, payload := readFrame(t, nc)
		switch typ {
		case FrameExit:
			return decodeExitFrame(t, payload)
		case FrameStdout:
			continue
		default:
			t.Fatalf("unexpected frame %#x before EXIT", typ)
		}
	}
}

func (h *harness) attachTo(name string) (net.Conn, Welcome) {
	h.t.Helper()
	nc := h.dial()
	h.hello(nc, Hello{Command: "attach", Session: name, Env: map[string]string{"TERM": "xterm-256color"}})
	typ, payload := readFrame(h.t, nc)
	if typ != FrameWelcome {
		h.t.Fatalf("attach: frame %#x, want WELCOME (%s)", typ, payload)
	}
	return nc, decodeWelcome(h.t, payload)
}

func (h *harness) control(hello Hello) Result {
	h.t.Helper()
	nc := h.dial()
	h.hello(nc, hello)
	typ, payload := readFrame(h.t, nc)
	if typ != FrameResult {
		h.t.Fatalf("control %q: frame %#x, want RESULT (%s)", hello.Command, typ, payload)
	}
	return decodeResult(h.t, payload)
}

func hasString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func hasPair(list [][2]int, cols, rows int) bool {
	for _, p := range list {
		if p[0] == cols && p[1] == rows {
			return true
		}
	}
	return false
}

func TestServerAttachCreatesMain(t *testing.T) {
	h := newHarness(t, 0)
	h.start(0)
	defer h.stop()

	_, w := h.attachTo("")
	if w.Session != "main" || !w.Created || w.Protocol != ProtocolVersion {
		t.Fatalf("WELCOME = %+v, want main/created", w)
	}
	f := h.fake("main")
	if f == nil {
		t.Fatal("no session registered as main")
	}
	waitFor(t, "attach", func() bool { return f.attachCalls() == 1 })
	if cols, rows := f.size(); cols != 120 || rows != 40 {
		t.Fatalf("size = %dx%d, want 120x40", cols, rows)
	}
	if env := f.envSeen(); env["TERM"] != "xterm-256color" {
		t.Fatalf("env = %v", env)
	}
}

func TestServerStdinStdoutFlow(t *testing.T) {
	h := newHarness(t, 0)
	h.start(0)
	defer h.stop()

	nc, _ := h.attachTo("")
	if err := WriteFrame(nc, FrameStdin, []byte("hello")); err != nil {
		t.Fatalf("write STDIN: %v", err)
	}
	waitFor(t, "STDOUT echo", func() bool {
		_ = nc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		typ, payload, err := ReadFrame(nc)
		if err != nil {
			return false
		}
		return typ == FrameStdout && string(payload) == "hello"
	})
}

func TestServerResizeForwarded(t *testing.T) {
	h := newHarness(t, 0)
	h.start(0)
	defer h.stop()

	nc, _ := h.attachTo("")
	payload, err := json.Marshal(Resize{Cols: 132, Rows: 50})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := WriteFrame(nc, FrameResize, payload); err != nil {
		t.Fatalf("write RESIZE: %v", err)
	}
	f := h.fake("main")
	waitFor(t, "resize", func() bool { return hasPair(f.resizesSeen(), 132, 50) })
}

func TestServerDetachFrame(t *testing.T) {
	h := newHarness(t, 0)
	h.start(0)
	defer h.stop()

	nc, _ := h.attachTo("")
	if err := WriteFrame(nc, FrameDetach, nil); err != nil {
		t.Fatalf("write DETACH: %v", err)
	}
	if e := expectExit(t, nc); e.Reason != "detached" {
		t.Fatalf("EXIT = %+v, want detached", e)
	}
	f := h.fake("main")
	waitFor(t, "detach", func() bool { return hasString(f.detachReasons(), "detached") })
	if f.isKilled() {
		t.Fatal("session was killed by a client detach")
	}
	res := h.control(Hello{Command: "ls"})
	if len(res.Sessions) != 1 || res.Sessions[0].Name != "main" || res.Sessions[0].Attached {
		t.Fatalf("ls after detach = %+v", res)
	}
}

func TestServerClientHangupDetaches(t *testing.T) {
	h := newHarness(t, 0)
	h.start(0)
	defer h.stop()

	nc, _ := h.attachTo("")
	_ = nc.Close()

	f := h.fake("main")
	waitFor(t, "hangup", func() bool { return hasString(f.detachReasons(), "hangup") })
	if f.isKilled() {
		t.Fatal("session was killed when the client went away")
	}
	res := h.control(Hello{Command: "ls"})
	if len(res.Sessions) != 1 {
		t.Fatalf("ls = %+v, want the session still there", res)
	}
}

func TestServerSecondClientReplacesFirst(t *testing.T) {
	h := newHarness(t, 0)
	h.start(0)
	defer h.stop()

	first, _ := h.attachTo("")
	second, w := h.attachTo("main")
	if w.Created {
		t.Fatal("second attach created the session again")
	}
	if e := expectExit(t, first); e.Reason != "replaced" {
		t.Fatalf("first client EXIT = %+v, want replaced", e)
	}
	if err := WriteFrame(second, FrameStdin, []byte("still here")); err != nil {
		t.Fatalf("write STDIN: %v", err)
	}
	waitFor(t, "second client echo", func() bool {
		_ = second.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		typ, payload, err := ReadFrame(second)
		return err == nil && typ == FrameStdout && string(payload) == "still here"
	})
}

func TestServerNewDuplicateName(t *testing.T) {
	h := newHarness(t, 0)
	h.start(0)
	defer h.stop()

	res := h.control(Hello{Command: "new", Session: "db", Detached: true})
	if !res.OK {
		t.Fatalf("first new = %+v", res)
	}
	res = h.control(Hello{Command: "new", Session: "db", Detached: true})
	if res.OK || !strings.Contains(res.Error, "already exists") {
		t.Fatalf("duplicate new = %+v, want an error", res)
	}
}

func TestServerLs(t *testing.T) {
	h := newHarness(t, 0)
	h.start(0)
	defer h.stop()

	if res := h.control(Hello{Command: "new", Session: "alpha", Detached: true}); !res.OK {
		t.Fatalf("new alpha = %+v", res)
	}
	if res := h.control(Hello{Command: "new", Session: "beta", Detached: true}); !res.OK {
		t.Fatalf("new beta = %+v", res)
	}
	res := h.control(Hello{Command: "ls"})
	if !res.OK || len(res.Sessions) != 2 {
		t.Fatalf("ls = %+v", res)
	}
	if res.Sessions[0].Name != "alpha" || res.Sessions[1].Name != "beta" {
		t.Fatalf("ls names = %v", res.Sessions)
	}
	for _, s := range res.Sessions {
		if s.Attached {
			t.Fatalf("ls says %q is attached", s.Name)
		}
	}
}

func TestServerKillSession(t *testing.T) {
	h := newHarness(t, 0)
	h.start(0)
	defer h.stop()

	if res := h.control(Hello{Command: "new", Session: "victim-1", Detached: true}); !res.OK {
		t.Fatalf("new = %+v", res)
	}
	nc, _ := h.attachTo("victim-1")
	res := h.control(Hello{Command: "kill-session", Session: "victim-1"})
	if !res.OK {
		t.Fatalf("kill-session = %+v", res)
	}
	if e := expectExit(t, nc); e.Reason != "ended" {
		t.Fatalf("client EXIT = %+v, want ended", e)
	}
	res = h.control(Hello{Command: "ls"})
	if len(res.Sessions) != 0 {
		t.Fatalf("ls after kill = %+v", res)
	}
}

func TestServerKillServerRemovesSocket(t *testing.T) {
	h := newHarness(t, 0)
	h.start(0)
	defer h.stop()

	res := h.control(Hello{Command: "kill-server"})
	if !res.OK {
		t.Fatalf("kill-server = %+v", res)
	}
	if err := h.waitExit(); err != nil {
		t.Fatalf("Serve returned %v", err)
	}
	waitFor(t, "socket removed", func() bool {
		_, err := os.Stat(h.sock)
		return errors.Is(err, os.ErrNotExist)
	})
}

func TestServerVersionMismatch(t *testing.T) {
	h := newHarness(t, 0)
	h.start(0)
	defer h.stop()

	nc := h.dial()
	h.hello(nc, Hello{Protocol: ProtocolVersion + 1, Command: "ls"})
	typ, payload := readFrame(t, nc)
	if typ != FrameExit {
		t.Fatalf("frame %#x, want EXIT", typ)
	}
	e := decodeExitFrame(t, payload)
	if e.Reason != "error" {
		t.Fatalf("EXIT = %+v, want reason error", e)
	}
	for _, want := range []string{"protocol 1", "kill-server"} {
		if !strings.Contains(e.Message, want) {
			t.Fatalf("EXIT message %q missing %q", e.Message, want)
		}
	}
}

func TestServerSecondServerFailsOnLock(t *testing.T) {
	h := newHarness(t, 0)
	h.start(0)
	defer h.stop()

	err := Serve(context.Background(), h.options(0))
	if err == nil {
		t.Fatal("second Serve succeeded, want a lock error")
	}
	if errs.KindOf(err) != errs.Config {
		t.Fatalf("kind = %s, want config (%v)", errs.KindOf(err), err)
	}
	if !strings.Contains(err.Error(), "server already running") {
		t.Fatalf("err = %v", err)
	}
	// The first server is unaffected.
	if res := h.control(Hello{Command: "ls"}); !res.OK {
		t.Fatalf("ls = %+v", res)
	}
}

func TestServerStaleSocketRemoved(t *testing.T) {
	h := newHarness(t, 0)
	if err := os.MkdirAll(h.run, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(h.sock, []byte("stale"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	h.start(0)
	defer h.stop()

	nc, _ := h.attachTo("")
	if err := WriteFrame(nc, FrameStdin, []byte("up")); err != nil {
		t.Fatalf("write STDIN: %v", err)
	}
	waitFor(t, "echo", func() bool {
		_ = nc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		typ, payload, err := ReadFrame(nc)
		return err == nil && typ == FrameStdout && string(payload) == "up"
	})
}

func TestServerSocketPermissions(t *testing.T) {
	h := newHarness(t, 0)
	h.start(0)
	defer h.stop()

	di, err := os.Stat(h.run)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %o, want 700", di.Mode().Perm())
	}
	si, err := os.Stat(h.sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if si.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %o, want 600", si.Mode().Perm())
	}
}

func TestServerLongSocketPathRejected(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "jumper-long-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	long := filepath.Join(dir, strings.Repeat("x", 120)+".sock")
	if len(long) <= maxSocketPath {
		t.Fatalf("test path is only %d bytes", len(long))
	}
	err = Serve(context.Background(), Options{
		SocketPath: long,
		LockPath:   filepath.Join(dir, "server.lock"),
		NewSession: func(string) (SessionHandle, error) { return newFakeSession("main"), nil },
	})
	if err == nil {
		t.Fatal("Serve accepted a long socket path")
	}
	if errs.KindOf(err) != errs.Config {
		t.Fatalf("kind = %s, want config (%v)", errs.KindOf(err), err)
	}
}
