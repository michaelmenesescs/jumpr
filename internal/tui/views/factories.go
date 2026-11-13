package views

import (
	"fmt"

	"jumper/internal/backend/linux"
	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/tui"
)

// Factories returns the dedicated views, keyed by kind. app merges them into
// Env.Factories; kinds without one fall back to the generic table.
func Factories() map[resource.KindName]tui.Factory {
	return map[resource.KindName]tui.Factory{
		linux.KindJournal: journalFactory,
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
