// Package sysssh runs remote commands through the local /usr/bin/ssh. It is
// the fallback transport for hosts the native client cannot handle
// (ProxyCommand, GSSAPI, exotic ssh_config options): the local ssh already
// understands all of them (SPEC §7.1 "Fallback transport").
//
// Every invocation is non-interactive: BatchMode=yes means ssh never prompts on
// a terminal it does not have. The control socket lives under the cache
// directory so repeated calls to the same host reuse one connection.
package sysssh

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/shq"
	"jumpr/internal/transport"
)

const (
	// DefaultSSHPath is the system OpenSSH client.
	DefaultSSHPath = "/usr/bin/ssh"
	// via is ExecResult.Via for every result this transport produces.
	via = "system-ssh"
	// defaultConnectTimeout is `ConnectTimeout` when Options leaves it zero
	// (BUILD-PLAN §8: SSH connect 10 s).
	defaultConnectTimeout = 10 * time.Second
	// defaultExecTimeout bounds one command when neither the Command nor the
	// Options set a timeout (BUILD-PLAN §8: exec 30 s).
	defaultExecTimeout = 30 * time.Second
	// defaultMaxOutput is the per-stream cap of the Command contract.
	defaultMaxOutput = 2 << 20
	// controlPersist keeps the master connection for five minutes, the value
	// the task fixed for the fallback transport.
	controlPersist = "5m"
	// rcSSHFailure is the exit status ssh itself reports when it could not run
	// the command (auth, host key, DNS, refused, its own timeout).
	rcSSHFailure = 255
	// streamLineMax is the longest piece delivered for one line; longer lines
	// are split.
	streamLineMax = 64 << 10
	// streamBuffer is the depth of the line channel.
	streamBuffer = 256
	// streamStderrTail is how much of stderr a stream result keeps: the tail,
	// because the last line of an error says why.
	streamStderrTail = 8 << 10
	// shellTerm is the TERM requested when PTYRequest.Term is empty.
	shellTerm = "xterm-256color"

	opExec   = "sysssh.exec"
	opStream = "sysssh.stream"
	opShell  = "sysssh.shell"
)

// Options configures a Transport. Zero values take the defaults above.
type Options struct {
	// SSHPath is the ssh binary to run; default DefaultSSHPath.
	SSHPath string
	// ConfigFile is passed as `-F`; empty means ssh reads its own default.
	ConfigFile string
	// CacheDir holds the control sockets (<CacheDir>/cm/%C). Empty disables
	// ControlMaster, so every call dials its own connection.
	CacheDir string
	// ConnectTimeout is passed as `-o ConnectTimeout=<seconds>`.
	ConnectTimeout time.Duration
	// ExecTimeout bounds one command when Command.Timeout is zero.
	ExecTimeout time.Duration
	// MaxOutput caps each output stream, in bytes, per command.
	MaxOutput int
	// Log receives debug lines. It may be nil.
	Log *slog.Logger
}

// Transport implements transport.Executor, transport.Streamer and
// transport.ShellOpener by running the system ssh client. It is safe for
// concurrent use.
type Transport struct {
	opts Options
	// initErr records a control directory that could not be created; every
	// call reports it instead of running ssh without a usable ControlPath.
	initErr error
}

var (
	_ transport.Executor    = (*Transport)(nil)
	_ transport.Streamer    = (*Transport)(nil)
	_ transport.ShellOpener = (*Transport)(nil)
)

// New returns a Transport for the options. The control socket directory is
// created 0700 once, here.
func New(o Options) *Transport {
	if o.SSHPath == "" {
		o.SSHPath = DefaultSSHPath
	}
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = defaultConnectTimeout
	}
	if o.ExecTimeout <= 0 {
		o.ExecTimeout = defaultExecTimeout
	}
	if o.MaxOutput <= 0 {
		o.MaxOutput = defaultMaxOutput
	}
	t := &Transport{opts: o}
	if o.CacheDir != "" {
		if err := os.MkdirAll(t.controlDir(), 0o700); err != nil {
			t.initErr = errs.Wrap(errs.Config, opExec, err).WithHost("")
		}
	}
	return t
}

// Name implements transport.Executor. It is the same string ExecResult.Via
// carries, so a via column and a header agree.
func (t *Transport) Name() string { return via }

