package k8s

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"jumpr/internal/errs"
)

const (
	// forwardOp is the errs.Error op of every failure this file reports.
	forwardOp = "k8s.portforward"
	// forwardLoopback is the only address a port-forward ever binds. A tunnel
	// listening on 0.0.0.0 would be reachable from the network, which is never
	// what a debugging port-forward is for.
	forwardLoopback = "127.0.0.1"
	// forwardStopTimeout bounds how long Stop waits for the pipes to drain.
	forwardStopTimeout = 5 * time.Second
)

// ForwardKind says what a port-forward targets.
type ForwardKind string

const (
	// ForwardPod forwards straight to the named pod.
	ForwardPod ForwardKind = "pod"
	// ForwardService resolves the service to one of its ready endpoint pods.
	ForwardService ForwardKind = "service"
)

// Normalize turns a kubectl alias ("po", "svc", "service") into a kind. An
// empty string means "decide by looking": a pod first, then a service.
func (k ForwardKind) Normalize() ForwardKind {
	switch ForwardKind(strings.ToLower(strings.TrimSpace(string(k)))) {
	case ForwardPod, "po", "pods":
		return ForwardPod
	case ForwardService, "svc", "services":
		return ForwardService
	}
	return ""
}

// connOpener opens one stream to the forwarded port. The real one creates a
// port-forward data stream; tests use an in-memory pair.
type connOpener func(ctx context.Context) (io.ReadWriteCloser, error)

// Forward is one running port-forward (SPEC §8.2): a local listener on
// 127.0.0.1 that copies every connection to a port in a pod.
type Forward struct {
	kind      ForwardKind
	namespace string
	name      string // the pod or service the user named
	pod       string // the pod the streams actually reach
	local     int
	remote    int
	since     time.Time
	addr      string

	ctx       context.Context
	cancel    context.CancelFunc
	ln        net.Listener
	open      connOpener
	closeOpen func()

	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	conns    atomic.Int64
	bytes    atomic.Int64

	mu  sync.Mutex
	err error
}

// newForward binds the local listener and starts copying. local may be 0 for an
// ephemeral port, which Local reports. The forward's own context is not derived
// from any caller's request: it lives until Stop, so a detach or a finished
// `:pf` command does not kill it.
func newForward(kind ForwardKind, ns, name, pod string, local, remote int,
	open connOpener, now func() time.Time) (*Forward, error) {
	if open == nil {
		return nil, errs.New(errs.Internal, forwardOp, "no stream opener")
	}
	if remote <= 0 || remote > 65535 {
		return nil, errs.New(errs.Config, forwardOp, "remote port "+strconv.Itoa(remote)+" is not a port")
	}
	if local < 0 || local > 65535 {
		return nil, errs.New(errs.Config, forwardOp, "local port "+strconv.Itoa(local)+" is not a port")
	}
	if now == nil {
		now = time.Now
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(forwardLoopback, strconv.Itoa(local)))
	if err != nil {
		return nil, &errs.Error{Kind: errs.KindOf(err), Op: forwardOp,
			Msg: "listen " + forwardLoopback + ":" + strconv.Itoa(local) + ": " + oneLine(err.Error()), Err: err}
	}
	fctx, cancel := context.WithCancel(context.Background())
	port := local
	if addr, ok := ln.Addr().(*net.TCPAddr); ok {
		port = addr.Port
	}
	f := &Forward{
		kind:      kind,
		namespace: ns,
		name:      name,
		pod:       pod,
		local:     port,
		remote:    remote,
		since:     now(),
		addr:      net.JoinHostPort(forwardLoopback, strconv.Itoa(port)),
		ctx:       fctx,
		cancel:    cancel,
		ln:        ln,
		open:      open,
		done:      make(chan struct{}),
	}
	go f.serve()
	return f, nil
}

// PortForward starts a port-forward to ns/name and returns it running. kind
// says whether name is a pod or a service; a service is resolved to one of its
// ready endpoint pods. local may be 0 for an ephemeral local port. The forward
// lives until Stop; it is not tied to the caller's request context, so a
// detach does not kill it.
func (b *Backend) PortForward(ctx context.Context, ns, name string, kind ForwardKind, local, remote int) (*Forward, error) {
	return b.portForward(ctx, ns, name, kind, local, remote, nil)
}

