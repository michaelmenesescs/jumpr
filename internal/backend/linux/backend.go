// Package linux is the Linux (SSH) backend. It serves the host, unit, process
// and socket kinds by running one probe batch per host over any
// transport.Executor; a batch is one round trip and its sections are split and
// parsed locally. Everything it sends is read-only and uses only commands that
// already exist on the node.
package linux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"jumper/internal/backend"
	"jumper/internal/errs"
	"jumper/internal/fanout"
	"jumper/internal/inventory"
	"jumper/internal/probe"
	"jumper/internal/resource"
	"jumper/internal/transport"
)

// backendName is used in every Ref and in error Ops.
const backendName = "linux"

// defaultTimeout bounds one host's batch. BUILD-PLAN §8: exec 30 s.
const defaultTimeout = 30 * time.Second

// healthSamples is how many recent host results Health looks at.
const healthSamples = 20

// Options configures a Backend. Exec and Inventory are required in production;
// Stream is only needed for journal follow. The rest fall back to safe defaults
// (30 s, a random nonce, the wall clock).
type Options struct {
	Exec        transport.Executor
	Stream      transport.Streamer
	Inventory   inventory.Inventory
	Concurrency int
	Timeout     time.Duration
	Nonce       func() string
	Now         func() time.Time
	Log         *slog.Logger
}

// Backend implements backend.Backend for Linux hosts reached over SSH.
type Backend struct {
	exec        transport.Executor
	stream      transport.Streamer
	inv         inventory.Inventory
	concurrency int
	timeout     time.Duration
	nonce       func() string
	now         func() time.Time
	log         *slog.Logger

	mu      sync.Mutex
	samples []bool // true = transport error; last healthSamples entries
}

var _ backend.Backend = (*Backend)(nil)

