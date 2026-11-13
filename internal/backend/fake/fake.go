// Package fake provides a deterministic in-memory backend for the demo mode
// and for tests. It mirrors the approved mockup: 1,512 web hosts, a failed
// nginx.service on two of them, and six alerts. No I/O, no network.
package fake

import (
	"context"
	"fmt"
	"strings"
	"time"

	"sitrep/internal/backend"
	"sitrep/internal/errs"
	"sitrep/internal/resource"
)

// batchSize is the maximum number of resources in one Result.
const batchSize = 100

// Options configures a Backend. The zero value is usable: no latency, no
// failing hosts and the wall clock.
type Options struct {
	Latency   time.Duration
	FailHosts map[resource.HostID]errs.Kind
	Now       func() time.Time
}

// Backend is the fake implementation of backend.Backend.
type Backend struct {
	latency   time.Duration
	failHosts map[resource.HostID]errs.Kind
	now       func() time.Time
}

var _ backend.Backend = (*Backend)(nil)

// New builds a fake backend from opts.
func New(opts Options) *Backend {
	b := &Backend{
		latency:   opts.Latency,
		failHosts: make(map[resource.HostID]errs.Kind, len(opts.FailHosts)),
		now:       opts.Now,
	}
	for host, kind := range opts.FailHosts {
		b.failHosts[resource.NormalizeHostID(string(host))] = kind
	}
	if b.now == nil {
		b.now = time.Now
	}
	return b
}

// Name returns the backend name.
func (b *Backend) Name() string { return "fake" }

// Kinds returns the kinds this backend serves.
func (b *Backend) Kinds() []resource.KindSpec {
	return []resource.KindSpec{
		{
			Name:    "host",
			Aliases: []string{"hosts", "h"},
			Title:   "Hosts",
			Scope:   resource.ScopeGlobal,
			Columns: []resource.Column{
				{ID: "host", Title: "HOST", Width: 16},
				{ID: "load", Title: "LOAD", Width: 6, Align: resource.AlignRight, Kind: resource.ColNumber},
				{ID: "mem", Title: "MEM%", Width: 6, Align: resource.AlignRight, Kind: resource.ColPercent},
				{ID: "psi_io", Title: "PSI-IO", Width: 7, Align: resource.AlignRight, Kind: resource.ColPercent},
				{ID: "failed", Title: "FAILED", Width: 7, Align: resource.AlignRight, Kind: resource.ColNumber},
				{ID: "units", Title: "UNITS", Width: 10},
				{ID: "err_rate", Title: "ERR%", Width: 7, Align: resource.AlignRight, Kind: resource.ColNumber},
				{ID: "spark", Title: "LOAD", Width: 14, Kind: resource.ColSpark},
			},
			DefaultSort: "-load",
		},
		{
			Name:    "unit",
			Aliases: []string{"units", "u"},
			Title:   "Units",
			Scope:   resource.ScopeHost,
			Columns: []resource.Column{
				{ID: "unit", Title: "UNIT", Width: 24},
				{ID: "load", Title: "LOAD", Width: 8},
				{ID: "active", Title: "ACTIVE", Width: 9},
				{ID: "sub", Title: "SUB", Width: 9},
				{ID: "description", Title: "DESCRIPTION", Width: 0},
			},
			DefaultSort: "unit",
		},
		{
			Name:    "alert",
			Aliases: []string{"alerts", "al"},
			Title:   "Alerts",
			Scope:   resource.ScopeGlobal,
			Columns: []resource.Column{
				{ID: "sev", Title: "SEV", Width: 10},
				{ID: "alert", Title: "ALERT", Width: 20},
				{ID: "labels", Title: "LABELS", Width: 34},
				{ID: "source", Title: "SOURCE", Width: 13},
				{ID: "start", Title: "START", Width: 20, Kind: resource.ColTime},
				{ID: "state", Title: "STATE", Width: 10},
			},
			DefaultSort: "-start",
		},
	}
}

// List streams the resources of q. Unknown kinds and host-scoped kinds without
// a host return an error; everything else streams from a goroutine.
func (b *Backend) List(ctx context.Context, q resource.Query) (<-chan backend.Result, error) {
	switch normalizeKind(q.Kind) {
	case "host":
		return b.listHosts(ctx, q), nil
	case "unit":
		hosts, err := unitHosts(q)
		if err != nil {
			return nil, err
		}
		return b.listUnits(ctx, q, hosts), nil
	case "alert":
		return b.listAlerts(ctx, q), nil
	default:
		return nil, errs.New(errs.Config, "fake.List", fmt.Sprintf("unknown kind %q", q.Kind))
	}
}

