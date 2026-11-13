package plugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"jumper/internal/backend"
	"jumper/internal/backend/linux"
	"jumper/internal/errs"
	"jumper/internal/fanout"
	"jumper/internal/resource"
	"jumper/internal/transport"
)

// A loaded plugin behaves like a built-in kind (SPEC §12, W5): its name is a
// kind, its command runs through the fan-out on the targets of the query, its
// parsed rows are a table, and its links are pivots to other queries. This file
// builds that backend and the Manager that keeps it in step with the files.

const (
	// defaultTimeout bounds one plugin command. It matches the ssh exec budget
	// of BUILD-PLAN §11 when the caller does not pass one.
	defaultTimeout = 30 * time.Second
	// backendOp is the errs.Error op of every failure in this file.
	backendOp = "plugin.backend"
	// hostColumn is the column every plugin table starts with: the host the row
	// came from (or, with emits_hosts, the host the row names).
	hostColumn = "host"
)

// BaseName is the backend name of a plugin: the plugin name itself, so that
// `:plugins` and the action records read the same word. The kind has the same
// name, which is why `:<name>` opens the table.
func BaseName(def Definition) string { return def.Name }

// ReadCommandError is returned by CheckReadCommand when a plugin's read command
// does an obvious write. The message names the construct so the author knows
// what to move into `actions`.
type ReadCommandError struct{ What string }

func (e *ReadCommandError) Error() string {
	return "the read command " + e.What + "; reads run without confirmation, so writes belong in actions (which go through the bus)"
}

var (
	// allowedRedirectRe is the part of a shell line that is not a write: the
	// usual output silencing (`2>/dev/null`, `>/dev/null`, `2>&1`).
	allowedRedirectRe = regexp.MustCompile(`[0-9&]?>>?\s*(?:/dev/null|&[0-9]+)`)
	// teeRe, rmRe and serviceRe are the write constructs SPEC §12 forbids in a
	// read command.
	teeRe     = regexp.MustCompile(`(?:^|[\s;&|(])tee(?:\s|$)`)
	rmRe      = regexp.MustCompile(`(?:^|[\s;&|(])rm\s`)
	serviceRe = regexp.MustCompile(`systemctl\s+(?:start|stop|restart|try-restart|reload|enable|disable)\b`)
)

// CheckReadCommand reports whether a plugin's read command does an obvious
// write. Reads run without confirmation because the plugin author vouches for
// them (SPEC §12), so the loader refuses the constructs below; a write belongs
// in `actions`, which always go through the action bus.
func CheckReadCommand(command string) error {
	cleaned := allowedRedirectRe.ReplaceAllString(command, " ")
	switch {
	case strings.Contains(cleaned, ">"):
		return &ReadCommandError{What: "redirects output with >"}
	case teeRe.MatchString(cleaned):
		return &ReadCommandError{What: "writes through tee"}
	case rmRe.MatchString(cleaned):
		return &ReadCommandError{What: "removes files with rm"}
	case serviceRe.MatchString(cleaned):
		return &ReadCommandError{What: "starts, stops or restarts a service"}
	}
	return nil
}

// BackendOptions is what a Backend needs: the definition and the way to reach
// hosts. Select and Resolve are optional; without them a query must carry
// explicit hosts.
type BackendOptions struct {
	Definition Definition
	Exec       transport.Executor
	// Timeout bounds one command; 0 means defaultTimeout.
	Timeout time.Duration
	// Concurrency bounds hosts in flight; 0 means fanout's default.
	Concurrency int
	// Now is the clock for Health; nil means time.Now.
	Now func() time.Time
	// Select resolves a selector against the inventory (may be nil).
	Select func(resource.Selector) []resource.HostID
	// Resolve maps a name or alias to a canonical host id (may be nil).
	Resolve func(string) (resource.HostID, bool)
}

// Backend is one loaded plugin as a backend.Backend: kind per plugin, list per
// host through the fan-out, parsed rows, links as pivots.
type Backend struct {
	def         Definition
	exec        transport.Executor
	timeout     time.Duration
	concurrency int
	now         func() time.Time
	selectHosts func(resource.Selector) []resource.HostID
	resolveHost func(string) (resource.HostID, bool)
}

var _ backend.Backend = (*Backend)(nil)

