package ssh

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skeema/knownhosts"
	gossh "golang.org/x/crypto/ssh"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/transport/ssh/sshtest"
)

// fakeClock is a settable clock for the idle reaper.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// poolKnownHosts writes one known_hosts file covering every loopback server.
func poolKnownHosts(t *testing.T, servers ...*sshtest.Server) string {
	t.Helper()
	var b strings.Builder
	for _, srv := range servers {
		b.WriteString(knownhosts.Line([]string{hostAddr("127.0.0.1", srv.Port())}, srv.HostKey()))
		b.WriteByte('\n')
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// poolFixture is a bastion, n targets behind it and one direct host, all on
// loopback, driven through a Pool resolved from a generated ssh_config.
type poolFixture struct {
	pool       *Pool
	bastion    *sshtest.Server
	direct     *sshtest.Server
	targets    []*sshtest.Server
	hosts      []resource.HostID
	directHost resource.HostID
	clock      *fakeClock
}

func (f *poolFixture) close(t *testing.T) {
	t.Helper()
	if err := f.pool.Close(); err != nil {
		t.Fatalf("close pool: %v", err)
	}
}

// newPoolFixture starts the servers and the pool. mutate may adjust the
// options before the pool is created.
func newPoolFixture(t *testing.T, n int, mutate func(*PoolOptions)) *poolFixture {
	t.Helper()
	priv, pub := newEd25519(t)
	keyFile := writePrivateKey(t, priv, "")

	handler := sshtest.CommandTable(map[string]string{"uptime": "ok\n"})
	bastion := sshtest.Start(t, sshtest.Options{AllowForward: true, AuthorizedKeys: []gossh.PublicKey{pub}})
	direct := sshtest.Start(t, sshtest.Options{AuthorizedKeys: []gossh.PublicKey{pub}, Handler: handler})
	targets := make([]*sshtest.Server, n)
	for i := range targets {
		targets[i] = sshtest.Start(t, sshtest.Options{AuthorizedKeys: []gossh.PublicKey{pub}, Handler: handler})
	}

	known := poolKnownHosts(t, append([]*sshtest.Server{bastion, direct}, targets...)...)

	var cfg strings.Builder
	fmt.Fprintf(&cfg, "Host bastion.example.internal\n"+
		"  HostName 127.0.0.1\n  Port %d\n  User tester\n  IdentityFile %s\n"+
		"  UserKnownHostsFile %s\n  StrictHostKeyChecking yes\n  ConnectTimeout 5\n\n",
		bastion.Port(), keyFile, known)
	fmt.Fprintf(&cfg, "Host direct.example.internal\n"+
		"  HostName 127.0.0.1\n  Port %d\n  User tester\n  IdentityFile %s\n"+
		"  UserKnownHostsFile %s\n  StrictHostKeyChecking yes\n  ConnectTimeout 5\n\n",
		direct.Port(), keyFile, known)

	hosts := make([]resource.HostID, n)
	for i, srv := range targets {
		alias := fmt.Sprintf("web-%02d.example.internal", i+1)
		fmt.Fprintf(&cfg, "Host %s\n"+
			"  HostName 127.0.0.1\n  Port %d\n  User tester\n  ProxyJump bastion.example.internal\n"+
			"  IdentityFile %s\n  UserKnownHostsFile %s\n  StrictHostKeyChecking yes\n  ConnectTimeout 5\n\n",
			alias, srv.Port(), keyFile, known)
		hosts[i] = resource.HostID(alias)
	}

	cfgPath := filepath.Join(t.TempDir(), "ssh_config")
	if err := os.WriteFile(cfgPath, []byte(cfg.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := NewResolver([]string{cfgPath}, t.TempDir(), "tester")
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}

	clock := &fakeClock{t: time.Now()}
	o := PoolOptions{Resolver: res, Dial: DialOptions{ConnectTimeout: 5 * time.Second}, Now: clock.Now}
	if mutate != nil {
		mutate(&o)
	}
	return &poolFixture{
		pool:       NewPool(o),
		bastion:    bastion,
		direct:     direct,
		targets:    targets,
		hosts:      hosts,
		directHost: "direct.example.internal",
		clock:      clock,
	}
}

// lease takes a lease or fails.
func (f *poolFixture) lease(t *testing.T, host resource.HostID) *Lease {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l, err := f.pool.Client(ctx, host)
	if err != nil {
		t.Fatalf("client %s: %v", host, err)
	}
	return l
}

// bastionTargets returns the target count of every connection in one bastion
// set, in creation order.
func (f *poolFixture) bastionTargets(alias string) []int {
	f.pool.mu.Lock()
	defer f.pool.mu.Unlock()
	s := f.pool.bastions[alias]
	if s == nil {
		return nil
	}
	out := make([]int, 0, len(s.conns))
	for _, c := range s.conns {
		out = append(out, c.targets)
	}
	return out
}

// assertChannelSplit checks that conns connections carry at most perConn
// targets each, and that no slot is idle while targets wait for one.
func assertChannelSplit(t *testing.T, targets []int, conns, perConn int) {
	t.Helper()
	if len(targets) != conns {
		t.Fatalf("bastion set has %d connections carrying %v targets, want %d", len(targets), targets, conns)
	}
	used := 0
	for _, n := range targets {
		if n > perConn {
			t.Fatalf("a bastion connection carries %d targets, want at most %d (%v)", n, perConn, targets)
		}
		used += n
	}
	if used != conns*perConn {
		t.Fatalf("targets per connection = %v, want %d full connections of %d", targets, conns, perConn)
	}
}

func TestPoolDirectHost(t *testing.T) {
	f := newPoolFixture(t, 1, nil)
	defer f.close(t)

	l1 := f.lease(t, f.directHost)
	if l1.Client() == nil {
		t.Fatal("lease has no client")
	}
	if got := l1.Via(); got != "" {
		t.Fatalf("Via() = %q, want empty for a direct host", got)
	}
	l1.Release()

	l2 := f.lease(t, f.directHost)
	l2.Release()

	if got := f.direct.Conns(); got != 1 {
		t.Fatalf("direct server connections = %d, want 1", got)
	}
	st := f.pool.Stats()
	if st.Bastions != 0 || st.Targets != 1 || st.Leases != 0 {
		t.Fatalf("stats = %+v, want 0 bastions, 1 target, 0 leases", st)
	}
}

func TestPoolReusesTargetConn(t *testing.T) {
	f := newPoolFixture(t, 1, nil)
	defer f.close(t)

	for i := 0; i < 2; i++ {
		l := f.lease(t, f.hosts[0])
		if got := l.Via(); got != "bastion.example.internal" {
			t.Fatalf("Via() = %q, want the bastion alias", got)
		}
		sess, err := l.Client().NewSession()
		if err != nil {
			t.Fatalf("session on lease %d: %v", i, err)
		}
		out, err := sess.Output("uptime")
		sess.Close()
		if err != nil || string(out) != "ok\n" {
			t.Fatalf("exec on lease %d: out=%q err=%v", i, out, err)
		}
		l.Release()
	}

	if got := f.targets[0].Conns(); got != 1 {
		t.Fatalf("target connections = %d, want 1 (reused)", got)
	}
	if got := f.bastion.Conns(); got != 1 {
		t.Fatalf("bastion connections = %d, want 1", got)
	}
	st := f.pool.Stats()
	if st.Bastions != 1 || st.Targets != 1 || st.Leases != 0 {
		t.Fatalf("stats = %+v, want 1 bastion, 1 target, 0 leases", st)
	}
}

func TestPoolSingleflight(t *testing.T) {
	f := newPoolFixture(t, 1, func(o *PoolOptions) { o.MaxSessionsPerHost = 32 })
	defer f.close(t)

	const n = 20
	start := make(chan struct{})
	errsCh := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			l, err := f.pool.Client(ctx, f.hosts[0])
			if err != nil {
				errsCh <- err
				return
			}
			l.Release()
			errsCh <- nil
		}()
	}
	close(start)
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		if err != nil {
			t.Fatalf("concurrent client: %v", err)
		}
	}

	if got := f.targets[0].Conns(); got != 1 {
		t.Fatalf("target connections = %d, want 1 shared dial", got)
	}
	if got := f.bastion.Conns(); got != 1 {
		t.Fatalf("bastion connections = %d, want 1", got)
	}
}

