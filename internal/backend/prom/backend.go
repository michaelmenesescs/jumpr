package prom

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"jumpr/internal/backend"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
)

// backendName is the name every Ref and registry lookup uses.
const backendName = "prom"

// healthTTL is how long a /-/healthy answer is reused; BUILD-PLAN §8 checks
// health every 30 s, one probe in flight per backend.
const healthTTL = 30 * time.Second

// defaultWindow is the range a series list uses when the session has no time
// window: the last hour.
const defaultWindow = time.Hour

// Querier is the slice of the typed client the PromQL view runs queries
// through. *Client implements it.
type Querier interface {
	Query(ctx context.Context, expr string, ts time.Time) (Vector, error)
	QueryRange(ctx context.Context, expr string, from, to time.Time, step time.Duration) (Matrix, error)
}

// NamedQuerier is one configured server's client under the name it was
// configured with.
type NamedQuerier struct {
	Name    string
	Querier Querier
}

// Server is one configured Prometheus instance.
type Server struct {
	// Name is the config entry's name. It scopes the rows the server returns
	// and labels its health dot.
	Name string
	// Client is the typed client for it. Nil makes the server unconfigured.
	Client *Client
}

// ServerStatus is one configured server's health.
type ServerStatus struct {
	Name   string
	Health backend.Health
}

// Options configures a Backend. An empty Servers list is a configured-but-
// unusable backend: every List fails with errs.Config and Health reports
// HealthUnconfigured.
type Options struct {
	Servers []Server
	Now     func() time.Time
	Log     *slog.Logger
}

// healthEntry is one cached probe.
type healthEntry struct {
	h  backend.Health
	at time.Time
}

// Backend implements backend.Backend for Prometheus. Every kind is global, so
// there is no per-host batching: one Result per configured server.
type Backend struct {
	servers []Server
	now     func() time.Time
	log     *slog.Logger

	mu     sync.Mutex
	health map[string]healthEntry
}

var _ backend.Backend = (*Backend)(nil)

