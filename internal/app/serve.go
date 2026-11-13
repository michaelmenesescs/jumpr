package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"jumper/internal/config"
	"jumper/internal/errs"
	"jumper/internal/interact"
	"jumper/internal/mux"
	"jumper/internal/server"
	"jumper/internal/tui"
)

// ServerOptions is what a `jumper server` run needs: the same Options a
// standalone run takes, plus the socket it listens on. The socket is resolved by
// the caller, so this file never reads the environment or the command line.
type ServerOptions struct {
	Options
	SocketPath string
	// Foreground says the server was started by hand in a terminal rather than
	// by a client. It changes nothing but the log line: both run in this
	// process, and only the log file is ever written to.
	Foreground bool
}

// RunServer runs the session server until ctx is canceled (SIGTERM, SIGINT),
// kill-server arrives, or server.Serve fails (BUILD-PLAN §6). It builds one
// Core for the process, gives every session that core, and logs to
// <cache>/server.log, which is also where a client looks when the server does
// not come up.
func RunServer(ctx context.Context, o ServerOptions) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if o.Home == "" {
		return errs.New(errs.Config, "app.RunServer", "no home directory")
	}
	paths := config.ResolvePaths(o.Home, o.ConfigDir)
	logger, logFile := newServerLogger(paths.CacheDir)
	if logFile != nil {
		defer logFile.Close() //nolint:errcheck // the log is advisory
	}
	defer recoverCrash(paths.CacheDir, logger, &err)

	cfg, err := config.Load(filepath.Join(paths.ConfigDir, "config.yaml"), o.Home)
	if err != nil {
		return err
	}
	// Prompts that arrive while no client is attached are refused by the
	// prompter; the tap counts them so the next attach can say what was
	// skipped (task 082 requirement 6).
	skips := newPromptSkips()
	tap := &promptTap{skips: skips}
	opts := o.Options
	supplied := opts.Prompter
	opts.Prompter = tap
	core, err := newCore(ctx, cfg, paths, o.Env, opts, logger)
	if err != nil {
		return err
	}
	// The core took the tap as its prompter (and handed it to the SSH stack).
	// The prompter it would otherwise have built needs the bridge, which exists
	// now; a prompter the caller supplied is relayed to instead.
	if supplied != nil {
		tap.setInner(supplied)
	} else {
		tap.setInner(tui.NewPrompter(core.bridge.Send, cfg.Interact.Timeout.Duration()))
	}
	defer func() {
		if cerr := core.Close(); cerr != nil {
			logger.Warn("core not closed cleanly", "err", cerr)
		}
	}()

	socket := o.SocketPath
	if socket == "" {
		socket = SocketPath(Invocation{ConfigDir: o.ConfigDir}, "", o.Home)
	}
	if err := CheckSocketPath(socket); err != nil {
		return err
	}

	// What this process is worth recording: a client whose --config-dir differs
	// gets a warning line before it attaches (task 081 requirement 6).
	statePath := StatePath(paths.CacheDir)
	st := ServerState{ConfigDir: paths.ConfigDir, Version: o.Version, PID: os.Getpid()}
	if werr := WriteServerState(statePath, st); werr != nil {
		logger.Warn("server state not written", "path", statePath, "err", werr)
	} else {
		defer func() { _ = os.Remove(statePath) }()
	}

	sessionsDir := filepath.Join(paths.CacheDir, "sessions")
	logger.Info("server starting", "socket", socket, "version", o.Version, "foreground", o.Foreground)
	return server.Serve(ctx, server.Options{
		SocketPath: socket,
		LockPath:   filepath.Join(paths.CacheDir, "server.lock"),
		Version:    o.Version,
		Log:        logger,
		NewSession: func(name string) (server.SessionHandle, error) {
			s, serr := core.NewSession(name)
			if serr != nil {
				return nil, serr
			}
			s.SetPromptSkips(skips)
			return newSessionHandle(s), nil
		},
		// Saved sessions are dormant: listed by ls and revived on first attach
		// (task 082 requirement 5). Serve reads this once.
		SavedSessions: func() []server.SavedSession { return savedSessions(sessionsDir) },
		// kill-session on a dormant session deletes its file.
		DeleteSaved: func(name string) error {
			if !server.ValidSessionName(name) {
				return errs.New(errs.Config, "app.DeleteSaved", fmt.Sprintf("invalid session name %q", name))
			}
			err := os.Remove(filepath.Join(sessionsDir, name+".json"))
			if err != nil && !os.IsNotExist(err) {
				return errs.Wrap(errs.Config, "app.DeleteSaved", err)
			}
			return nil
		},
	})
}

