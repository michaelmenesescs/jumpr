package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"jumper/internal/config"
	"jumper/internal/errs"
	"jumper/internal/server"
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
	core, err := newCore(ctx, cfg, paths, o.Env, o.Options, logger)
	if err != nil {
		return err
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
			return newSessionHandle(s), nil
		},
	})
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

// sessionHandle adapts *Session to the server's SessionHandle (BUILD-PLAN §6:
// the server does not import app, so it gets this thin adapter). It is what
// turns the session's detach into the EXIT the attached client is waiting for.
type sessionHandle struct {
	s  *Session
	mu sync.Mutex
	// onDetach belongs to the connection that attached last: the server uses
	// one per client and hands the session a new one on every attach.
	onDetach func(reason string)
}

var _ server.SessionHandle = (*sessionHandle)(nil)

// newSessionHandle wraps a session for the server.
func newSessionHandle(s *Session) *sessionHandle {
	return &sessionHandle{s: s}
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