func TestPoolBastionChannelLimit(t *testing.T) {
	f := newPoolFixture(t, 7, func(o *PoolOptions) {
		o.MaxConnsPerBastion = 2
		o.MaxChannelsPerConn = 3
	})
	defer f.close(t)

	leases := make([]*Lease, 6)
	for i := range leases {
		leases[i] = f.lease(t, f.hosts[i])
	}
	st := f.pool.Stats()
	if st.Bastions != 2 || st.Targets != 6 {
		t.Fatalf("stats = %+v, want 2 bastions carrying 6 targets", st)
	}
	if got := f.bastion.Conns(); got != 2 {
		t.Fatalf("bastion connections = %d, want exactly 2", got)
	}
	// sshtest counts forwarded channels server-wide, so the aggregate must be
	// conns x channels; the per-connection split is the pool's own bookkeeping.
	assertChannelSplit(t, f.bastionTargets("bastion.example.internal"), 2, 3)
	if got := f.bastion.MaxConcurrentChannels(); got != 6 {
		t.Fatalf("bastion saw %d concurrent channels, want 6 (2 connections x 3)", got)
	}

	type result struct {
		lease *Lease
		err   error
	}
	done := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		l, err := f.pool.Client(ctx, f.hosts[6])
		done <- result{lease: l, err: err}
	}()

	// The seventh target must wait: both connections are at their channel cap.
	time.Sleep(50 * time.Millisecond)
	select {
	case r := <-done:
		if r.lease != nil {
			r.lease.Release()
		}
		t.Fatalf("seventh client returned before a channel was free (err %v)", r.err)
	default:
	}

	// Freeing one channel lets it through, without opening a third connection.
	f.pool.Forget(f.hosts[0])
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("seventh client after freeing a slot: %v", r.err)
		}
		r.lease.Release()
	case <-time.After(5 * time.Second):
		t.Fatal("seventh client still blocked after a channel was freed")
	}

	if got := f.bastion.Conns(); got != 2 {
		t.Fatalf("bastion connections = %d, want still 2", got)
	}
	assertChannelSplit(t, f.bastionTargets("bastion.example.internal"), 2, 3)
	if got := f.bastion.MaxConcurrentChannels(); got > 6 {
		t.Fatalf("bastion saw %d concurrent channels, want at most 6", got)
	}
	for _, l := range leases[1:] {
		l.Release()
	}
	if st := f.pool.Stats(); st.Leases != 0 {
		t.Fatalf("leases = %d, want 0 after every release", st.Leases)
	}
}

