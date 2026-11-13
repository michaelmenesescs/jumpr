package ssh

import (
	"context"
	"errors"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/singleflight"

	"jumper/internal/errs"
	"jumper/internal/resource"
)

const (
	// defaultMaxConnsPerBastion is the ssh.max_conns_per_bastion default
	// (BUILD-PLAN §8, §11).
	defaultMaxConnsPerBastion = 4
	// defaultMaxChannelsPerConn is the ssh.max_channels_per_conn default.
	defaultMaxChannelsPerConn = 8
	// defaultMaxSessionsPerHost is the per-target session limit (BUILD-PLAN §8).
	defaultMaxSessionsPerHost = 4
	// defaultIdleTimeout is the ssh.idle_timeout default.
	defaultIdleTimeout = 5 * time.Minute
	// reapInterval is how often the idle reaper runs.
	reapInterval = 30 * time.Second
)

// PoolOptions configures a Pool. Zero limits and idle timeouts take their
// defaults: 4 connections per bastion, 8 channels per connection, 4 sessions
// per host, 5 minutes.
type PoolOptions struct {
	// Resolver supplies the jump chain for every host. Required.
	Resolver *Resolver
	// Dial configures every connection the pool opens.
	Dial DialOptions
	// MaxConnsPerBastion caps the connections held to one first-hop bastion.
	MaxConnsPerBastion int
	// MaxChannelsPerConn caps the target connections tunnelled through one
	// bastion connection.
	MaxChannelsPerConn int
	// MaxSessionsPerHost caps the concurrent leases of one target connection.
	MaxSessionsPerHost int
	// IdleTimeout is how long a target connection without a lease is kept.
	IdleTimeout time.Duration
	// Now is the clock, injected for tests. Nil means time.Now.
	Now func() time.Time
}

// Stats is a snapshot of what the pool holds, for :health and tests.
type Stats struct {
	Bastions int // open first-hop bastion connections
	Targets  int // pooled target connections (direct hosts included)
	Leases   int // leases currently held by callers
}

// Lease is a borrowed connection with one of its host's session slots held.
// Release frees the slot; the connection itself stays pooled.
type Lease struct {
	p      *Pool
	entry  *hostEntry
	client *ssh.Client
	via    string
	once   sync.Once
}

// Client returns the connection. It is nil after Release.
func (l *Lease) Client() *ssh.Client {
	if l == nil {
		return nil
	}
	return l.client
}

// Via names the first-hop bastion that carries the connection, or "" for a
// direct connection.
func (l *Lease) Via() string {
	if l == nil {
		return ""
	}
	return l.via
}

// Release frees the lease's session slot. It is safe to call more than once,
// and after the pool dropped the connection the lease was taken on.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.client = nil
		l.p.release(l.entry)
	})
}

// Pool keeps SSH connections alive and shares them: one connection per target
// host, and a capped, multiplexed set of connections per first-hop bastion.
// All methods are safe for concurrent use.
type Pool struct {
	opts PoolOptions

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	dials singleflight.Group

	mu         sync.Mutex
	closed     bool
	hosts      map[resource.HostID]*hostEntry
	bastions   map[string]*bastionSet
	reaperDone chan struct{}

	wg        sync.WaitGroup
	closeOnce sync.Once
}

// hostEntry is one pooled target connection. It survives redials so the
// session semaphore stays stable; client is nil while the host is down.
// Every field is guarded by Pool.mu.
type hostEntry struct {
	host    resource.HostID
	sess    chan struct{} // session slots; cap = MaxSessionsPerHost
	client  *ssh.Client
	via     *bastionConn  // nil for a direct dial
	hops    []*ssh.Client // jump hosts between the bastion and the target
	viaName string        // bastion name for Lease.Via()
	leases  int
	lastUse time.Time
}

// bastionSet is the connection set of one first-hop bastion.
// Every field is guarded by Pool.mu.
type bastionSet struct {
	alias   string
	spec    HostSpec
	conns   []*bastionConn
	dialing int           // connections being dialled right now
	notify  chan struct{} // closed and replaced whenever capacity changes
}

