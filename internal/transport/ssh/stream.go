package ssh

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/shq"
	"jumpr/internal/transport"
)

const (
	// streamOp is the Op of transport-level stream errors.
	streamOp = "ssh.stream"
	// streamLineMax is the longest piece delivered for one line; longer lines
	// are split (a journal line can be arbitrarily long).
	streamLineMax = 64 << 10
	// streamBuffer is the depth of the line channel.
	streamBuffer = 256
	// streamStderrTail is how much of stderr the result keeps: the tail,
	// because the last line of an error says why.
	streamStderrTail = 8 << 10
)

// Start implements transport.Streamer: it runs cmd on host and delivers its
// stdout line by line. The session lease is held for the stream's lifetime and
// released by Close or by Wait.
func (t *Transport) Start(ctx context.Context, host resource.HostID, cmd transport.Command) (transport.Stream, error) {
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

	st := &stream{
		pool:     t.pool,
		host:     host,
		lease:    lease,
		sess:     sess,
		via:      viaLabel(lease.Via()),
		sudo:     cmd.Sudo,
		start:    time.Now(),
		log:      t.opts.Log,
		lines:    make(chan []byte, streamBuffer),
		abort:    make(chan struct{}),
		finished: make(chan struct{}),
		errTail:  newTailWriter(streamStderrTail),
	}

	out, err := sess.StdoutPipe()
	if err != nil {
		lease.Release()
		return nil, errs.Wrap(errs.Internal, streamOp, err).WithHost(string(host))
	}
	sess.Stderr = st.errTail
	if len(cmd.Stdin) > 0 {
		sess.Stdin = bytes.NewReader(cmd.Stdin)
	}
	if err := sess.Start(shq.Wrap(cmd.Script, cmd.Sudo)); err != nil {
		_ = sess.Close()
		lease.Release()
		return nil, errs.Wrap(errs.Unreachable, streamOp, err).WithHost(string(host))
	}

	go st.read(out)
	go st.watch(ctx)
	return st, nil
}

// stream is one running command whose stdout is delivered as lines.
type stream struct {
	pool  *Pool
	host  resource.HostID
	lease *Lease
	sess  *ssh.Session
	via   string
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

// Close kills the command and releases the lease. It is safe to call more than
// once, and to call without Wait.
func (s *stream) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.abort)
		_ = s.sess.Signal(ssh.SIGKILL)
		_ = s.sess.Close()
		s.lease.Release()
	})
	return nil
}

// watch closes the stream when ctx is canceled, and stops when the stream ends
// on its own.
func (s *stream) watch(ctx context.Context) {
	select {
	case <-ctx.Done():
		_ = s.Close()
	case <-s.finished:
	}
}

// read delivers stdout as lines and then reaps the session. It is the only
// goroutine that closes lines, and it always finishes the stream.
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
				// Close is killing the session; nobody wants the rest.
				return
			}
		}
		if err != nil {
			break
		}
	}
	s.reap()
}

// reap waits for the remote side to exit, records the result and frees the
// lease.
func (s *stream) reap() {
	err := s.sess.Wait()
	s.record(err)
	_ = s.sess.Close()
	s.lease.Release()
}

// record turns the session's exit into an ExecResult and an error.
func (s *stream) record(err error) {
	res := transport.ExecResult{
		Host:      s.host,
		Stderr:    s.errTail.Bytes(),
		Truncated: s.errTail.Truncated(),
		RC:        -1,
		Duration:  time.Since(s.start),
		Via:       s.via,
	}

	var exit *ssh.ExitError
	switch {
	case s.closed.Load():
		s.store(res, errs.New(errs.Canceled, streamOp, "stream closed").WithHost(string(s.host)))
	case err != nil && !errors.As(err, &exit):
		// *ssh.ExitMissingError, or a copy error: the connection went away.
		s.pool.Forget(s.host)
		s.store(res, errs.Wrap(errs.Unreachable, streamOp, err).WithHost(string(s.host)))
	default:
		if err != nil {
			res.RC = exit.ExitStatus()
		} else {
			res.RC = 0
		}
		var werr error
		if cerr := shq.Classify(res.RC, string(res.Stderr), s.sudo); errs.KindOf(cerr) == errs.Sudo {
			werr = hostErr(cerr, s.host)
		}
		s.store(res, werr)
	}
	if s.log != nil {
		s.log.Debug("ssh stream ended", "host", string(s.host), "rc", res.RC)
	}
}

// store publishes the result before finished is closed.
func (s *stream) store(res transport.ExecResult, err error) {
	s.mu.Lock()
	s.res, s.werr = res, err
	s.mu.Unlock()
}

// finish closes finished exactly once, waking Wait and the ctx watcher.
func (s *stream) finish() {
	s.finishOnce.Do(func() { close(s.finished) })
}

// readLine returns one line without its trailing newline. A line longer than
// streamLineMax is returned in pieces of at most streamLineMax bytes, so the
// caller sees the same bytes over several lines instead of an unbounded
// allocation. io.EOF is returned with the last piece.
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

// tailWriter keeps the last limit bytes written to it and accepts the rest.
// Like capWriter it never returns an error.
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