// New builds a Prometheus backend over the configured servers.
func New(o Options) *Backend {
	b := &Backend{
		servers: append([]Server{}, o.Servers...),
		now:     o.Now,
		log:     o.Log,
		health:  map[string]healthEntry{},
	}
	if b.now == nil {
		b.now = time.Now
	}
	if b.log == nil {
		b.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return b
}

// Name returns "prom".
func (b *Backend) Name() string { return backendName }

// Kinds returns the read-only Prometheus kinds.
func (b *Backend) Kinds() []resource.KindSpec { return kindSpecs() }

// Servers returns the configured server names, sorted.
func (b *Backend) Servers() []string {
	out := make([]string, 0, len(b.servers))
	for _, s := range b.servers {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

// Queriers returns one queryable client per configured server, in configured
// order, for the PromQL view.
func (b *Backend) Queriers() []NamedQuerier {
	out := make([]NamedQuerier, 0, len(b.servers))
	for _, s := range b.servers {
		if s.Client == nil {
			continue
		}
		out = append(out, NamedQuerier{Name: s.Name, Querier: s.Client})
	}
	return out
}

// selectServers returns the servers a query may use: all of them, or the one
// named in args[ArgServer].
func (b *Backend) selectServers(name string) []Server {
	name = strings.TrimSpace(name)
	if name == "" {
		return b.servers
	}
	for _, s := range b.servers {
		if s.Name == name {
			return []Server{s}
		}
	}
	return nil
}

// List streams one Result per configured server, in configured order.
func (b *Backend) List(ctx context.Context, q resource.Query) (<-chan backend.Result, error) {
	kind, ok := normalizeKind(q.Kind)
	if !ok {
		return nil, errs.New(errs.Config, "prom.List", fmt.Sprintf("unknown kind %q", q.Kind))
	}
	if len(b.servers) == 0 {
		return nil, errs.New(errs.Config, "prom.List", "prometheus backend not configured: no servers")
	}
	servers := b.selectServers(q.Args[ArgServer])
	if len(servers) == 0 {
		return nil, errs.New(errs.Config, "prom.List",
			fmt.Sprintf("unknown prometheus server %q", q.Args[ArgServer]))
	}
	if kind == KindSeries && strings.TrimSpace(q.Args[ArgExpr]) == "" {
		return nil, errs.New(errs.Config, "prom.List", "a series list needs an expression")
	}

	out := make(chan backend.Result, 1)
	go func() {
		defer close(out)
		for i, s := range servers {
			items, err := b.rows(ctx, s, kind, q)
			res := backend.Result{Items: items, Done: i + 1, Total: len(servers)}
			if err != nil {
				res.Items, res.Err = nil, err
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

// rows fetches one server's rows for one kind.
func (b *Backend) rows(ctx context.Context, s Server, kind resource.KindName, q resource.Query) ([]resource.Resource, error) {
	if s.Client == nil {
		return nil, errs.New(errs.Config, "prom.List",
			fmt.Sprintf("server %q has no client", s.Name))
	}
	switch kind {
	case KindTarget:
		targets, err := s.Client.Targets(ctx)
		if err != nil {
			return nil, err
		}
		return targetRows(s.Name, targets, b.now()), nil
	case KindRule:
		groups, err := s.Client.Rules(ctx)
		if err != nil {
			return nil, err
		}
		return ruleRows(s.Name, groups, b.now()), nil
	case KindSeries:
		return b.seriesRows(ctx, s, q)
	}
	return nil, errs.New(errs.Config, "prom.List", fmt.Sprintf("unknown kind %q", kind))
}

// seriesRows evaluates q.Args[ArgExpr] at the end of the session window and
// gives every resulting series a sparkline drawn from the same window.
func (b *Backend) seriesRows(ctx context.Context, s Server, q resource.Query) ([]resource.Resource, error) {
	expr := strings.TrimSpace(q.Args[ArgExpr])
	from, to := b.window(q.Window)
	vec, err := s.Client.Query(ctx, expr, to)
	if err != nil {
		return nil, err
	}
	mat, err := s.Client.QueryRange(ctx, expr, from, to, Step(from, to, MaxPoints))
	if err != nil {
		return nil, err
	}
	sparks := map[string][]float64{}
	for _, m := range mat {
		sparks[metricKey(m.Metric)] = values(m.Values)
	}
	out := make([]resource.Resource, 0, len(vec))
	for _, v := range vec {
		val := 0.0
		if len(v.Values) > 0 {
			val = v.Values[len(v.Values)-1].V
		}
		out = append(out, resource.Resource{
			Ref: resource.Ref{
				Backend:   backendName,
				Kind:      KindSeries,
				Namespace: s.Name,
				Name:      LabelString(v.Metric),
			},
			Fields: map[string]any{
				"labels": LabelString(v.Metric),
				"value":  val,
				"spark":  sparks[metricKey(v.Metric)],
			},
			Labels:    v.Metric,
			Status:    resource.StatusOK,
			FetchedAt: b.now(),
		})
	}
	return out, nil
}

// window resolves the session time window; a zero window means the last hour.
func (b *Backend) window(w resource.TimeWindow) (time.Time, time.Time) {
	now := b.now()
	if w.IsZero() {
		return now.Add(-defaultWindow), now
	}
	return w.Bounds(now)
}

// Get returns the row a ref names.
func (b *Backend) Get(ctx context.Context, ref resource.Ref) (resource.Resource, error) {
	kind, ok := normalizeKind(ref.Kind)
	if !ok {
		return resource.Resource{}, errs.New(errs.Config, "prom.Get", fmt.Sprintf("unknown kind %q", ref.Kind))
	}
	if strings.TrimSpace(ref.Name) == "" {
		return resource.Resource{}, errs.New(errs.Config, "prom.Get", "ref needs a name")
	}
	ch, err := b.List(ctx, resource.Query{Kind: kind, Args: map[string]string{ArgServer: ref.Namespace}})
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
	return resource.Resource{}, errs.New(errs.NotAvailable, "prom.Get", "no resource "+ref.String())
}

// Links returns the pivots from r: a target's instance is a host, and the
// series of a query share a server.
func (b *Backend) Links(r resource.Resource) []resource.Link {
	kind, ok := normalizeKind(r.Ref.Kind)
	if !ok {
		return nil
	}
	if kind == KindSeries {
		return nil
	}
	instance := strings.TrimSpace(r.Labels["instance"])
	if instance == "" {
		return nil
	}
	host := instanceHost(instance)
	if host == "" {
		return nil
	}
	return []resource.Link{{
		Label: "host",
		Key:   "enter",
		To:    resource.Query{Kind: "host", Hosts: []resource.HostID{host}},
	}}
}

// Health folds every configured server's probe into one answer.
func (b *Backend) Health(ctx context.Context) backend.Health {
	if len(b.servers) == 0 {
		return backend.Health{State: backend.HealthUnconfigured, Detail: "prometheus not configured", Checked: b.now()}
	}
	statuses := b.Status(ctx)
	h := backend.Health{Checked: b.now()}
	up := 0
	parts := make([]string, 0, len(statuses))
	for _, st := range statuses {
		if st.Health.State == backend.HealthOK {
			up++
		} else {
			parts = append(parts, st.Name+": "+st.Health.Detail)
		}
	}
	switch {
	case up == len(statuses):
		h.State = backend.HealthOK
		h.Detail = fmt.Sprintf("%d/%d servers healthy", up, len(statuses))
	case up == 0:
		h.State = backend.HealthDown
	default:
		h.State = backend.HealthDegraded
	}
	if len(parts) > 0 {
		h.Detail = strings.Join(parts, "; ")
	}
	return h
}

// Status returns one health per configured server, named by config name, so
// the status line can show a dot per Prometheus.
func (b *Backend) Status(ctx context.Context) []ServerStatus {
	out := make([]ServerStatus, 0, len(b.servers))
	for _, s := range b.servers {
		out = append(out, ServerStatus{Name: s.Name, Health: b.serverHealth(ctx, s)})
	}
	return out
}

// serverHealth probes one server, reusing an answer younger than healthTTL.
func (b *Backend) serverHealth(ctx context.Context, s Server) backend.Health {
	now := b.now()
	b.mu.Lock()
	if e, ok := b.health[s.Name]; ok && now.Sub(e.at) >= 0 && now.Sub(e.at) < healthTTL {
		b.mu.Unlock()
		return e.h
	}
	b.mu.Unlock()

	h := backend.Health{Checked: now}
	if s.Client == nil {
		h.State, h.Detail = backend.HealthUnconfigured, "no client"
		return h
	}
	probe := s.Client.Healthy(ctx)
	h.Checked = b.now()
	h.Detail = s.Name + ": " + probe.Detail
	if probe.Health {
		h.State = backend.HealthOK
	} else {
		h.State = backend.HealthDown
	}

	b.mu.Lock()
	b.health[s.Name] = healthEntry{h: h, at: h.Checked}
	b.mu.Unlock()
	return h
}

// targetRows maps targets onto rows of the target kind.
func targetRows(server string, targets []Target, now time.Time) []resource.Resource {
	out := make([]resource.Resource, 0, len(targets))
	for _, t := range targets {
		job := t.Labels["job"]
		instance := t.Labels["instance"]
		out = append(out, resource.Resource{
			Ref: resource.Ref{
				Backend:   backendName,
				Kind:      KindTarget,
				Namespace: server,
				Name:      job + "/" + instance,
			},
			Fields: map[string]any{
				"job":      job,
				"instance": instance,
				"health":   t.Health,
				"scrape":   t.LastScrape,
				"error":    t.LastError,
			},
			Labels:    t.Labels,
			Status:    targetStatus(t.Health),
			FetchedAt: now,
		})
	}
	return out
}

// ruleRows maps rule groups onto rows of the rule kind.
func ruleRows(server string, groups []RuleGroup, now time.Time) []resource.Resource {
	var out []resource.Resource
	for _, g := range groups {
		for _, r := range g.Rules {
			out = append(out, resource.Resource{
				Ref: resource.Ref{
					Backend:   backendName,
					Kind:      KindRule,
					Namespace: server,
					Name:      g.Name + "/" + r.Name,
				},
				Fields: map[string]any{
					"group":  g.Name,
					"name":   r.Name,
					"state":  r.State,
					"health": r.Health,
				},
				Labels:    map[string]string{"group": g.Name, "rule": r.Name, "server": server},
				Status:    ruleStatus(r.Health),
				FetchedAt: now,
			})
		}
	}
	return out
}

// metricKey is a series' identity: its sorted label set.
func metricKey(m map[string]string) string { return LabelString(m) }

// values extracts the finite values of a series, keeping NaN as a gap.
func values(samples []Sample) []float64 {
	out := make([]float64, 0, len(samples))
	for _, s := range samples {
		out = append(out, s.V)
	}
	return out
}

// instanceHost turns a Prometheus instance label into an inventory host ID:
// the port is stripped, `user@` dropped and the name lower-cased.
func instanceHost(instance string) resource.HostID {
	h := instance
	if i := strings.LastIndexByte(h, '@'); i >= 0 {
		h = h[i+1:]
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.Trim(h, "[]")
	if h == "" {
		return ""
	}
	return resource.NormalizeHostID(h)
}
