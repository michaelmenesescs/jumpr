package grafana

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"jumpr/internal/backend"
	"jumpr/internal/clip"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
)

// backendName is the name every Ref and registry lookup uses.
const backendName = "grafana"

// healthTTL is how long an /api/health answer is reused; BUILD-PLAN §8 checks
// health every 30 s.
const healthTTL = 30 * time.Second

// alertSourcePrefix labels Grafana alert rows so :alerts can tell them from
// Alertmanager rows (SPEC §8.4).
const alertSourcePrefix = "grafana:"

// Kind names this backend serves.
const (
	// KindDashboard is one dashboard of /api/search; enter fetches its panels.
	KindDashboard resource.KindName = "dashboard"
	// KindAlert is one Grafana alert. Alertmanager rows use the same kind name;
	// the source column keeps the two apart.
	KindAlert resource.KindName = "alert"
)

// resource.Query argument keys this backend reads.
const (
	// ArgQuery is the text /api/search matches dashboard titles with.
	ArgQuery = "query"
	// ArgServer limits a list to one configured server by name.
	ArgServer = "server"
)

// Kind aliases accepted in the command bar.
const (
	aliasDash       = "dash"
	aliasDashboards = "dashboards"
)

// Server is one configured Grafana instance.
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
	// Open is the app's browser hook: `o` passes the dashboard URL to it.
	Open func(url string) error
	// Clipboard is the default `o` behaviour when Open is nil: the URL is
	// copied instead of opened (BUILD-PLAN §6.1 rule 6 keeps both out of the
	// backend's own process handling).
	Clipboard clip.Clipboard
}

// healthEntry is one cached probe.
type healthEntry struct {
	h  backend.Health
	at time.Time
}

// Backend implements backend.Backend for Grafana. Every kind is global: one
// Result per configured server.
type Backend struct {
	servers []Server
	now     func() time.Time
	log     *slog.Logger
	open    func(string) error
	clip    clip.Clipboard

	mu     sync.Mutex
	health map[string]healthEntry
}

var _ backend.Backend = (*Backend)(nil)

// New builds a Grafana backend over the configured servers.
func New(o Options) *Backend {
	b := &Backend{
		servers: append([]Server{}, o.Servers...),
		now:     o.Now,
		log:     logOrDiscard(o.Log),
		open:    o.Open,
		clip:    o.Clipboard,
		health:  map[string]healthEntry{},
	}
	if b.now == nil {
		b.now = time.Now
	}
	return b
}

// Name returns "grafana".
func (b *Backend) Name() string { return backendName }

// Kinds returns the read-only Grafana kinds.
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
		return nil, errs.New(errs.Config, "grafana.List", fmt.Sprintf("unknown kind %q", q.Kind))
	}
	if len(b.servers) == 0 {
		return nil, errs.New(errs.Config, "grafana.List", "grafana backend not configured: no servers")
	}
	servers := b.selectServers(q.Args[ArgServer])
	if len(servers) == 0 {
		return nil, errs.New(errs.Config, "grafana.List",
			fmt.Sprintf("unknown grafana server %q", q.Args[ArgServer]))
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
		return nil, errs.New(errs.Config, "grafana.List",
			fmt.Sprintf("server %q has no client", s.Name))
	}
	switch kind {
	case KindDashboard:
		hits, err := s.Client.Search(ctx, q.Args[ArgQuery])
		if err != nil {
			return nil, err
		}
		return dashboardRows(s, hits, b.now()), nil
	case KindAlert:
		alerts, err := s.Client.Alerts(ctx)
		if err != nil {
			return nil, err
		}
		return alertRows(s.Name, alerts, b.now()), nil
	}
	return nil, errs.New(errs.Config, "grafana.List", fmt.Sprintf("unknown kind %q", kind))
}

