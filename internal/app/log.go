package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"jumper/internal/redact"
	"jumper/internal/resource"
	"jumper/internal/transport"
	"jumper/internal/tui"
)

const (
	// logRotateBytes is where a log file is rotated: 10 MiB (task 126
	// requirement 4).
	logRotateBytes = 10 << 20
	// logRotateKeep is how many rotated files are kept (jumper.log.1 … .3).
	logRotateKeep = 3
	// jumperLogName is the client's debug log: <cache>/jumper.log.
	jumperLogName = "jumper.log"
	// commandLineCap bounds the command text one log line carries, after
	// redaction. A line longer than this is cut, never split across lines.
	commandLineCap = 512
)

// logCloser is what a log file hands its owner: the process closes it on the
// way out, and a rotating writer is not an *os.File.
type logCloser interface{ Close() error }

// newLogger returns the application logger: JSON to <cache>/jumper.log at
// Debug level when debug is on, a discard handler otherwise. The file rotates
// at 10 MiB and keeps three older files. A log file that cannot be opened
// degrades to the discard handler; logging never stops the program.
func newLogger(cacheDir string, debug bool) *slog.Logger {
	if !debug {
		return slog.New(slog.DiscardHandler)
	}
	if cacheDir == "" {
		return slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return slog.New(slog.DiscardHandler)
	}
	f, err := openRotating(filepath.Join(cacheDir, jumperLogName), logRotateBytes, logRotateKeep, 0o644)
	if err != nil {
		return slog.New(slog.DiscardHandler)
	}
	return slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// rotatingFile is an append-only log file that rotates when the next write
// would take it past max bytes, keeping keep older files (path.1 is the newest).
// One mutex serialises writers, so a JSON line is never interleaved with
// another one, and rotation cannot race a write.
type rotatingFile struct {
	mu    sync.Mutex
	path  string
	perm  os.FileMode
	max   int64
	keep  int
	f     *os.File
	size  int64
	rotat int // how many rotations happened, for tests
}

// openRotating opens path for appending, creating it when it is missing. The
// current size is read so a restart does not have to reach max before the first
// rotation.
func openRotating(path string, max int64, keep int, perm os.FileMode) (*rotatingFile, error) {
	w := &rotatingFile{path: path, perm: perm, max: max, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

// open (re)opens the live file and records its size.
func (w *rotatingFile) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, w.perm)
	if err != nil {
		return err
	}
	info, serr := f.Stat()
	if serr != nil {
		_ = f.Close()
		return serr
	}
	w.f = f
	w.size = info.Size()
	return nil
}

// Write appends p, rotating first when it would cross the limit. A write that
// fails leaves the previous file in place; logging is advisory and never
// returns an error the caller would act on.
func (w *rotatingFile) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.max > 0 && w.size+int64(len(p)) > w.max {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate moves the live file to path.1 and shifts the older ones down, dropping
// the one that falls off the end.
func (w *rotatingFile) rotate() error {
	if err := w.f.Close(); err != nil {
		return err
	}
	w.f = nil
	if w.keep > 0 {
		_ = os.Remove(w.numbered(w.keep))
		for i := w.keep - 1; i >= 1; i-- {
			if err := os.Rename(w.numbered(i), w.numbered(i+1)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	if err := os.Rename(w.path, w.numbered(1)); err != nil && !os.IsNotExist(err) {
		return err
	}
	w.size = 0
	w.rotat++
	return w.open()
}

// numbered is the name of the i-th rotated file.
func (w *rotatingFile) numbered(i int) string {
	return fmt.Sprintf("%s.%d", w.path, i)
}

// Close closes the live file.
func (w *rotatingFile) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// loggingExecutor is the remote-command log of §15: one debug line per command
// with its host, its redacted command line, how long it took, the exit code and
// the byte counts. It logs the command, never the output and never stdin.
type loggingExecutor struct {
	inner transport.Executor
	log   *slog.Logger
	red   *redact.Redactor
}

var _ transport.Executor = (*loggingExecutor)(nil)

// newLoggingExecutor wraps ex so every command it runs is logged. A nil logger
// returns ex unchanged: nothing is logged, and nothing is wrapped either.
func newLoggingExecutor(ex transport.Executor, log *slog.Logger, red *redact.Redactor) transport.Executor {
	if ex == nil || log == nil {
		return ex
	}
	return &loggingExecutor{inner: ex, log: log, red: red}
}

// Name implements transport.Executor.
func (l *loggingExecutor) Name() string { return l.inner.Name() }

// PoolStats forwards the pool's holdings to the TUI when the wrapped transport
// has them, so wrapping does not cost the status line its numbers.
func (l *loggingExecutor) PoolStats() tui.PoolStats {
	if ps, ok := l.inner.(tui.PoolStatter); ok {
		return ps.PoolStats()
	}
	return tui.PoolStats{}
}

// Exec runs one command and logs how it went.
func (l *loggingExecutor) Exec(ctx context.Context, host resource.HostID, cmd transport.Command) (transport.ExecResult, error) {
	start := time.Now()
	res, err := l.inner.Exec(ctx, host, cmd)
	dur := res.Duration
	if dur <= 0 {
		dur = time.Since(start)
	}
	l.logCommand(host, cmd, res, err, dur)
	return res, err
}

// logCommand writes the one line a remote command leaves in the log.
func (l *loggingExecutor) logCommand(host resource.HostID, cmd transport.Command, res transport.ExecResult, err error, dur time.Duration) {
	if l.log == nil {
		return
	}
	attrs := []any{
		"host", string(host),
		"cmd", l.commandText(cmd),
		"sudo", cmd.Sudo,
		"duration", dur.String(),
		"rc", res.RC,
		"out_bytes", len(res.Stdout),
		"err_bytes", len(res.Stderr),
		"truncated", res.Truncated,
	}
	if res.Via != "" {
		attrs = append(attrs, "via", res.Via)
	}
	if err != nil {
		// The error text is redacted too: a helper's message can echo the
		// command it was given.
		attrs = append(attrs, "err", l.redact(err.Error()))
	}
	l.log.Debug("remote command", attrs...)
}

// commandText is the command line as the log may keep it: redacted first, then
// cut to commandLineCap so one command cannot fill the file.
func (l *loggingExecutor) commandText(cmd transport.Command) string {
	line := cmd.Script
	if l.red != nil {
		line = l.red.String(line)
		line = l.red.Cap(line, commandLineCap)
	}
	return line
}

// logCommands returns the same transports with every remote command logged
// (task 126 requirement 3). Shells are not commands and are left alone. A nil
// logger returns e unchanged.
func (e *execSet) logCommands(log *slog.Logger, red *redact.Redactor) *execSet {
	if e == nil || log == nil {
		return e
	}
	out := *e
	if e.Executor != nil {
		out.Executor = newLoggingExecutor(e.Executor, log, red)
	}
	if e.Streamer != nil {
		out.Streamer = newLoggingStreamer(e.Streamer, log, red)
	}
	return &out
}

// redact cleans one string with the process redactor, if there is one.
func (l *loggingExecutor) redact(s string) string {
	if l.red == nil {
		return s
	}
	return l.red.String(s)
}

// loggingStreamer is the same logging around long-running commands: the start
// is logged with the command line, the end with the counts. The stream itself
// is handed over untouched, so a follower never waits on the log.
type loggingStreamer struct {
	inner transport.Streamer
	log   *slog.Logger
	red   *redact.Redactor
}

var _ transport.Streamer = (*loggingStreamer)(nil)

// newLoggingStreamer wraps s so every followed command is logged.
func newLoggingStreamer(s transport.Streamer, log *slog.Logger, red *redact.Redactor) transport.Streamer {
	if s == nil || log == nil {
		return s
	}
	return &loggingStreamer{inner: s, log: log, red: red}
}

// Start opens the stream and logs that the command started.
func (l *loggingStreamer) Start(ctx context.Context, host resource.HostID, cmd transport.Command) (transport.Stream, error) {
	start := time.Now()
	st, err := l.inner.Start(ctx, host, cmd)
	le := &loggingExecutor{log: l.log, red: l.red}
	attrs := []any{"host", string(host), "cmd", le.commandText(cmd), "sudo", cmd.Sudo}
	if err != nil {
		attrs = append(attrs, "err", le.redact(err.Error()))
		l.log.Debug("remote stream failed to start", attrs...)
		return st, err
	}
	l.log.Debug("remote stream started", attrs...)
	return &loggingStream{Stream: st, log: l.log, host: host, cmd: le.commandText(cmd), start: start}, nil
}

// loggingStream logs the end of a followed command when the caller waits for
// it. Lines are never logged.
type loggingStream struct {
	transport.Stream
	log   *slog.Logger
	host  resource.HostID
	cmd   string
	start time.Time
}

// Wait waits for the remote command to end and logs the outcome.
func (s *loggingStream) Wait() (transport.ExecResult, error) {
	res, err := s.Stream.Wait()
	if s.log == nil {
		return res, err
	}
	attrs := []any{
		"host", string(s.host),
		"cmd", s.cmd,
		"duration", time.Since(s.start).String(),
		"rc", res.RC,
		"out_bytes", len(res.Stdout),
		"err_bytes", len(res.Stderr),
	}
	if err != nil {
		attrs = append(attrs, "err", err.Error())
	}
	s.log.Debug("remote stream done", attrs...)
	return res, err
}