// controlDir is the directory holding the ssh control sockets.
func (t *Transport) controlDir() string { return filepath.Join(t.opts.CacheDir, "cm") }

// controlPath is the ControlPath value: one short socket per host, expanded by
// ssh's %C so the path stays under the unix socket limit.
func (t *Transport) controlPath() string {
	if t.opts.CacheDir == "" {
		return ""
	}
	return filepath.Join(t.controlDir(), "%C")
}

// baseArgs is what every ssh invocation gets: a fixed config file and options
// that never prompt and share one connection per host.
func (t *Transport) baseArgs() []string {
	args := make([]string, 0, 14)
	if t.opts.ConfigFile != "" {
		args = append(args, "-F", t.opts.ConfigFile)
	}
	args = append(args, "-o", "BatchMode=yes")
	if cp := t.controlPath(); cp != "" {
		args = append(args,
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+cp,
			"-o", "ControlPersist="+controlPersist,
		)
	}
	seconds := int(t.opts.ConnectTimeout / time.Second)
	if seconds <= 0 {
		seconds = 1
	}
	return append(args, "-o", "ConnectTimeout="+strconv.Itoa(seconds))
}

// execTimeout is the timeout for one command: the Command's own, else the
// transport default.
func (t *Transport) execTimeout(cmd transport.Command) time.Duration {
	if cmd.Timeout > 0 {
		return cmd.Timeout
	}
	if t.opts.ExecTimeout > 0 {
		return t.opts.ExecTimeout
	}
	return defaultExecTimeout
}

// maxOutput is the per-stream cap for one command: the Command's own, else the
// transport default.
func (t *Transport) maxOutput(cmd transport.Command) int {
	if cmd.MaxOutput > 0 {
		return cmd.MaxOutput
	}
	if t.opts.MaxOutput > 0 {
		return t.opts.MaxOutput
	}
	return defaultMaxOutput
}

// command is a freshly built ssh invocation.
func (t *Transport) command(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, t.opts.SSHPath, args...)
}

// Exec implements transport.Executor. A non-zero exit code of the remote
// command is not an error; ssh's own 255 is.
func (t *Transport) Exec(ctx context.Context, host resource.HostID, cmd transport.Command) (transport.ExecResult, error) {
	start := time.Now()
	res := transport.ExecResult{Host: host, RC: -1, Via: via}
	if ctx == nil {
		ctx = context.Background()
	}
	if t.initErr != nil {
		res.Duration = time.Since(start)
		return res, hostErr(t.initErr, host)
	}

	parent := ctx
	timeout := t.execTimeout(cmd)
	runCtx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	args := append(t.baseArgs(), "--", string(host), shq.Wrap(cmd.Script, cmd.Sudo))
	c := t.command(runCtx, args...)
	out := newCapWriter(t.maxOutput(cmd))
	errOut := newCapWriter(t.maxOutput(cmd))
	c.Stdout, c.Stderr = out, errOut
	if len(cmd.Stdin) > 0 {
		c.Stdin = bytes.NewReader(cmd.Stdin)
	}

	err := c.Run()
	res.Stdout, res.Stderr = out.Bytes(), errOut.Bytes()
	res.Truncated = out.Truncated() || errOut.Truncated()
	res.Duration = time.Since(start)

	if runCtx.Err() != nil {
		// The context ended while ssh was running: our own timeout, or the
		// caller's cancel.
		if parent.Err() != nil {
			return res, ctxErr(parent, host, opExec)
		}
		return res, errs.New(errs.Timeout, opExec, "command timed out after "+timeout.String()).WithHost(string(host))
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			res.RC = exit.ExitCode()
		} else {
			// ssh itself could not be started.
			return res, errs.Wrap(errs.Unreachable, opExec, err).WithHost(string(host))
		}
	} else {
		res.RC = 0
	}

	if res.RC == rcSSHFailure {
		// ssh never ran the command: RC stays -1 per the ExecResult contract.
		res.RC = -1
		err = classifySSH(string(res.Stderr), host, opExec)
		t.logDebug("sysssh exec failed", host, "rc 255", res.Duration)
		return res, err
	}
	if cerr := shq.Classify(res.RC, string(res.Stderr), cmd.Sudo); errs.KindOf(cerr) == errs.Sudo {
		return res, hostErr(cerr, host)
	}
	t.logDebug("sysssh exec done", host, nil, "rc", res.RC)
	return res, nil
}

