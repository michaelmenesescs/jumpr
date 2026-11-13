package server

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The session UX of task 082, from the server's side: switching a client
// without reconnecting it, and dormant sessions coming back from their files.

// switchSession is a fake session that takes a SessionControl, so a test can
// drive the registry the way the sessions dialog does.
type switchSession struct {
	*fakeSession

	mu   sync.Mutex
	ctrl SessionControl
}

func newSwitchSession(name string) *switchSession {
	return &switchSession{fakeSession: newFakeSession(name)}
}

// SetSessionControl records the registry handle the server handed out.
func (s *switchSession) SetSessionControl(ctrl SessionControl) {
	s.mu.Lock()
	s.ctrl = ctrl
	s.mu.Unlock()
}

// control is the handle the session was given, or nil.
func (s *switchSession) control() SessionControl {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctrl
}

// sessionHarness is one server with switchable sessions and, optionally, saved
// sessions to be dormant at start.
type sessionHarness struct {
	t    *testing.T
	sock string
	lock string

	cancel   context.CancelFunc
	done     chan error
	finished bool

	mu     sync.Mutex
	fakes  map[string]*switchSession
	order  []string
	saved  map[string]SavedSession
	files  map[string]string // saved session name → its file
	delete map[string]string // name → the file DeleteSaved removed
}

func newSessionHarness(t *testing.T) *sessionHarness {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "jumper-sess-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.MkdirAll(filepath.Join(dir, "run"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return &sessionHarness{
		t:      t,
		sock:   filepath.Join(dir, "run", "s.sock"),
		lock:   filepath.Join(dir, "run", "s.lock"),
		fakes:  make(map[string]*switchSession),
		saved:  make(map[string]SavedSession),
		files:  make(map[string]string),
		delete: make(map[string]string),
	}
}

// options is the server configuration of this harness.
func (h *sessionHarness) options() Options {
	return Options{
		SocketPath: h.sock,
		LockPath:   h.lock,
		Version:    "test",
		NewSession: func(name string) (SessionHandle, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			s := newSwitchSession(name)
			h.fakes[name] = s
			h.order = append(h.order, name)
			return s, nil
		},
		SavedSessions: func() []SavedSession {
			h.mu.Lock()
			defer h.mu.Unlock()
			out := make([]SavedSession, 0, len(h.saved))
			for _, sv := range h.saved {
				out = append(out, sv)
			}
			return out
		},
		DeleteSaved: func(name string) error {
			h.mu.Lock()
			path, ok := h.files[name]
			h.mu.Unlock()
			if !ok {
				return nil
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			h.mu.Lock()
			h.delete[name] = path
			h.mu.Unlock()
			return nil
		},
	}
}

// start serves until stop.
func (h *sessionHarness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)
	o := h.options()
	go func() { h.done <- Serve(ctx, o) }()
	waitFor(h.t, "socket", func() bool {
		fi, err := os.Stat(h.sock)
		return err == nil && fi.Mode()&os.ModeSocket != 0
	})
}

// stop ends the server.
func (h *sessionHarness) stop() {
	if h.cancel == nil || h.finished {
		return
	}
	h.cancel()
	select {
	case err := <-h.done:
		h.finished = true
		if err != nil {
			h.t.Errorf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		h.t.Errorf("server did not stop")
	}
}

// fake returns the session the factory built for name.
func (h *sessionHarness) fake(name string) *switchSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fakes[name]
}

// created lists the sessions in factory order.
func (h *sessionHarness) created() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.order...)
}

// attach connects to a session and returns the client plus its WELCOME.
func (h *sessionHarness) attach(name string) (net.Conn, Welcome) {
	h.t.Helper()
	nc, err := net.DialTimeout("unix", h.sock, 2*time.Second)
	if err != nil {
		h.t.Fatalf("dial: %v", err)
	}
	h.t.Cleanup(func() { _ = nc.Close() })
	if err := WriteJSON(nc, FrameHello, Hello{
		Protocol: ProtocolVersion, Command: "attach", Session: name,
		Cols: 120, Rows: 40, Env: map[string]string{"TERM": "xterm-256color"},
	}); err != nil {
		h.t.Fatalf("write HELLO: %v", err)
	}
	_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	typ, payload, err := ReadFrame(nc)
	if err != nil {
		h.t.Fatalf("read WELCOME: %v", err)
	}
	if typ != FrameWelcome {
		h.t.Fatalf("frame %#x, want WELCOME", typ)
	}
	w := decodeWelcome(h.t, payload)
	_ = nc.SetReadDeadline(time.Time{})
	return nc, w
}

