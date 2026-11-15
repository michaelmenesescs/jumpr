package termpane

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/errs"
	"jumpr/internal/mux"
	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
	"jumpr/internal/transport/transporttest"
)

// ---- test shell ----

// testShell is an in-memory PTY: output injected with push comes out of Read,
// writes are recorded, and end makes Read return io.EOF.
type testShell struct {
	mu      sync.Mutex
	cond    *sync.Cond
	out     []byte
	writes  []byte
	resizes [][2]int
	eof     bool
	closed  bool
}

func newTestShell() *testShell {
	s := &testShell{}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *testShell) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.out) == 0 && !s.eof && !s.closed {
		s.cond.Wait()
	}
	if len(s.out) == 0 {
		if s.closed {
			return 0, io.ErrClosedPipe
		}
		return 0, io.EOF
	}
	n := copy(p, s.out)
	s.out = s.out[n:]
	return n, nil
}

func (s *testShell) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	s.writes = append(s.writes, p...)
	return len(p), nil
}

func (s *testShell) Resize(cols, rows int) error {
	s.mu.Lock()
	s.resizes = append(s.resizes, [2]int{cols, rows})
	s.mu.Unlock()
	return nil
}

func (s *testShell) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cond.Broadcast()
	return nil
}

func (s *testShell) Wait() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.closed && !s.eof {
		s.cond.Wait()
	}
	return nil
}

func (s *testShell) push(b string) {
	s.mu.Lock()
	s.out = append(s.out, []byte(b)...)
	s.mu.Unlock()
	s.cond.Broadcast()
}

func (s *testShell) end() {
	s.mu.Lock()
	s.eof = true
	s.mu.Unlock()
	s.cond.Broadcast()
}

func (s *testShell) got() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.writes)
}

func (s *testShell) resizeList() [][2]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][2]int(nil), s.resizes...)
}

// testOpener is a ShellOpener that records every call and hands out testShells.
type testOpener struct {
	mu     sync.Mutex
	err    error
	calls  int
	reqs   []transport.PTYRequest
	shells []*testShell
}

func (o *testOpener) OpenShell(ctx context.Context, host resource.HostID, req transport.PTYRequest) (transport.Shell, error) {
	o.mu.Lock()
	o.calls++
	o.reqs = append(o.reqs, req)
	err := o.err
	if err == nil {
		s := newTestShell()
		o.shells = append(o.shells, s)
		o.mu.Unlock()
		return s, nil
	}
	o.mu.Unlock()
	return nil, err
}

func (o *testOpener) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

func (o *testOpener) last() *testShell {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.shells) == 0 {
		return nil
	}
	return o.shells[len(o.shells)-1]
}

// ---- pump ----

// pump runs the commands a pane returns in goroutines and collects the
// messages they produce. Waiting commands (dirty, exit) block, so they can
// never run on the test goroutine.
type pump struct {
	t  *testing.T
	p  *Pane
	ch chan tea.Msg
}

func newPump(t *testing.T, p *Pane) *pump {
	t.Helper()
	return &pump{t: t, p: p, ch: make(chan tea.Msg, 64)}
}

func (h *pump) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		switch m := cmd().(type) {
		case nil:
		case tea.BatchMsg:
			for _, c := range m {
				h.run(c)
			}
		case mux.PaneMsg:
			h.ch <- m.Msg
		default:
			h.ch <- m
		}
	}()
}

// update feeds msg to the pane and runs the commands it returns.
func (h *pump) update(msg tea.Msg) {
	_, cmd := h.p.Update(msg)
	h.run(cmd)
}

// next waits for the next message the pane's commands produce.
func (h *pump) next() tea.Msg {
	h.t.Helper()
	select {
	case m := <-h.ch:
		return m
	case <-time.After(3 * time.Second):
		h.t.Fatal("timed out waiting for a pane message")
		return nil
	}
}