func TestPoolSessionSlots(t *testing.T) {
	f := newPoolFixture(t, 1, func(o *PoolOptions) { o.MaxSessionsPerHost = 2 })
	defer f.close(t)

	l1 := f.lease(t, f.hosts[0])
	l2 := f.lease(t, f.hosts[0])

	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	_, err := f.pool.Client(ctx, f.hosts[0])
	timer.Stop()
	cancel()
	if err == nil {
		t.Fatal("third lease succeeded with MaxSessionsPerHost=2")
	}
	if got := errs.KindOf(err); got != errs.Canceled {
		t.Fatalf("kind = %s, want canceled (%v)", got, err)
	}
	if st := f.pool.Stats(); st.Leases != 2 {
		t.Fatalf("leases = %d, want 2", st.Leases)
	}

	// Releasing a slot lets the next lease through on the same connection.
	l1.Release()
	l3 := f.lease(t, f.hosts[0])
	l3.Release()
	l2.Release()

	if got := f.targets[0].Conns(); got != 1 {
		t.Fatalf("target connections = %d, want 1", got)
	}
}

func TestPoolBastionDeathDropsTargets(t *testing.T) {
	f := newPoolFixture(t, 2, func(o *PoolOptions) {
		o.Dial.Keepalive = 40 * time.Millisecond
	})
	defer f.close(t)

	for _, host := range f.hosts {
		f.lease(t, host).Release()
	}
	if st := f.pool.Stats(); st.Bastions != 1 || st.Targets != 2 {
		t.Fatalf("stats = %+v, want 1 bastion carrying 2 targets", st)
	}

	f.bastion.Kill()

	deadline := time.Now().Add(3 * time.Second)
	for {
		st := f.pool.Stats()
		if st.Bastions == 0 && st.Targets == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool still holds %+v after the bastion died", st)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Nothing is cached any more, so the next call redials — and finds the
	// bastion gone.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l, err := f.pool.Client(ctx, f.hosts[0])
	if err == nil {
		l.Release()
		t.Fatal("client succeeded through a killed bastion")
	}
	if got := errs.KindOf(err); got != errs.Unreachable {
		t.Fatalf("kind = %s, want unreachable (%v)", got, err)
	}
	if got := f.targets[0].Conns(); got != 1 {
		t.Fatalf("target connections = %d, want the original 1", got)
	}
}

func TestPoolIdleReap(t *testing.T) {
	f := newPoolFixture(t, 1, func(o *PoolOptions) { o.IdleTimeout = time.Minute })
	defer f.close(t)

	f.lease(t, f.hosts[0]).Release()

	// Just inside the timeout: nothing is closed.
	f.clock.Advance(30 * time.Second)
	f.pool.reap()
	if st := f.pool.Stats(); st.Targets != 1 || st.Bastions != 1 {
		t.Fatalf("stats = %+v after a reap inside the timeout", st)
	}

	// Past the timeout: the target goes, then the bastion with no targets.
	f.clock.Advance(2 * time.Minute)
	f.pool.reap()
	if st := f.pool.Stats(); st.Targets != 0 || st.Bastions != 0 || st.Leases != 0 {
		t.Fatalf("stats = %+v after the idle reap, want nothing held", st)
	}

	f.lease(t, f.hosts[0]).Release()
	if got := f.targets[0].Conns(); got != 2 {
		t.Fatalf("target connections = %d, want 2 after the redial", got)
	}
	if got := f.bastion.Conns(); got != 2 {
		t.Fatalf("bastion connections = %d, want 2 after the redial", got)
	}
}

func TestPoolCloseStopsGoroutines(t *testing.T) {
	f := newPoolFixture(t, 2, nil)
	base := runtime.NumGoroutine()

	l1 := f.lease(t, f.hosts[0])
	l2 := f.lease(t, f.hosts[1])

	if err := f.pool.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := f.pool.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if st := f.pool.Stats(); st != (Stats{}) {
		t.Fatalf("stats after close = %+v, want zero", st)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := f.pool.Client(ctx, f.hosts[0]); err == nil {
		t.Fatal("client after close succeeded")
	}

	select {
	case <-f.pool.reaperDone:
	default:
		t.Fatal("reaper goroutine still running after close")
	}

	// The leases are dead but releasing them must not block or panic.
	l1.Release()
	l2.Release()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if runtime.NumGoroutine() <= base+2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines = %d after close, baseline %d", runtime.NumGoroutine(), base)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
