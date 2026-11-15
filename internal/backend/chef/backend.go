package chef

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"jumpr/internal/backend"
	"jumpr/internal/errs"
	"jumpr/internal/redact"
	"jumpr/internal/resource"
)

// backendName is used in every Ref and in error Ops.
const backendName = "chef"

const (
	// healthTTL is how long a Health answer is reused. The status line polls
	// every 30 s and `knife node list` is one round trip.
	healthTTL = 30 * time.Second
	// healthTimeout bounds the knife call Health makes.
	healthTimeout = 5 * time.Second
	// defaultStaleAfter is the check-in age that turns a node Warn (task 113
	// requirement 3).
	defaultStaleAfter = 2 * time.Hour
	// defaultCriticalAfter is the check-in age that turns a node Crit.
	defaultCriticalAfter = 24 * time.Hour
)

// Options configures a Backend. Runner and Workstation are what make it useful;
// Workstation empty means Chef is unconfigured. A nil Redactor only caps the
// node attributes it does not redact; callers pass one in production.
type Options struct {
	Runner      *Runner
	Workstation resource.HostID
	Redactor    *redact.Redactor
	Now         func() time.Time
	Log         *slog.Logger
	// StaleAfter and CriticalAfter are the check-in thresholds; zero means
	// 2 h and 24 h.
	StaleAfter    time.Duration
	CriticalAfter time.Duration
}

// Backend implements backend.Backend for Chef. Everything it does is read-only:
// it runs knife on the workstation over the runner.
type Backend struct {
	runner   *Runner
	ws       resource.HostID
	red      *redact.Redactor
	now      func() time.Time
	log      *slog.Logger
	stale    time.Duration
	critical time.Duration

	mu       sync.Mutex
	health   backend.Health
	healthAt time.Time
	healthOK bool
}

var _ backend.Backend = (*Backend)(nil)

// New builds a Chef backend from o.
func New(o Options) *Backend {
	b := &Backend{
		runner:   o.Runner,
		ws:       resource.NormalizeHostID(string(o.Workstation)),
		red:      o.Redactor,
		now:      o.Now,
		log:      o.Log,
		stale:    o.StaleAfter,
		critical: o.CriticalAfter,
	}
	if b.ws == "" && b.runner != nil {
		b.ws = b.runner.Workstation()
	}
	if b.now == nil {
		b.now = time.Now
	}
	if b.log == nil {
		b.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if b.stale <= 0 {
		b.stale = defaultStaleAfter
	}
	if b.critical <= 0 {
		b.critical = defaultCriticalAfter
	}
	return b
}

// Name returns the backend name used in Refs and registry lookups.
func (b *Backend) Name() string { return backendName }

// Kinds returns the read-only Chef kinds.
func (b *Backend) Kinds() []resource.KindSpec { return kindSpecs() }

// configured reports whether the backend has a workstation and a way to reach
// it.
func (b *Backend) configured() bool { return b.ws != "" && b.runner != nil }

// List streams one Result holding every row the kind has. The Chef kinds are
// global, so there is no per-host batch to stream.
func (b *Backend) List(ctx context.Context, q resource.Query) (<-chan backend.Result, error) {
	kind, ok := normalizeKind(q.Kind)
	if !ok {
		return nil, errs.New(errs.Config, "chef.List", fmt.Sprintf("unknown kind %q", q.Kind))
	}
	if !b.configured() {
		return nil, errs.New(errs.Config, "chef.List", "chef backend not configured: no workstation or no runner")
	}
	out := make(chan backend.Result, 1)
	go func() {
		defer close(out)
		items, err := b.list(ctx, kind)
		res := backend.Result{Items: items, Done: 1, Total: 1}
		if err != nil {
			res = backend.Result{Err: err, Done: 1, Total: 1}
		}
		select {
		case out <- res:
		case <-ctx.Done():
		}
	}()
	return out, nil
}

// list builds the rows of one kind. Each kind is one knife call, except the role
// and environment lists, which count their nodes with a best-effort status call.
func (b *Backend) list(ctx context.Context, kind resource.KindName) ([]resource.Resource, error) {
	switch kind {
	case KindNode:
		nodes, err := b.status(ctx)
		if err != nil {
			return nil, err
		}
		now := b.now()
		out := make([]resource.Resource, 0, len(nodes))
		for _, n := range nodes {
			out = append(out, b.nodeResource(n, now))
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Ref.Name < out[j].Ref.Name })
		return out, nil
	case KindRole, KindEnv:
		names, err := b.nameList(ctx, kind)
		if err != nil {
			return nil, err
		}
		roleCounts, envCounts := b.nodeCounts(ctx)
		counts := roleCounts
		if kind == KindEnv {
			counts = envCounts
		}
		now := b.now()
		out := make([]resource.Resource, 0, len(names))
		for _, name := range names {
			row := resource.Resource{
				Ref:       resource.Ref{Backend: backendName, Kind: kind, Name: name},
				Fields:    map[string]any{"name": name},
				Status:    resource.StatusOK,
				FetchedAt: now,
			}
			if n, ok := counts[name]; ok {
				row.Fields["nodes"] = int64(n)
			}
			out = append(out, row)
		}
		return out, nil
	default:
		return nil, errs.New(errs.Config, "chef.list", fmt.Sprintf("unknown kind %q", kind))
	}
}