// connect takes a fresh pane through Init to the connected state.
func connect(t *testing.T, p *Pane, h *pump) {
	t.Helper()
	h.run(p.Init())
	msg := h.next()
	cm, ok := msg.(connectedMsg)
	if !ok {
		t.Fatalf("first message is %T, want connectedMsg", msg)
	}
	if cm.shell == nil {
		t.Fatal("connectedMsg carries no shell")
	}
	h.update(cm)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---- tests ----

func TestConnectsAndRendersOutput(t *testing.T) {
	const host = resource.HostID("web-ewr-001")
	opener := transporttest.New() // echoes writes back, like a real PTY
	p := New(1, context.Background(), opener, host, Options{Now: time.Now})
	h := newPump(t, p)
	connect(t, p, h)

	opener.ShellOutput(host, []byte("hello\r\n"))
	msg := h.next()
	if _, ok := msg.(dirtyMsg); !ok {
		t.Fatalf("message after output is %T, want dirtyMsg", msg)
	}
	h.update(msg)

	view := p.View(40, 10)
	if !strings.Contains(view, "hello") {
		t.Fatalf("view does not contain hello:\n%s", view)
	}
	if got, want := p.Title(), "ssh web-ewr-001"; got != want {
		t.Fatalf("Title() = %q, want %q", got, want)
	}
	if !p.RawKeys() || !p.Busy() {
		t.Fatalf("Pane is not busy/raw while connected")
	}
	if got := p.State(); got.Type != "term" || got.Host != string(host) {
		t.Fatalf("State() = %+v, want a term pane for %s", got, host)
	}
}

func TestKeysAreSent(t *testing.T) {
	opener := &testOpener{}
	p := New(2, context.Background(), opener, "web-ewr-001", Options{})
	h := newPump(t, p)
	connect(t, p, h)
	sh := opener.last()

	p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ls")})
	p.Update(tea.KeyMsg{Type: tea.KeyEnter})

	waitUntil(t, "the shell to receive ls\\r", func() bool { return sh.got() == "ls\r" })
}

func TestResizePropagates(t *testing.T) {
	const host = resource.HostID("web-ewr-002")
	opener := transporttest.New()
	p := New(3, context.Background(), opener, host, Options{})
	h := newPump(t, p)
	connect(t, p, h)

	p.View(100, 30)
	waitUntil(t, "a resize of 100x30", func() bool {
		for _, r := range opener.Resizes(host) {
			if r[0] == 100 && r[1] == 30 {
				return true
			}
		}
		return false
	})
}

func TestRemoteExitShowsDisconnected(t *testing.T) {
	opener := &testOpener{}
	p := New(4, context.Background(), opener, "web-ewr-003", Options{})
	h := newPump(t, p)
	connect(t, p, h)
	sh := opener.last()
	sh.push("bye\r\n")
	exit := h.next() // the dirty message for the output
	h.update(exit)

	sh.end()
	msg := h.next()
	cm, ok := msg.(closedMsg)
	if !ok {
		t.Fatalf("message after EOF is %T, want closedMsg", msg)
	}
	h.update(cm)

	view := p.View(60, 8)
	if !strings.Contains(view, "[disconnected:") {
		t.Fatalf("view has no disconnected line:\n%s", view)
	}
	if !strings.Contains(view, "remote exit") {
		t.Fatalf("view has no remote-exit reason:\n%s", view)
	}
	if !strings.Contains(view, "r reconnect") || !strings.Contains(view, "x close") {
		t.Fatalf("view has no reconnect hint:\n%s", view)
	}
	if p.RawKeys() {
		t.Fatal("a closed pane still takes raw keys")
	}
}

func TestReconnect(t *testing.T) {
	opener := &testOpener{}
	p := New(5, context.Background(), opener, "web-ewr-004", Options{})
	h := newPump(t, p)
	connect(t, p, h)
	opener.last().end()
	h.update(h.next()) // closedMsg

	h.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	// The reconnect command was run by the pump; wait for the new connection.
	msg := h.next()
	cm, ok := msg.(connectedMsg)
	if !ok {
		t.Fatalf("message after r is %T, want connectedMsg", msg)
	}
	h.update(cm)
	if got := opener.count(); got != 2 {
		t.Fatalf("OpenShell called %d times, want 2", got)
	}
	opener.last().push("again\r\n")
	h.update(h.next()) // dirtyMsg
	view := p.View(40, 6)
	if strings.Contains(view, "[disconnected:") {
		t.Fatalf("reconnected pane still shows the disconnected line:\n%s", view)
	}
	if !strings.Contains(view, "again") {
		t.Fatalf("reconnected pane does not show new output:\n%s", view)
	}
}

func TestCloseStopsGoroutines(t *testing.T) {
	opener := &testOpener{}
	p := New(6, context.Background(), opener, "web-ewr-005", Options{})
	h := newPump(t, p)
	connect(t, p, h)
	if got := p.running.Load(); got != 3 {
		t.Fatalf("%d goroutines running after connect, want 3", got)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitUntil(t, "the connection goroutines to stop", func() bool { return p.running.Load() == 0 })
	if p.Busy() {
		t.Fatal("a closed pane is still busy")
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestViewExactSize(t *testing.T) {
	const host = resource.HostID("web-ewr-006")
	opener := transporttest.New()
	p := New(7, context.Background(), opener, host, Options{})
	p.SetFocus(true)
	h := newPump(t, p)
	connect(t, p, h)

	opener.ShellOutput(host, []byte("hello\r\n"))
	h.update(h.next()) // dirtyMsg

	const w, hgt = 30, 6
	view := p.View(w, hgt)
	lines := strings.Split(view, "\n")
	if len(lines) != hgt {
		t.Fatalf("view has %d lines, want %d", len(lines), hgt)
	}
	for i, line := range lines {
		if got := render.Width(line); got != w {
			t.Fatalf("line %d is %d cells wide, want %d: %q", i, got, w, line)
		}
	}
	if !strings.Contains(view, "hello") {
		t.Fatalf("view lost the output:\n%s", view)
	}
	if !strings.Contains(view, "\x1b[7m") {
		t.Fatalf("focused view draws no cursor:\n%q", view)
	}
}

func TestOpenFailureShowsKind(t *testing.T) {
	opener := &testOpener{err: errs.New(errs.Auth, "ssh.dial", "denied")}
	p := New(8, context.Background(), opener, "web-ewr-007", Options{})
	h := newPump(t, p)
	h.run(p.Init())
	msg := h.next()
	fm, ok := msg.(failedMsg)
	if !ok {
		t.Fatalf("message after a failed open is %T, want failedMsg", msg)
	}
	if !errs.Is(fm.err, errs.Auth) {
		t.Fatalf("failure error is %v, want an auth error", fm.err)
	}
	h.update(fm)

	view := p.View(60, 8)
	if !strings.Contains(view, "[disconnected: auth") {
		t.Fatalf("view does not show the auth kind:\n%s", view)
	}
	if p.RawKeys() {
		t.Fatal("a failed pane takes raw keys")
	}
}

func TestRestoredStartsClosed(t *testing.T) {
	opener := &testOpener{}
	p := New(9, context.Background(), opener, "web-ewr-008", Options{})
	p.MarkRestored()
	if cmd := p.Init(); cmd != nil {
		t.Fatalf("Init of a restored pane returned a command, want nil")
	}
	time.Sleep(10 * time.Millisecond)
	if got := opener.count(); got != 0 {
		t.Fatalf("a restored pane reconnected (%d OpenShell calls)", got)
	}
	view := p.View(40, 6)
	if !strings.Contains(view, "restored") {
		t.Fatalf("restored pane shows no reason:\n%s", view)
	}
}

func TestCopyLinesIncludesScrollback(t *testing.T) {
	const host = resource.HostID("web-ewr-009")
	opener := transporttest.New()
	p := New(10, context.Background(), opener, host, Options{Scrollback: 100})
	p.View(40, 3)
	h := newPump(t, p)
	connect(t, p, h)

	opener.ShellOutput(host, []byte("one\r\ntwo\r\nthree\r\nfour\r\nfive\r\n"))
	h.update(h.next())

	var lines []string
	waitUntil(t, "the output to arrive", func() bool {
		lines = p.CopyLines()
		return len(lines) > 0 && strings.Contains(strings.Join(lines, "\n"), "five")
	})
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "one") {
		t.Fatalf("copy lines have no scrollback:\n%s", joined)
	}
}
