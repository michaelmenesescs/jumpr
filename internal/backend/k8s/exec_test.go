package k8s

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"jumpr/internal/errs"
	"jumpr/internal/transport"
)

// fakeExecutor is a stand-in for a real exec transport: it echoes stdin to
// stdout, records the sizes the pane pushes and ends when stdin closes.
type fakeExecutor struct {
	streamErr error

	mu      sync.Mutex
	sizes   []remotecommand.TerminalSize
	started bool
}

var _ remotecommand.Executor = (*fakeExecutor)(nil)

func (f *fakeExecutor) Stream(o remotecommand.StreamOptions) error {
	return f.StreamWithContext(context.Background(), o)
}

func (f *fakeExecutor) StreamWithContext(ctx context.Context, o remotecommand.StreamOptions) error {
	f.mu.Lock()
	f.started = true
	f.mu.Unlock()
	if o.TerminalSizeQueue != nil {
		go func() {
			for {
				size := o.TerminalSizeQueue.Next()
				if size == nil {
					return
				}
				f.mu.Lock()
				f.sizes = append(f.sizes, *size)
				f.mu.Unlock()
			}
		}()
	}
	if f.streamErr != nil {
		// A failing transport reports at once, the way a refused upgrade does.
		return f.streamErr
	}
	if o.Stdin != nil && o.Stdout != nil {
		if _, err := io.Copy(o.Stdout, o.Stdin); err != nil {
			// The local side closed the session.
			return err
		}
	}
	return nil
}

// sizesSeen returns the sizes the executor received so far.
func (f *fakeExecutor) sizesSeen() []remotecommand.TerminalSize {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]remotecommand.TerminalSize, len(f.sizes))
	copy(out, f.sizes)
	return out
}

// maker returns an executorMaker for one fake executor.
func maker(ex *fakeExecutor) executorMaker {
	return func(*rest.Config, *url.URL) (remotecommand.Executor, error) { return ex, nil }
}

// failingMaker is an executorMaker whose transport cannot even be built.
func failingMaker() executorMaker {
	return func(*rest.Config, *url.URL) (remotecommand.Executor, error) {
		return nil, errors.New("unable to upgrade connection: 400 Bad Request")
	}
}

// readFor reads n bytes from r, failing the test after a second.
func readFor(t *testing.T, r io.Reader, n int) string {
	t.Helper()
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, n)
		_, err := io.ReadFull(r, buf)
		ch <- result{b: buf, err: err}
	}()
	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatalf("read: %v", got.err)
		}
		return string(got.b)
	case <-time.After(2 * time.Second):
		t.Fatalf("read blocked for a second")
		return ""
	}
}

// writeFor writes b, failing the test after a second.
func writeFor(t *testing.T, w io.Writer, b string) {
	t.Helper()
	ch := make(chan error, 1)
	go func() {
		_, err := w.Write([]byte(b))
		ch <- err
	}()
	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("write: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("write blocked for a second")
	}
}

// waitFor waits for a shell's stream to end.
func waitShell(t *testing.T, sh transport.Shell) error {
	t.Helper()
	ch := make(chan error, 1)
	go func() { ch <- sh.Wait() }()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("Wait blocked for a second")
		return nil
	}
}