// bastionConn is one connection to a bastion and the number of target
// connections tunnelled through it. Every field is guarded by Pool.mu.
type bastionConn struct {
	alias   string
	set     *bastionSet
	client  *ssh.Client
	targets int
}

// NewPool returns a pool and starts its idle reaper. Call Close when done.
func NewPool(o PoolOptions) *Pool {
	if o.MaxConnsPerBastion <= 0 {
		o.MaxConnsPerBastion = defaultMaxConnsPerBastion
	}
	if o.MaxChannelsPerConn <= 0 {
		o.MaxChannelsPerConn = defaultMaxChannelsPerConn
	}
	if o.MaxSessionsPerHost <= 0 {
		o.MaxSessionsPerHost = defaultMaxSessionsPerHost
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = defaultIdleTimeout
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		opts:       o,
		ctx:        ctx,
		cancel:     cancel,
		done:       make(chan struct{}),
		hosts:      make(map[resource.HostID]*hostEntry),
		bastions:   make(map[string]*bastionSet),
		reaperDone: make(chan struct{}),
	}
	p.wg.Add(1)
	go p.reapLoop()
	return p
}

// Close closes every connection and stops the background goroutines. It is
// safe to call more than once.
func (p *Pool) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		var clients []*ssh.Client
		for _, e := range p.hosts {
			abandonLocked(e)
			if e.client == nil {
				continue
			}
			clients = append(clients, e.client)
			clients = append(clients, e.hops...)
			e.client, e.hops, e.via = nil, nil, nil
		}
		for _, s := range p.bastions {
			for _, c := range s.conns {
				clients = append(clients, c.client)
			}
			s.conns = nil
		}
		p.mu.Unlock()

		close(p.done)
		p.cancel()
		for _, c := range clients {
			_ = c.Close()
		}
		p.wg.Wait()
	})
	return nil
}

// Stats returns what the pool currently holds.
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := Stats{}
	for _, e := range p.hosts {
		if e.client != nil {
			st.Targets++
		}
		st.Leases += e.leases
	}
	for _, s := range p.bastions {
		st.Bastions += len(s.conns)
	}
	return st
}

// Client returns a ready connection to host, dialling it (and any bastions the
// ssh_config ProxyJump chain needs) when it is not pooled yet. The lease holds
// one of the host's session slots until Release is called.
func (p *Pool) Client(ctx context.Context, host resource.HostID) (*Lease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	e, err := p.acquireSession(ctx, host)
	if err != nil {
		return nil, err
	}
	client, via, err := p.conn(ctx, host)
	if err != nil {
		p.release(e)
		return nil, err
	}
	return &Lease{p: p, entry: e, client: client, via: via}, nil
}

// acquireSession takes one session slot for host, waiting (ctx-aware) for a
// free one, and counts the lease.
func (p *Pool) acquireSession(ctx context.Context, host resource.HostID) (*hostEntry, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errPoolClosed(host)
	}
	e := p.entryLocked(host)
	p.mu.Unlock()

	select {
	case e.sess <- struct{}{}:
	case <-ctx.Done():
		return nil, classifyPoolWait(ctx.Err(), string(host))
	case <-p.done:
		return nil, errPoolClosed(host)
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		<-e.sess
		return nil, errPoolClosed(host)
	}
	e.leases++
	e.lastUse = p.now()
	p.mu.Unlock()
	return e, nil
}

// release frees one session slot and marks the host as just used.
func (p *Pool) release(e *hostEntry) {
	if e == nil {
		return
	}
	p.mu.Lock()
	if e.leases > 0 {
		e.leases--
		<-e.sess
	}
	e.lastUse = p.now()
	p.mu.Unlock()
}

// entryLocked returns host's entry, creating it. The caller holds p.mu.
func (p *Pool) entryLocked(host resource.HostID) *hostEntry {
	e := p.hosts[host]
	if e == nil {
		e = &hostEntry{host: host, sess: make(chan struct{}, p.opts.MaxSessionsPerHost)}
		p.hosts[host] = e
	}
	return e
}