// savedSessions lists the session files in dir. A file that cannot be read is
// skipped: a broken layout must not stop the server from starting.
func savedSessions(dir string) []server.SavedSession {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]server.SavedSession, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		f, lerr := mux.Load(path)
		if lerr != nil {
			continue
		}
		created, activity := f.SavedAt, f.SavedAt
		if info, serr := entry.Info(); serr == nil {
			activity = info.ModTime()
		}
		out = append(out, server.SavedSession{
			Name:     f.Name,
			Windows:  len(f.Windows),
			Created:  created,
			Activity: activity,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// newServerLogger opens <cache>/server.log for appending. The server always logs
// there: it is a daemon, and the file is the only place it can say what went
// wrong. A log that cannot be opened degrades to a discard logger.
func newServerLogger(cacheDir string) (*slog.Logger, *os.File) {
	if cacheDir == "" {
		return slog.New(slog.DiscardHandler), nil
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return slog.New(slog.DiscardHandler), nil
	}
	f, err := os.OpenFile(filepath.Join(cacheDir, serverLogName),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return slog.New(slog.DiscardHandler), nil
	}
	return slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})), f
}

// promptSkips counts the prompts the server refused because nobody was attached:
// host-key and passphrase questions that would have needed a human
// (task 082 requirement 6).
type promptSkips struct {
	mu         sync.Mutex
	hostKeys   int
	passphrase int
}

// newPromptSkips returns an empty counter.
func newPromptSkips() *promptSkips { return &promptSkips{} }

// note counts one refused prompt of kind.
func (p *promptSkips) note(kind interact.Kind) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch kind {
	case interact.HostKeyUnknown:
		p.hostKeys++
	case interact.Passphrase:
		p.passphrase++
	}
}

