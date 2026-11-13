package salt

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"jumper/internal/backend"
	"jumper/internal/errs"
	"jumper/internal/inventory"
	"jumper/internal/resource"
)

// backendName is used in every Ref and in error Ops.
const backendName = "salt"

const (
	// healthTTL is how long a Health answer is reused. manage.status pings
	// every minion, so it must not run more often than this.
	healthTTL = 30 * time.Second
	// healthTimeout bounds the manage.status call Health makes.
	healthTimeout = 5 * time.Second
	// keyTTL is how long the accepted-key set used for identity mapping lives.
	keyTTL = 60 * time.Second
)

// Options configures a Backend. Runner and Inventory are what make it useful;
// Master is the inventory host of the Salt master and empty means Salt is
// unconfigured.
type Options struct {
	Runner    *Runner
	Inventory inventory.Inventory
	Master    resource.HostID
	Now       func() time.Time
	Log       *slog.Logger
}

// healthCall is one in-flight manage.status call that other Health callers can
// join instead of starting their own.
type healthCall struct {
	done chan struct{}
	h    backend.Health
}

// Backend implements backend.Backend for Salt. Everything it does is
// read-only: it runs salt, salt-run and salt-key on the master over the
// runner.
type Backend struct {
	runner *Runner
	inv    inventory.Inventory
	master resource.HostID
	now    func() time.Time
	log    *slog.Logger

	mu       sync.Mutex
	health   backend.Health
	healthAt time.Time
	healthOK bool
	inFlight *healthCall

	accepted   map[string]bool
	acceptedAt time.Time
	acceptedOK bool
}

var _ backend.Backend = (*Backend)(nil)