// New builds a Linux backend from o. A nil Log discards; a nil Now uses the
// wall clock; a nil Nonce uses crypto/rand.
func New(o Options) *Backend {
	b := &Backend{
		exec:        o.Exec,
		stream:      o.Stream,
		inv:         o.Inventory,
		concurrency: o.Concurrency,
		timeout:     o.Timeout,
		nonce:       o.Nonce,
		now:         o.Now,
		log:         o.Log,
	}
	if b.timeout <= 0 {
		b.timeout = defaultTimeout
	}
	if b.now == nil {
		b.now = time.Now
	}
	if b.nonce == nil {
		b.nonce = randomNonce
	}
	if b.log == nil {
		b.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return b
}

// Name returns the backend name used in Refs and registry lookups.
func (b *Backend) Name() string { return backendName }

// Kinds returns the four core Linux kinds.
func (b *Backend) Kinds() []resource.KindSpec { return kindSpecs() }

// timeoutDuration is the per-host exec timeout.
func (b *Backend) timeoutDuration() time.Duration {
	if b.timeout <= 0 {
		return defaultTimeout
	}
	return b.timeout
}

// newNonce mints one batch nonce.
func (b *Backend) newNonce() string {
	if b.nonce == nil {
		return randomNonce()
	}
	return b.nonce()
}

// batch compiles the kind's probe batch under a fresh nonce.
func (b *Backend) batch(kind resource.KindName) (probe.Batch, error) {
	return newBatch(b.newNonce(), kind)
}

// randomNonce returns 8 hex characters, as probe.NewBatch requires.
func randomNonce() string {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(raw[:])
}

// List streams one Result per host. Hosts come from Query.Hosts, then
// Args["host"], then the inventory selector. Section-level failures degrade the
// affected cells on the host kind and fail the host on single-section kinds.
func (b *Backend) List(ctx context.Context, q resource.Query) (<-chan backend.Result, error) {
	kind, ok := normalizeKind(q.Kind)
	if !ok {
		return nil, errs.New(errs.Config, "linux.List", fmt.Sprintf("unknown kind %q", q.Kind))
	}
	hosts, err := b.hosts(q)
	if err != nil {
		return nil, err
	}
	if b.exec == nil {
		return nil, errs.New(errs.Config, "linux.List", "no transport executor configured")
	}
	if kind == KindJournal {
		// journalctl is not a probe batch: one command per host, raw output.
		out := make(chan backend.Result)
		go b.listJournal(ctx, out, hosts, q)
		return out, nil
	}
	batch, err := b.batch(kind)
	if err != nil {
		return nil, err
	}

	out := make(chan backend.Result)
	go func() {
		defer close(out)
		total := len(hosts)
		done := 0
		results := fanout.Run(ctx, b.exec, hosts, batch.Command(b.timeoutDuration()), fanout.Options{Concurrency: b.concurrency})
		for hr := range results {
			done++
			res := backend.Result{Host: hr.Host, Done: done, Total: total}
			if hr.Err != nil {
				b.record(true)
				res.Err = hr.Err
			} else {
				b.record(false)
				sec, bad, retried := b.collect(ctx, kind, hr.Host, batch, hr.Result)
				items, rerr := b.rows(kind, hr.Host, q, sec, bad, retried)
				if rerr != nil {
					res.Err = rerr
				} else {
					res.Items = items
				}
			}
			select {
			case <-ctx.Done():
				return
			case out <- res:
			}
		}
	}()
	return out, nil
}

// hosts resolves the target list for q. Explicit hosts win over Args["host"],
// which wins over the inventory selector.
func (b *Backend) hosts(q resource.Query) ([]resource.HostID, error) {
	if len(q.Hosts) > 0 {
		return dedupeHosts(q.Hosts), nil
	}
	if raw := strings.TrimSpace(q.Args["host"]); raw != "" {
		names := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
		out := make([]resource.HostID, 0, len(names))
		for _, name := range names {
			out = append(out, resource.NormalizeHostID(name))
		}
		if len(out) > 0 {
			return dedupeHosts(out), nil
		}
	}
	if b.inv != nil {
		found := b.inv.Select(q.Selector)
		out := make([]resource.HostID, 0, len(found))
		for _, h := range found {
			out = append(out, h.ID)
		}
		if len(out) > 0 {
			return dedupeHosts(out), nil
		}
	}
	return nil, errs.New(errs.Config, "linux.List", "no hosts match")
}

// dedupeHosts normalizes and de-duplicates hosts, keeping first occurrence.
func dedupeHosts(in []resource.HostID) []resource.HostID {
	seen := make(map[resource.HostID]bool, len(in))
	out := make([]resource.HostID, 0, len(in))
	for _, h := range in {
		id := resource.NormalizeHostID(string(h))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// Get runs the kind's batch for ref.Host and returns the matching row.
func (b *Backend) Get(ctx context.Context, ref resource.Ref) (resource.Resource, error) {
	kind, ok := normalizeKind(ref.Kind)
	if !ok {
		return resource.Resource{}, errs.New(errs.Config, "linux.Get", fmt.Sprintf("unknown kind %q", ref.Kind))
	}
	host := ref.Host
	if host == "" {
		host = resource.NormalizeHostID(ref.Name)
	}
	host = resource.NormalizeHostID(string(host))
	if host == "" {
		return resource.Resource{}, errs.New(errs.Config, "linux.Get", "ref needs a host")
	}
	if b.exec == nil {
		return resource.Resource{}, errs.New(errs.Config, "linux.Get", "no transport executor configured")
	}
	batch, err := b.batch(kind)
	if err != nil {
		return resource.Resource{}, err
	}

	cctx, cancel := context.WithTimeout(ctx, b.timeoutDuration())
	defer cancel()
	res, err := b.exec.Exec(cctx, host, batch.Command(b.timeoutDuration()))
	if err != nil {
		return resource.Resource{}, err
	}
	sec, bad, retried := b.collect(cctx, kind, host, batch, res)
	items, err := b.rows(kind, host, resource.Query{Kind: kind}, sec, bad, retried)
	if err != nil {
		return resource.Resource{}, err
	}
	want := ref.Name
	if kind == KindHost && want == "" {
		want = string(host)
	}
	for _, row := range items {
		if row.Ref.Name == want {
			return row, nil
		}
	}
	return resource.Resource{}, errs.New(errs.NotAvailable, "linux.Get", "no resource "+ref.String())
}

// HostFacts is the typed detail of a host row that the flat Fields map does not
// carry: full load averages, the pressure lines, the memory breakdown, the
// processor count and the failed units. Have* is false when that probe section
// failed to parse; the row then has no value for it.
//
// It is the exported face of the host row's Raw payload, so the host overview
// view (another package) can render it without reaching into an unexported
// type.
type HostFacts struct {
	Load        Load
	HaveLoad    bool
	Mem         Mem
	HaveMem     bool
	PSI         PSI
	HavePSI     bool
	NProc       int
	FailedUnits []Unit
}

// HostFactsOf returns the typed detail of a host row. ok is false when r is not
// a Linux host row.
func HostFactsOf(r resource.Resource) (HostFacts, bool) {
	info, ok := r.Raw.(*hostInfo)
	if !ok || info == nil {
		return HostFacts{}, false
	}
	f := HostFacts{NProc: info.NProc, FailedUnits: append([]Unit(nil), info.FailedUnits...)}
	if info.Load != nil {
		f.Load, f.HaveLoad = *info.Load, true
	}
	if info.Mem != nil {
		f.Mem, f.HaveMem = *info.Mem, true
	}
	if info.PSI != nil {
		f.PSI, f.HavePSI = *info.PSI, true
	}
	return f, true
}

// Links returns the pivots from r: a host opens its overview on Enter, then its
// units, processes, sockets and journal; a unit opens its journal.
func (b *Backend) Links(r resource.Resource) []resource.Link {
	kind, ok := normalizeKind(r.Ref.Kind)
	if !ok {
		return nil
	}
	host := r.Ref.Host
	if host == "" {
		host = resource.NormalizeHostID(r.Ref.Name)
	}
	if host == "" {
		return nil
	}
	host = resource.NormalizeHostID(string(host))
	hosts := []resource.HostID{host}

	switch kind {
	case KindHost:
		return []resource.Link{
			{Label: "overview", Key: "enter", To: resource.Query{Kind: KindHostOverview, Hosts: hosts}},
			{Label: "units", Key: "u", To: resource.Query{Kind: KindUnit, Hosts: hosts}},
			{Label: "processes", Key: "p", To: resource.Query{Kind: KindProcess, Hosts: hosts}},
			{Label: "sockets", Key: "s", To: resource.Query{Kind: KindSocket, Hosts: hosts}},
			{Label: "journal", Key: "j", To: resource.Query{Kind: KindJournal, Hosts: hosts}},
			{Label: "mounts", Key: "m", To: resource.Query{Kind: KindMount, Hosts: hosts}},
			{Label: "disks", Key: "D", To: resource.Query{Kind: KindDisk, Hosts: hosts}},
			{Label: "interfaces", Key: "i", To: resource.Query{Kind: KindInterface, Hosts: hosts}},
		}
	case KindUnit:
		if r.Ref.Name == "" {
			return nil
		}
		return []resource.Link{{
			Label: "journal",
			Key:   "j",
			To: resource.Query{
				Kind:  KindJournal,
				Hosts: hosts,
				Args:  map[string]string{"unit": r.Ref.Name},
			},
		}}
	}
	return nil
}

// Health reports the recent transport error rate. It never runs a command.
func (b *Backend) Health(ctx context.Context) backend.Health {
	h := backend.Health{Checked: b.now()}
	if b.inv == nil || len(b.inv.Hosts()) == 0 {
		h.State = backend.HealthUnconfigured
		h.Detail = "no hosts in inventory"
		return h
	}
	b.mu.Lock()
	n := len(b.samples)
	failed := 0
	for _, bad := range b.samples {
		if bad {
			failed++
		}
	}
	b.mu.Unlock()
	if n == 0 {
		h.State = backend.HealthOK
		h.Detail = "no probes yet"
		return h
	}
	if failed*2 >= n {
		h.State = backend.HealthDegraded
	} else {
		h.State = backend.HealthOK
	}
	h.Detail = fmt.Sprintf("%d/%d hosts failed", failed, n)
	return h
}

// record appends one host outcome to the health window.
func (b *Backend) record(transportErr bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.samples) == healthSamples {
		copy(b.samples, b.samples[1:])
		b.samples = b.samples[:healthSamples-1]
	}
	b.samples = append(b.samples, transportErr)
}
