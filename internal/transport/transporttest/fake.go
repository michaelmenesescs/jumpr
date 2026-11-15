// Package transporttest provides a scripted in-memory transport for tests.
package transporttest

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
)

const defaultMaxOutput = 2 << 20

// Reply is a scripted answer.
type Reply struct {
	Stdout, Stderr string
	RC             int
	Err            error
	Delay          time.Duration
	Lines          []string
}

// Call records one Exec or Start.
type Call struct {
	Host    resource.HostID
	Command transport.Command
}

type rule struct {
	host  resource.HostID
	match string
	reply Reply
}

// Fake implements transport.Executor, Streamer and ShellOpener.
type Fake struct {
	mu       sync.Mutex
	rules    []rule
	hostRule []rule
	funcs    []func(resource.HostID, transport.Command) (Reply, bool)
	calls    []Call
	inFlight int
	maxSeen  int
	shells   map[resource.HostID][]*fakeShell
	resizes  map[resource.HostID][][2]int
}

var (
	_ transport.Executor    = (*Fake)(nil)
	_ transport.Streamer    = (*Fake)(nil)
	_ transport.ShellOpener = (*Fake)(nil)
)

// New returns an empty Fake.
func New() *Fake {
	return &Fake{shells: map[resource.HostID][]*fakeShell{}, resizes: map[resource.HostID][][2]int{}}
}

// Name returns "fake".
func (f *Fake) Name() string { return "fake" }

// On registers a rule for any host; the first registered match wins.
func (f *Fake) On(match string, r Reply) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, rule{match: match, reply: r})
	return f
}

// OnHost registers a host-specific rule, which takes precedence over On rules.
func (f *Fake) OnHost(host resource.HostID, match string, r Reply) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hostRule = append(f.hostRule, rule{host: host, match: match, reply: r})
	return f
}

// OnFunc registers a dynamic reply function, consulted before static rules.
func (f *Fake) OnFunc(fn func(host resource.HostID, cmd transport.Command) (Reply, bool)) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.funcs = append(f.funcs, fn)
	return f
}

// Calls returns a copy of the recorded calls.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// InFlightMax reports the highest number of concurrent Exec calls seen.
func (f *Fake) InFlightMax() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxSeen
}

