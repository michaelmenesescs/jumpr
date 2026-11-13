package views

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend"
	"jumper/internal/backend/linux"
	"jumper/internal/backend/salt"
	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/tui"
)

// Factories returns the dedicated views, keyed by kind. app merges them into
// Env.Factories; kinds without one fall back to the generic table.
func Factories() map[resource.KindName]tui.Factory {
	return map[resource.KindName]tui.Factory{
		linux.KindJournal:      journalFactory,
		linux.KindHostOverview: hostFactory,
		linux.KindUnitDetail:   unitDetailFactory,
		salt.KindJobs:          jobsFactory,
		KindSaltJobDetail:      saltJobFactory,
		KindSaltState:          saltStateFactory,
		salt.KindSls:           saltTreeFactory,
		salt.KindMine:          mineFactory,
		KindGrains:             grainsFactory,
		KindPillar:             pillarFactory,
		KindSaltEvents:         saltEventsFactory,
		tui.KindTimeline:       timelineFactory,
	}
}

// timelineFactory builds the investigation timeline. It is keyed by the
// synthetic kind tui.KindTimeline, which the root pushes for `:timeline` and
// `T`; no backend serves it.
func timelineFactory(env *tui.Env, _ resource.Query) (tui.View, error) {
	return NewTimeline(env), nil
}

// journalFactory builds the journal view for a query, resolving the backend
// through the registry like the root does.
func journalFactory(env *tui.Env, q resource.Query) (tui.View, error) {
	if env == nil || env.Registry == nil {
		return nil, errs.New(errs.Config, "views.journalFactory", "no backends configured")
	}
	b, _, ok := env.Registry.Lookup(string(q.Kind))
	if !ok {
		return nil, errs.New(errs.Config, "views.journalFactory", fmt.Sprintf("unknown kind %q", q.Kind))
	}
	return NewJournal(env, b, q), nil
}

// hostFactory builds the one-screen host overview for the query's host.
func hostFactory(env *tui.Env, q resource.Query) (tui.View, error) {
	if env == nil || env.Registry == nil {
		return nil, errs.New(errs.Config, "views.hostFactory", "no backends configured")
	}
	b, _, ok := env.Registry.Lookup(string(q.Kind))
	if !ok {
		return nil, errs.New(errs.Config, "views.hostFactory", fmt.Sprintf("unknown kind %q", q.Kind))
	}
	host := hostFromQuery(q)
	if host == "" {
		return nil, errs.New(errs.Config, "views.hostFactory", "overview needs a host")
	}
	return NewHost(env, b, host), nil
}

// unitDetailFactory builds the tabbed detail of the query's unit. The query
// carries the host and the unit name (the unit row's `enter` link sets both).
func unitDetailFactory(env *tui.Env, q resource.Query) (tui.View, error) {
	if env == nil || env.Registry == nil {
		return nil, errs.New(errs.Config, "views.unitDetailFactory", "no backends configured")
	}
	b, _, ok := env.Registry.Lookup(string(linux.KindUnit))
	if !ok {
		return nil, errs.New(errs.Config, "views.unitDetailFactory", "no backend serves the unit kind")
	}
	host := hostFromQuery(q)
	if host == "" {
		return nil, errs.New(errs.Config, "views.unitDetailFactory", "unit detail needs a host")
	}
	unit := strings.TrimSpace(q.Args["unit"])
	if unit == "" {
		return nil, errs.New(errs.Config, "views.unitDetailFactory", "unit detail needs a unit")
	}
	return NewUnit(env, b, host, unit), nil
}

// jobsFactory builds the jobs browser: the generic table plus the auto-refresh
// hook a running job needs.
func jobsFactory(env *tui.Env, q resource.Query) (tui.View, error) {
	if env == nil || env.Registry == nil {
		return nil, errs.New(errs.Config, "views.jobsFactory", "no backends configured")
	}
	b, spec, ok := env.Registry.Lookup(string(q.Kind))
	if !ok {
		return nil, errs.New(errs.Config, "views.jobsFactory", fmt.Sprintf("unknown kind %q", q.Kind))
	}
	return newJobsTable(env, b, spec, q), nil
}