// conn returns the pooled connection for host, sharing one dial between
// concurrent callers. A failed dial is not cached.
func (p *Pool) conn(ctx context.Context, host resource.HostID) (*ssh.Client, string, error) {
	ch := p.dials.DoChan("host:"+string(host), func() (any, error) {
		return p.dial(ctx, host)
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, "", res.Err
		}
		e, ok := res.Val.(*hostEntry)
		if !ok || e == nil {
			return nil, "", errs.New(errs.Internal, "ssh.pool", "dial finished without a connection").WithHost(string(host))
		}
		return e.client, e.viaName, nil
	case <-ctx.Done():
		return nil, "", classifyPoolWait(ctx.Err(), string(host))
	case <-p.done:
		return nil, "", errPoolClosed(host)
	}
}

// dial opens the chain ending at host and installs the final client. It never
// holds the pool mutex across a dial.
func (p *Pool) dial(ctx context.Context, host resource.HostID) (*hostEntry, error) {
	if p.opts.Resolver == nil {
		return nil, errs.New(errs.Config, "ssh.pool", "no ssh_config resolver").WithHost(string(host))
	}
	chain, err := p.opts.Resolver.Chain(string(host))
	if err != nil {
		return nil, err
	}
	if len(chain) == 0 {
		return nil, errs.New(errs.Config, "ssh.pool", "empty jump chain").WithHost(string(host))
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errPoolClosed(host)
	}
	if e := p.hosts[host]; e != nil && e.client != nil {
		// A previous dial already installed a connection for this host.
		p.mu.Unlock()
		return e, nil
	}
	p.mu.Unlock()

	if len(chain) == 1 {
		client, derr := DialDirect(ctx, chain[0], p.opts.Dial)
		if derr != nil {
			return nil, derr
		}
		return p.install(host, client, nil, nil, "")
	}

	// Every host whose chain starts at this bastion shares one bastion set.
	first := chain[0]
	bc, err := p.bastion(ctx, first)
	if err != nil {
		return nil, err
	}

	var hops []*ssh.Client
	fail := func(err error) (*hostEntry, error) {
		for i := len(hops) - 1; i >= 0; i-- {
			_ = hops[i].Close()
		}
		p.releaseBastion(bc)
		return nil, err
	}

	prev := bc.client
	for _, spec := range chain[1 : len(chain)-1] {
		hop, derr := DialVia(ctx, prev, spec, p.opts.Dial)
		if derr != nil {
			return fail(derr)
		}
		hops = append(hops, hop)
		prev = hop
	}
	client, err := DialVia(ctx, prev, chain[len(chain)-1], p.opts.Dial)
	if err != nil {
		return fail(err)
	}
	return p.install(host, client, bc, hops, specHost(first))
}

// install publishes a freshly dialled connection as host's entry and starts its
// health watchers. A connection another dial installed first wins; the fresh
// one is then closed again.
func (p *Pool) install(host resource.HostID, client *ssh.Client, bc *bastionConn, hops []*ssh.Client, viaName string) (*hostEntry, error) {
	closeAll := func() {
		_ = client.Close()
		for i := len(hops) - 1; i >= 0; i-- {
			_ = hops[i].Close()
		}
		if bc != nil {
			p.releaseBastion(bc)
		}
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		closeAll()
		return nil, errPoolClosed(host)
	}
	if e := p.hosts[host]; e != nil && e.client != nil {
		p.mu.Unlock()
		closeAll()
		return e, nil
	}
	e := p.entryLocked(host)
	e.client = client
	e.via = bc
	e.hops = hops
	e.viaName = viaName
	e.lastUse = p.now()
	p.goLocked(func() {
		_ = client.Wait()
		p.hostDied(host, e)
	})
	p.goLocked(func() { p.keepaliveLoop(client) })
	p.mu.Unlock()
	return e, nil
}