// portForward is PortForward with the stream opener injected.
func (b *Backend) portForward(ctx context.Context, ns, name string, kind ForwardKind, local, remote int,
	open connOpener) (*Forward, error) {
	if b == nil {
		return nil, errs.New(errs.Internal, forwardOp, "no backend")
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
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errs.New(errs.Config, forwardOp, "a pod or service needs a name")
	}
	kind = kind.Normalize()
	pod, err := b.forwardPod(ctx, ap, ns, name, kind)
	if err != nil {
		return nil, err
	}
	if open == nil {
		cfg, err := b.restConfig()
		if err != nil {
			return nil, err
		}
		var closeFn func()
		open, closeFn, err = spdyForwardOpener(ctx, cfg, ns, pod, remote)
		if err != nil {
			return nil, err
		}
		f, err := newForward(kind, ns, name, pod, local, remote, open, b.now)
		if err != nil {
			closeFn()
			return nil, err
		}
		f.closeOpen = closeFn
		return f, nil
	}
	return newForward(kind, ns, name, pod, local, remote, open, b.now)
}

// forwardPod is the pod a forward reaches: the named pod, or a ready endpoint
// pod of the named (or discovered) service.
func (b *Backend) forwardPod(ctx context.Context, ap api, ns, name string, kind ForwardKind) (string, error) {
	switch kind {
	case ForwardPod:
		return name, nil
	case ForwardService:
		return b.servicePod(ctx, ap, ns, name)
	}
	// No kind given: a pod of that name wins, then a service.
	if _, err := ap.typed.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
		return name, nil
	} else if !apierrors.IsNotFound(err) {
		return "", &errs.Error{Kind: Classify(err), Op: forwardOp,
			Msg: "pod " + ns + "/" + name + ": " + oneLine(err.Error()), Err: err}
	}
	return b.servicePod(ctx, ap, ns, name)
}

// servicePod picks one ready endpoint pod of svc. An endpoint address with a
// pod TargetRef answers directly; otherwise the addresses are matched against
// the pods' IPs.
func (b *Backend) servicePod(ctx context.Context, ap api, ns, svc string) (string, error) {
	eps, err := ap.typed.CoreV1().Endpoints(ns).Get(ctx, svc, metav1.GetOptions{})
	if err != nil {
		return "", &errs.Error{Kind: Classify(err), Op: forwardOp,
			Msg: "service " + ns + "/" + svc + ": " + oneLine(err.Error()), Err: err}
	}
	var addrs []string
	for _, sub := range eps.Subsets {
		for _, a := range sub.Addresses { // Addresses are the ready ones
			if a.TargetRef != nil && a.TargetRef.Kind == "Pod" && a.TargetRef.Name != "" {
				return a.TargetRef.Name, nil
			}
			if a.IP != "" {
				addrs = append(addrs, a.IP)
			}
		}
	}
	if len(addrs) == 0 {
		return "", errs.New(errs.NotAvailable, forwardOp,
			"service "+ns+"/"+svc+" has no ready endpoint")
	}
	pods, err := ap.typed.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", &errs.Error{Kind: Classify(err), Op: forwardOp,
			Msg: "pods in " + ns + ": " + oneLine(err.Error()), Err: err}
	}
	for _, ip := range addrs {
		for i := range pods.Items {
			if podHasIP(pods.Items[i], ip) {
				return pods.Items[i].Name, nil
			}
		}
	}
	return "", errs.New(errs.NotAvailable, forwardOp,
		"service "+ns+"/"+svc+" has no pod matching its endpoints")
}

// podHasIP reports whether p runs on ip.
func podHasIP(p corev1.Pod, ip string) bool {
	if p.Status.PodIP == ip {
		return true
	}
	for _, pip := range p.Status.PodIPs {
		if pip.IP == ip {
			return true
		}
	}
	return false
}

// portForwardURL is POST <host>/api/v1/namespaces/<ns>/pods/<pod>/portforward.
func portForwardURL(cfg *rest.Config, ns, pod string) (*url.URL, error) {
	u, err := url.Parse(cfg.Host)
	if err != nil {
		return nil, errs.Wrap(errs.Config, forwardOp, err)
	}
	u.Path = path.Join("/api/v1/namespaces/"+ns+"/pods/"+pod, "portforward")
	return u, nil
}