// Start implements transport.Streamer: it runs cmd on host and delivers its
// stdout line by line.
func (t *Transport) Start(ctx context.Context, host resource.HostID, cmd transport.Command) (transport.Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if t.initErr != nil {
		return nil, hostErr(t.initErr, host)
	}

	args := append(t.baseArgs(), "--", string(host), shq.Wrap(cmd.Script, cmd.Sudo))
	c := t.command(ctx, args...)
	errTail := newTailWriter(streamStderrTail)
	c.Stderr = errTail
	if len(cmd.Stdin) > 0 {
		c.Stdin = bytes.NewReader(cmd.Stdin)
	}
	out, err := c.StdoutPipe()
	if err != nil {
		return nil, errs.Wrap(errs.Internal, opStream, err).WithHost(string(host))
	}
	if err := c.Start(); err != nil {
		return nil, errs.Wrap(errs.Unreachable, opStream, err).WithHost(string(host))
	}

	st := &stream{
		cmd:      c,
		host:     host,
		sudo:     cmd.Sudo,
		start:    time.Now(),
		log:      t.opts.Log,
		lines:    make(chan []byte, streamBuffer),
		abort:    make(chan struct{}),
		finished: make(chan struct{}),
		errTail:  errTail,
	}
	go st.read(out)
	go st.watch(ctx)
	return st, nil
}

// stream is one running ssh command whose stdout is delivered as lines.
type stream struct {
	cmd   *exec.Cmd
	host  resource.HostID
	sudo  bool
	start time.Time
	log   *slog.Logger

	lines    chan []byte
	abort    chan struct{} // closed by Close, stops the reader's sends
	finished chan struct{} // closed once the result is recorded

	errTail *tailWriter

	mu         sync.Mutex
	res        transport.ExecResult
	werr       error
	closeOnce  sync.Once
	finishOnce sync.Once
	closed     atomic.Bool
}

var _ transport.Stream = (*stream)(nil)

// Lines returns the stdout line channel; it is closed when the command ends or
// the stream is closed. Lines carry no trailing newline.
func (s *stream) Lines() <-chan []byte { return s.lines }

// Wait blocks until the command ends and returns its result. Stdout is empty;
// Stderr is the capped tail.
func (s *stream) Wait() (transport.ExecResult, error) {
	<-s.finished
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.res, s.werr
}

// Close kills ssh and releases the pipe. It is safe to call more than once and
// to call without Wait.
func (s *stream) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.abort)
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
	})
	return nil
}

// watch closes the stream when ctx is canceled, and stops when it ends on its
// own.
func (s *stream) watch(ctx context.Context) {
	select {
	case <-ctx.Done():
		_ = s.Close()
	case <-s.finished:
	}
}

// read delivers stdout as lines and then reaps ssh. It is the only goroutine
// that closes lines, and it always finishes the stream.
func (s *stream) read(r io.Reader) {
	defer s.finish()
	defer close(s.lines)

	br := bufio.NewReaderSize(r, streamLineMax)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			select {
			case s.lines <- line:
			case <-s.abort:
				// Close is killing ssh; nobody wants the rest.
				return
			}
		}
		if err != nil {
			break
		}
	}
	s.record(s.cmd.Wait())
}

// record turns ssh's exit into an ExecResult and an error.
func (s *stream) record(err error) {
	res := transport.ExecResult{
		Host:      s.host,
		Stderr:    s.errTail.Bytes(),
		Truncated: s.errTail.Truncated(),
		RC:        -1,
		Duration:  time.Since(s.start),
		Via:       via,
	}
	var werr error
	switch {
	case s.closed.Load():
		werr = errs.New(errs.Canceled, opStream, "stream closed").WithHost(string(s.host))
	case err != nil:
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			werr = errs.Wrap(errs.Unreachable, opStream, err).WithHost(string(s.host))
			break
		}
		if exit.ExitCode() == rcSSHFailure {
			werr = classifySSH(string(res.Stderr), s.host, opStream)
			break
		}
		res.RC = exit.ExitCode()
		if cerr := shq.Classify(res.RC, string(res.Stderr), s.sudo); errs.KindOf(cerr) == errs.Sudo {
			werr = hostErr(cerr, s.host)
		}
	default:
		res.RC = 0
	}
	s.mu.Lock()
	s.res, s.werr = res, werr
	s.mu.Unlock()
	if s.log != nil {
		s.log.Debug("sysssh stream ended", "host", string(s.host), "rc", res.RC)
	}
}