// bastion returns a bastion connection for the first hop that has a free
// channel slot, dialling a new one when every existing one is full (up to
// MaxConnsPerBastion) and waiting, ctx-aware, when the cap is reached.
func (p *Pool) bastion(ctx context.Context, spec HostSpec) (*bastionConn, error) {
	alias := specHost(spec)
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errPoolClosed(resource.HostID(alias))
		}
		s := p.bastions[alias]
		if s == nil {
			s = &bastionSet{alias: alias, spec: spec, notify: make(chan struct{})}
			p.bastions[alias] = s
		}
		var best *bastionConn
		for _, c := range s.conns {
			if c.targets >= p.opts.MaxChannelsPerConn {
				continue
			}
			if best == nil || c.targets < best.targets {
				best = c
			}
		}
		if best != nil {
			best.targets++
			p.mu.Unlock()
			return best, nil
		}
		if len(s.conns)+s.dialing >= p.opts.MaxConnsPerBastion {
			wait := s.notify
			p.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, classifyPoolWait(ctx.Err(), alias)
			case <-p.done:
				return nil, errPoolClosed(resource.HostID(alias))
			}
		}
		s.dialing++
		p.mu.Unlock()

		client, err := DialDirect(ctx, spec, p.opts.Dial)

		p.mu.Lock()
		s.dialing--
		switch {
		case err != nil:
			p.broadcastLocked(s)
			p.mu.Unlock()
			return nil, err
		case p.closed:
			p.broadcastLocked(s)
			p.mu.Unlock()
			_ = client.Close()
			return nil, errPoolClosed(resource.HostID(alias))
		case len(s.conns) >= p.opts.MaxConnsPerBastion:
			// Another dial filled the cap while this one ran.
			p.broadcastLocked(s)
			p.mu.Unlock()
			_ = client.Close()
			continue
		}
		c := &bastionConn{alias: alias, set: s, client: client, targets: 1}
		s.conns = append(s.conns, c)
		p.broadcastLocked(s)
		p.goLocked(func() {
			_ = client.Wait()
			p.bastionDied(alias, s, c)
		})
		p.goLocked(func() { p.keepaliveLoop(client) })
		p.mu.Unlock()
		return c, nil
	}
}

// hostDied removes a target connection whose client is gone.
func (p *Pool) hostDied(host resource.HostID, e *hostEntry) {
	p.mu.Lock()
	if p.hosts[host] != e || e.client == nil {
		p.mu.Unlock()
		return
	}
	client, hops, bc := e.client, e.hops, e.via
	abandonLocked(e)
	e.client, e.hops, e.via, e.viaName = nil, nil, nil, ""
	p.mu.Unlock()

	_ = client.Close()
	for i := len(hops) - 1; i >= 0; i-- {
		_ = hops[i].Close()
	}
	p.releaseBastion(bc)
}

// bastionDied removes a dead bastion connection together with every target
// connection that rode on it.
func (p *Pool) bastionDied(alias string, s *bastionSet, c *bastionConn) {
	p.mu.Lock()
	idx := -1
	for i, x := range s.conns {
		if x == c {
			idx = i
			break
		}
	}
	if idx >= 0 {
		s.conns = append(s.conns[:idx], s.conns[idx+1:]...)
	}
	if len(s.conns) == 0 && s.dialing == 0 && p.bastions[alias] == s {
		delete(p.bastions, alias)
	}
	p.broadcastLocked(s)
	var doomed []*hostEntry
	for host, e := range p.hosts {
		if e.via != c {
			continue
		}
		abandonLocked(e)
		doomed = append(doomed, e)
		delete(p.hosts, host)
	}
	p.mu.Unlock()

	for _, e := range doomed {
		e.closeClients()
	}
	_ = c.client.Close()
}

// Forget drops host's pooled connection, freeing its bastion channel. The next
// Client call dials again.
func (p *Pool) Forget(host resource.HostID) {
	p.mu.Lock()
	e := p.hosts[host]
	if e == nil || e.client == nil {
		p.mu.Unlock()
		return
	}
	client, hops, bc := e.client, e.hops, e.via
	abandonLocked(e)
	e.client, e.hops, e.via, e.viaName = nil, nil, nil, ""
	p.mu.Unlock()

	_ = client.Close()
	for i := len(hops) - 1; i >= 0; i-- {
		_ = hops[i].Close()
	}
	p.releaseBastion(bc)
}

// releaseBastion frees one target slot on bc and wakes anyone waiting for one.
func (p *Pool) releaseBastion(bc *bastionConn) {
	if bc == nil {
		return
	}
	p.mu.Lock()
	if bc.targets > 0 {
		bc.targets--
	}
	if bc.set != nil {
		p.broadcastLocked(bc.set)
	}
	p.mu.Unlock()
}