// spdyForwardOpener dials the port-forward subprotocol once and returns the
// opener that creates one stream pair per local connection, plus the function
// that closes the connection. ctx bounds only this dial: the forward itself
// outlives the command that started it.
func spdyForwardOpener(ctx context.Context, cfg *rest.Config, ns, pod string, remote int) (connOpener, func(), error) {
	u, err := portForwardURL(cfg, ns, pod)
	if err != nil {
		return nil, nil, err
	}
	rt, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return nil, nil, errs.Wrap(errs.Config, forwardOp, err)
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: rt}, http.MethodPost, u)
	type dialResult struct {
		conn httpstream.Connection
		err  error
	}
	ch := make(chan dialResult, 1)
	go func() {
		conn, _, err := dialer.Dial(portforward.PortForwardProtocolV1Name)
		ch <- dialResult{conn: conn, err: err}
	}()
	var conn httpstream.Connection
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, nil, &errs.Error{Kind: Classify(r.err), Op: forwardOp,
				Msg: "port-forward " + ns + "/" + pod + ": " + oneLine(r.err.Error()), Err: r.err}
		}
		conn = r.conn
	case <-ctx.Done():
		return nil, nil, errs.Wrap(errs.KindOf(ctx.Err()), forwardOp, ctx.Err())
	}
	fc := &forwardConn{conn: conn, port: strconv.Itoa(remote)}
	closeFn := func() { _ = conn.Close() }
	return fc.open, closeFn, nil
}

// forwardConn creates the data/error stream pair one local connection needs.
type forwardConn struct {
	conn httpstream.Connection
	port string
	next atomic.Int64
}

// open starts the streams for one connection.
func (fc *forwardConn) open(_ context.Context) (io.ReadWriteCloser, error) {
	id := strconv.FormatInt(fc.next.Add(1), 10)
	header := func(streamType string) http.Header {
		h := http.Header{}
		h.Set(corev1.StreamType, streamType)
		h.Set(corev1.PortHeader, fc.port)
		h.Set(corev1.PortForwardRequestIDHeader, id)
		return h
	}
	errStream, err := fc.conn.CreateStream(header(corev1.StreamTypeError))
	if err != nil {
		return nil, &errs.Error{Kind: Classify(err), Op: forwardOp, Msg: "port-forward stream: " + oneLine(err.Error()), Err: err}
	}
	_ = errStream.Close()
	dataStream, err := fc.conn.CreateStream(header(corev1.StreamTypeData))
	if err != nil {
		fc.conn.RemoveStreams(errStream)
		return nil, &errs.Error{Kind: Classify(err), Op: forwardOp, Msg: "port-forward stream: " + oneLine(err.Error()), Err: err}
	}
	s := &forwardStream{conn: fc.conn, data: dataStream, errStream: errStream}
	// The error stream carries the API server's refusal message, e.g. when the
	// pod port is closed; reading it ends the connection instead of hanging.
	go s.drain(errStream)
	return s, nil
}

// forwardStream is one forwarded TCP connection.
type forwardStream struct {
	conn      httpstream.Connection
	data      httpstream.Stream
	errStream httpstream.Stream
	once      sync.Once
}

// Read returns bytes from the pod.
func (s *forwardStream) Read(p []byte) (int, error) { return s.data.Read(p) }

// Write sends bytes to the pod.
func (s *forwardStream) Write(p []byte) (int, error) { return s.data.Write(p) }

// drain reads the error stream and resets the data stream when it ends, which
// is how a refused port becomes a closed local connection.
func (s *forwardStream) drain(errStream httpstream.Stream) {
	_, _ = io.Copy(io.Discard, errStream)
	_ = s.data.Reset()
}

// Close releases the streams.
func (s *forwardStream) Close() error {
	s.once.Do(func() {
		_ = s.data.Reset()
		s.conn.RemoveStreams(s.data, s.errStream)
	})
	return nil
}

// serve accepts local connections until the forward is stopped.
func (f *Forward) serve() {
	defer close(f.done)
	for {
		c, err := f.ln.Accept()
		if err != nil {
			if f.ctx.Err() != nil {
				return
			}
			f.setErr(err)
			return
		}
		f.conns.Add(1)
		f.wg.Add(1)
		go f.pipe(c)
	}
}

// pipe copies one local connection to the pod and back, counting bytes.
func (f *Forward) pipe(c net.Conn) {
	defer f.wg.Done()
	defer func() { _ = c.Close() }()
	rc, err := f.open(f.ctx)
	if err != nil {
		f.setErr(err)
		return
	}
	defer func() { _ = rc.Close() }()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(countWriter{w: rc, n: &f.bytes}, c)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(countWriter{w: c, n: &f.bytes}, rc)
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-f.ctx.Done():
	}
}