// Get returns the row a ref names. A dashboard ref fetches the dashboard so the
// detail view has its panels and variables in Raw.
func (b *Backend) Get(ctx context.Context, ref resource.Ref) (resource.Resource, error) {
	kind, ok := normalizeKind(ref.Kind)
	if !ok {
		return resource.Resource{}, errs.New(errs.Config, "grafana.Get", fmt.Sprintf("unknown kind %q", ref.Kind))
	}
	if strings.TrimSpace(ref.Name) == "" {
		return resource.Resource{}, errs.New(errs.Config, "grafana.Get", "ref needs a name")
	}
	servers := b.selectServers(ref.Namespace)
	if len(servers) == 0 {
		return resource.Resource{}, errs.New(errs.Config, "grafana.Get",
			fmt.Sprintf("unknown grafana server %q", ref.Namespace))
	}
	if kind == KindDashboard {
		var lastErr error
		for _, s := range servers {
			if s.Client == nil {
				lastErr = errs.New(errs.Config, "grafana.Get",
					fmt.Sprintf("server %q has no client", s.Name))
				continue
			}
			d, err := s.Client.Dashboard(ctx, ref.Name)
			if err != nil {
				lastErr = err
				continue
			}
			return dashboardResource(s.Name, d, b.now()), nil
		}
		if lastErr == nil {
			lastErr = errs.New(errs.NotAvailable, "grafana.Get", "no resource "+ref.String())
		}
		return resource.Resource{}, lastErr
	}

	// An alert row is cheap to rebuild: list and match its identity.
	ch, err := b.List(ctx, resource.Query{Kind: KindAlert, Args: map[string]string{ArgServer: ref.Namespace}})
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
	return resource.Resource{}, errs.New(errs.NotAvailable, "grafana.Get", "no resource "+ref.String())
}

// Links returns the pivots from r. Grafana rows have none: a dashboard is not a
// host and an alert's instance is already a Prometheus target.
func (b *Backend) Links(r resource.Resource) []resource.Link { return nil }

