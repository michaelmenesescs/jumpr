package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"jumper/internal/errs"
	"jumper/internal/safe"
)

const (
	// serverStartTimeout is how long EnsureServer waits for a server it just
	// started to answer on the socket (task 081).
	serverStartTimeout = 5 * time.Second
	// serverPollInterval is how often EnsureServer tries the socket again.
	serverPollInterval = 50 * time.Millisecond
	// serverDialTimeout bounds the probe dial itself: a connect to a unix
	// socket is local and either works at once or not at all.
	serverDialTimeout = 250 * time.Millisecond
	// serverLogLines is how much of the server log an error carries.
	serverLogLines = 20
	// serverEnvVar marks a process as the session server.
	serverEnvVar = "JUMPER_SERVER"
	// serverLogName and serverStateName are the server's files in the cache
	// directory: its log and what it records about itself.
	serverLogName   = "server.log"
	serverStateName = "server.json"
)

// EnsureServer makes sure a session server answers on socketPath (BUILD-PLAN
// §6: one server per user, started on demand). It first tries to dial; when
// nothing answers it starts exe with args detached from this terminal — its own
// session (setsid), stdin from /dev/null, stdout and stderr appended to logPath,
// working directory /, and env plus JUMPER_SERVER=1 — and polls the socket every
// 50 ms for up to 5 s.
//
// It never waits for the server it started: the child outlives the client and
// closing the terminal must not kill it. A server that does not answer in time,
// or that exits first, is reported as errs.Unreachable carrying the last lines
// of its log.
func EnsureServer(ctx context.Context, socketPath, exe string, args []string, env []string, logPath string) error {
	if err := CheckSocketPath(socketPath); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Already running: nothing to start.
	if dialServer(socketPath) == nil {
		return nil
	}
	if exe == "" {
		return errs.New(errs.Internal, "app.ensure_server", "no executable to start the server")
	}

	logFile, err := openServerLog(logPath)
	if err != nil {
		return err
	}
	defer logFile.Close() //nolint:errcheck // the child keeps its own handle
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return errs.Wrap(errs.Internal, "app.ensure_server", err)
	}
	defer devNull.Close()

	cmd := exec.Command(exe, args...)
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Dir = "/"
	cmd.Env = serverEnv(env)
	// A new session: closing the client's terminal sends SIGHUP to the client's
	// process group and must not reach the server (task 081 pitfall).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return errs.Wrap(errs.Unreachable, "app.ensure_server", err)
	}

	// The client does not wait for the server. Somebody still has to reap it,
	// so the exit is collected here and reported if it comes too early.
	exited := make(chan error, 1)
	// The reaper outlives this function, so its logger is the server's own log
	// file: a panic there is logged where the server's problems belong.
	reaperLog := slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelError}))
	safe.Go(reaperLog, "server reaper", func() { exited <- cmd.Wait() })

	deadline := time.Now().Add(serverStartTimeout)
	for {
		if dialServer(socketPath) == nil {
			return nil
		}
		select {
		case <-exited:
			return serverStartError("the server exited before it listened", logPath)
		default:
		}
		select {
		case <-ctx.Done():
			return errs.Wrap(errs.Canceled, "app.ensure_server", ctx.Err())
		default:
		}
		if !time.Now().Before(deadline) {
			return serverStartError(
				fmt.Sprintf("no server answered on %s after %s", socketPath, serverStartTimeout), logPath)
		}
		select {
		case <-ctx.Done():
			return errs.Wrap(errs.Canceled, "app.ensure_server", ctx.Err())
		case <-time.After(serverPollInterval):
		}
	}
}

// dialServer reports whether a server accepts a connection on socketPath.
func dialServer(socketPath string) error {
	c, err := net.DialTimeout("unix", socketPath, serverDialTimeout)
	if err != nil {
		return err
	}
	return c.Close()
}

// serverStartError is the error EnsureServer returns for a server that did not
// come up: the message plus the tail of the server's log, which is where the
// reason is.
func serverStartError(msg, logPath string) error {
	if tail := tailLines(logPath, serverLogLines); tail != "" {
		msg += ":\n" + tail
	}
	return errs.New(errs.Unreachable, "app.ensure_server", msg)
}

// openServerLog opens the server's log for appending, creating the cache
// directory first. An empty path sends the log to /dev/null.
func openServerLog(logPath string) (*os.File, error) {
	if logPath == "" {
		f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, "app.ensure_server", err)
		}
		return f, nil
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, errs.Wrap(errs.Config, "app.ensure_server", err)
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, errs.Wrap(errs.Config, "app.ensure_server", err)
	}
	return f, nil
}

// serverEnv is the environment of the started server: the client's, plus the
// marker that says this process is the server.
func serverEnv(env []string) []string {
	base := env
	if len(base) == 0 {
		base = os.Environ()
	}
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if strings.HasPrefix(kv, serverEnvVar+"=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, serverEnvVar+"=1")
}

// lastLines returns the last n lines of the file at path, or "" when it cannot
// be read or is empty. It is the tail an error carries.
func tailLines(path string, n int) string {
	if path == "" || n <= 0 {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	text := strings.TrimRight(string(data), "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	tail := strings.TrimSpace(strings.Join(lines, "\n"))
	if tail == "" {
		return ""
	}
	return "server log (" + path + "):\n" + tail
}

// ServerState is what a running server records about itself in
// <cache>/server.json, so a client can tell that the server it found was
// started from a different configuration directory.
type ServerState struct {
	ConfigDir string `json:"config_dir"`
	Version   string `json:"version"`
	PID       int    `json:"pid"`
}

// LogPath is the server's log: <cache>/server.log.
func LogPath(cacheDir string) string {
	return filepath.Join(cacheDir, serverLogName)
}

// StatePath is the server's state file: <cache>/server.json.
func StatePath(cacheDir string) string {
	return filepath.Join(cacheDir, serverStateName)
}

// WriteServerState records st at path, creating the directory first.
func WriteServerState(path string, st ServerState) error {
	if path == "" {
		return errs.New(errs.Config, "app.server_state", "empty state path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return errs.Wrap(errs.Config, "app.server_state", err)
	}
	data, err := json.Marshal(st)
	if err != nil {
		return errs.Wrap(errs.Internal, "app.server_state", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return errs.Wrap(errs.Config, "app.server_state", err)
	}
	return nil
}

// ReadServerState reads the state a running server wrote. A missing file is an
// error: it means the server did not record itself.
func ReadServerState(path string) (ServerState, error) {
	var st ServerState
	if path == "" {
		return st, errs.New(errs.Config, "app.server_state", "empty state path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return st, errs.Wrap(errs.Config, "app.server_state", err)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return ServerState{}, errs.Wrap(errs.Parse, "app.server_state", err)
	}
	return st, nil
}

// ConfigWarning is the line a client prints before attaching when the server it
// found was started from another configuration directory. It is "" when the
// directories match or when the server did not record itself: an old server
// saying nothing is better than a guess.
func ConfigWarning(statePath, configDir string) string {
	st, err := ReadServerState(statePath)
	if err != nil || st.ConfigDir == "" {
		return ""
	}
	if filepath.Clean(st.ConfigDir) == filepath.Clean(configDir) {
		return ""
	}
	return fmt.Sprintf("jumper: warning: the running server uses config directory %s", st.ConfigDir)
}