// jobsTable is the jobs list. It is the generic Table plus AutoRefresh, so the
// root re-lists the jobs every few seconds while a job is still running.
type jobsTable struct {
	*Table
}

// newJobsTable wraps a fresh table over the jobs kind.
func newJobsTable(env *tui.Env, b backend.Backend, spec resource.KindSpec, q resource.Query) *jobsTable {
	return &jobsTable{Table: NewTable(env, b, spec, q)}
}

// Update keeps the wrapper around the table Update returns.
func (v *jobsTable) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	view, cmd := v.Table.Update(msg)
	if next, ok := view.(*Table); ok {
		return &jobsTable{Table: next}, cmd
	}
	return view, cmd
}

// AutoRefresh is zero unless one of the listed jobs is still running.
func (v *jobsTable) AutoRefresh() time.Duration {
	for _, r := range v.Table.rows {
		if r.Str("state") == salt.JobRunning {
			return saltJobAutoRefresh
		}
	}
	return 0
}

// saltJobFactory builds the grouped detail of the query's job.
func saltJobFactory(env *tui.Env, q resource.Query) (tui.View, error) {
	if env == nil || env.Registry == nil {
		return nil, errs.New(errs.Config, "views.saltJobFactory", "no backends configured")
	}
	jid := strings.TrimSpace(q.Args["jid"])
	if jid == "" {
		return nil, errs.New(errs.Config, "views.saltJobFactory", "job detail needs a jid")
	}
	if name := strings.TrimSpace(q.Backend); name != "" {
		if b, ok := env.Registry.Get(name); ok {
			if sb, ok := b.(SaltJobBackend); ok {
				return NewSaltJob(env, sb, jid), nil
			}
		}
	}
	b, _, ok := env.Registry.Lookup(string(salt.KindJobs))
	if !ok {
		return nil, errs.New(errs.Config, "views.saltJobFactory", "no backend serves salt jobs")
	}
	sb, ok := b.(SaltJobBackend)
	if !ok {
		return nil, errs.New(errs.Config, "views.saltJobFactory", "backend does not read jobs")
	}
	return NewSaltJob(env, sb, jid), nil
}

// saltStateFactory builds the dry-run state view of the query's sls. It is
// keyed by the synthetic kind salt-state; the salt backend runs the dry run.
func saltStateFactory(env *tui.Env, q resource.Query) (tui.View, error) {
	if env == nil || env.Registry == nil {
		return nil, errs.New(errs.Config, "views.saltStateFactory", "no backends configured")
	}
	if name := strings.TrimSpace(q.Backend); name != "" {
		if b, ok := env.Registry.Get(name); ok {
			if sb, ok := b.(SaltStateBackend); ok {
				return NewSaltState(env, sb, q.Args["sls"]), nil
			}
		}
	}
	b, _, ok := env.Registry.Lookup(string(salt.KindMinion))
	if !ok {
		return nil, errs.New(errs.Config, "views.saltStateFactory", "no backend serves salt minions")
	}
	sb, ok := b.(SaltStateBackend)
	if !ok {
		return nil, errs.New(errs.Config, "views.saltStateFactory", "backend cannot run a state dry run")
	}
	return NewSaltState(env, sb, q.Args["sls"]), nil
}

// hostFromQuery is the single host a query names: Query.Hosts first, then the
// "host" argument the `overview --host <name>` command line sets.
func hostFromQuery(q resource.Query) resource.HostID {
	if len(q.Hosts) > 0 {
		return resource.NormalizeHostID(string(q.Hosts[0]))
	}
	if h := strings.TrimSpace(q.Args["host"]); h != "" {
		return resource.NormalizeHostID(h)
	}
	return ""
}
