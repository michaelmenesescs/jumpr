package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"jumpr/internal/errs"
)

const (
	// maxSocketPath is the longest usable unix socket path: macOS `sun_path`
	// is 104 bytes including the terminator (BUILD-PLAN §6).
	maxSocketPath = 103
	// helloTimeout bounds the wait for the first frame: HELLO or nothing.
	helloTimeout = 5 * time.Second
	// writeTimeout bounds one frame write. A slow or dead client must never
	// block a session, so a write that takes longer detaches it.
	writeTimeout = 5 * time.Second
)

// SessionHandle is what the server needs from a session. It is deliberately a
// small interface so this package does not import app; app.Session gets a thin
// adapter.
type SessionHandle interface {
	Attach(in io.Reader, out io.Writer, cols, rows int, env map[string]string, onDetach func(reason string)) error
	Detach(reason string)
	Resize(cols, rows int)
	Done() <-chan struct{}
	Kill()
	Info() SessionInfo
}

// SessionControl is what the server gives a session so the sessions dialog can
// act on the registry: list it, switch this client to another session, create
// one, kill one, rename this one. It is bound to the entry that asked, so
// "this session" always means the caller's (task 082).
type SessionControl interface {
	List() SessionList
	Switch(name string) error
	New(name string) error
	Kill(name string) error
	Rename(name string) error
}

// SessionList is the registry as the UI sees it: every session, live or
// dormant, and the names of the dormant ones so the dialog can mark them
// (saved).
type SessionList struct {
	Sessions []SessionInfo
	Dormant  []string
}

// Switcher is implemented by a session handle that wants a Control. A handle
// that does not implement it simply has no sessions dialog.
type Switcher interface {
	SetSessionControl(ctrl SessionControl)
}

// SavedSession is a session file found on disk when the server started. It is
// dormant: listed by ls and revived on first attach (BUILD-PLAN §6). Its
// layout is not read here; the caller that owns the files supplies this.
type SavedSession struct {
	Name     string
	Windows  int
	Created  time.Time
	Activity time.Time
}

// Options is everything Serve needs from the caller. SocketPath and LockPath are
// usually under ~/.cache/jumpr; tests pass a short temp dir.
type Options struct {
	SocketPath string
	LockPath   string
	NewSession func(name string) (SessionHandle, error)
	Version    string
	Log        *slog.Logger
	IdleExit   time.Duration
	// SavedSessions lists the session files on disk. Serve calls it once, at
	// start, and keeps the result: a dormant session is listed but not
	// restored (task 082 requirement 5).
	SavedSessions func() []SavedSession
	// DeleteSaved removes a dormant session's file, which is what kill-session
	// on one of them does.
	DeleteSaved func(name string) error
}

// sessionEntry is one registered session plus the client attached to it.
type sessionEntry struct {
	name     string
	handle   SessionHandle
	created  time.Time
	activity time.Time
	client   *conn
}

// server is the accept loop plus its registry. All fields under mu are the
// registry state; the listener and the quit channel are set once at start.
type server struct {
	o   Options
	log *slog.Logger
	ln  net.Listener

	mu       sync.Mutex
	sessions map[string]*sessionEntry
	conns    map[*conn]struct{}
	// saved are the dormant sessions: files on disk, listed but not restored.
	saved map[string]SavedSession

	quitOnce sync.Once
	quit     chan struct{}
}