// status runs `knife status -F json`.
func (b *Backend) status(ctx context.Context) ([]StatusNode, error) {
	out, err := b.runner.Run(ctx, "status")
	if err != nil {
		return nil, err
	}
	return ParseStatus(out)
}

// nameList runs the list command of the role or environment kind.
func (b *Backend) nameList(ctx context.Context, kind resource.KindName) ([]string, error) {
	word := "role"
	if kind == KindEnv {
		word = "environment"
	}
	out, err := b.runner.Run(ctx, word, "list")
	if err != nil {
		return nil, err
	}
	return ParseNodeNames(out)
}

// nodeCounts counts the nodes per role and per environment from one status
// call. A failed call yields no counts rather than failing the list: the names
// are still worth showing.
func (b *Backend) nodeCounts(ctx context.Context) (roles, envs map[string]int) {
	roles = map[string]int{}
	envs = map[string]int{}
	nodes, err := b.status(ctx)
	if err != nil {
		if b.log != nil {
			b.log.Debug("chef node counts unavailable", "err", err.Error())
		}
		return roles, envs
	}
	for _, n := range nodes {
		if n.Environment != "" {
			envs[n.Environment]++
		}
		for _, role := range rolesFromRunList(n.RunList) {
			roles[role]++
		}
	}
	return roles, envs
}

// nodeResource turns one status row into a table row: the environment, the
// platform, the roles of its run list, the run list itself and the check-in age.
func (b *Backend) nodeResource(n StatusNode, now time.Time) resource.Resource {
	roles := rolesFromRunList(n.RunList)
	platform := n.Platform
	if n.PlatformVersion != "" {
		platform = strings.TrimSpace(platform + " " + n.PlatformVersion)
	}
	fields := map[string]any{
		"name":        n.Name,
		"environment": n.Environment,
		"platform":    platform,
		"roles":       strings.Join(roles, ","),
		"run_list":    shortRunList(n.RunList),
	}
	status := resource.StatusNA
	if !n.OhaiTime.IsZero() {
		age := now.Sub(n.OhaiTime)
		if age < 0 {
			age = 0
		}
		fields["checkin"] = age
		status = b.checkinStatus(age)
	}
	labels := map[string]string{
		"chef.environment": n.Environment,
		"chef.roles":       strings.Join(roles, ","),
		"chef.platform":    n.Platform,
	}
	return resource.Resource{
		Ref:       resource.Ref{Backend: backendName, Kind: KindNode, Name: n.Name},
		Fields:    fields,
		Labels:    labels,
		Status:    status,
		Raw:       n,
		FetchedAt: now,
	}
}

// checkinStatus maps a check-in age onto a status: OK, Warn past stale, Crit
// past critical.
func (b *Backend) checkinStatus(age time.Duration) resource.Status {
	switch {
	case age > b.critical:
		return resource.StatusCrit
	case age > b.stale:
		return resource.StatusWarn
	default:
		return resource.StatusOK
	}
}

// Get returns one node, role or environment. A node comes back with the full
// detail of `knife node show -l` in Raw, so the detail view does not need a
// second round trip.
func (b *Backend) Get(ctx context.Context, ref resource.Ref) (resource.Resource, error) {
	const op = "chef.Get"
	kind, ok := normalizeKind(ref.Kind)
	if !ok {
		return resource.Resource{}, errs.New(errs.Config, op, fmt.Sprintf("unknown kind %q", ref.Kind))
	}
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return resource.Resource{}, errs.New(errs.Config, op, "ref needs a name")
	}
	if !b.configured() {
		return resource.Resource{}, errs.New(errs.Config, op, "chef backend not configured: no workstation or no runner")
	}
	switch kind {
	case KindNode:
		d, err := b.NodeDetail(ctx, name)
		if err != nil {
			return resource.Resource{}, err
		}
		row := b.nodeResource(statusNodeOf(d), d.FetchedAt)
		row.Raw = d
		return row, nil
	case KindRole, KindEnv:
		word := "role"
		if kind == KindEnv {
			word = "environment"
		}
		out, err := b.runner.Run(ctx, word, "show", name)
		if err != nil {
			return resource.Resource{}, err
		}
		data, err := FirstJSON(out)
		if err != nil {
			return resource.Resource{}, err
		}
		var raw any
		if err := DecodeInto(data, &raw); err != nil {
			return resource.Resource{}, err
		}
		return resource.Resource{
			Ref:       resource.Ref{Backend: backendName, Kind: kind, Name: name},
			Fields:    map[string]any{"name": name},
			Status:    resource.StatusOK,
			Raw:       raw,
			FetchedAt: b.now(),
		}, nil
	default:
		return resource.Resource{}, errs.New(errs.Config, op, fmt.Sprintf("unknown kind %q", kind))
	}
}

