package ssh

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"

	"golang.org/x/crypto/ssh"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
)

const (
	// shellOp is the Op of transport-level shell errors.
	shellOp = "ssh.shell"
	// defaultShellTerm is the TERM requested when PTYRequest.Term is empty.
	defaultShellTerm = "xterm-256color"
	// shellTTYSpeed is the PTY line speed advertised to the remote side.
	shellTTYSpeed = 38400
)

var _ transport.ShellOpener = (*Transport)(nil)

// OpenShell implements transport.ShellOpener: it opens an interactive PTY
// session on host and hands back its byte stream. The pool lease is held until
// the shell closes. The user gets their normal login shell (no sudo, no command
// wrapping, no LC_ALL) unless req.Command names a command to run instead.
func (t *Transport) OpenShell(ctx context.Context, host resource.HostID, req transport.PTYRequest) (transport.Shell, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	lease, err := t.pool.Client(ctx, host)
	if err != nil {
		return nil, hostErr(err, host)
	}
	sess, err := openSession(ctx, host, lease)
	if err != nil {
		lease.Release()
		return nil, err
	}

	term := req.Term
	if term == "" {
		term = defaultShellTerm
	}
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: shellTTYSpeed,
		ssh.TTY_OP_OSPEED: shellTTYSpeed,
	}
	if err := sess.RequestPty(term, req.Rows, req.Cols, modes); err != nil {
		_ = sess.Close()
		lease.Release()
		return nil, errs.Wrap(errs.Unreachable, shellOp, err).WithHost(string(host))
	}

	// stderr is not piped: the PTY merges it into stdout.
	stdin, err := sess.StdinPipe()
	if err != nil {
		_ = sess.Close()
		lease.Release()
		return nil, errs.Wrap(errs.Internal, shellOp, err).WithHost(string(host))
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		lease.Release()
		return nil, errs.Wrap(errs.Internal, shellOp, err).WithHost(string(host))
	}

	if req.Command == "" {
		err = sess.Shell()
	} else {
		err = sess.Start(req.Command)
	}
	if err != nil {
		_ = sess.Close()
		lease.Release()
		return nil, errs.Wrap(errs.Unreachable, shellOp, err).WithHost(string(host))
	}

	sh := &shell{
		pool:   t.pool,
		host:   host,
		lease:  lease,
		sess:   sess,
		stdin:  stdin,
		stdout: stdout,
		log:    t.opts.Log,
		done:   make(chan struct{}),
	}
	go sh.watch(ctx)
	return sh, nil
}

// shell is one interactive PTY session. Read returns terminal output, Write
// sends keystrokes, and Close tears the session down exactly once.
type shell struct {
	pool   *Pool
	host   resource.HostID
	lease  *Lease
	sess   *ssh.Session
	stdin  io.WriteCloser
	stdout io.Reader
	log    *slog.Logger

	done      chan struct{} // closed by Close, stops the ctx watcher
	closeOnce sync.Once

	waitOnce sync.Once
	waitErr  error
}

var _ transport.Shell = (*shell)(nil)

// Read returns the terminal output of the remote session.
func (s *shell) Read(p []byte) (int, error) { return s.stdout.Read(p) }

// Write sends keystrokes to the remote session.
func (s *shell) Write(p []byte) (int, error) { return s.stdin.Write(p) }

// Resize tells the remote PTY its new size. The transport.Shell signature is
// (cols, rows); ssh.Session.WindowChange takes them the other way round.
func (s *shell) Resize(cols, rows int) error { return s.sess.WindowChange(rows, cols) }

// Wait returns when the remote side exits and releases the session. An exit
// status is not an error: the pane does not care about the code.
func (s *shell) Wait() error {
	s.waitOnce.Do(func() {
		s.waitErr = s.classifyWait(s.sess.Wait())
		_ = s.Close()
	})
	return s.waitErr
}

// Close closes stdin and the session and releases the lease. It is safe to
// call more than once, and to call it from the pane and from the context
// cancel at the same time.
func (s *shell) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		if s.stdin != nil {
			_ = s.stdin.Close()
		}
		_ = s.sess.Close()
		s.lease.Release()
		if s.log != nil {
			s.log.Debug("ssh shell closed", "host", string(s.host))
		}
	})
	return nil
}

// watch closes the shell when ctx is canceled and stops when it is closed for
// any other reason.
func (s *shell) watch(ctx context.Context) {
	select {
	case <-ctx.Done():
		_ = s.Close()
	case <-s.done:
	}
}

// classifyWait turns the session's end into what the pane should see: nil for a
// clean exit, an exit status or a close we asked for, and an unreachable error
// when the connection went away underneath us.
func (s *shell) classifyWait(err error) error {
	if err == nil {
		return nil
	}
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		return nil
	}
	select {
	case <-s.done:
		// Close ended the session; that is not a failure.
		return nil
	default:
	}
	s.pool.Forget(s.host)
	return errs.Wrap(errs.Unreachable, shellOp, err).WithHost(string(s.host))
}
