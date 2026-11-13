package k8s

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	kexec "k8s.io/client-go/util/exec"

	"jumper/internal/errs"
	"jumper/internal/transport"
)

const (
	// defaultExecScript is the shell a pod exec starts when the caller asked for
	// no command: bash when the image has it, sh otherwise. It is passed as the
	// argv of `sh -c`, never spliced into a shell line here.
	defaultExecScript = "command -v bash >/dev/null && exec bash || exec sh"
	// execOp is the errs.Error op of every failure this file reports.
	execOp = "k8s.exec"
)

// executorMaker builds one exec transport for u. The SPDY and websocket
// variants below are the two real ones; tests pass fakes.
type executorMaker func(cfg *rest.Config, u *url.URL) (remotecommand.Executor, error)

// spdyExecutor is the transport every API server speaks.
func spdyExecutor(cfg *rest.Config, u *url.URL) (remotecommand.Executor, error) {
	return remotecommand.NewSPDYExecutor(cfg, http.MethodPost, u)
}

// webSocketExecutor is the newer transport, tried when the SPDY upgrade fails.
func webSocketExecutor(cfg *rest.Config, u *url.URL) (remotecommand.Executor, error) {
	return remotecommand.NewWebSocketExecutor(cfg, http.MethodPost, u.String())
}

// OpenShell opens an interactive PTY in container of the pod ns/pod (SPEC
// §8.2). It returns a transport.Shell, so a terminal pane hosts it exactly like
// an SSH shell (task 110). ns or container may be empty: the session namespace
// and the pod's default container are used then.
func (b *Backend) OpenShell(ctx context.Context, ns, pod, container string, req transport.PTYRequest) (transport.Shell, error) {
	return b.openShell(ctx, ns, pod, container, req, spdyExecutor, webSocketExecutor)
}

// openShell is OpenShell with the two executor transports injected.
func (b *Backend) openShell(ctx context.Context, ns, pod, container string, req transport.PTYRequest,
	primary, secondary executorMaker) (transport.Shell, error) {
	if b == nil {
		return nil, errs.New(errs.Internal, execOp, "no backend")
	}
	ap, err := b.api()
	if err != nil {
		return nil, err
	}
	ns = strings.TrimSpace(ns)
	if ns == "" {
		ns = ap.namespace
	}
	if ns == "" {
		ns = defaultNamespace
	}
	pod = strings.TrimSpace(pod)
	container = strings.TrimSpace(container)
	if pod == "" {
		return nil, errs.New(errs.Config, execOp, "a pod needs a name")
	}
	if err := validateExecTarget(ns, pod, container); err != nil {
		return nil, err
	}
	cfg, err := b.restConfig()
	if err != nil {
		return nil, err
	}
	u := execURL(cfg, ns, pod, container, execCommand(req.Command))
	s := newExecShell(ctx)
	go s.run(cfg, u, primary, secondary)
	return s, nil
}

// restConfig is the rest config of the current context. A backend the tests
// built around a fake clientset has no kubeconfig, so it gets an empty config
// and the injected executors ignore it.
func (b *Backend) restConfig() (*rest.Config, error) {
	if b.client == nil {
		return &rest.Config{}, nil
	}
	return b.client.RESTConfig()
}

// validateExecTarget rejects names that would change the request path. The API
// server would refuse them too; failing here keeps the URL well formed.
func validateExecTarget(ns, pod, container string) error {
	if problems := validation.IsDNS1123Label(ns); len(problems) > 0 {
		return errs.New(errs.Config, execOp, "namespace "+strconv.Quote(ns)+": "+problems[0])
	}
	if problems := validation.IsDNS1123Subdomain(pod); len(problems) > 0 {
		return errs.New(errs.Config, execOp, "pod "+strconv.Quote(pod)+": "+problems[0])
	}
	if container != "" {
		if problems := validation.IsDNS1123Label(container); len(problems) > 0 {
			return errs.New(errs.Config, execOp, "container "+strconv.Quote(container)+": "+problems[0])
		}
	}
	return nil
}