// statusNodeOf reduces a node detail to the status row fields.
func statusNodeOf(d NodeDetail) StatusNode {
	return StatusNode{
		Name:            d.Name,
		Environment:     d.Environment,
		Platform:        d.Platform,
		PlatformVersion: d.PlatformVersion,
		RunList:         d.RunList,
		OhaiTime:        ohaiTimeOf(d),
	}
}

// ohaiTimeOf reads the automatic attributes' ohai_time, which is where a node
// keeps its last check-in.
func ohaiTimeOf(d NodeDetail) time.Time {
	for _, g := range d.Attributes {
		if g.Precedence != "automatic" {
			continue
		}
		for _, line := range g.Lines {
			key, value, ok := strings.Cut(line, " = ")
			if !ok || key != "ohai_time" {
				continue
			}
			if f, err := parseFloat(value); err == nil {
				return epochTime(f)
			}
		}
	}
	return time.Time{}
}

// NodeDetail runs `knife node show <n> -l -F json` and returns the detail the
// view's summary, run-list and attribute tabs render.
func (b *Backend) NodeDetail(ctx context.Context, name string) (NodeDetail, error) {
	const op = "chef.NodeDetail"
	name = strings.TrimSpace(name)
	if !validName(name) {
		return NodeDetail{}, nameError(op, "node", name)
	}
	if !b.configured() {
		return NodeDetail{}, errs.New(errs.Config, op, "chef backend not configured: no workstation or no runner")
	}
	out, err := b.runner.Run(ctx, "node", "show", name, "-l")
	if err != nil {
		return NodeDetail{}, err
	}
	d, err := ParseNodeDetail(out, b.red)
	if err != nil {
		return NodeDetail{}, err
	}
	if d.Name == "" {
		d.Name = name
	}
	d.FetchedAt = b.now()
	return d, nil
}

// Search runs `knife search node <query> -a <attr>` and returns the rows. The
// query is Solr syntax typed by the user: it is passed as one quoted argument,
// never interpolated into the command line (task 113 pitfall).
func (b *Backend) Search(ctx context.Context, query string, attrs ...string) ([]SearchRow, error) {
	const op = "chef.Search"
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errs.New(errs.Config, op, "empty search query")
	}
	if !b.configured() {
		return nil, errs.New(errs.Config, op, "chef backend not configured: no workstation or no runner")
	}
	args := make([]string, 0, 4+2*len(attrs))
	args = append(args, "search", "node", query)
	for _, a := range attrs {
		a = strings.TrimSpace(a)
		if !validAttr(a) {
			return nil, errs.New(errs.Config, op, fmt.Sprintf("invalid attribute name %q", a))
		}
		args = append(args, "-a", a)
	}
	out, err := b.runner.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return ParseSearch(out)
}

// Links returns the pivots from a Chef row: a node is a host, so `enter` opens
// the host the node describes.
func (b *Backend) Links(r resource.Resource) []resource.Link {
	kind, ok := normalizeKind(r.Ref.Kind)
	if !ok || kind != KindNode {
		return nil
	}
	name := strings.TrimSpace(r.Ref.Name)
	if name == "" {
		return nil
	}
	return []resource.Link{{
		Label: "host",
		Key:   "enter",
		To:    resource.Query{Kind: "host", Hosts: []resource.HostID{resource.NormalizeHostID(name)}},
	}}
}

// Health probes the Chef server with `knife node list` and caches the answer for
// healthTTL.
func (b *Backend) Health(ctx context.Context) backend.Health {
	if !b.configured() {
		return backend.Health{State: backend.HealthUnconfigured, Detail: "chef not configured", Checked: b.now()}
	}
	b.mu.Lock()
	if b.healthOK && b.now().Sub(b.healthAt) < healthTTL {
		h := b.health
		b.mu.Unlock()
		return h
	}
	b.mu.Unlock()

	h := b.probeHealth(ctx)

	b.mu.Lock()
	b.health, b.healthAt, b.healthOK = h, b.now(), true
	b.mu.Unlock()
	return h
}

// probeHealth runs the health round trip once.
func (b *Backend) probeHealth(ctx context.Context) backend.Health {
	cctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	out, err := b.runner.Run(cctx, "node", "list")
	if err != nil {
		return backend.Health{State: backend.HealthDown, Detail: b.redact(err.Error()), Checked: b.now()}
	}
	names, err := ParseNodeNames(out)
	if err != nil {
		return backend.Health{State: backend.HealthDown, Detail: b.redact(err.Error()), Checked: b.now()}
	}
	return backend.Health{
		State:   backend.HealthOK,
		Detail:  fmt.Sprintf("%d nodes", len(names)),
		Checked: b.now(),
	}
}

// redact passes s through the redactor when there is one.
func (b *Backend) redact(s string) string {
	if b.red == nil {
		return s
	}
	return b.red.String(s)
}

// parseFloat parses a decimal number.
func parseFloat(s string) (float64, error) {
	return strconv.ParseFloat(strings.TrimSpace(s), 64)
}