// ls asks the server for the registry.
func (h *sessionHarness) ls() Result {
	h.t.Helper()
	nc, err := net.DialTimeout("unix", h.sock, 2*time.Second)
	if err != nil {
		h.t.Fatalf("dial: %v", err)
	}
	defer func() { _ = nc.Close() }()
	if err := WriteJSON(nc, FrameHello, Hello{Protocol: ProtocolVersion, Command: "ls"}); err != nil {
		h.t.Fatalf("write HELLO: %v", err)
	}
	_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	typ, payload, err := ReadFrame(nc)
	if err != nil {
		h.t.Fatalf("read RESULT: %v", err)
	}
	if typ != FrameResult {
		h.t.Fatalf("frame %#x, want RESULT", typ)
	}
	return decodeResult(h.t, payload)
}

// resize sends one RESIZE frame.
func (h *sessionHarness) resize(nc net.Conn, cols, rows int) {
	h.t.Helper()
	if err := WriteJSON(nc, FrameResize, Resize{Cols: cols, Rows: rows}); err != nil {
		h.t.Fatalf("write RESIZE: %v", err)
	}
}

// TestSwitchSessionKeepsConnection is requirement 3: switching sessions detaches
// from the current one and attaches the other on the same connection. The client
// keeps its socket, the old session keeps running, and the old session ending
// later does not end the client.
func TestSwitchSessionKeepsConnection(t *testing.T) {
	h := newSessionHarness(t)
	h.start()
	defer h.stop()

	nc, w := h.attach("")
	if w.Session != "main" {
		t.Fatalf("WELCOME session = %q, want main", w.Session)
	}
	main := h.fake("main")
	waitFor(t, "main attach", func() bool { return main.attachCalls() == 1 })
	ctrl := main.control()
	if ctrl == nil {
		t.Fatal("the server handed the session no control")
	}

	if err := ctrl.Switch("db-incident"); err != nil {
		t.Fatalf("switch: %v", err)
	}
	other := h.fake("db-incident")
	if other == nil {
		t.Fatal("the switch did not create db-incident")
	}
	waitFor(t, "db-incident attach", func() bool { return other.attachCalls() == 1 })
	if !hasString(main.detachReasons(), "switch") {
		t.Fatalf("main detach reasons = %v, want a switch", main.detachReasons())
	}
	if main.isKilled() {
		t.Fatal("the switch killed the session the client left")
	}
	if cols, rows := other.size(); cols != 120 || rows != 40 {
		t.Fatalf("the new session got %dx%d, want the client's 120x40", cols, rows)
	}

	// The client never reconnected: its frames reach the new session.
	h.resize(nc, 100, 30)
	waitFor(t, "resize on the new session", func() bool {
		return hasPair(other.resizesSeen(), 100, 30)
	})

	// The session the client left ending must not send it EXIT.
	main.Kill()
	time.Sleep(100 * time.Millisecond)
	h.resize(nc, 90, 28)
	waitFor(t, "resize after the old session ended", func() bool {
		return hasPair(other.resizesSeen(), 90, 28)
	})
	_ = nc.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if typ, payload, err := ReadFrame(nc); err == nil {
		t.Fatalf("the client received frame %#x (%s) after the old session ended", typ, payload)
	}
	_ = nc.SetReadDeadline(time.Time{})
}

// TestDormantSessionRestoredOnAttach is requirement 5: a saved session is
// listed at start but not restored until a client attaches to it.
func TestDormantSessionRestoredOnAttach(t *testing.T) {
	h := newSessionHarness(t)
	h.saved["db-incident"] = SavedSession{
		Name:     "db-incident",
		Windows:  3,
		Created:  time.Now().Add(-time.Hour),
		Activity: time.Now().Add(-time.Minute),
	}
	h.start()
	defer h.stop()

	res := h.ls()
	if !res.OK || len(res.Sessions) != 1 {
		t.Fatalf("ls = %+v, want the one dormant session", res)
	}
	if got := res.Sessions[0]; got.Name != "db-incident" || got.Windows != 3 || got.Attached {
		t.Fatalf("ls row = %+v, want db-incident with 3 windows and detached", got)
	}
	if created := h.created(); len(created) != 0 {
		t.Fatalf("sessions created at start: %v, want none (lazy restore)", created)
	}

	_, w := h.attach("db-incident")
	if w.Session != "db-incident" {
		t.Fatalf("WELCOME session = %q", w.Session)
	}
	if created := h.created(); len(created) != 1 || created[0] != "db-incident" {
		t.Fatalf("created = %v, want the dormant session revived once", created)
	}
	res = h.ls()
	if len(res.Sessions) != 1 || !res.Sessions[0].Attached {
		t.Fatalf("ls after attach = %+v, want one attached row", res)
	}
}