// execCommand is the argv the exec request starts: the caller's command through
// `sh -c`, or the default shell when there is none.
func execCommand(command string) []string {
	if command = strings.TrimSpace(command); command == "" {
		return []string{"sh", "-c", defaultExecScript}
	}
	return []string{"sh", "-c", command}
}

// execURL is POST <host>/api/v1/namespaces/<ns>/pods/<pod>/exec with the
// parameters the API server expects from an interactive TTY session.
func execURL(cfg *rest.Config, ns, pod, container string, argv []string) *url.URL {
	u, err := url.Parse(cfg.Host)
	if err != nil || u.Host == "" {
		// A config without a usable host cannot be reached; the executor
		// reports the dial error and the pane shows its kind.
		u = &url.URL{}
	}
	u.Path = path.Join("/api/v1/namespaces/"+ns+"/pods/"+pod, "exec")
	q := url.Values{}
	if container != "" {
		q.Set("container", container)
	}
	for _, a := range argv {
		q.Add("command", a)
	}
	q.Set("stdin", "true")
	q.Set("stdout", "true")
	q.Set("stderr", "true")
	q.Set("tty", "true")
	u.RawQuery = q.Encode()
	return u
}

// terminalSizeQueue is the remotecommand.TerminalSizeQueue a pane feeds from
// Resize. Only the newest size matters, so a push never blocks and replaces a
// size the remote side has not read yet.
type terminalSizeQueue struct {
	mu     sync.Mutex
	ch     chan remotecommand.TerminalSize
	closed bool
}

func newTerminalSizeQueue() *terminalSizeQueue {
	return &terminalSizeQueue{ch: make(chan remotecommand.TerminalSize, 1)}
}

// push records one size.
func (q *terminalSizeQueue) push(cols, rows int) {
	if cols <= 0 || rows <= 0 {
		return
	}
	size := remotecommand.TerminalSize{Width: uint16(cols), Height: uint16(rows)}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	select {
	case q.ch <- size:
	default:
		// Replace the stale size: a terminal only cares about the latest.
		select {
		case <-q.ch:
		default:
		}
		select {
		case q.ch <- size:
		default:
		}
	}
}

// Next implements remotecommand.TerminalSizeQueue. It returns nil once the
// shell is closed, which ends the executor's resize goroutine.
func (q *terminalSizeQueue) Next() *remotecommand.TerminalSize {
	q.mu.Lock()
	ch, closed := q.ch, q.closed
	q.mu.Unlock()
	if closed {
		return nil
	}
	size, ok := <-ch
	if !ok {
		return nil
	}
	return &size
}

// close ends Next for good.
func (q *terminalSizeQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	close(q.ch)
}

// execShell is the transport.Shell over one pod exec stream: bytes written go
// to the remote stdin, bytes read come from its stdout, and Resize feeds the
// terminal size queue.
type execShell struct {
	ctx    context.Context
	cancel context.CancelFunc
	sizes  *terminalSizeQueue
	stdinR *io.PipeReader
	stdinW *io.PipeWriter
	outR   *io.PipeReader
	outW   *io.PipeWriter
	done   chan struct{}

	mu     sync.Mutex
	err    error
	closed bool
}

var _ transport.Shell = (*execShell)(nil)

// newExecShell builds a shell whose stream is not started yet.
func newExecShell(ctx context.Context) *execShell {
	if ctx == nil {
		ctx = context.Background()
	}
	sctx, cancel := context.WithCancel(ctx)
	stdinR, stdinW := io.Pipe()
	outR, outW := io.Pipe()
	return &execShell{
		ctx:    sctx,
		cancel: cancel,
		sizes:  newTerminalSizeQueue(),
		stdinR: stdinR,
		stdinW: stdinW,
		outR:   outR,
		outW:   outW,
		done:   make(chan struct{}),
	}
}