// normalizeKind maps the canonical name and the aliases onto one word.
func normalizeKind(kind resource.KindName) string {
	switch strings.ToLower(strings.TrimSpace(string(kind))) {
	case "hosts", "h":
		return "host"
	case "units", "u":
		return "unit"
	case "alerts", "al":
		return "alert"
	default:
		return strings.ToLower(strings.TrimSpace(string(kind)))
	}
}

// unitHosts resolves the host list a unit query applies to: Query.Hosts wins,
// then Args["host"] (comma or space separated).
func unitHosts(q resource.Query) ([]resource.HostID, error) {
	if len(q.Hosts) > 0 {
		return dedupeHosts(q.Hosts), nil
	}
	raw := strings.TrimSpace(q.Args["host"])
	if raw == "" {
		return nil, errs.New(errs.Config, "fake.List", "unit kind needs Query.Hosts or Args[\"host\"]")
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	return dedupeHosts(hostIDs(fields)), nil
}

func hostIDs(names []string) []resource.HostID {
	out := make([]resource.HostID, 0, len(names))
	for _, name := range names {
		out = append(out, resource.NormalizeHostID(name))
	}
	return out
}

func dedupeHosts(in []resource.HostID) []resource.HostID {
	seen := make(map[resource.HostID]bool, len(in))
	out := make([]resource.HostID, 0, len(in))
	for _, host := range in {
		id := resource.NormalizeHostID(string(host))
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func (b *Backend) listHosts(ctx context.Context, q resource.Query) <-chan backend.Result {
	ch := make(chan backend.Result)
	go func() {
		defer close(ch)

		var names []string
		if len(q.Hosts) > 0 {
			for _, host := range dedupeHosts(q.Hosts) {
				names = append(names, string(host))
			}
		} else {
			for _, name := range fleetNames() {
				if q.Selector.IsZero() || q.Selector.Matches(hostLabels(name, dcOf(name))) {
					names = append(names, name)
				}
			}
		}

		total := len(names)
		processed := 0
		var batch []resource.Resource
		flush := func() bool {
			if len(batch) == 0 {
				return true
			}
			result := backend.Result{Items: batch, Done: processed, Total: total}
			batch = nil
			return b.send(ctx, ch, result)
		}

		for _, name := range names {
			id := resource.HostID(name)
			if kind, bad := b.failHosts[id]; bad {
				if !flush() {
					return
				}
				processed++
				if !b.send(ctx, ch, backend.Result{Host: id, Err: failErr(kind, id), Done: processed, Total: total}) {
					return
				}
				continue
			}
			batch = append(batch, hostRow(b.now(), name))
			processed++
			if len(batch) >= batchSize && !flush() {
				return
			}
		}
		flush()
	}()
	return ch
}

func (b *Backend) listUnits(ctx context.Context, q resource.Query, hosts []resource.HostID) <-chan backend.Result {
	ch := make(chan backend.Result)
	go func() {
		defer close(ch)

		if len(q.Hosts) == 0 && !q.Selector.IsZero() {
			kept := hosts[:0]
			for _, host := range hosts {
				if q.Selector.Matches(hostLabels(string(host), dcOf(string(host)))) {
					kept = append(kept, host)
				}
			}
			hosts = kept
		}

		total := len(hosts)
		processed := 0
		for _, host := range hosts {
			processed++
			if kind, bad := b.failHosts[host]; bad {
				if !b.send(ctx, ch, backend.Result{Host: host, Err: failErr(kind, host), Done: processed, Total: total}) {
					return
				}
				continue
			}
			items := unitRows(b.now(), host)
			if want := strings.TrimSpace(q.Args["unit"]); want != "" {
				items = filterUnits(items, want)
			}
			if !b.send(ctx, ch, backend.Result{Host: host, Items: items, Done: processed, Total: total}) {
				return
			}
		}
	}()
	return ch
}

func filterUnits(items []resource.Resource, name string) []resource.Resource {
	out := items[:0]
	for _, item := range items {
		if item.Ref.Name == name {
			out = append(out, item)
		}
	}
	return out
}

func (b *Backend) listAlerts(ctx context.Context, q resource.Query) <-chan backend.Result {
	ch := make(chan backend.Result)
	go func() {
		defer close(ch)

		rows := alertRows(b.now())
		var filtered []resource.Resource
		for _, row := range rows {
			if q.Selector.IsZero() || q.Selector.Matches(row.Labels) {
				filtered = append(filtered, row)
			}
		}

		total := len(filtered)
		processed := 0
		for start := 0; start < len(filtered); start += batchSize {
			end := start + batchSize
			if end > len(filtered) {
				end = len(filtered)
			}
			batch := filtered[start:end]
			processed += len(batch)
			if !b.send(ctx, ch, backend.Result{Items: batch, Done: processed, Total: total}) {
				return
			}
		}
	}()
	return ch
}

// send sleeps for the configured latency and then delivers one Result, giving
// up as soon as ctx is done. A cancelled list never blocks the producer.
func (b *Backend) send(ctx context.Context, ch chan<- backend.Result, r backend.Result) bool {
	if b.latency > 0 {
		timer := time.NewTimer(b.latency)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
		}
	}
	select {
	case <-ctx.Done():
		return false
	case ch <- r:
		return true
	}
}

func failErr(kind errs.Kind, host resource.HostID) error {
	return errs.New(kind, "fake.List", "injected failure").WithHost(string(host))
}

// Get returns one resource by Ref.
func (b *Backend) Get(ctx context.Context, ref resource.Ref) (resource.Resource, error) {
	switch normalizeKind(ref.Kind) {
	case "host":
		name := firstNonEmpty(ref.Name, string(ref.Host))
		if name == "" {
			return resource.Resource{}, errs.New(errs.Config, "fake.Get", "host ref needs a name")
		}
		if !isFleetHost(resource.HostID(name)) {
			return resource.Resource{}, notFound(ref)
		}
		return hostRow(b.now(), name), nil
	case "unit":
		if ref.Host == "" || ref.Name == "" {
			return resource.Resource{}, errs.New(errs.Config, "fake.Get", "unit ref needs a host and a name")
		}
		for _, row := range unitRows(b.now(), ref.Host) {
			if row.Ref.Name == ref.Name {
				return row, nil
			}
		}
		return resource.Resource{}, notFound(ref)
	case "alert":
		for _, row := range alertRows(b.now()) {
			if row.Ref.Name == ref.Name {
				return row, nil
			}
		}
		return resource.Resource{}, notFound(ref)
	default:
		return resource.Resource{}, errs.New(errs.Config, "fake.Get", fmt.Sprintf("unknown kind %q", ref.Kind))
	}
}

func notFound(ref resource.Ref) error {
	return errs.New(errs.NotAvailable, "fake.Get", fmt.Sprintf("no resource %s", ref.String()))
}

func isFleetHost(id resource.HostID) bool {
	parts := strings.Split(string(id), "-")
	if len(parts) != 3 || parts[0] != "web" {
		return false
	}
	knownDC := false
	for _, dc := range datacenters {
		if parts[1] == dc {
			knownDC = true
			break
		}
	}
	if !knownDC {
		return false
	}
	var n int
	if _, err := fmt.Sscanf(parts[2], "%d", &n); err != nil {
		return false
	}
	return n >= 1 && n <= hostsPerDC
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Links returns the pivots available from r: a host links to its units.
func (b *Backend) Links(r resource.Resource) []resource.Link {
	if normalizeKind(r.Ref.Kind) != "host" {
		return nil
	}
	host := rowHost(r)
	if host == "" {
		return nil
	}
	return []resource.Link{{
		Label: "units",
		Key:   "u",
		To:    resource.Query{Kind: "unit", Hosts: []resource.HostID{host}},
	}}
}

// rowHost derives the host identity of a resource row.
func rowHost(r resource.Resource) resource.HostID {
	if r.Ref.Host != "" {
		return r.Ref.Host
	}
	if r.Ref.Name != "" {
		return resource.NormalizeHostID(r.Ref.Name)
	}
	if v, ok := r.Fields["host"].(string); ok {
		return resource.NormalizeHostID(v)
	}
	return ""
}

// Health always reports ok, with the source of the data in the detail.
func (b *Backend) Health(ctx context.Context) backend.Health {
	return backend.Health{State: backend.HealthOK, Detail: "fake data", Checked: b.now()}
}