// NewBackend builds the backend of one definition. It refuses a definition that
// is not a host plugin (the HTTP and inventory targets have their own paths) and
// a read command that writes.
func NewBackend(o BackendOptions) (*Backend, error) {
	def := o.Definition
	if strings.TrimSpace(def.Name) == "" {
		return nil, errs.New(errs.Config, backendOp, "plugin has no name")
	}
	if def.Target != TargetHost {
		return nil, errs.New(errs.Config, backendOp,
			fmt.Sprintf("plugin %q has target %q; only host plugins register a kind", def.Name, def.Target))
	}
	if err := CheckReadCommand(def.Command); err != nil {
		var rce *ReadCommandError
		if errors.As(err, &rce) {
			return nil, err
		}
		return nil, errs.Wrap(errs.Config, backendOp, err)
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Backend{
		def:         def,
		exec:        o.Exec,
		timeout:     timeout,
		concurrency: o.Concurrency,
		now:         now,
		selectHosts: o.Select,
		resolveHost: o.Resolve,
	}, nil
}

// Definition returns the definition the backend was built from. A reload
// replaces the whole backend, so this never changes underneath a viewer.
func (b *Backend) Definition() Definition { return b.def }

// Name is the backend name: the plugin name.
func (b *Backend) Name() string { return BaseName(b.def) }

// Kind is the kind the plugin serves: its own name.
func (b *Backend) Kind() resource.KindName { return resource.KindName(b.def.Name) }

// GroupByOutput reports view.group_by_output: the view of this kind is the
// grouped fan-out rather than a table.
func (b *Backend) GroupByOutput() bool { return b.def.View.GroupByOutput }

// UniformCommand reports whether one command serves every host, which is what
// the fan-out view needs: a command that substitutes {{ .Host }} renders
// differently per target and can only be listed as a table.
func (b *Backend) UniformCommand() bool {
	parts, err := parseTemplate(b.def.Command)
	if err != nil {
		return false
	}
	return !usesHost(parts)
}

// Kinds returns the one kind of the plugin: host-scoped, with a host column
// followed by the declared view columns.
func (b *Backend) Kinds() []resource.KindSpec {
	cols := make([]resource.Column, 0, 1+len(b.def.View.Columns))
	cols = append(cols, resource.Column{ID: hostColumn, Title: "HOST", Kind: resource.ColText})
	seen := map[string]bool{hostColumn: true}
	for _, id := range b.def.View.Columns {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		cols = append(cols, resource.Column{ID: id, Title: stringTitle(id), Kind: resource.ColText})
	}
	title := strings.TrimSpace(b.def.Description)
	if title == "" {
		title = b.def.Name
	}
	return []resource.KindSpec{{
		Name:        b.Kind(),
		Title:       title,
		Scope:       resource.ScopeHost,
		Columns:     cols,
		DefaultSort: hostColumn,
	}}
}

// stringTitle upper-cases a column id for the header: "max_age" → "MAX AGE".
func stringTitle(id string) string {
	return strings.ToUpper(strings.ReplaceAll(id, "_", " "))
}

// Command builds the transport command run for one host under args. Every
// substituted value is quoted by RenderCommand, so a hostile parameter value
// never becomes a second command.
func (b *Backend) Command(host resource.HostID, args map[string]string) (transport.Command, error) {
	script, err := RenderCommand(b.def.Command, args, string(host))
	if err != nil {
		return transport.Command{}, errs.Wrap(errs.Config, backendOp, err).WithHost(string(host))
	}
	return transport.Command{Script: script, Sudo: b.def.Privileged, Timeout: b.timeout}, nil
}

// Hosts resolves the target list of q: explicit hosts, then Args["host"], then
// the inventory selector. It is the same rule the built-in host-scoped kinds
// use, and it is exported so the grouped view resolves exactly the same hosts.
func (b *Backend) Hosts(q resource.Query) ([]resource.HostID, error) {
	if len(q.Hosts) > 0 {
		return dedupe(q.Hosts), nil
	}
	if raw := strings.TrimSpace(q.Args[hostColumn]); raw != "" {
		fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
		out := make([]resource.HostID, 0, len(fields))
		for _, f := range fields {
			out = append(out, resource.HostID(f))
		}
		if len(out) > 0 {
			return dedupe(out), nil
		}
	}
	if b.selectHosts != nil {
		if hosts := b.selectHosts(q.Selector); len(hosts) > 0 {
			return dedupe(hosts), nil
		}
	}
	return nil, errs.New(errs.Config, backendOp, "no targets: the query has no hosts and no selector matched")
}

// dedupe normalizes, sorts and de-duplicates hosts.
func dedupe(hosts []resource.HostID) []resource.HostID {
	seen := make(map[resource.HostID]bool, len(hosts))
	out := make([]resource.HostID, 0, len(hosts))
	for _, h := range hosts {
		h = resource.NormalizeHostID(string(h))
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// List streams one Result per host: the command through the fan-out, the output
// parsed into rows of this kind. A host whose command fails, or whose output
// does not parse, reports it in Result.Err; the rest still stream.
func (b *Backend) List(ctx context.Context, q resource.Query) (<-chan backend.Result, error) {
	if q.Kind != "" && q.Kind != b.Kind() {
		return nil, errs.New(errs.Config, backendOp,
			fmt.Sprintf("plugin %q does not serve kind %q", b.def.Name, q.Kind))
	}
	hosts, err := b.Hosts(q)
	if err != nil {
		return nil, err
	}
	if b.exec == nil {
		return nil, errs.New(errs.Config, backendOp, "no transport executor configured")
	}
	// Render every command before any work starts: a template that cannot be
	// filled is a query error, not a per-host failure.
	cmds := make([]transport.Command, len(hosts))
	for i, h := range hosts {
		c, cerr := b.Command(h, q.Args)
		if cerr != nil {
			return nil, cerr
		}
		cmds[i] = c
	}

	out := make(chan backend.Result)
	go func() {
		defer close(out)
		total := len(hosts)
		done := 0
		for _, grp := range groupByScript(hosts, cmds) {
			results := fanout.Run(ctx, b.exec, grp.hosts, grp.cmd, fanout.Options{Concurrency: b.concurrency})
			for hr := range results {
				done++
				res := b.result(hr, q, done, total)
				select {
				case <-ctx.Done():
					return
				case out <- res:
				}
			}
		}
	}()
	return out, nil
}

// scriptGroup is a set of hosts that run the same rendered command.
type scriptGroup struct {
	cmd   transport.Command
	hosts []resource.HostID
}

// groupByScript groups hosts by their rendered command, keeping the order in
// which the hosts first appeared. A command that does not substitute the host
// yields a single group, which is the common case: one fan-out for everybody.
func groupByScript(hosts []resource.HostID, cmds []transport.Command) []scriptGroup {
	index := make(map[string]int, len(hosts))
	out := make([]scriptGroup, 0, 1)
	for i, h := range hosts {
		key := cmds[i].Script
		if at, ok := index[key]; ok {
			out[at].hosts = append(out[at].hosts, h)
			continue
		}
		index[key] = len(out)
		out = append(out, scriptGroup{cmd: cmds[i], hosts: []resource.HostID{h}})
	}
	return out
}

// result turns one fan-out outcome into a backend.Result: the parsed rows, or
// the reason nothing could be read. A non-zero exit code with no rows is a
// failed read; with rows it is not, because plenty of read commands exit 1 on
// "nothing matched" and still print what they found.
func (b *Backend) result(hr fanout.HostResult, q resource.Query, done, total int) backend.Result {
	res := backend.Result{Host: hr.Host, Done: done, Total: total}
	if hr.Err != nil {
		res.Err = hr.Err
		return res
	}
	rows, err := Parse(b.def.Parse, hr.Result.Stdout)
	if err != nil {
		res.Err = errs.Wrap(errs.Parse, backendOp, err).WithHost(string(hr.Host))
		return res
	}
	items := b.items(hr.Host, rows)
	if len(items) == 0 && hr.Result.RC != 0 {
		res.Err = &errs.Error{Kind: errs.Remote, Op: backendOp, Host: string(hr.Host),
			Msg: fmt.Sprintf("rc %d: %s", hr.Result.RC, firstLine(string(hr.Result.Stderr)))}
		return res
	}
	res.Items = items
	return res
}

// items turns parsed rows into resources. With emits_hosts the row's host
// column names another machine, which becomes the row's host (and the pivot);
// otherwise the row belongs to the host the command ran on.
func (b *Backend) items(host resource.HostID, rows []map[string]any) []resource.Resource {
	items := make([]resource.Resource, 0, len(rows))
	for _, row := range rows {
		fields := make(map[string]any, len(row)+1)
		for k, v := range row {
			fields[k] = v
		}
		rowHost := host
		if b.def.EmitsHosts != "" {
			emitted := strings.TrimSpace(fieldString(row[hostColumn]))
			if emitted != "" {
				rowHost = b.canonicalHost(emitted)
				fields[hostColumn] = string(rowHost)
			}
		}
		if _, ok := fields[hostColumn]; !ok || fields[hostColumn] == nil {
			fields[hostColumn] = string(rowHost)
		}
		name := ""
		if b.def.EmitsHosts != "" {
			name = string(rowHost)
		}
		items = append(items, resource.Resource{
			Ref: resource.Ref{
				Backend: b.def.Name,
				Kind:    b.Kind(),
				Host:    rowHost,
				Name:    name,
			},
			Fields:    fields,
			Raw:       row,
			FetchedAt: b.now(),
		})
	}
	return items
}

// canonicalHost resolves an emitted host name through the inventory when one is
// wired; without one the normalized name is the best identity available.
func (b *Backend) canonicalHost(name string) resource.HostID {
	if b.resolveHost != nil {
		if id, ok := b.resolveHost(name); ok && id != "" {
			return id
		}
	}
	return resource.NormalizeHostID(name)
}

// fieldString renders one parsed value as text, for the host column.
func fieldString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

// RenderArgs expands a link or query template without shell quoting: an
// argument value is data in a Query, not part of a command line. The allowed
// substitutions are the ones parseTemplate accepts, so a template can still only
// reach its declared parameters and the host.
func RenderArgs(text string, values map[string]string, host string) (string, error) {
	parts, err := parseTemplate(text)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range parts {
		switch {
		case p.param != "":
			v, ok := values[p.param]
			if !ok {
				return "", fmt.Errorf("parameter %q has no value", p.param)
			}
			b.WriteString(v)
		case p.host:
			b.WriteString(host)
		default:
			b.WriteString(p.lit)
		}
	}
	return b.String(), nil
}

// fileBase is the last element of a path, without touching the file system.
func fileBase(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[i+1:]
	}
	return path
}

// firstLine is the first non-empty line of s, trimmed.
func firstLine(s string) string {
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if v := strings.TrimSpace(line); v != "" {
			if len(v) > 120 {
				return v[:119] + "…"
			}
			return v
		}
	}
	return ""
}

// Get reads one row of this kind. A host-scoped plugin row has no name of its
// own, so ref.Host names the machine and the first parsed row is returned.
func (b *Backend) Get(ctx context.Context, ref resource.Ref) (resource.Resource, error) {
	host := resource.NormalizeHostID(string(ref.Host))
	if host == "" {
		host = b.canonicalHost(ref.Name)
	}
	if host == "" {
		return resource.Resource{}, errs.New(errs.Config, backendOp, "ref has no host")
	}
	cmd, err := b.Command(host, nil)
	if err != nil {
		return resource.Resource{}, err
	}
	if b.exec == nil {
		return resource.Resource{}, errs.New(errs.Config, backendOp, "no transport executor configured")
	}
	res, err := b.exec.Exec(ctx, host, cmd)
	if err != nil {
		return resource.Resource{}, err
	}
	rows, perr := Parse(b.def.Parse, res.Stdout)
	if perr != nil {
		return resource.Resource{}, errs.Wrap(errs.Parse, backendOp, perr).WithHost(string(host))
	}
	items := b.items(host, rows)
	if len(items) == 0 {
		return resource.Resource{}, errs.New(errs.NotAvailable, backendOp,
			fmt.Sprintf("plugin %q produced no row for %s", b.def.Name, host)).WithHost(string(host))
	}
	return items[0], nil
}

// Links returns the pivots of a row: the host overview first (so `enter` opens
// it, after the inventory resolved the name), then the links the definition
// declares, with their arguments rendered from the row.
func (b *Backend) Links(r resource.Resource) []resource.Link {
	if r.Ref.Kind != "" && r.Ref.Kind != b.Kind() {
		return nil
	}
	host := resource.NormalizeHostID(string(r.Ref.Host))
	if host == "" {
		host = resource.NormalizeHostID(fieldString(r.Fields[hostColumn]))
	}
	var out []resource.Link
	if host != "" {
		out = append(out, resource.Link{
			Label: "host overview",
			Key:   "enter",
			To:    resource.Query{Kind: linux.KindHostOverview, Hosts: []resource.HostID{host}},
		})
	}
	for _, l := range b.def.Links {
		to, ok := b.linkQuery(l, r, host)
		if !ok {
			continue
		}
		out = append(out, resource.Link{Label: l.Label, To: to})
	}
	return out
}

// linkQuery renders one declared link into a query. A link whose argument
// template cannot be filled from the row is skipped: links are a pure function
// and must never fail a view.
func (b *Backend) linkQuery(l LinkDef, r resource.Resource, host resource.HostID) (resource.Query, bool) {
	to := strings.TrimSpace(l.To)
	if to == "" {
		return resource.Query{}, false
	}
	args := make(map[string]string, len(l.Args))
	values := make(map[string]string, len(r.Fields))
	for k, v := range r.Fields {
		values[k] = fieldString(v)
	}
	for k, tmpl := range l.Args {
		text, err := RenderArgs(tmpl, values, string(host))
		if err != nil {
			return resource.Query{}, false
		}
		args[k] = text
	}
	q := resource.Query{Kind: resource.KindName(to), Args: args}
	if host != "" {
		q.Hosts = []resource.HostID{host}
	}
	return q, true
}

// Health reports the plugin as configured when it loaded; the manager is what
// knows whether the file went bad afterwards.
func (b *Backend) Health(context.Context) backend.Health {
	detail := "plugin loaded"
	if b.def.File != "" {
		detail += ": " + b.def.File
	}
	return backend.Health{State: backend.HealthOK, Detail: detail, Checked: b.now()}
}

// ManagerOptions is what the Manager needs: the plugin directory, the registry
// to keep in step and the way to reach hosts.
type ManagerOptions struct {
	// Dir is the plugins directory (~/.config/jumper/plugins). Empty means no
	// plugins are ever loaded.
	Dir string
	// Registry is where the plugin kinds are registered. Required.
	Registry *backend.Registry
	// Exec runs plugin commands and actions; nil leaves reads and actions
	// unavailable (a plugin still registers its kind).
	Exec transport.Executor
	// Timeout bounds one command; 0 means defaultTimeout.
	Timeout time.Duration
	// Concurrency bounds hosts in flight.
	Concurrency int
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Select and Resolve are the inventory lookups (both may be nil).
	Select  func(resource.Selector) []resource.HostID
	Resolve func(string) (resource.HostID, bool)
	// Log receives one line per reload with problems.
	Log *slog.Logger
}

// Plugin states as `:plugins` shows them.
const (
	// StateLoaded is a plugin whose current file loaded and registered.
	StateLoaded = "loaded"
	// StateError is a plugin whose current file did not load. A plugin that
	// loaded before keeps that version registered.
	StateError = "error"
	// StateNotKind is a plugin that loaded but serves no kind here: an HTTP or
	// inventory plugin is read by its own path, not by a host command.
	StateNotKind = "not a kind"
)

// Plugin is one entry of the `:plugins` listing.
type Plugin struct {
	Name        string
	Target      string
	Description string
	Path        string
	State       string
	Problems    []Problem
}

// Manager keeps the plugin backends in step with the plugin directory. Reload
// loads every file, registers new kinds and swaps changed ones through
// Registry.Replace; a file that no longer loads keeps its last good version
// registered and reports its problems instead.
type Manager struct {
	mu       sync.RWMutex
	dir      string
	registry *backend.Registry
	opts     ManagerOptions
	defs     map[string]Definition
	backs    map[string]*Backend
	problems []Problem
}

// NewManager builds a Manager. Nothing is loaded until Reload (or Load) is
// called, so the caller decides what a broken directory means.
func NewManager(o ManagerOptions) (*Manager, error) {
	if o.Registry == nil {
		return nil, errs.New(errs.Config, backendOp, "plugin manager needs a registry")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Manager{
		dir:      o.Dir,
		registry: o.Registry,
		opts:     o,
		defs:     make(map[string]Definition),
		backs:    make(map[string]*Backend),
	}, nil
}

// Dir is the plugin directory the manager watches.
func (m *Manager) Dir() string { return m.dir }

// Load loads the directory for the first time and returns the problems it
// found. It is Reload under a name that reads better at start-up.
func (m *Manager) Load() []Problem { return m.Reload() }

// Reload reads the plugin directory and brings the registry in step with it.
// Every problem is returned with its file and line; a broken file never
// unregisters the version that was loaded before. The first registered backend
// of a name wins, so a file that only changes the description still swaps
// cleanly.
func (m *Manager) Reload() []Problem {
	if m == nil {
		return nil
	}
	ok, problems := LoadDir(m.dir)

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, def := range ok {
		if def.Target != TargetHost {
			// An HTTP or inventory plugin has no host command, so it registers
			// no kind: its own track reads it. It is still listed, and never a
			// problem here.
			m.defs[def.Name] = def
			continue
		}
		b, err := NewBackend(BackendOptions{
			Definition:  def,
			Exec:        m.opts.Exec,
			Timeout:     m.opts.Timeout,
			Concurrency: m.opts.Concurrency,
			Now:         m.opts.Now,
			Select:      m.opts.Select,
			Resolve:     m.opts.Resolve,
		})
		if err != nil {
			problems = append(problems, problemFor(def, err))
			continue
		}
		if _, loaded := m.backs[def.Name]; loaded {
			if err := m.registry.Replace(b); err != nil {
				problems = append(problems, problemFor(def, err))
				continue
			}
		} else if err := m.registry.Register(b); err != nil {
			problems = append(problems, problemFor(def, err))
			continue
		}
		m.defs[def.Name] = def
		m.backs[def.Name] = b
	}
	sort.SliceStable(problems, func(i, j int) bool {
		if problems[i].File != problems[j].File {
			return problems[i].File < problems[j].File
		}
		return problems[i].Line < problems[j].Line
	})
	m.problems = problems
	if m.opts.Log != nil && len(problems) > 0 {
		m.opts.Log.Warn("plugin problems", "dir", m.dir, "problems", len(problems))
	}
	out := make([]Problem, len(m.problems))
	copy(out, m.problems)
	return out
}

// problemFor turns a registration error into a Problem that points at the file
// and the name line of the definition.
func problemFor(def Definition, err error) Problem {
	p := Problem{File: def.File, Line: def.Line, Field: "name", Msg: err.Error()}
	if p.Line == 0 {
		p.Line = 1
	}
	return p
}

// Problems returns the problems of the last load, sorted by file and line.
func (m *Manager) Problems() []Problem {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Problem, len(m.problems))
	copy(out, m.problems)
	return out
}

// Definitions returns the loaded definitions, sorted by name.
func (m *Manager) Definitions() []Definition {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Definition, 0, len(m.defs))
	for _, d := range m.defs {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Backend returns the backend of one loaded plugin.
func (m *Manager) Backend(name string) (*Backend, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.backs[name]
	return b, ok
}

// Plugins is the `:plugins` listing: every loaded plugin with its file and
// state, plus one entry per file that has problems, so a broken edit is visible
// next to the version still serving.
func (m *Manager) Plugins() []Plugin {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	byFile := make(map[string][]Problem, len(m.problems))
	for _, p := range m.problems {
		byFile[p.File] = append(byFile[p.File], p)
	}
	out := make([]Plugin, 0, len(m.defs)+len(byFile))
	for _, d := range m.defs {
		entry := Plugin{
			Name:        d.Name,
			Target:      d.Target,
			Description: d.Description,
			Path:        d.File,
			State:       StateLoaded,
		}
		if _, hasKind := m.backs[d.Name]; !hasKind {
			entry.State = StateNotKind
		}
		if probs := byFile[d.File]; len(probs) > 0 {
			// The file is broken now; the previous version is still serving.
			entry.State = StateError
			entry.Problems = probs
			delete(byFile, d.File)
		}
		out = append(out, entry)
	}
	// Files that never loaded (or have no usable name) still get a line.
	files := make([]string, 0, len(byFile))
	for file := range byFile {
		files = append(files, file)
	}
	sort.Strings(files)
	for _, file := range files {
		out = append(out, Plugin{
			Name:     strings.TrimSuffix(strings.TrimSuffix(fileBase(file), ".yaml"), ".yml"),
			Path:     file,
			State:    StateError,
			Problems: byFile[file],
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Path < out[j].Path
	})
	return out
}