// take returns the counts and starts again from zero.
func (p *promptSkips) take() (hostKeys, passphrase int) {
	if p == nil {
		return 0, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	hostKeys, passphrase = p.hostKeys, p.passphrase
	p.hostKeys, p.passphrase = 0, 0
	return hostKeys, passphrase
}

// promptTap is the prompter the server's core is built with: it relays every
// question to the real prompter and counts the ones that came back
// errs.Interaction, which is how a prompt fails when no client can answer.
type promptTap struct {
	mu    sync.Mutex
	inner interact.Prompter
	skips *promptSkips
}

var _ interact.Prompter = (*promptTap)(nil)

// setInner points the tap at the prompter the core ended up with. It is called
// once, before any session or dial exists.
func (t *promptTap) setInner(p interact.Prompter) {
	t.mu.Lock()
	t.inner = p
	t.mu.Unlock()
}

// Ask relays the question and counts a refusal.
func (t *promptTap) Ask(ctx context.Context, prompt interact.Prompt) (interact.Answer, error) {
	t.mu.Lock()
	inner := t.inner
	t.mu.Unlock()
	if inner == nil {
		return interact.Answer{}, errs.New(errs.Interaction, "app.prompt", "no prompter configured")
	}
	ans, err := inner.Ask(ctx, prompt)
	if err != nil && errs.KindOf(err) == errs.Interaction {
		t.skips.note(prompt.Kind)
	}
	return ans, err
}

// SetAttached tells the real prompter whether a client can answer, so a
// detached prompt waits for interact.timeout and then fails with
// errs.Interaction (BUILD-PLAN §6).
func (t *promptTap) SetAttached(on bool) {
	t.mu.Lock()
	inner := t.inner
	t.mu.Unlock()
	if p, ok := inner.(interface{ SetAttached(bool) }); ok {
		p.SetAttached(on)
	}
}

// sessionHandle adapts *Session to the server's SessionHandle (BUILD-PLAN §6:
// the server does not import app, so it gets this thin adapter). It is what
// turns the session's detach into the EXIT the attached client is waiting for,
// and what turns the sessions dialog into requests to the server registry.
type sessionHandle struct {
	s  *Session
	mu sync.Mutex
	// onDetach belongs to the connection that attached last: the server uses
	// one per client and hands the session a new one on every attach.
	onDetach func(reason string)
	// ctrl is the server registry as this session may use it (task 082).
	ctrl server.SessionControl
}

var _ server.SessionHandle = (*sessionHandle)(nil)

// The registry operations the sessions dialog needs. server.Switcher is what
// makes the server hand this handle a Control (task 082).
var _ server.Switcher = (*sessionHandle)(nil)

// Rename renames the session and its autosave file (server's rename-session).
var _ interface{ Rename(string) error } = (*sessionHandle)(nil)

// newSessionHandle wraps a session for the server.
func newSessionHandle(s *Session) *sessionHandle {
	return &sessionHandle{s: s}
}

// SetSessionControl installs the registry operations on the session's root: the
// sessions dialog, `prefix $` and `:rename-session` then act through the server
// instead of pretending (task 082 requirements 3 and 4).
func (h *sessionHandle) SetSessionControl(ctrl server.SessionControl) {
	h.mu.Lock()
	h.ctrl = ctrl
	h.mu.Unlock()

	h.s.SetSwitchHook(ctrl.Switch)
	h.s.SetSessionFuncs(tui.SessionFuncs{
		List: func() ([]tui.SessionSummary, error) {
			list := ctrl.List()
			dormant := make(map[string]bool, len(list.Dormant))
			for _, name := range list.Dormant {
				dormant[name] = true
			}
			out := make([]tui.SessionSummary, 0, len(list.Sessions))
			for _, info := range list.Sessions {
				out = append(out, tui.SessionSummary{
					Name:     info.Name,
					Windows:  info.Windows,
					Attached: info.Attached,
					Dormant:  dormant[info.Name],
					Created:  info.Created,
					Activity: info.Activity,
				})
			}
			return out, nil
		},
		Switch: h.s.SwitchTo,
		New:    ctrl.New,
		Kill:   ctrl.Kill,
		Rename: ctrl.Rename,
	})
}

// Attach wires the client's streams to the session and installs the detach
// callback, so `prefix d`, `:detach` and a replaced client all end with an EXIT
// on the client's connection while the session keeps running.
func (h *sessionHandle) Attach(in io.Reader, out io.Writer, cols, rows int, env map[string]string, onDetach func(string)) error {
	h.mu.Lock()
	h.onDetach = onDetach
	h.mu.Unlock()

	h.s.SetDetachHook(func() { h.s.Detach("detached") })
	h.s.SetOnDetach(h.notify)
	return h.s.Attach(in, out, Size{Cols: cols, Rows: rows}, env)
}

// notify calls the current connection's detach callback.
func (h *sessionHandle) notify(reason string) {
	h.mu.Lock()
	cb := h.onDetach
	h.mu.Unlock()
	if cb != nil {
		cb(reason)
	}
}

// Detach unplugs the client.
func (h *sessionHandle) Detach(reason string) { h.s.Detach(reason) }

// Resize forwards the client's size.
func (h *sessionHandle) Resize(cols, rows int) { h.s.Resize(Size{Cols: cols, Rows: rows}) }

// Done is closed when the session ends.
func (h *sessionHandle) Done() <-chan struct{} { return h.s.Done() }

// Kill stops the session now.
func (h *sessionHandle) Kill() { h.s.Kill() }

// Rename renames the session: the registry entry, the session state and the
// autosave file (task 082 requirement 4).
func (h *sessionHandle) Rename(name string) error { return h.s.Rename(name) }

// Info reports the session as the server's registry wants it.
func (h *sessionHandle) Info() server.SessionInfo {
	info := h.s.Info()
	return server.SessionInfo{
		Name:     info.Name,
		Windows:  info.Windows,
		Attached: info.Attached,
		Created:  info.Created,
		Activity: info.Activity,
	}
}
