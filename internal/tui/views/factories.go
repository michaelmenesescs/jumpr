package views

import (
	"fmt"
	"strings"

	"jumper/internal/backend/linux"
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
	}
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
