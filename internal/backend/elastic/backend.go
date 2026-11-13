package elastic

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"jumper/internal/backend"
	"jumper/internal/errs"
	"jumper/internal/resource"
)

// backendName is the name every Ref and registry lookup uses.
const backendName = "elastic"

// healthTTL is how long a `GET /` answer is reused; BUILD-PLAN §8 checks health
// every 30 s.
const healthTTL = 30 * time.Second

// defaultTimeField is the time field a log source gets when it does not name
// one: the Elastic Common Schema default.
const defaultTimeField = "@timestamp"

// KindLog is one line of a configured log source (SPEC §8.5).
const KindLog resource.KindName = "log"

// Kind aliases accepted in the command bar.
const (
	aliasLogs = "logs"
	aliasLog  = "log"
)

// resource.Query argument keys this backend reads.
const (
	// ArgQuery is the `query_string` text.
	ArgQuery = "query"
	// ArgSource limits a list to one configured log source by name.
	ArgSource = "source"
	// ArgCluster limits a list to one configured cluster by name.
	ArgCluster = "cluster"
	// ArgColumns lists extra `_source` fields (comma separated) the user added
	// as columns.
	ArgColumns = "columns"
)

// LogSource is one saved log source (SPEC §8.5): where a kind of log lives
// inside a cluster, and which fields carry time, host, message and level.
type LogSource struct {
	Name         string
	Index        string
	TimeField    string
	HostField    string
	MessageField string
	LevelField   string
}

// Normalized returns the source with its defaults applied: an empty time field
// becomes @timestamp, the Elastic Common Schema default. Everything else stays
// as configured; an empty host, message or level field simply means that
// filter or column is unavailable.
func (s LogSource) Normalized() LogSource {
	s.Name = strings.TrimSpace(s.Name)
	s.Index = strings.TrimSpace(s.Index)
	s.TimeField = strings.TrimSpace(s.TimeField)
	s.HostField = strings.TrimSpace(s.HostField)
	s.MessageField = strings.TrimSpace(s.MessageField)
	s.LevelField = strings.TrimSpace(s.LevelField)
	if s.TimeField == "" {
		s.TimeField = defaultTimeField
	}
	return s
}

// Cluster is one configured Elasticsearch cluster with its log sources.
type Cluster struct {
	Name    string
	Client  *Client
	Sources []LogSource
}

// SourceRef names one configured log source of one cluster.
type SourceRef struct {
	Cluster string
	Source  LogSource
}

// ClusterStatus is one configured cluster's health.
type ClusterStatus struct {
	Name   string
	Health backend.Health
}

// LogQuery is one page of one configured log source.
type LogQuery struct {
	// Cluster and Source name the configured entry. Empty Source means the
	// cluster's first source, which is what the `:logs` command line uses.
	Cluster string
	Source  string
	// Hosts are the session targets; the query matches their full and short
	// names on the source's host field.
	Hosts []resource.HostID
	// Query is the user's `query_string` text.
	Query string
	// Window is the investigation time window.
	Window resource.TimeWindow
	// Columns are extra `_source` fields the user added as columns.
	Columns []string
	// After is the sort of the previous page, for search_after paging.
	After []any
	// Size is the page size; zero means SearchSize.
	Size int
	// Now is the clock the window is resolved against; zero means the
	// backend's clock.
	Now time.Time
}

// LogRow is one log line with the fields the view's columns need.
type LogRow struct {
	Cluster string
	Source  string
	Index   string
	ID      string
	Time    time.Time
	Host    string
	Level   string
	Message string
	// Sort is the hit's sort values: what the next page's search_after needs.
	Sort []any
	// Doc is the whole document as indented JSON: what `enter` shows.
	Doc string
	// Fields carries every configured and added field by name, so a column the
	// user added has a value.
	Fields map[string]string
}

// LogPage is one page of a log source. Full is true when the page is as large
// as the size asked for, so more rows are probably waiting.
type LogPage struct {
	Rows  []LogRow
	After []any
	Full  bool
}

// Options configures a Backend. An empty Clusters list is a configured-but-
// unusable backend: every List fails with errs.Config and Health reports
// HealthUnconfigured.
type Options struct {
	Clusters []Cluster
	Now      func() time.Time
	Log      *slog.Logger
	// Redact cleans every document before it leaves the backend (BUILD-PLAN
	// §7). Nil leaves text alone.
	Redact func(string) string
}

// healthEntry is one cached probe.
type healthEntry struct {
	h  backend.Health
	at time.Time
}

// Backend implements backend.Backend for Elasticsearch. The log kind is global:
// a Result is one page of one configured source.
type Backend struct {
	clusters []Cluster
	now      func() time.Time
	log      *slog.Logger
	redact   func(string) string

	mu     sync.Mutex
	health map[string]healthEntry
}