// run drives the stream until it ends, then closes the pipes so Read returns
// EOF.
func (s *execShell) run(cfg *rest.Config, u *url.URL, primary, secondary executorMaker) {
	defer close(s.done)
	err := s.stream(cfg, u, primary, secondary)
	_ = s.outW.Close()
	_ = s.stdinR.Close()
	s.sizes.close()
	s.mu.Lock()
	s.err = normalizeExecError(s.ctx, err)
	s.mu.Unlock()
}

// stream starts one executor and pumps the session through it.
func (s *execShell) stream(cfg *rest.Config, u *url.URL, primary, secondary executorMaker) error {
	ex, err := pickExecutor(cfg, u, primary, secondary)
	if err != nil {
		return err
	}
	// TTY mode merges stderr into stdout on the wire, so Stderr must stay nil.
	return ex.StreamWithContext(s.ctx, remotecommand.StreamOptions{
		Stdin:             s.stdinR,
		Stdout:            s.outW,
		Tty:               true,
		TerminalSizeQueue: s.sizes,
	})
}

// pickExecutor builds the primary transport and wraps it with the websocket
// fallback, so an API server that refuses the SPDY upgrade still gets a shell.
func pickExecutor(cfg *rest.Config, u *url.URL, primary, secondary executorMaker) (remotecommand.Executor, error) {
	first, err := primary(cfg, u)
	if err != nil {
		if secondary == nil {
			return nil, err
		}
		return secondary(cfg, u)
	}
	if secondary == nil {
		return first, nil
	}
	second, err := secondary(cfg, u)
	if err != nil {
		return first, nil
	}
	fb, err := remotecommand.NewFallbackExecutor(first, second, upgradeFailure)
	if err != nil {
		return first, nil
	}
	return fb, nil
}

// upgradeFailure reports whether err means the server refused the transport
// rather than the command: only then is the fallback worth a second attempt.
func upgradeFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, frag := range []string{
		"unable to upgrade connection",
		"upgrade request required",
		"bad handshake",
		"unexpected response code",
		"websocket:",
		"protocol error",
	} {
		if strings.Contains(msg, frag) {
			return true
		}
	}
	return false
}

// normalizeExecError turns the end of a stream into what a terminal pane
// expects. A shell that exits is a normal close even when the API server ends
// the stream with an error instead of EOF (task 110 pitfall); anything else is
// an *errs.Error with the right kind.
func normalizeExecError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return nil // closed by us
	}
	var exit *kexec.CodeExitError
	if errors.As(err, &exit) {
		return nil
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	msg := strings.ToLower(err.Error())
	for _, frag := range []string{
		"stream error",
		"connection reset",
		"use of closed network connection",
		"broken pipe",
		"unexpected eof",
		"command terminated with",
	} {
		if strings.Contains(msg, frag) {
			return nil
		}
	}
	return &errs.Error{Kind: Classify(err), Op: execOp, Msg: oneLine(err.Error()), Err: err}
}

// Read returns remote output.
func (s *execShell) Read(p []byte) (int, error) { return s.outR.Read(p) }

// Write sends keystrokes to the remote stdin.
func (s *execShell) Write(p []byte) (int, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return 0, io.ErrClosedPipe
	}
	return s.stdinW.Write(p)
}

// Resize records a new terminal size; the executor sends it to the pod.
func (s *execShell) Resize(cols, rows int) error {
	s.sizes.push(cols, rows)
	return nil
}

// Wait blocks until the stream ends and reports a failure that was not the
// shell exiting.
func (s *execShell) Wait() error {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close ends the stream. It does not block: the stream goroutine finishes on
// its own and closes done.
func (s *execShell) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	s.cancel()
	_ = s.stdinW.CloseWithError(io.ErrClosedPipe)
	_ = s.outR.Close()
	return nil
}