func (f *Fake) lookup(host resource.HostID, cmd transport.Command) Reply {
	f.mu.Lock()
	funcs := append([]func(resource.HostID, transport.Command) (Reply, bool)(nil), f.funcs...)
	f.mu.Unlock()
	for _, fn := range funcs {
		if r, ok := fn(host, cmd); ok {
			return r
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.hostRule {
		if r.host == host && strings.Contains(cmd.Script, r.match) {
			return r.reply
		}
	}
	for _, r := range f.rules {
		if strings.Contains(cmd.Script, r.match) {
			return r.reply
		}
	}
	word := ""
	if fs := strings.Fields(cmd.Script); len(fs) > 0 {
		word = fs[0]
	}
	return Reply{RC: 127, Stderr: "sh: " + word + ": command not found"}
}

// wait sleeps for d, honouring ctx and an optional timeout.
func wait(ctx context.Context, d, timeout time.Duration, host resource.HostID) error {
	if d <= 0 {
		if err := ctx.Err(); err != nil {
			return errs.New(errs.Canceled, "fake.exec", "canceled").WithHost(string(host))
		}
		return nil
	}
	var tc <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		tc = t.C
	}
	dt := time.NewTimer(d)
	defer dt.Stop()
	select {
	case <-dt.C:
		return nil
	case <-tc:
		return errs.New(errs.Timeout, "fake.exec", "timed out").WithHost(string(host))
	case <-ctx.Done():
		if ctx.Err() == context.DeadlineExceeded {
			return errs.New(errs.Timeout, "fake.exec", "timed out").WithHost(string(host))
		}
		return errs.New(errs.Canceled, "fake.exec", "canceled").WithHost(string(host))
	}
}

func capOut(s string, max int) ([]byte, bool) {
	if max <= 0 {
		max = defaultMaxOutput
	}
	if len(s) > max {
		return []byte(s[:max]), true
	}
	return []byte(s), false
}

func (f *Fake) result(host resource.HostID, cmd transport.Command, r Reply, start time.Time) transport.ExecResult {
	out, t1 := capOut(r.Stdout, cmd.MaxOutput)
	se, t2 := capOut(r.Stderr, cmd.MaxOutput)
	return transport.ExecResult{
		Host: host, Stdout: out, Stderr: se, RC: r.RC, Truncated: t1 || t2,
		Duration: time.Since(start), Via: "fake",
	}
}

// Exec runs the scripted reply for cmd.
func (f *Fake) Exec(ctx context.Context, host resource.HostID, cmd transport.Command) (transport.ExecResult, error) {
	start := time.Now()
	f.mu.Lock()
	f.calls = append(f.calls, Call{Host: host, Command: cmd})
	f.inFlight++
	if f.inFlight > f.maxSeen {
		f.maxSeen = f.inFlight
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()

	r := f.lookup(host, cmd)
	if err := wait(ctx, r.Delay, cmd.Timeout, host); err != nil {
		return transport.ExecResult{Host: host, RC: -1, Duration: time.Since(start), Via: "fake"}, err
	}
	if r.Err != nil {
		return transport.ExecResult{Host: host, RC: -1, Duration: time.Since(start), Via: "fake"}, r.Err
	}
	return f.result(host, cmd, r, start), nil
}

type fakeStream struct {
	lines chan []byte
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once
	res   transport.ExecResult
	err   error
}

func (s *fakeStream) Lines() <-chan []byte { return s.lines }

func (s *fakeStream) Wait() (transport.ExecResult, error) {
	<-s.done
	return s.res, s.err
}

func (s *fakeStream) Close() error {
	s.once.Do(func() { close(s.stop) })
	<-s.done
	return nil
}

// Start emits Reply.Lines one by one, waiting Delay before each, then closes.
func (f *Fake) Start(ctx context.Context, host resource.HostID, cmd transport.Command) (transport.Stream, error) {
	start := time.Now()
	f.mu.Lock()
	f.calls = append(f.calls, Call{Host: host, Command: cmd})
	f.mu.Unlock()
	r := f.lookup(host, cmd)
	if r.Err != nil {
		return nil, r.Err
	}
	s := &fakeStream{lines: make(chan []byte), stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer close(s.lines)
		for _, l := range r.Lines {
			if r.Delay > 0 {
				t := time.NewTimer(r.Delay)
				select {
				case <-t.C:
				case <-s.stop:
					t.Stop()
					s.finish(host, r, start, errs.New(errs.Canceled, "fake.start", "stream closed"))
					return
				case <-ctx.Done():
					t.Stop()
					s.finish(host, r, start, errs.New(errs.Canceled, "fake.start", "canceled"))
					return
				}
			}
			select {
			case s.lines <- []byte(l):
			case <-s.stop:
				s.finish(host, r, start, errs.New(errs.Canceled, "fake.start", "stream closed"))
				return
			case <-ctx.Done():
				s.finish(host, r, start, errs.New(errs.Canceled, "fake.start", "canceled"))
				return
			}
		}
		s.finish(host, r, start, nil)
	}()
	return s, nil
}

func (s *fakeStream) finish(host resource.HostID, r Reply, start time.Time, err error) {
	se, _ := capOut(r.Stderr, 0)
	s.res = transport.ExecResult{Host: host, Stderr: se, RC: r.RC, Duration: time.Since(start), Via: "fake"}
	s.err = err
}

// ---- shell ----

type fakeShell struct {
	f    *Fake
	host resource.HostID
	mu   sync.Mutex
	cond *sync.Cond
	buf  []byte
	shut bool
}

func newShell(f *Fake, host resource.HostID) *fakeShell {
	s := &fakeShell{f: f, host: host}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *fakeShell) push(b []byte) {
	s.mu.Lock()
	s.buf = append(s.buf, b...)
	s.mu.Unlock()
	s.cond.Broadcast()
}

func (s *fakeShell) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.buf) == 0 && !s.shut {
		s.cond.Wait()
	}
	if len(s.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

func (s *fakeShell) Write(p []byte) (int, error) {
	s.mu.Lock()
	if s.shut {
		s.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	s.mu.Unlock()
	s.push(p)
	return len(p), nil
}

func (s *fakeShell) Resize(cols, rows int) error {
	s.f.mu.Lock()
	s.f.resizes[s.host] = append(s.f.resizes[s.host], [2]int{cols, rows})
	s.f.mu.Unlock()
	return nil
}

func (s *fakeShell) Close() error {
	s.mu.Lock()
	s.shut = true
	s.mu.Unlock()
	s.cond.Broadcast()
	return nil
}

func (s *fakeShell) Wait() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.shut {
		s.cond.Wait()
	}
	return nil
}

// OpenShell returns an in-memory echo shell.
func (f *Fake) OpenShell(ctx context.Context, host resource.HostID, req transport.PTYRequest) (transport.Shell, error) {
	if err := ctx.Err(); err != nil {
		return nil, errs.Wrap(errs.Canceled, "fake.shell", err).WithHost(string(host))
	}
	s := newShell(f, host)
	f.mu.Lock()
	f.shells[host] = append(f.shells[host], s)
	f.mu.Unlock()
	return s, nil
}

// ShellOutput injects terminal output into every open shell on host.
func (f *Fake) ShellOutput(host resource.HostID, b []byte) {
	f.mu.Lock()
	ss := append([]*fakeShell(nil), f.shells[host]...)
	f.mu.Unlock()
	for _, s := range ss {
		s.push(b)
	}
}

// Resizes returns the recorded (cols, rows) resizes for host.
func (f *Fake) Resizes(host resource.HostID) [][2]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]int(nil), f.resizes[host]...)
}