// countWriter counts the bytes that went through it.
type countWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// setErr remembers the first failure.
func (f *Forward) setErr(err error) {
	f.mu.Lock()
	if f.err == nil {
		f.err = err
	}
	f.mu.Unlock()
}

// Local is the local port the forward listens on.
func (f *Forward) Local() int { return f.local }

// Remote is the pod port the forward reaches.
func (f *Forward) Remote() int { return f.remote }

// Addr is the local address, always 127.0.0.1.
func (f *Forward) Addr() string { return f.addr }

// Kind says whether the forward was started on a pod or a service.
func (f *Forward) Kind() ForwardKind { return f.kind }

// Name is the pod or service the user named.
func (f *Forward) Name() string { return f.name }

// Target is the pod the streams reach.
func (f *Forward) Target() string { return f.pod }

// Namespace is the namespace of the target.
func (f *Forward) Namespace() string { return f.namespace }

// Since is when the forward started.
func (f *Forward) Since() time.Time { return f.since }

// Conns is how many local connections the forward accepted.
func (f *Forward) Conns() int64 { return f.conns.Load() }

// Bytes is how many bytes crossed the forward in both directions.
func (f *Forward) Bytes() int64 { return f.bytes.Load() }

// Done is closed when the accept loop ended.
func (f *Forward) Done() <-chan struct{} { return f.done }

// Err is the failure that stopped the forward, or nil.
func (f *Forward) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// Stop ends the forward: the listener closes, the pipes drain and the SPDY
// connection goes. It is safe to call more than once.
func (f *Forward) Stop() error {
	f.stopOnce.Do(func() {
		f.cancel()
		_ = f.ln.Close()
	})
	select {
	case <-f.done:
	case <-time.After(forwardStopTimeout):
	}
	drained := make(chan struct{})
	go func() {
		f.wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(forwardStopTimeout):
	}
	if f.closeOpen != nil {
		f.closeOpen()
	}
	return f.Err()
}

// ForwardSet is the port-forwards of one session, in the order they started.
// It is a plain value type: the session owns one and hands it to the `:pf`
// command, so the forwards survive a detach with the session server.
type ForwardSet struct {
	mu sync.Mutex
	m  map[int]*Forward
}

// NewForwardSet builds an empty set.
func NewForwardSet() *ForwardSet { return &ForwardSet{m: make(map[int]*Forward)} }

// Add records a running forward. A second forward on the same local port is an
// error: two listeners cannot share a port.
func (s *ForwardSet) Add(f *Forward) error {
	if f == nil {
		return errs.New(errs.Internal, forwardOp, "no forward")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[f.Local()]; ok {
		return errs.New(errs.Config, forwardOp, "local port "+strconv.Itoa(f.Local())+" is already forwarded")
	}
	s.m[f.Local()] = f
	return nil
}

// List returns the running forwards sorted by local port.
func (s *ForwardSet) List() []*Forward {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Forward, 0, len(s.m))
	for _, f := range s.m {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Local() < out[j].Local() })
	return out
}

// Get returns the forward on local port.
func (s *ForwardSet) Get(local int) (*Forward, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.m[local]
	return f, ok
}

// Stop stops the forward on local port and forgets it.
func (s *ForwardSet) Stop(local int) error {
	s.mu.Lock()
	f, ok := s.m[local]
	if ok {
		delete(s.m, local)
	}
	s.mu.Unlock()
	if !ok {
		return errs.New(errs.Config, forwardOp, "no port-forward on "+strconv.Itoa(local))
	}
	return f.Stop()
}

// StopAll stops every forward.
func (s *ForwardSet) StopAll() error {
	var errsFound []string
	s.mu.Lock()
	forwards := make([]*Forward, 0, len(s.m))
	for port, f := range s.m {
		forwards = append(forwards, f)
		delete(s.m, port)
	}
	s.mu.Unlock()
	for _, f := range forwards {
		if err := f.Stop(); err != nil {
			errsFound = append(errsFound, err.Error())
		}
	}
	if len(errsFound) > 0 {
		return errs.New(errs.Remote, forwardOp, strings.Join(errsFound, "; "))
	}
	return nil
}