// Health folds every configured server's /api/health probe into one answer.
func (b *Backend) Health(ctx context.Context) backend.Health {
	if len(b.servers) == 0 {
		return backend.Health{State: backend.HealthUnconfigured, Detail: "grafana not configured", Checked: b.now()}
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
// the status line can show a dot per Grafana.
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
	h.Detail = probe.Detail
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

// OpenURL opens a dashboard URL through the app hook, or copies it to the
// clipboard when no hook is configured (the default).
func (b *Backend) OpenURL(url string) error {
	url = strings.TrimSpace(url)
	if url == "" {
		return errs.New(errs.Config, "grafana.open", "no URL to open")
	}
	if b.open != nil {
		if err := b.open(url); err != nil {
			return errs.Wrap(errs.Internal, "grafana.open", err)
		}
		return nil
	}
	if b.clip != nil {
		if err := b.clip.Copy(url); err != nil {
			return errs.Wrap(errs.Internal, "grafana.open", err)
		}
		return nil
	}
	return errs.New(errs.Config, "grafana.open", "no browser opener or clipboard configured")
}

// OpenResource opens (or copies) the browser URL of a dashboard row, which is
// what `o` does on the dashboard list and in the panel grid.
func (b *Backend) OpenResource(r resource.Resource) error {
	url, _ := r.Fields["url"].(string)
	if strings.TrimSpace(url) == "" {
		return errs.New(errs.Config, "grafana.open", "the row has no browser URL")
	}
	return b.OpenURL(url)
}

// ResolveVariable returns the options of one dashboard variable: the values of
// a simple label_values query through /api/ds/query, a select's declared
// options, or nil for a free-text variable.
func (b *Backend) ResolveVariable(ctx context.Context, server string, d *Dashboard, name string) ([]string, error) {
	if d == nil {
		return nil, errs.New(errs.Config, "grafana.variable", "no dashboard")
	}
	v, ok := d.Variable(name)
	if !ok {
		return nil, errs.New(errs.Config, "grafana.variable",
			fmt.Sprintf("the dashboard has no variable %q", name))
	}
	expr, ok := v.LabelValuesExpr()
	if !ok {
		return optionValues(v.Options), nil
	}
	servers := b.selectServers(server)
	if len(servers) == 0 {
		return nil, errs.New(errs.Config, "grafana.variable",
			fmt.Sprintf("unknown grafana server %q", server))
	}
	if servers[0].Client == nil {
		return nil, errs.New(errs.Config, "grafana.variable",
			fmt.Sprintf("server %q has no client", servers[0].Name))
	}
	return servers[0].Client.ResolveLabelValues(ctx, v.Datasource, expr)
}

// optionValues returns the values a select variable declares.
func optionValues(options []VariableOption) []string {
	if len(options) == 0 {
		return nil
	}
	out := make([]string, 0, len(options))
	for _, o := range options {
		out = append(out, o.Value)
	}
	return out
}

// dashboardRows maps search hits onto rows of the dashboard kind.
func dashboardRows(s Server, hits []SearchHit, now time.Time) []resource.Resource {
	out := make([]resource.Resource, 0, len(hits))
	for _, h := range hits {
		out = append(out, resource.Resource{
			Ref: resource.Ref{
				Backend:   backendName,
				Kind:      KindDashboard,
				Namespace: s.Name,
				Name:      h.UID,
			},
			Fields: map[string]any{
				"title":  h.Title,
				"folder": h.Folder,
				"tags":   strings.Join(h.Tags, ", "),
				"uid":    h.UID,
				"url":    s.Client.Absolute(h.URL),
			},
			Labels:    map[string]string{"folder": h.Folder, "uid": h.UID},
			Status:    resource.StatusOK,
			FetchedAt: now,
		})
	}
	return out
}

// dashboardResource makes the row Get returns: the fetched dashboard in Raw and
// its identity in the columns.
func dashboardResource(server string, d *Dashboard, now time.Time) resource.Resource {
	return resource.Resource{
		Ref: resource.Ref{
			Backend:   backendName,
			Kind:      KindDashboard,
			Namespace: server,
			Name:      d.UID,
		},
		Fields: map[string]any{
			"title":  d.Title,
			"folder": d.Folder,
			"tags":   strings.Join(d.Tags, ", "),
			"uid":    d.UID,
			"url":    d.URL,
			"panels": len(d.PanelRows()),
		},
		Labels:    map[string]string{"folder": d.Folder, "uid": d.UID},
		Status:    resource.StatusOK,
		Raw:       d,
		FetchedAt: now,
	}
}

// alertRows maps Grafana alerts onto rows of the alert kind, source
// "grafana:<server>" (SPEC §8.4).
func alertRows(server string, alerts []GrafanaAlert, now time.Time) []resource.Resource {
	source := alertSourcePrefix + server
	out := make([]resource.Resource, 0, len(alerts))
	for _, a := range alerts {
		labels := make(map[string]string, len(a.Labels)+1)
		for k, v := range a.Labels {
			labels[k] = v
		}
		labels["source"] = source
		state := strings.TrimSpace(a.State)
		if state == "" {
			state = "firing"
		}
		out = append(out, resource.Resource{
			Ref: resource.Ref{
				Backend:   backendName,
				Kind:      KindAlert,
				Namespace: server,
				Name:      LabelString(a.Labels),
			},
			Fields: map[string]any{
				"sev":    a.Labels["severity"],
				"alert":  a.Labels["alertname"],
				"labels": LabelString(a.Labels),
				"source": source,
				"start":  a.ActiveAt,
				"state":  state,
			},
			Labels:    labels,
			Status:    alertStatus(state),
			FetchedAt: now,
		})
	}
	return out
}

// alertStatus colours an alert row by its state.
func alertStatus(state string) resource.Status {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "firing":
		return resource.StatusCrit
	case "pending", "nodata", "error":
		return resource.StatusWarn
	case "normal", "inactive", "resolved":
		return resource.StatusOK
	}
	return resource.StatusUnknown
}

// kindSpecs returns the two kinds, in the order the command bar lists them.
func kindSpecs() []resource.KindSpec {
	return []resource.KindSpec{
		{
			Name:        KindDashboard,
			Aliases:     []string{aliasDash, aliasDashboards},
			Title:       "Grafana dashboards",
			Scope:       resource.ScopeGlobal,
			DefaultSort: "title",
			Columns: []resource.Column{
				{ID: "title", Title: "TITLE", Width: 0},
				{ID: "folder", Title: "FOLDER", Width: 20},
				{ID: "tags", Title: "TAGS", Width: 28},
				{ID: "uid", Title: "UID", Width: 14},
			},
		},
		{
			Name:        KindAlert,
			Title:       "Alerts",
			Scope:       resource.ScopeGlobal,
			DefaultSort: "-start",
			Columns: []resource.Column{
				{ID: "sev", Title: "SEV", Width: 10},
				{ID: "alert", Title: "ALERT", Width: 20},
				{ID: "labels", Title: "LABELS", Width: 34},
				{ID: "source", Title: "SOURCE", Width: 15},
				{ID: "start", Title: "START", Width: 20, Kind: resource.ColTime},
				{ID: "state", Title: "STATE", Width: 10},
			},
		},
	}
}

// normalizeKind maps a kind name or alias onto its canonical name.
func normalizeKind(k resource.KindName) (resource.KindName, bool) {
	switch k {
	case KindDashboard, aliasDash, aliasDashboards:
		return KindDashboard, true
	case KindAlert:
		return KindAlert, true
	}
	return "", false
}