// Serve runs the session server until ctx is canceled, kill-server arrives or
// IdleExit fires with no sessions. It owns the socket: it creates the directory,
// takes the lock, removes a stale socket, listens and removes the socket again
// on the way out.
func Serve(ctx context.Context, o Options) error {
	if o.SocketPath == "" {
		return errs.New(errs.Config, "server.serve", "empty socket path")
	}
	if len(o.SocketPath) > maxSocketPath {
		return errs.New(errs.Config, "server.serve",
			fmt.Sprintf("socket path is %d bytes; the limit is %d", len(o.SocketPath), maxSocketPath))
	}
	if o.LockPath == "" {
		return errs.New(errs.Config, "server.serve", "empty lock path")
	}
	if o.NewSession == nil {
		return errs.New(errs.Config, "server.serve", "missing NewSession")
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	if err := os.MkdirAll(filepath.Dir(o.SocketPath), 0o700); err != nil {
		return errs.Wrap(errs.Config, "server.mkdir", err)
	}
	// The lock goes first: two racing servers must not delete each other's
	// socket file (task 079 pitfall).
	lock, err := acquireLock(o.LockPath)
	if err != nil {
		return err
	}
	defer lock.release()

	// Holding the lock proves that any socket file at this path is stale.
	if rerr := os.Remove(o.SocketPath); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return errs.Wrap(errs.Config, "server.remove_stale_socket", rerr)
	}
	ln, err := net.Listen("unix", o.SocketPath)
	if err != nil {
		return errs.Wrap(errs.Config, "server.listen", err)
	}
	if cerr := os.Chmod(o.SocketPath, 0o600); cerr != nil {
		_ = ln.Close()
		return errs.Wrap(errs.Config, "server.chmod", cerr)
	}

	s := &server{
		o:        o,
		log:      log,
		ln:       ln,
		sessions: make(map[string]*sessionEntry),
		conns:    make(map[*conn]struct{}),
		saved:    make(map[string]SavedSession),
		quit:     make(chan struct{}),
	}
	// The saved sessions are listed once: a dormant one is revived on first
	// attach, never eagerly at start (task 082 requirement 5, pitfall 2).
	if o.SavedSessions != nil {
		for _, sv := range o.SavedSessions() {
			if sv.Name != "" {
				s.saved[sv.Name] = sv
			}
		}
	}
	go func() {
		<-ctx.Done()
		s.shutdown()
	}()
	if o.IdleExit > 0 {
		go s.watchIdle(ctx)
	}
	log.Info("server listening", "socket", o.SocketPath, "version", o.Version)

	err = s.accept()
	s.shutdown()
	return err
}

// accept takes connections until the listener is closed. A peer that is not the
// same uid is dropped before it can say anything.
func (s *server) accept() error {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return nil
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return errs.Wrap(errs.Internal, "server.accept", err)
		}
		uc, ok := nc.(*net.UnixConn)
		if !ok || !peerAllowed(uc) {
			_ = nc.Close()
			continue
		}
		c := newConn(s, nc)
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		go func() {
			defer s.forget(c)
			c.serve()
		}()
	}
}

// shutdown ends every client and session, removes the socket and makes the
// accept loop return. It runs exactly once.
func (s *server) shutdown() {
	s.quitOnce.Do(func() {
		close(s.quit)
		_ = s.ln.Close()

		s.mu.Lock()
		entries := make([]*sessionEntry, 0, len(s.sessions))
		for _, e := range s.sessions {
			entries = append(entries, e)
		}
		conns := make([]*conn, 0, len(s.conns))
		for c := range s.conns {
			conns = append(conns, c)
		}
		s.mu.Unlock()

		for _, c := range conns {
			// Concurrently: a wedged client must not delay the shutdown.
			go c.end("shutdown", "", true)
		}
		for _, e := range entries {
			e.handle.Detach("shutdown")
			e.handle.Kill()
		}
		if err := os.Remove(s.o.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("could not remove socket", "socket", s.o.SocketPath, "err", err)
		}
		s.log.Info("server stopped")
	})
}

// watchIdle exits the server when it has had no sessions for IdleExit.
func (s *server) watchIdle(ctx context.Context) {
	interval := s.o.IdleExit / 4
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	if interval > time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	emptySince := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.quit:
			return
		case <-t.C:
			if s.sessionCount() > 0 {
				emptySince = time.Now()
				continue
			}
			if time.Since(emptySince) >= s.o.IdleExit {
				s.log.Info("no sessions left, exiting")
				s.shutdown()
				return
			}
		}
	}
}

