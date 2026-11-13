package ssh

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"time"

	"golang.org/x/crypto/ssh"

	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/shq"
	"jumper/internal/transport"
)

const (
	// defaultExecTimeout bounds one command when neither the Command nor the
	// Options set a timeout (BUILD-PLAN §8: exec 30 s).
	defaultExecTimeout = 30 * time.Second
	// defaultExecMaxOutput is the per-stream cap of the Command contract.
	defaultExecMaxOutput = 2 << 20
	// sessionRetryDelay is how long a channel-open failure is waited out
	// before the session channel is opened again: sshd refuses a new channel
	// while MaxSessions sessions are open, and the other one is usually
	// closing.
	sessionRetryDelay = 200 * time.Millisecond
	// abortGrace bounds how long Exec waits for the remote side to notice a
	// killed session before it returns the timeout anyway.
	abortGrace = 2 * time.Second
	// execOp is the Op of transport-level exec errors.
	execOp = "ssh.exec"
)

// Options configures a Transport. Zero values take the defaults above.
type Options struct {
	// ExecTimeout bounds one command when Command.Timeout is zero.
	ExecTimeout time.Duration
	// MaxOutput caps each output stream, in bytes, per command.
	MaxOutput int
	// Log receives connection and command debug lines. It may be nil.
	Log *slog.Logger
}

// Transport implements transport.Executor and transport.Streamer over the
// pooled SSH connections of a Pool. It is safe for concurrent use.
type Transport struct {
	pool *Pool
	opts Options
}

var (
	_ transport.Executor = (*Transport)(nil)
	_ transport.Streamer = (*Transport)(nil)
)

// New returns a Transport that borrows connections from pool. A nil pool is
// not usable: every call then fails like a closed pool.
func New(pool *Pool, o Options) *Transport {
	if o.ExecTimeout <= 0 {
		o.ExecTimeout = defaultExecTimeout
	}
	if o.MaxOutput <= 0 {
		o.MaxOutput = defaultExecMaxOutput
	}
	return &Transport{pool: pool, opts: o}
}

// Name implements transport.Executor.
func (t *Transport) Name() string { return "ssh" }

// Exec runs one command on host and returns its output and exit code. A
// non-zero exit code is not an error; only transport-level failures are
// (BUILD-PLAN §4.3, §7).
func (t *Transport) Exec(ctx context.Context, host resource.HostID, cmd transport.Command) (transport.ExecResult, error) {
	start := time.Now()
	res := transport.ExecResult{Host: host, RC: -1}
	if ctx == nil {
		ctx = context.Background()
	}

	parent := ctx
	timeout := t.execTimeout(cmd)
	runCtx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	lease, err := t.pool.Client(runCtx, host)
	if err != nil {
		res.Duration = time.Since(start)
		return res, hostErr(err, host)
	}
	defer lease.Release()
	res.Via = viaLabel(lease.Via())

	sess, err := openSession(runCtx, host, lease)
	if err != nil {
		res.Duration = time.Since(start)
		return res, err
	}
	defer func() { _ = sess.Close() }()

	limit := t.maxOutput(cmd)
	out := newCapWriter(limit)
	errOut := newCapWriter(limit)
	sess.Stdout = out
	sess.Stderr = errOut
	if len(cmd.Stdin) > 0 {
		sess.Stdin = bytes.NewReader(cmd.Stdin)
	}

	// Session.Run returns only once stdout and stderr are drained, so a
	// timeout has to close the session to unblock it.
	done := make(chan error, 1)
	go func() { done <- sess.Run(shq.Wrap(cmd.Script, cmd.Sudo)) }()

	select {
	case err = <-done:
	case <-runCtx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		grace := time.NewTimer(abortGrace)
		defer grace.Stop()
		select {
		case <-done:
		case <-grace.C:
		}
		res.Stdout, res.Stderr = out.Bytes(), errOut.Bytes()
		res.Truncated = out.Truncated() || errOut.Truncated()
		res.Duration = time.Since(start)
		if parent.Err() != nil {
			return res, ctxErr(parent, host, execOp)
		}
		return res, errs.New(errs.Timeout, execOp, "command timed out after "+timeout.String()).WithHost(string(host))
	}

	res.Stdout, res.Stderr = out.Bytes(), errOut.Bytes()
	res.Truncated = out.Truncated() || errOut.Truncated()
	res.Duration = time.Since(start)

	if err != nil {
		var exit *ssh.ExitError
		if errors.As(err, &exit) {
			// The command ran and said how it ended.
			res.RC = exit.ExitStatus()
		} else {
			// *ssh.ExitMissingError, or a copy error: the connection went away
			// before the command reported a status. Drop the pooled
			// connection so the next call dials again.
			t.pool.Forget(host)
			t.logDebug("ssh exec lost its connection", host, err)
			return res, errs.Wrap(errs.Unreachable, execOp, err).WithHost(string(host))
		}
	} else {
		res.RC = 0
	}

	// A `sudo -n` failure is a privilege problem the caller must see. rc 127
	// is left with the caller: batch scripts handle it per section.
	if cerr := shq.Classify(res.RC, string(res.Stderr), cmd.Sudo); errs.KindOf(cerr) == errs.Sudo {
		return res, hostErr(cerr, host)
	}
	t.logDebug("ssh exec done", host, nil, "rc", res.RC, "bytes", len(res.Stdout))
	return res, nil
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
	return defaultExecMaxOutput
}

// logDebug writes one debug line when a logger is configured, without holding
// anything across the call.
func (t *Transport) logDebug(msg string, host resource.HostID, err error, attrs ...any) {
	if t.opts.Log == nil {
		return
	}
	all := make([]any, 0, len(attrs)+2)
	all = append(all, "host", string(host))
	all = append(all, attrs...)
	if err != nil {
		all = append(all, "err", err.Error())
	}
	t.opts.Log.Debug(msg, all...)
}

// openSession opens a session channel on lease. A channel-open failure is
// transient when sshd's MaxSessions is reached: the session is retried once
// after sessionRetryDelay and reported as unreachable when that also fails.
func openSession(ctx context.Context, host resource.HostID, lease *Lease) (*ssh.Session, error) {
	client := lease.Client()
	if client == nil {
		return nil, errs.New(errs.Unreachable, execOp, "no connection").WithHost(string(host))
	}
	sess, err := client.NewSession()
	if err == nil {
		return sess, nil
	}
	if !sleepCtx(ctx, sessionRetryDelay) {
		return nil, ctxErr(ctx, host, execOp)
	}
	sess, err = client.NewSession()
	if err == nil {
		return sess, nil
	}
	return nil, &errs.Error{
		Kind: errs.Unreachable,
		Op:   execOp,
		Host: string(host),
		Msg:  "session rejected (MaxSessions?)",
		Err:  err,
	}
}

// sleepCtx waits d unless ctx finishes first; it reports whether the wait
// completed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
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
	return errs.Wrap(errs.KindOf(err), execOp, err).WithHost(string(host))
}

// viaLabel is ExecResult.Via: the transport, and the bastion that carried the
// connection when there is one.
func viaLabel(bastion string) string {
	if bastion == "" {
		return "ssh"
	}
	return "ssh via " + bastion
}