var _ backend.Backend = (*Backend)(nil)

// New builds an Elasticsearch backend over the configured clusters.
func New(o Options) *Backend {
	b := &Backend{
		clusters: append([]Cluster{}, o.Clusters...),
		now:      o.Now,
		log:      logOrDiscard(o.Log),
		redact:   o.Redact,
		health:   map[string]healthEntry{},
	}
	if b.now == nil {
		b.now = time.Now
	}
	return b
}

// Name returns "elastic".
func (b *Backend) Name() string { return backendName }

// Kinds returns the log kind.
func (b *Backend) Kinds() []resource.KindSpec { return kindSpecs() }

// Clusters returns the configured cluster names, sorted.
func (b *Backend) Clusters() []string {
	out := make([]string, 0, len(b.clusters))
	for _, c := range b.clusters {
		out = append(out, c.Name)
	}
	sort.Strings(out)
	return out
}

// LogSources returns every configured log source, cluster by cluster in the
// configured order, which is what `:logs sources` lists.
func (b *Backend) LogSources() []SourceRef {
	var out []SourceRef
	for _, c := range b.clusters {
		for _, s := range c.Sources {
			out = append(out, SourceRef{Cluster: c.Name, Source: s.Normalized()})
		}
	}
	return out
}

// resolve finds the cluster and source a LogQuery names.
func (b *Backend) resolve(q LogQuery) (Cluster, LogSource, error) {
	clusterName := strings.TrimSpace(q.Cluster)
	sourceName := strings.TrimSpace(q.Source)
	var cluster Cluster
	found := false
	for _, c := range b.clusters {
		if clusterName == "" || c.Name == clusterName {
			cluster, found = c, true
			break
		}
	}
	if !found {
		return Cluster{}, LogSource{}, errs.New(errs.Config, "elastic.logs",
			fmt.Sprintf("unknown elastic cluster %q", q.Cluster))
	}
	if cluster.Client == nil {
		return Cluster{}, LogSource{}, errs.New(errs.Config, "elastic.logs",
			fmt.Sprintf("cluster %q has no client", cluster.Name))
	}
	for _, s := range cluster.Sources {
		src := s.Normalized()
		if sourceName == "" || src.Name == sourceName {
			return cluster, src, nil
		}
	}
	return Cluster{}, LogSource{}, errs.New(errs.Config, "elastic.logs",
		fmt.Sprintf("cluster %q has no log source %q", cluster.Name, q.Source))
}

// Logs runs one page of one log source. It is what the `:logs` view calls, and
// it does the I/O a view is not allowed to do: the caller runs it in a tea.Cmd.
func (b *Backend) Logs(ctx context.Context, q LogQuery) (LogPage, error) {
	cluster, src, err := b.resolve(q)
	if err != nil {
		return LogPage{}, err
	}
	now := q.Now
	if now.IsZero() {
		now = b.now()
	}
	from, to := q.Window.Bounds(now)
	page, err := cluster.Client.Search(ctx, SearchOptions{
		Source:      src,
		Hosts:       q.Hosts,
		QueryString: q.Query,
		From:        from,
		To:          to,
		Columns:     q.Columns,
		SearchAfter: q.After,
		Size:        q.Size,
	})
	if err != nil {
		return LogPage{}, err
	}
	out := LogPage{Rows: b.rows(cluster.Name, src, page.Hits, q.Columns), Full: len(page.Hits) >= pageSize(q.Size)}
	if n := len(out.Rows); n > 0 {
		out.After = out.Rows[n-1].Sort
	}
	return out, nil
}

// pageSize is the page size a query uses.
func pageSize(size int) int {
	if size <= 0 {
		return SearchSize
	}
	return size
}

// rows maps hits onto log rows, reading the configured fields from each
// document and redacting the text before it leaves the backend.
func (b *Backend) rows(cluster string, src LogSource, hits []Hit, columns []string) []LogRow {
	out := make([]LogRow, 0, len(hits))
	for _, h := range hits {
		row := LogRow{
			Cluster: cluster,
			Source:  src.Name,
			Index:   h.Index,
			ID:      h.ID,
			Time:    timeValue(lookupPath(h.Source, src.TimeField)),
			Host:    textValue(lookupPath(h.Source, src.HostField)),
			Level:   textValue(lookupPath(h.Source, src.LevelField)),
			Message: textValue(lookupPath(h.Source, src.MessageField)),
			Sort:    h.Sort,
			Doc:     docString(h.Source),
		}
		fields := make(map[string]string, 4+len(columns))
		for _, name := range sourceFields(src, columns) {
			fields[name] = textValue(lookupPath(h.Source, name))
		}
		row.Fields = fields
		if b.redact != nil {
			row.Host = b.redact(row.Host)
			row.Level = b.redact(row.Level)
			row.Message = b.redact(row.Message)
			row.Doc = b.redact(row.Doc)
			for k, v := range fields {
				fields[k] = b.redact(v)
			}
		}
		out = append(out, row)
	}
	return out
}