// New builds a Salt backend from o. A nil Now uses the wall clock; a nil Log
// discards.
func New(o Options) *Backend {
	b := &Backend{
		runner: o.Runner,
		inv:    o.Inventory,
		master: resource.NormalizeHostID(string(o.Master)),
		now:    o.Now,
		log:    o.Log,
	}
	if b.now == nil {
		b.now = time.Now
	}
	if b.log == nil {
		b.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return b
}

// Name returns the backend name used in Refs and registry lookups.
func (b *Backend) Name() string { return backendName }

// Kinds returns the read-only Salt kinds.
func (b *Backend) Kinds() []resource.KindSpec { return kindSpecs() }

// configured reports whether the backend has a master and a way to reach it.
func (b *Backend) configured() bool { return b.master != "" && b.runner != nil }

// List streams one Result holding every row the kind has. Both kinds are
// global, so there is no per-host batch to stream.
func (b *Backend) List(ctx context.Context, q resource.Query) (<-chan backend.Result, error) {
	kind, ok := normalizeKind(q.Kind)
	if !ok {
		return nil, errs.New(errs.Config, "salt.List", fmt.Sprintf("unknown kind %q", q.Kind))
	}
	if !b.configured() {
		return nil, errs.New(errs.Config, "salt.List", "salt backend not configured: no master or no runner")
	}
	out := make(chan backend.Result, 1)
	go func() {
		defer close(out)
		var (
			items []resource.Resource
			err   error
		)
		switch kind {
		case KindMinion:
			items, err = b.minionRows(ctx)
		case KindKey:
			items, err = b.keyRows(ctx)
		case KindJobs:
			items, err = b.jobRows(ctx, q)
		}
		res := backend.Result{Items: items, Done: 1, Total: 1}
		if err != nil {
			res.Items, res.Err = nil, err
		}
		select {
		case <-ctx.Done():
		case out <- res:
		}
	}()
	return out, nil
}

// Get lists the ref's kind and returns the named row.
func (b *Backend) Get(ctx context.Context, ref resource.Ref) (resource.Resource, error) {
	kind, ok := normalizeKind(ref.Kind)
	if !ok {
		return resource.Resource{}, errs.New(errs.Config, "salt.Get", fmt.Sprintf("unknown kind %q", ref.Kind))
	}
	if strings.TrimSpace(ref.Name) == "" {
		return resource.Resource{}, errs.New(errs.Config, "salt.Get", "ref needs a name")
	}
	ch, err := b.List(ctx, resource.Query{Kind: kind})
	if err != nil {
		return resource.Resource{}, err
	}
	var batchErr error
	for res := range ch {
		if res.Err != nil {
			batchErr = res.Err
			continue
		}
		for _, row := range res.Items {
			if row.Ref.Name == ref.Name {
				return row, nil
			}
		}
	}
	if batchErr != nil {
		return resource.Resource{}, batchErr
	}
	return resource.Resource{}, errs.New(errs.NotAvailable, "salt.Get", "no resource "+ref.String())
}

// Links returns the pivots from r: the host overview when the minion ID maps
// to an inventory host and the jobs of that minion, or the job detail of a
// job row.
func (b *Backend) Links(r resource.Resource) []resource.Link {
	kind, known := normalizeKind(r.Ref.Kind)
	if !known {
		return nil
	}
	if kind == KindJobs {
		return b.jobLinks(r)
	}
	if kind != KindMinion {
		return nil
	}
	id := strings.TrimSpace(r.Ref.Name)
	if id == "" {
		return nil
	}
	var out []resource.Link
	if host, ok := b.MinionHost(id); ok {
		out = append(out, resource.Link{
			Label: "host",
			Key:   "enter",
			To:    resource.Query{Kind: "host", Hosts: []resource.HostID{host}},
		})
	}
	out = append(out, resource.Link{
		Label: "jobs",
		Key:   "J",
		To:    resource.Query{Kind: "salt-job", Args: map[string]string{"minion": id}},
	})
	return out
}

// Health reports the last manage.status result, refreshing it at most every
// healthTTL with a healthTimeout deadline. Concurrent callers share one call.
func (b *Backend) Health(ctx context.Context) backend.Health {
	if b.master == "" {
		return backend.Health{
			State:   backend.HealthUnconfigured,
			Detail:  "salt master not configured",
			Checked: b.now(),
		}
	}
	b.mu.Lock()
	if b.healthOK && b.fresh(b.healthAt, healthTTL) {
		h := b.health
		b.mu.Unlock()
		return h
	}
	if call := b.inFlight; call != nil {
		b.mu.Unlock()
		select {
		case <-call.done:
			return call.h
		case <-ctx.Done():
			return backend.Health{
				State:   backend.HealthDown,
				Detail:  string(errs.KindOf(ctx.Err())),
				Checked: b.now(),
			}
		}
	}
	call := &healthCall{done: make(chan struct{})}
	b.inFlight = call
	b.mu.Unlock()

	h := b.fetchHealth(ctx)

	b.mu.Lock()
	b.health = h
	b.healthAt = h.Checked
	b.healthOK = true
	b.inFlight = nil
	b.mu.Unlock()
	call.h = h
	close(call.done)
	return h
}

// fetchHealth runs one bounded manage.status call and grades the answer.
func (b *Backend) fetchHealth(ctx context.Context) backend.Health {
	h := backend.Health{Checked: b.now()}
	if b.runner == nil {
		h.State = backend.HealthDown
		h.Detail = string(errs.Internal)
		return h
	}
	cctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	out, err := b.runner.Run(cctx, Call{Tool: toolRun, Fun: "manage.status", Timeout: healthTimeout})
	if err != nil {
		h.State = backend.HealthDown
		h.Detail = string(errs.KindOf(err))
		return h
	}
	st, err := ParseManageStatus(out)
	if err != nil {
		h.State = backend.HealthDown
		h.Detail = string(errs.KindOf(err))
		return h
	}
	up, total := len(st.Up), len(st.Up)+len(st.Down)
	switch {
	case total == 0:
		h.State = backend.HealthOK
		h.Detail = "no minions"
	case len(st.Down) == 0:
		h.State = backend.HealthOK
		h.Detail = fmt.Sprintf("%d/%d minions up", up, total)
	default:
		h.State = backend.HealthDegraded
		h.Detail = fmt.Sprintf("%d/%d up", up, total)
	}
	return h
}

// fresh reports whether t is less than ttl old according to b.now.
func (b *Backend) fresh(t time.Time, ttl time.Duration) bool {
	d := b.now().Sub(t)
	return d >= 0 && d < ttl
}

// MinionHost maps a minion ID onto the inventory host it belongs to. Minion IDs
// are not hostnames, so this always goes through Resolve.
func (b *Backend) MinionHost(id string) (resource.HostID, bool) {
	id = strings.TrimSpace(id)
	if b.inv == nil || id == "" {
		return "", false
	}
	h, ok := b.inv.Resolve(id)
	if !ok {
		return "", false
	}
	return h.ID, true
}

// HostMinion maps a host onto its minion ID: the salt.id label when present,
// else an alias that is an accepted key, else the host's short name when that
// is an accepted key. The accepted set is cached for keyTTL.
func (b *Backend) HostMinion(host resource.HostID) (string, bool) {
	if b.inv == nil || strings.TrimSpace(string(host)) == "" {
		return "", false
	}
	h, ok := b.inv.Resolve(string(host))
	if !ok {
		return "", false
	}
	if id := strings.TrimSpace(h.Labels["salt.id"]); id != "" {
		return id, true
	}
	accepted, err := b.acceptedKeys(context.Background())
	if err != nil {
		return "", false
	}
	for _, alias := range h.Aliases {
		if accepted[alias] {
			return alias, true
		}
	}
	if short := shortName(string(h.ID)); short != "" && accepted[short] {
		return short, true
	}
	return "", false
}

// acceptedKeys returns the set of accepted minion IDs, cached for keyTTL.
func (b *Backend) acceptedKeys(ctx context.Context) (map[string]bool, error) {
	b.mu.Lock()
	if b.acceptedOK && b.fresh(b.acceptedAt, keyTTL) {
		set := b.accepted
		b.mu.Unlock()
		return set, nil
	}
	b.mu.Unlock()

	if b.runner == nil {
		return nil, errs.New(errs.Config, "salt.keys", "salt backend not configured: no runner")
	}
	out, err := b.runner.Run(ctx, Call{Tool: toolKey, Flags: []string{"-L"}})
	if err != nil {
		return nil, err
	}
	keys, err := ParseKeys(out)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		if k.State == KeyAccepted {
			set[k.ID] = true
		}
	}
	b.mu.Lock()
	b.accepted = set
	b.acceptedAt = b.now()
	b.acceptedOK = true
	b.mu.Unlock()
	return set, nil
}

// shortName is the part of a host ID before the first dot.
func shortName(id string) string {
	if i := strings.IndexByte(id, '.'); i >= 0 {
		return id[:i]
	}
	return id
}