// finish closes finished exactly once, waking Wait and the ctx watcher.
func (s *stream) finish() {
	s.finishOnce.Do(func() { close(s.finished) })
}

// OpenShell implements transport.ShellOpener: ssh runs under a local PTY with
// `-tt`, so the remote side gets a terminal and the pane gets a byte stream.
func (t *Transport) OpenShell(ctx context.Context, host resource.HostID, req transport.PTYRequest) (transport.Shell, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if t.initErr != nil {
		return nil, hostErr(t.initErr, host)
	}

	term := req.Term
	if term == "" {
		term = shellTerm
	}
	args := append(t.baseArgs(), "-tt", "--", string(host))
	if req.Command != "" {
		args = append(args, shq.Wrap(req.Command, false))
	}
	c := t.command(ctx, args...)
	c.Env = append(os.Environ(), "TERM="+term)

	cols, rows := req.Cols, req.Rows
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	ptmx, err := pty.StartWithSize(c, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, errs.Wrap(errs.Unreachable, opShell, err).WithHost(string(host))
	}

	sh := &shell{cmd: c, ptmx: ptmx, host: host, ctx: ctx, log: t.opts.Log, done: make(chan struct{})}
	go sh.watch()
	return sh, nil
}

// shell is one interactive session through the system ssh client.
type shell struct {
	cmd  *exec.Cmd
	ptmx *os.File
	host resource.HostID
	ctx  context.Context
	log  *slog.Logger
	done chan struct{}

	closeOnce sync.Once
	waitOnce  sync.Once
	waitErr   error
}

var _ transport.Shell = (*shell)(nil)

// Read returns the terminal output of the remote session.
func (s *shell) Read(p []byte) (int, error) { return s.ptmx.Read(p) }

// Write sends keystrokes to the remote session.
func (s *shell) Write(p []byte) (int, error) { return s.ptmx.Write(p) }

// Resize tells the local and thereby the remote PTY its new size.
func (s *shell) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	return pty.Setsize(s.ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// Wait returns when ssh exits. An exit status is not an error: the pane does
// not care about the code.
func (s *shell) Wait() error {
	s.waitOnce.Do(func() {
		err := s.cmd.Wait()
		s.waitErr = s.classifyWait(err)
		_ = s.Close()
	})
	return s.waitErr
}

// Close kills ssh and closes the PTY. It is safe to call more than once, and to
// call from the pane and from the context cancel at the same time.
func (s *shell) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		_ = s.ptmx.Close()
		if s.log != nil {
			s.log.Debug("sysssh shell closed", "host", string(s.host))
		}
	})
	return nil
}

// watch closes the shell when ctx is canceled and stops when it is closed for
// any other reason.
func (s *shell) watch() {
	select {
	case <-s.ctx.Done():
		_ = s.Close()
	case <-s.done:
	}
}

// classifyWait turns the end of ssh into what the pane should see: nil for a
// clean exit, an exit status or a close we asked for, and a classified error
// when ssh itself failed.
func (s *shell) classifyWait(err error) error {
	if err == nil {
		return nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		select {
		case <-s.done:
			return nil
		default:
		}
		return errs.Wrap(errs.Unreachable, opShell, err).WithHost(string(s.host))
	}
	select {
	case <-s.done:
		// Close ended the session; that is not a failure.
		return nil
	default:
	}
	// ssh's own failure text went to the PTY, so the pane already shows it;
	// the code is all that is left to report.
	return errs.New(errs.Unreachable, opShell, "ssh exited with status "+strconv.Itoa(exit.ExitCode())).WithHost(string(s.host))
}