// TestExecShellEchoAndResize: the shell pipes stdin to the executor, the
// executor's output back out, and Resize reaches the terminal size queue.
func TestExecShellEchoAndResize(t *testing.T) {
	b, _ := testBackend(t, "prod")
	ex := &fakeExecutor{}
	sh, err := b.openShell(context.Background(), "prod", "web-1", "app",
		transport.PTYRequest{Term: "xterm-256color", Cols: 80, Rows: 24}, maker(ex), nil)
	if err != nil {
		t.Fatalf("openShell: %v", err)
	}
	if err := sh.Resize(120, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	writeFor(t, sh, "echo hi\n")
	if got := readFor(t, sh, len("echo hi\n")); got != "echo hi\n" {
		t.Fatalf("read = %q, want the echoed bytes", got)
	}
	waitFor(t, time.Second, func() bool { return len(ex.sizesSeen()) > 0 })
	sizes := ex.sizesSeen()
	if len(sizes) == 0 {
		t.Fatal("the executor never received a terminal size")
	}
	if last := sizes[len(sizes)-1]; last.Width != 120 || last.Height != 40 {
		t.Fatalf("size = %dx%d, want 120x40", last.Width, last.Height)
	}
	if err := sh.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := waitShell(t, sh); err != nil {
		t.Fatalf("Wait: %v, want nil after the local side closed", err)
	}
}

// TestExecShellExitIsNormal: the stream ends with an error after the shell
// exits on some API servers; that is a normal close, not a failure.
func TestExecShellExitIsNormal(t *testing.T) {
	b, _ := testBackend(t, "prod")
	ex := &fakeExecutor{streamErr: errors.New("stream error: stream ID 3; INTERNAL_ERROR")}
	sh, err := b.openShell(context.Background(), "", "web-1", "",
		transport.PTYRequest{}, maker(ex), nil)
	if err != nil {
		t.Fatalf("openShell: %v", err)
	}
	_ = sh.Close()
	if err := waitShell(t, sh); err != nil {
		t.Fatalf("Wait = %v, want nil for a stream that ended after exit", err)
	}
}

// TestExecShellStreamError: a real failure keeps its kind.
func TestExecShellStreamError(t *testing.T) {
	b, _ := testBackend(t, "prod")
	ex := &fakeExecutor{streamErr: errors.New("forbidden: pods \"web-1\" is forbidden")}
	sh, err := b.openShell(context.Background(), "prod", "web-1", "app",
		transport.PTYRequest{}, maker(ex), nil)
	if err != nil {
		t.Fatalf("openShell: %v", err)
	}
	e := waitShell(t, sh)
	if e == nil {
		t.Fatal("Wait = nil, want the stream failure")
	}
	_ = sh.Close()
	var je *errs.Error
	if !errors.As(e, &je) {
		t.Fatalf("Wait = %T, want *errs.Error", e)
	}
	if je.Kind != errs.Auth {
		t.Fatalf("kind = %q, want auth", je.Kind)
	}
}

// TestExecSizeQueueKeepsLatest: only the newest size is delivered, and a closed
// queue ends Next.
func TestExecSizeQueueKeepsLatest(t *testing.T) {
	q := newTerminalSizeQueue()
	q.push(80, 24)
	q.push(100, 30)
	q.push(132, 43)
	first := q.Next()
	if first == nil || first.Width != 132 || first.Height != 43 {
		t.Fatalf("first = %+v, want the latest size 132x43", first)
	}
	q.close()
	if got := q.Next(); got != nil {
		t.Fatalf("Next after close = %+v, want nil", got)
	}
	q.push(10, 10) // must not panic on a closed queue
}

// TestExecURLParameters: the request URL is the exec subresource with TTY
// parameters and one command parameter per argv element.
func TestExecURLParameters(t *testing.T) {
	cfg := &rest.Config{Host: "https://api.example.internal:6443"}
	u := execURL(cfg, "prod", "web-1", "app", execCommand(""))
	if u.Path != "/api/v1/namespaces/prod/pods/web-1/exec" {
		t.Fatalf("path = %q", u.Path)
	}
	q := u.Query()
	if got := q.Get("tty"); got != "true" {
		t.Fatalf("tty = %q, want true", got)
	}
	if got := q.Get("container"); got != "app" {
		t.Fatalf("container = %q, want app", got)
	}
	if got := q["command"]; len(got) != 3 || got[0] != "sh" || got[1] != "-c" || got[2] != defaultExecScript {
		t.Fatalf("command = %v, want the default shell through sh -c", got)
	}
	// An explicit command goes through sh -c unchanged.
	u = execURL(cfg, "prod", "web-1", "", execCommand("tail -f /var/log/app.log"))
	if got := u.Query()["command"]; len(got) != 3 || got[2] != "tail -f /var/log/app.log" {
		t.Fatalf("command = %v, want the caller's command", got)
	}
	if got := u.Query().Get("container"); got != "" {
		t.Fatalf("container = %q, want empty so the pod default is used", got)
	}
}

// TestExecRejectsBadNames: a name the URL cannot carry is a config error, not a
// request to somewhere else.
func TestExecRejectsBadNames(t *testing.T) {
	b, _ := testBackend(t, "prod")
	cases := []struct{ ns, pod, container string }{
		{"../etc", "web-1", ""},
		{"prod", "web-1/exec", ""},
		{"prod", "web-1", "App"},
		{"prod", "", ""},
	}
	for _, c := range cases {
		if _, err := b.openShell(context.Background(), c.ns, c.pod, c.container,
			transport.PTYRequest{}, maker(&fakeExecutor{}), nil); err == nil {
			t.Fatalf("openShell(%q, %q, %q) succeeded, want an error", c.ns, c.pod, c.container)
		}
	}
}

// TestExecRequestReachesServer: with a real rest config the executor POSTs to
// the exec subresource; a fake API server records the request.
func TestExecRequestReachesServer(t *testing.T) {
	var mu sync.Mutex
	var method, path string
	var query url.Values
	got := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		method, path, query = r.Method, r.URL.Path, r.URL.Query()
		mu.Unlock()
		select {
		case got <- struct{}{}:
		default:
		}
		http.Error(w, "Upgrade request required", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	path0 := writeKubeconfig(t, "lab", ctxSpec{name: "lab", server: srv.URL, namespace: "prod"})
	c, err := NewClient(ClientOptions{Kubeconfig: path0, Home: t.TempDir()})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	b := NewWithClient(c)

	sh, err := b.OpenShell(context.Background(), "prod", "web-1", "app",
		transport.PTYRequest{Term: "xterm-256color", Cols: 90, Rows: 30})
	if err != nil {
		t.Fatalf("OpenShell: %v", err)
	}
	defer func() { _ = sh.Close() }()
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("the API server never saw the exec request")
	}
	mu.Lock()
	defer mu.Unlock()
	if method != http.MethodPost {
		t.Fatalf("method = %q, want POST", method)
	}
	if path != "/api/v1/namespaces/prod/pods/web-1/exec" {
		t.Fatalf("path = %q", path)
	}
	if query.Get("tty") != "true" || query.Get("stdin") != "true" || query.Get("container") != "app" {
		t.Fatalf("query = %v, want an interactive TTY session", query)
	}
	if cmd := query["command"]; len(cmd) != 3 || cmd[2] != defaultExecScript {
		t.Fatalf("command = %v, want the default shell", cmd)
	}
}

// TestExecFallsBackToWebSocket: when the SPDY transport cannot be built, the
// websocket transport opens the session instead.
func TestExecFallsBackToWebSocket(t *testing.T) {
	b, _ := testBackend(t, "prod")
	ex := &fakeExecutor{}
	sh, err := b.openShell(context.Background(), "prod", "web-1", "app",
		transport.PTYRequest{}, failingMaker(), maker(ex))
	if err != nil {
		t.Fatalf("openShell: %v", err)
	}
	writeFor(t, sh, "ls\n")
	if got := readFor(t, sh, 3); got != "ls\n" {
		t.Fatalf("read = %q, want the echoed bytes", got)
	}
	_ = sh.Close()
	if err := waitShell(t, sh); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// TestExecUpgradeFailure: only a failed upgrade, not a failed command, makes the
// fallback worth trying.
func TestExecUpgradeFailure(t *testing.T) {
	yes := []string{
		"unable to upgrade connection: 400 Bad Request",
		"websocket: bad handshake",
		"unexpected response code: 404",
	}
	no := []string{
		"forbidden",
		"command terminated with exit code 1",
		"",
	}
	for _, msg := range yes {
		if !upgradeFailure(errors.New(msg)) {
			t.Errorf("upgradeFailure(%q) = false, want true", msg)
		}
	}
	for _, msg := range no {
		var err error
		if msg != "" {
			err = errors.New(msg)
		}
		if upgradeFailure(err) {
			t.Errorf("upgradeFailure(%q) = true, want false", msg)
		}
	}
}

// TestExecCommandDefault: an empty request runs bash when the image has it.
func TestExecCommandDefault(t *testing.T) {
	got := strings.Join(execCommand("  "), " ")
	if got != "sh -c "+defaultExecScript {
		t.Fatalf("execCommand = %q", got)
	}
}