// FieldCaps lists the completable fields of one configured source.
func (b *Backend) FieldCaps(ctx context.Context, cluster, source string) ([]Field, error) {
	c, src, err := b.resolve(LogQuery{Cluster: cluster, Source: source})
	if err != nil {
		return nil, err
	}
	return c.Client.FieldCaps(ctx, src.Index, "*")
}

// Resolve lists the indices one configured source's pattern matches.
func (b *Backend) Resolve(ctx context.Context, cluster, source string) ([]IndexInfo, error) {
	c, src, err := b.resolve(LogQuery{Cluster: cluster, Source: source})
	if err != nil {
		return nil, err
	}
	return c.Client.Resolve(ctx, src.Index)
}

// List streams one Result per page of every selected log source, in configured
// order. Paging uses search_after and stops when a page comes back short.
func (b *Backend) List(ctx context.Context, q resource.Query) (<-chan backend.Result, error) {
	kind, ok := normalizeKind(q.Kind)
	if !ok {
		return nil, errs.New(errs.Config, "elastic.List", fmt.Sprintf("unknown kind %q", q.Kind))
	}
	if kind != KindLog {
		return nil, errs.New(errs.Config, "elastic.List", fmt.Sprintf("unknown kind %q", q.Kind))
	}
	if len(b.clusters) == 0 {
		return nil, errs.New(errs.Config, "elastic.List", "elastic backend not configured: no clusters")
	}
	sources, err := b.selectSources(q.Args[ArgCluster], q.Args[ArgSource])
	if err != nil {
		return nil, err
	}

	out := make(chan backend.Result, 1)
	go func() {
		defer close(out)
		var after []any
		done := 0
		for _, ref := range sources {
			after = nil
			for page := 0; ; page++ {
				if ctx.Err() != nil {
					return
				}
				lq := LogQuery{
					Cluster: ref.Cluster,
					Source:  ref.Source.Name,
					Hosts:   q.Hosts,
					Query:   q.Args[ArgQuery],
					Window:  q.Window,
					Columns: splitColumns(q.Args[ArgColumns]),
					After:   after,
				}
				got, err := b.Logs(ctx, lq)
				res := backend.Result{Done: done, Total: len(sources)}
				if err != nil {
					res.Err = err
					select {
					case <-ctx.Done():
						return
					case out <- res:
					}
					break
				}
				res.Items = logResources(got.Rows, b.now())
				select {
				case <-ctx.Done():
					return
				case out <- res:
				}
				if !got.Full || len(got.After) == 0 {
					break
				}
				after = got.After
			}
			done++
		}
	}()
	return out, nil
}

// selectSources returns the sources a query may read: all of them, or the ones
// narrowed by cluster and source name.
func (b *Backend) selectSources(clusterName, sourceName string) ([]SourceRef, error) {
	clusterName = strings.TrimSpace(clusterName)
	sourceName = strings.TrimSpace(sourceName)
	var out []SourceRef
	known := false
	for _, c := range b.clusters {
		if clusterName != "" && c.Name != clusterName {
			continue
		}
		known = true
		for _, s := range c.Sources {
			src := s.Normalized()
			if sourceName != "" && src.Name != sourceName {
				continue
			}
			out = append(out, SourceRef{Cluster: c.Name, Source: src})
		}
	}
	if !known {
		return nil, errs.New(errs.Config, "elastic.List",
			fmt.Sprintf("unknown elastic cluster %q", clusterName))
	}
	if len(out) == 0 {
		return nil, errs.New(errs.Config, "elastic.List",
			fmt.Sprintf("no log source matches %q", sourceName))
	}
	return out, nil
}