// forget removes a connection from the set shutdown walks.
func (s *server) forget(c *conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

// sessionCount is the number of registered sessions.
func (s *server) sessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// lookupEntry returns the named session, or nil. A dormant session is revived
// on first look: attaching to it restores its saved layout, and kill-session on
// it deletes the file, because a killed session never comes back (task 082
// requirement 5).
func (s *server) lookupEntry(name string) *sessionEntry {
	s.mu.Lock()
	e := s.sessions[name]
	s.mu.Unlock()
	if e != nil || name == "" {
		return e
	}
	return s.reviveSaved(name)
}

// reviveSaved turns a dormant session into a live one, or returns nil when the
// name has no saved file. The layout comes back by itself: the caller's
// NewSession restores <cache>/sessions/<name>.json (BUILD-PLAN §6).
func (s *server) reviveSaved(name string) *sessionEntry {
	s.mu.Lock()
	_, dormant := s.saved[name]
	s.mu.Unlock()
	if !dormant {
		return nil
	}
	e, _, err := s.createEntry(name)
	if err != nil {
		s.log.Warn("saved session not revived", "session", name, "err", err)
		return nil
	}
	return e
}

// mostRecent returns the session with the latest activity, or nil.
func (s *server) mostRecent() *sessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *sessionEntry
	for _, e := range s.sessions {
		if best == nil || e.activity.After(best.activity) ||
			(e.activity.Equal(best.activity) && e.created.After(best.created)) {
			best = e
		}
	}
	return best
}

// autoName is base while it is free, then base-2, base-3, …
func (s *server) autoName(base string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[base]; !ok {
		return base
	}
	for i := 2; ; i++ {
		name := fmt.Sprintf("%s-%d", base, i)
		if _, ok := s.sessions[name]; !ok {
			return name
		}
	}
}

// createEntry asks the caller's factory for a session and registers it. A
// session that ends is removed from the registry by a watcher.
func (s *server) createEntry(name string) (*sessionEntry, bool, error) {
	s.mu.Lock()
	if _, exists := s.sessions[name]; exists {
		s.mu.Unlock()
		return nil, false, errs.New(errs.Config, "server.new_session",
			fmt.Sprintf("session %q already exists", name))
	}
	s.mu.Unlock()

	inner, err := s.o.NewSession(name)
	if err != nil {
		var ee *errs.Error
		if errors.As(err, &ee) {
			return nil, false, ee
		}
		return nil, false, errs.Wrap(errs.Internal, "server.new_session", err)
	}
	if inner == nil {
		return nil, false, errs.New(errs.Internal, "server.new_session", "no session returned")
	}
	// The wrapper is what makes a switch possible: it remembers the client's
	// streams and stops an old session's end from ending a client that has
	// moved on (task 082 requirement 3).
	h := &streamHandle{SessionHandle: inner}
	now := time.Now()
	s.mu.Lock()
	if _, exists := s.sessions[name]; exists {
		s.mu.Unlock()
		h.Kill()
		return nil, false, errs.New(errs.Config, "server.new_session",
			fmt.Sprintf("session %q already exists", name))
	}
	e := &sessionEntry{name: name, handle: h, created: now, activity: now}
	s.sessions[name] = e
	// A revived dormant session is live now, so ls must not list it twice.
	delete(s.saved, name)
	s.mu.Unlock()

	// The registry watches the session itself, not the wrapper: the wrapper's
	// Done is per connection, and a client that switched away stops watching.
	go func() {
		<-inner.Done()
		s.removeSession(e)
	}()
	if sw, ok := inner.(Switcher); ok {
		sw.SetSessionControl(entryControl{s: s, e: e})
	}
	return e, true, nil
}

// removeSession drops an entry, provided the registry still holds that one.
func (s *server) removeSession(e *sessionEntry) {
	if e == nil {
		return
	}
	s.mu.Lock()
	if cur, ok := s.sessions[e.name]; ok && cur == e {
		delete(s.sessions, e.name)
	}
	s.mu.Unlock()
}