// broadcastLocked wakes everyone waiting on s. The caller holds p.mu.
func (p *Pool) broadcastLocked(s *bastionSet) {
	if s == nil {
		return
	}
	close(s.notify)
	s.notify = make(chan struct{})
}

// reap closes target connections idle past IdleTimeout and then bastion
// connections left without targets. The reaper calls it every 30 s; tests call
// it directly with a fake clock.
func (p *Pool) reap() {
	now := p.now()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	var doomed []*hostEntry
	for host, e := range p.hosts {
		if e.client == nil || e.leases > 0 {
			continue
		}
		if now.Sub(e.lastUse) <= p.opts.IdleTimeout {
			continue
		}
		delete(p.hosts, host)
		doomed = append(doomed, e)
	}
	// Closing a target frees its bastion slot, so the bastion sweep below
	// sees the true target count.
	for _, e := range doomed {
		if e.via != nil && e.via.targets > 0 {
			e.via.targets--
		}
	}
	var clients []*ssh.Client
	for alias, s := range p.bastions {
		keep := s.conns[:0]
		for _, c := range s.conns {
			if c.targets == 0 && s.dialing == 0 {
				clients = append(clients, c.client)
				continue
			}
			keep = append(keep, c)
		}
		s.conns = keep
		if len(s.conns) == 0 && s.dialing == 0 {
			delete(p.bastions, alias)
		}
		p.broadcastLocked(s)
	}
	p.mu.Unlock()

	for _, e := range doomed {
		e.closeClients()
	}
	for _, c := range clients {
		_ = c.Close()
	}
}

// reapLoop runs reap every reapInterval until the pool closes.
func (p *Pool) reapLoop() {
	defer p.wg.Done()
	defer close(p.reaperDone)
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-t.C:
			p.reap()
		}
	}
}

// keepaliveLoop probes client until the pool closes or the peer dies, then
// closes the connection so the connection's watcher drops it.
func (p *Pool) keepaliveLoop(client *ssh.Client) {
	every := p.opts.Dial.Keepalive
	if every <= 0 {
		every = defaultKeepalive
	}
	t := time.NewTicker(every)
	defer t.Stop()
	failures := 0
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-t.C:
		}
		err := keepaliveOnce(p.ctx, client)
		if err == nil {
			failures = 0
			continue
		}
		if errors.Is(err, context.Canceled) {
			return
		}
		failures++
		if failures >= keepaliveFailures || errors.Is(err, errKeepaliveTimeout) {
			_ = client.Close()
			return
		}
	}
}

// goLocked starts fn in a goroutine tracked by the pool's WaitGroup. The caller
// holds p.mu so Close cannot begin waiting between the closed check and the
// Add.
func (p *Pool) goLocked(fn func()) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		fn()
	}()
}

// closeClients closes a target connection and the hops that carried it.
func (e *hostEntry) closeClients() {
	if e.client != nil {
		_ = e.client.Close()
	}
	for i := len(e.hops) - 1; i >= 0; i-- {
		_ = e.hops[i].Close()
	}
}

// abandonLocked drops e's outstanding leases: its connection is gone, so its
// session slots are free again and a late Release is a no-op. The caller holds
// p.mu.
func abandonLocked(e *hostEntry) {
	for e.leases > 0 {
		e.leases--
		<-e.sess
	}
}

func (p *Pool) now() time.Time {
	if p.opts.Now == nil {
		return time.Now()
	}
	return p.opts.Now()
}

// errPoolClosed is what every call returns once Close has run.
func errPoolClosed(host resource.HostID) error {
	e := errs.New(errs.Canceled, "ssh.pool", "connection pool is closed")
	if host != "" {
		e = e.WithHost(string(host))
	}
	return e
}

// classifyPoolWait turns a context error from a pool wait into the right kind.
func classifyPoolWait(err error, host string) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errs.Wrap(errs.Timeout, "ssh.pool", err).WithHost(host)
	}
	return errs.Wrap(errs.Canceled, "ssh.pool", err).WithHost(host)
}