// classifySSH maps ssh's own failure (exit 255) to the transport kind
// BUILD-PLAN §7 asks for, using its stderr.
func classifySSH(stderr string, host resource.HostID, op string) error {
	low := strings.ToLower(stderr)
	var kind errs.Kind
	switch {
	case strings.Contains(low, "permission denied"):
		kind = errs.Auth
	case strings.Contains(low, "host key verification failed"), strings.Contains(low, "host key changed"):
		kind = errs.HostKey
	case strings.Contains(low, "timed out"), strings.Contains(low, "timeout"):
		kind = errs.Timeout
	case strings.Contains(low, "could not resolve"), strings.Contains(low, "refused"),
		strings.Contains(low, "no route to host"), strings.Contains(low, "connection closed"):
		kind = errs.Unreachable
	default:
		// ssh could not get a connection or a command; nothing else it
		// exits 255 for is a remote failure we know.
		kind = errs.Unreachable
	}
	msg := firstLine(stderr)
	if msg == "" {
		msg = "ssh failed (rc 255)"
	}
	return errs.New(kind, op, msg).WithHost(string(host))
}

// firstLine is the first non-empty stderr line, trimmed: the one that says why.
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			return ln
		}
	}
	return ""
}

// ctxErr turns a finished context into the transport error it means.
func ctxErr(ctx context.Context, host resource.HostID, op string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errs.New(errs.Timeout, op, "context deadline exceeded").WithHost(string(host))
	}
	return errs.New(errs.Canceled, op, "context canceled").WithHost(string(host))
}

// hostErr makes sure an error that crosses the package boundary is an
// *errs.Error naming the host.
func hostErr(err error, host resource.HostID) error {
	if err == nil {
		return nil
	}
	var e *errs.Error
	if errors.As(err, &e) {
		if e.Host == "" {
			return e.WithHost(string(host))
		}
		return err
	}
	return errs.Wrap(errs.KindOf(err), opExec, err).WithHost(string(host))
}

// logDebug writes one debug line when a logger is configured.
func (t *Transport) logDebug(msg string, host resource.HostID, attrs ...any) {
	if t.opts.Log == nil {
		return
	}
	all := make([]any, 0, len(attrs)+2)
	all = append(all, "host", string(host))
	all = append(all, attrs...)
	t.opts.Log.Debug(msg, all...)
}

// readLine returns one line without its trailing newline. A line longer than
// streamLineMax is returned in pieces of at most streamLineMax bytes.
func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		frag, more, err := br.ReadLine()
		if len(frag) > 0 {
			buf = append(buf, frag...)
		}
		if err != nil {
			return buf, err
		}
		if !more || len(buf) >= streamLineMax {
			return buf, nil
		}
	}
}

// capWriter keeps the first limit bytes written to it and accepts the rest.
// Write never returns an error: a full buffer must not kill ssh.
type capWriter struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

// newCapWriter returns a writer that keeps the first limit bytes.
func newCapWriter(limit int) *capWriter {
	if limit < 0 {
		limit = 0
	}
	return &capWriter{limit: limit}
}

// Write keeps what still fits and accepts the rest silently.
func (w *capWriter) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	room := w.limit - len(w.buf)
	if room <= 0 {
		w.truncated = true
		return n, nil
	}
	if n > room {
		w.buf = append(w.buf, p[:room]...)
		w.truncated = true
		return n, nil
	}
	w.buf = append(w.buf, p...)
	return n, nil
}

// Bytes returns a copy of what was kept.
func (w *capWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]byte, len(w.buf))
	copy(out, w.buf)
	return out
}

// Truncated reports whether bytes were dropped.
func (w *capWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.truncated
}

// tailWriter keeps the last limit bytes written to it.
type tailWriter struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

// newTailWriter returns a writer that keeps the last limit bytes.
func newTailWriter(limit int) *tailWriter {
	if limit < 0 {
		limit = 0
	}
	return &tailWriter{limit: limit}
}

// Write keeps the tail of what was written.
func (w *tailWriter) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.limit <= 0 {
		w.truncated = true
		return n, nil
	}
	if n >= w.limit {
		w.buf = append(w.buf[:0], p[n-w.limit:]...)
		w.truncated = true
		return n, nil
	}
	if over := len(w.buf) + n - w.limit; over > 0 {
		copy(w.buf, w.buf[over:])
		w.buf = w.buf[:len(w.buf)-over]
		w.truncated = true
	}
	w.buf = append(w.buf, p...)
	return n, nil
}

// Bytes returns a copy of the kept tail.
func (w *tailWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]byte, len(w.buf))
	copy(out, w.buf)
	return out
}

// Truncated reports whether bytes were dropped from the front.
func (w *tailWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.truncated
}