// setClient attaches c as the session's client and returns the previous one.
func (s *server) setClient(e *sessionEntry, c *conn) *conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := e.client
	e.client = c
	e.activity = time.Now()
	return prev
}

// clearClient detaches c from the session if it is still the current client.
func (s *server) clearClient(e *sessionEntry, c *conn) {
	if e == nil {
		return
	}
	s.mu.Lock()
	if e.client == c {
		e.client = nil
	}
	e.activity = time.Now()
	s.mu.Unlock()
}

// touch records activity for the session, if there is one.
func (s *server) touch(e *sessionEntry) {
	if e == nil {
		return
	}
	s.mu.Lock()
	e.activity = time.Now()
	s.mu.Unlock()
}

// sessionInfos is the RESULT of ls, in name order: the live sessions and the
// dormant ones (task 082 requirement 5).
func (s *server) sessionInfos() []SessionInfo {
	s.mu.Lock()
	entries := make([]*sessionEntry, 0, len(s.sessions))
	for _, e := range s.sessions {
		entries = append(entries, e)
	}
	attached := make(map[*sessionEntry]bool, len(entries))
	for _, e := range entries {
		attached[e] = e.client != nil
	}
	saved := make([]SavedSession, 0, len(s.saved))
	for _, sv := range s.saved {
		if _, live := s.sessions[sv.Name]; live {
			continue
		}
		saved = append(saved, sv)
	}
	s.mu.Unlock()

	infos := make([]SessionInfo, 0, len(entries)+len(saved))
	for _, e := range entries {
		infos = append(infos, sessionInfo(e, attached[e]))
	}
	for _, sv := range saved {
		infos = append(infos, SessionInfo{
			Name:     sv.Name,
			Windows:  sv.Windows,
			Attached: false,
			Created:  sv.Created,
			Activity: sv.Activity,
		})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos
}

// sessionList is the registry as the sessions dialog needs it: the same rows as
// ls, plus the names of the dormant ones so the dialog marks them (saved).
func (s *server) sessionList() SessionList {
	s.mu.Lock()
	dormant := make([]string, 0, len(s.saved))
	for name := range s.saved {
		if _, live := s.sessions[name]; !live {
			dormant = append(dormant, name)
		}
	}
	s.mu.Unlock()
	sort.Strings(dormant)
	return SessionList{Sessions: s.sessionInfos(), Dormant: dormant}
}

// sessionInfo reads one session's own report, filling what it left empty from
// the registry and forcing Attached to what the registry knows.
func sessionInfo(e *sessionEntry, attached bool) SessionInfo {
	info := e.handle.Info()
	if info.Name == "" {
		info.Name = e.name
	}
	if info.Created.IsZero() {
		info.Created = e.created
	}
	if info.Activity.IsZero() {
		info.Activity = e.activity
	}
	info.Attached = attached
	return info
}

// messageOf is the user-facing text of an error: an *errs.Error's Msg when it
// has one, since EXIT and RESULT carry the message alone.
func messageOf(err error) string {
	if err == nil {
		return ""
	}
	var ee *errs.Error
	if errors.As(err, &ee) && ee.Msg != "" {
		return ee.Msg
	}
	return err.Error()
}

// --- the sessions dialog and switching a client (task 082) ---

// entryControl is SessionControl bound to one entry: every "this session" the
// dialog says means that one, whichever client is on it now.
type entryControl struct {
	s *server
	e *sessionEntry
}

var _ SessionControl = entryControl{}

// List reports the whole registry, live and dormant.
func (c entryControl) List() SessionList { return c.s.sessionList() }

// Switch hands this client to another session.
func (c entryControl) Switch(name string) error { return c.s.switchClient(c.e, name) }

// New creates a session. It is created detached: the caller's client stays
// where it is until it switches to the new session.
func (c entryControl) New(name string) error {
	name = strings.TrimSpace(name)
	if !ValidSessionName(name) {
		return errs.New(errs.Config, "server.new_session",
			fmt.Sprintf("invalid session name %q", name))
	}
	_, _, err := c.s.createEntry(name)
	return err
}

// Kill ends a session, or deletes a dormant one's file.
func (c entryControl) Kill(name string) error { return c.s.killSession(name) }

// Rename renames this session: the registry entry and the autosave file.
func (c entryControl) Rename(name string) error { return c.s.renameSession(c.e, name) }

// switchClient hands the client attached to from to the session named name,
// creating that session if it does not exist yet. The old session detaches and
// keeps running; the new one attaches to the same terminal and repaints
// (task 082 requirement 3).
func (s *server) switchClient(from *sessionEntry, name string) error {
	const op = "server.switch"
	if from == nil {
		return errs.New(errs.Internal, op, "no session")
	}
	name = strings.TrimSpace(name)
	if !ValidSessionName(name) {
		return errs.New(errs.Config, op, fmt.Sprintf("invalid session name %q", name))
	}
	if name == from.name {
		return nil
	}
	s.mu.Lock()
	c := from.client
	s.mu.Unlock()
	if c == nil {
		return errs.New(errs.Config, op, "no client attached to session "+from.name)
	}
	to := s.lookupEntry(name)
	if to == nil {
		var err error
		to, _, err = s.createEntry(name)
		if err != nil {
			return err
		}
	}
	if to == from {
		return nil
	}
	return c.handover(from, to)
}

// killSession ends a live session or deletes a dormant one's file. A killed
// session must not come back as dormant after a restart, so killing a live one
// deletes its file too (task 082 requirement 5).
func (s *server) killSession(name string) error {
	const op = "server.kill_session"
	name = strings.TrimSpace(name)
	if name == "" {
		return errs.New(errs.Config, op, "no session name")
	}
	s.mu.Lock()
	e := s.sessions[name]
	_, dormant := s.saved[name]
	s.mu.Unlock()
	switch {
	case e != nil:
		s.removeSession(e)
		e.handle.Kill()
		s.deleteSaved(name)
		return nil
	case dormant:
		s.deleteSaved(name)
		return nil
	}
	return errs.New(errs.Config, op, "no such session "+name)
}

// deleteSaved removes a session's file, if the caller has a store for it.
func (s *server) deleteSaved(name string) {
	if s.o.DeleteSaved == nil {
		return
	}
	if err := s.o.DeleteSaved(name); err != nil {
		s.log.Warn("saved session not deleted", "session", name, "err", err)
	}
	s.mu.Lock()
	delete(s.saved, name)
	s.mu.Unlock()
}

// renameSession renames a live session: the handle renames the session and its
// autosave file, then the registry entry moves to the new key.
func (s *server) renameSession(e *sessionEntry, name string) error {
	const op = "server.rename_session"
	if e == nil {
		return errs.New(errs.Internal, op, "no session")
	}
	name = strings.TrimSpace(name)
	if !ValidSessionName(name) {
		return errs.New(errs.Config, op, fmt.Sprintf("invalid session name %q", name))
	}
	if name == e.name {
		return nil
	}
	s.mu.Lock()
	_, exists := s.sessions[name]
	s.mu.Unlock()
	if exists {
		return errs.New(errs.Config, op, fmt.Sprintf("session %q already exists", name))
	}
	if r, ok := e.handle.(interface{ Rename(string) error }); ok {
		if err := r.Rename(name); err != nil {
			return err
		}
	}
	s.mu.Lock()
	delete(s.sessions, e.name)
	e.name = name
	s.sessions[name] = e
	s.mu.Unlock()
	return nil
}

// streamHandle wraps a session handle. It remembers the streams of the client
// that attached last, so a switch can hand another session the same terminal,
// and it decides which detaches reach the connection: a switch is not the end
// of the client (task 082 requirement 3).
type streamHandle struct {
	SessionHandle

	srv  *server
	name string

	mu        sync.Mutex
	in        io.Reader
	out       io.Writer
	cols      int
	rows      int
	env       map[string]string
	switching bool
	gen       int
}

var _ SessionHandle = (*streamHandle)(nil)

// Attach records the client's streams and passes them, and a detach callback
// that skips a switch, to the session.
func (w *streamHandle) Attach(in io.Reader, out io.Writer, cols, rows int, env map[string]string, onDetach func(string)) error {
	w.mu.Lock()
	w.in, w.out, w.cols, w.rows, w.env = in, out, cols, rows, env
	w.switching = false
	w.mu.Unlock()
	return w.SessionHandle.Attach(in, out, cols, rows, env, func(reason string) {
		if w.suppressed() {
			return
		}
		if onDetach != nil {
			onDetach(reason)
		}
	})
}

// suppressed reports, once, whether the detach that is happening is a switch:
// the client keeps its connection and must not be told to go away.
func (w *streamHandle) suppressed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	sw := w.switching
	w.switching = false
	return sw
}