// TestKillDormantDeletesFile is requirement 5's other half: killing a dormant
// session deletes its file instead of restoring it first.
func TestKillDormantDeletesFile(t *testing.T) {
	h := newSessionHarness(t)
	dir := t.TempDir()
	h.files["gone"] = filepath.Join(dir, "gone.json")
	if err := os.WriteFile(h.files["gone"], []byte(`{"version":1,"name":"gone"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	h.saved["gone"] = SavedSession{Name: "gone", Windows: 1}

	opts := h.options()
	srv := &server{
		o:        opts,
		log:      slog.New(slog.DiscardHandler),
		sessions: make(map[string]*sessionEntry),
		conns:    make(map[*conn]struct{}),
		saved:    map[string]SavedSession{"gone": {Name: "gone", Windows: 1}},
		quit:     make(chan struct{}),
	}
	if err := srv.killSession("gone"); err != nil {
		t.Fatalf("kill dormant: %v", err)
	}
	if _, err := os.Stat(h.files["gone"]); !os.IsNotExist(err) {
		t.Fatalf("the saved file is still there (err = %v)", err)
	}
	if len(srv.sessions) != 0 {
		t.Fatalf("killing a dormant session created %d sessions", len(srv.sessions))
	}
	if len(srv.saved) != 0 {
		t.Fatalf("the dormant entry is still listed: %v", srv.saved)
	}
	if err := srv.killSession("gone"); err == nil {
		t.Fatal("killing it twice reported no error")
	}
}

// TestKillLiveSessionDeletesFile keeps the registry honest: a killed session
// must not come back as dormant after a restart.
func TestKillLiveSessionDeletesFile(t *testing.T) {
	h := newSessionHarness(t)
	dir := t.TempDir()
	h.files["live"] = filepath.Join(dir, "live.json")
	if err := os.WriteFile(h.files["live"], []byte(`{"version":1,"name":"live"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	opts := h.options()
	srv := &server{
		o:        opts,
		log:      slog.New(slog.DiscardHandler),
		sessions: make(map[string]*sessionEntry),
		conns:    make(map[*conn]struct{}),
		saved:    map[string]SavedSession{},
		quit:     make(chan struct{}),
	}
	e, _, err := srv.createEntry("live")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := srv.killSession("live"); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if !h.fake("live").isKilled() {
		t.Fatal("the session was not killed")
	}
	if _, ok := srv.sessions[e.name]; ok {
		t.Fatal("the killed session is still registered")
	}
	if _, err := os.Stat(h.files["live"]); !os.IsNotExist(err) {
		t.Fatalf("the saved file survived the kill (err = %v)", err)
	}
}

// TestSessionListMarksDormant is the dialog's view of the registry: the live
// sessions and the dormant names.
func TestSessionListMarksDormant(t *testing.T) {
	h := newSessionHarness(t)
	opts := h.options()
	srv := &server{
		o:        opts,
		log:      slog.New(slog.DiscardHandler),
		sessions: make(map[string]*sessionEntry),
		conns:    make(map[*conn]struct{}),
		saved:    map[string]SavedSession{"saved-one": {Name: "saved-one", Windows: 2}},
		quit:     make(chan struct{}),
	}
	if _, _, err := srv.createEntry("live-one"); err != nil {
		t.Fatalf("create: %v", err)
	}
	list := srv.sessionList()
	if len(list.Sessions) != 2 {
		t.Fatalf("list = %+v, want two rows", list)
	}
	if len(list.Dormant) != 1 || list.Dormant[0] != "saved-one" {
		t.Fatalf("dormant = %v, want [saved-one]", list.Dormant)
	}
}

// TestRenameSessionMovesRegistry is requirement 4 on the server side: the entry
// moves to the new name and the session is told.
func TestRenameSessionMovesRegistry(t *testing.T) {
	h := newSessionHarness(t)
	opts := h.options()
	srv := &server{
		o:        opts,
		log:      slog.New(slog.DiscardHandler),
		sessions: make(map[string]*sessionEntry),
		conns:    make(map[*conn]struct{}),
		saved:    map[string]SavedSession{},
		quit:     make(chan struct{}),
	}
	e, _, err := srv.createEntry("old")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := srv.renameSession(e, "new"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, ok := srv.sessions["old"]; ok {
		t.Fatal("the old name is still registered")
	}
	if got := srv.sessions["new"]; got != e {
		t.Fatalf("the new name holds %v", got)
	}
	if e.name != "new" {
		t.Fatalf("entry name = %q", e.name)
	}
	if err := srv.renameSession(e, "bad name"); err == nil {
		t.Fatal("an invalid name was accepted")
	}
}
