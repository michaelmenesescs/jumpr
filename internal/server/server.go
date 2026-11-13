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
	"sync"
	"time"

	"jumper/internal/errs"
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

// Options is everything Serve needs from the caller. SocketPath and LockPath are
// usually under ~/.cache/jumper; tests pass a short temp dir.
type Options struct {
	SocketPath string
	LockPath   string
	NewSession func(name string) (SessionHandle, error)
	Version    string
	Log        *slog.Logger
	IdleExit   time.Duration
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
		quit:     make(chan struct{}),
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

// lookupEntry returns the named session, or nil.
func (s *server) lookupEntry(name string) *sessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[name]
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

	h, err := s.o.NewSession(name)
	if err != nil {
		var ee *errs.Error
		if errors.As(err, &ee) {
			return nil, false, ee
		}
		return nil, false, errs.Wrap(errs.Internal, "server.new_session", err)
	}
	if h == nil {
		return nil, false, errs.New(errs.Internal, "server.new_session", "no session returned")
	}
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
	s.mu.Unlock()

	go func() {
		<-h.Done()
		s.removeSession(e)
	}()
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

// sessionInfos is the RESULT of ls, in name order.
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
	s.mu.Unlock()

	infos := make([]SessionInfo, 0, len(entries))
	for _, e := range entries {
		infos = append(infos, sessionInfo(e, attached[e]))
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos
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