// streams are the streams of the client attached last.
func (w *streamHandle) streams() (io.Reader, io.Writer, int, int, map[string]string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.in == nil || w.out == nil {
		return nil, nil, 0, 0, nil, false
	}
	return w.in, w.out, w.cols, w.rows, w.env, true
}

// beginSwitch marks the next detach as a switch and stops reporting this
// session's end to the connection that is leaving it.
func (w *streamHandle) beginSwitch() {
	w.mu.Lock()
	w.switching = true
	w.gen++
	w.mu.Unlock()
}

// Done reports the session's end to the connection watching it. A client that
// was switched away from this session stops watching it: its watch belongs to
// the session it is on now, and killing this one must not end the client.
func (w *streamHandle) Done() <-chan struct{} {
	w.mu.Lock()
	gen := w.gen
	w.mu.Unlock()
	out := make(chan struct{})
	inner := w.SessionHandle.Done()
	go func() {
		<-inner
		w.mu.Lock()
		same := w.gen == gen
		w.mu.Unlock()
		if same {
			close(out)
		}
	}()
	return out
}

// Kill stops the session. Its saved file stays: a session killed by the server
// shutting down has to come back (the sessions dialog's kill deletes the file
// itself).
func (w *streamHandle) Kill() { w.SessionHandle.Kill() }