// splitColumns splits the comma-separated column argument.
func splitColumns(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Get returns the row a ref names by listing its source and matching the
// document id. A log row is cheap to rebuild: the detail comes from the same
// search.
func (b *Backend) Get(ctx context.Context, ref resource.Ref) (resource.Resource, error) {
	kind, ok := normalizeKind(ref.Kind)
	if !ok || kind != KindLog {
		return resource.Resource{}, errs.New(errs.Config, "elastic.Get",
			fmt.Sprintf("unknown kind %q", ref.Kind))
	}
	cluster, source := splitNamespace(ref.Namespace)
	ch, err := b.List(ctx, resource.Query{
		Kind:  KindLog,
		Hosts: nil,
		Args:  map[string]string{ArgCluster: cluster, ArgSource: source},
	})
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
	return resource.Resource{}, errs.New(errs.NotAvailable, "elastic.Get", "no resource "+ref.String())
}

// splitNamespace splits a Ref namespace of the form "<cluster>/<source>".
func splitNamespace(ns string) (string, string) {
	if i := strings.IndexByte(ns, '/'); i >= 0 {
		return ns[:i], ns[i+1:]
	}
	return ns, ""
}

// Links returns the pivots from r. A log line has none: its host is already the
// session's target.
func (b *Backend) Links(r resource.Resource) []resource.Link { return nil }

// Health folds every configured cluster's `GET /` probe into one answer.
func (b *Backend) Health(ctx context.Context) backend.Health {
	if len(b.clusters) == 0 {
		return backend.Health{State: backend.HealthUnconfigured, Detail: "elasticsearch not configured", Checked: b.now()}
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
		h.Detail = fmt.Sprintf("%d/%d clusters healthy", up, len(statuses))
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

// Status returns one health per configured cluster, so the status line can show
// a dot per cluster.
func (b *Backend) Status(ctx context.Context) []ClusterStatus {
	out := make([]ClusterStatus, 0, len(b.clusters))
	for _, c := range b.clusters {
		out = append(out, ClusterStatus{Name: c.Name, Health: b.clusterHealth(ctx, c)})
	}
	return out
}

// clusterHealth probes one cluster, reusing an answer younger than healthTTL.
func (b *Backend) clusterHealth(ctx context.Context, c Cluster) backend.Health {
	now := b.now()
	b.mu.Lock()
	if e, ok := b.health[c.Name]; ok && now.Sub(e.at) >= 0 && now.Sub(e.at) < healthTTL {
		b.mu.Unlock()
		return e.h
	}
	b.mu.Unlock()

	h := backend.Health{Checked: now}
	if c.Client == nil {
		h.State, h.Detail = backend.HealthUnconfigured, "no client"
		return h
	}
	probe := c.Client.Healthy(ctx)
	h.Checked = b.now()
	h.Detail = probe.Detail
	if probe.Health {
		h.State = backend.HealthOK
	} else {
		h.State = backend.HealthDown
	}

	b.mu.Lock()
	b.health[c.Name] = healthEntry{h: h, at: h.Checked}
	b.mu.Unlock()
	return h
}

// logResources maps log rows onto resources of the log kind.
func logResources(rows []LogRow, now time.Time) []resource.Resource {
	out := make([]resource.Resource, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Resource(now))
	}
	return out
}

// Resource is one log row as a table row.
func (r LogRow) Resource(now time.Time) resource.Resource {
	return resource.Resource{
		Ref: resource.Ref{
			Backend:   backendName,
			Kind:      KindLog,
			Namespace: r.Cluster + "/" + r.Source,
			Name:      r.Index + "/" + r.ID,
		},
		Fields: map[string]any{
			"time":    r.Time,
			"level":   r.Level,
			"host":    r.Host,
			"message": r.Message,
		},
		Labels: map[string]string{
			"cluster": r.Cluster,
			"source":  r.Source,
			"index":   r.Index,
			"host":    r.Host,
			"level":   strings.ToLower(r.Level),
		},
		Status:    levelStatus(r.Level),
		Raw:       r,
		FetchedAt: now,
	}
}

// levelStatus colours a row by its log level.
func levelStatus(level string) resource.Status {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "error", "err", "fatal", "critical", "crit", "emergency", "alert":
		return resource.StatusCrit
	case "warn", "warning":
		return resource.StatusWarn
	case "info", "notice", "debug", "trace":
		return resource.StatusOK
	}
	return resource.StatusUnknown
}

// kindSpecs returns the log kind.
func kindSpecs() []resource.KindSpec {
	return []resource.KindSpec{
		{
			Name:        KindLog,
			Aliases:     []string{aliasLogs},
			Title:       "Logs",
			Scope:       resource.ScopeGlobal,
			DefaultSort: "time",
			Columns: []resource.Column{
				{ID: "time", Title: "TIME", Width: 20, Kind: resource.ColTime},
				{ID: "level", Title: "LEVEL", Width: 7},
				{ID: "host", Title: "HOST", Width: 22},
				{ID: "message", Title: "MESSAGE", Width: 0},
			},
		},
	}
}

// normalizeKind maps a kind name or alias onto its canonical name.
func normalizeKind(k resource.KindName) (resource.KindName, bool) {
	switch k {
	case KindLog, aliasLogs:
		return KindLog, true
	}
	return "", false
}

// logOrDiscard keeps the backend's logger non-nil without importing slog in
// every method: callers pass one, tests may not.
func logOrDiscard(l *slog.Logger) *slog.Logger {
	if l != nil {
		return l
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