// Rename renames the session, when it can be renamed.
func (w *streamHandle) Rename(name string) error {
	if r, ok := w.SessionHandle.(interface{ Rename(string) error }); ok {
		return r.Rename(name)
	}
	return nil
}

// SetSessionControl passes the registry handle to the session, so its dialog
// can act on it.
func (w *streamHandle) SetSessionControl(ctrl SessionControl) {
	if sw, ok := w.SessionHandle.(Switcher); ok {
		sw.SetSessionControl(ctrl)
	}
}

// handover moves this connection from one session to another: the old session
// detaches without ending the client, the new one attaches to the same streams
// and repaints, and the connection keeps running. It is the same detach/attach
// pair a reconnect uses (task 082 pitfall 1).
func (c *conn) handover(from, to *sessionEntry) error {
	fh, ok := from.handle.(*streamHandle)
	if !ok {
		return errs.New(errs.Internal, "server.switch", "session cannot be switched")
	}
	in, out, cols, rows, env, ok := fh.streams()
	if !ok {
		return errs.New(errs.Config, "server.switch", "the session has no attached terminal")
	}

	// The client is not going away: the old session's detach is a switch, and
	// the connection must not be told to exit.
	fh.beginSwitch()
	from.handle.Detach("switch")
	c.s.clearClient(from, c)

	c.mu.Lock()
	c.entry = to
	c.mu.Unlock()
	if prev := c.s.setClient(to, c); prev != nil && prev != c {
		prev.end("replaced", "", true)
	}
	go c.watchSession(to)
	go func() {
		err := to.handle.Attach(in, out, cols, rows, env, func(reason string) {
			c.end(reason, "", true)
		})
		if err != nil {
			c.end("error", messageOf(err), true)
		}
	}()
	return nil
}
