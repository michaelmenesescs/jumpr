package tui

import (
	"log/slog"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/action"
	"jumper/internal/backend"
	"jumper/internal/bus"
	"jumper/internal/inventory"
	"jumper/internal/investigation"
	"jumper/internal/redact"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/targets"
	"jumper/internal/transport"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
)

// Env is the shared, read-mostly environment handed to every view and command.
// Packages that do not exist yet (bus, transport) add their fields in the task
// that introduces them (BUILD-PLAN §4.8 "Growing structs").
type Env struct {
	Registry  *backend.Registry
	Inventory inventory.Inventory   // nil until the app wires the inventory in
	Sets      *targets.Store        // saved target sets; nil = none configured
	Session   *SessionState         // targets, window, investigation, readonly
	Exec      transport.Executor    // remote reads for probes and fan-outs; nil = none
	Shells    transport.ShellOpener // interactive SSH shells; nil in --demo
	Theme     *theme.Theme
	Glyphs    render.GlyphSet // active set, resolved once by internal/app
	Keys      *keymap.Map
	Now       func() time.Time
	Factories map[resource.KindName]Factory
	Log       *slog.Logger
	// Redact is the redactor every view passes text through before showing
	// or recording it (BUILD-PLAN §7). A nil Redactor leaves text alone.
	Redact *redact.Redactor
	// Recorder is the investigation timeline writer. It is nil when no
	// investigation is open; a view that records checks Current() first.
	Recorder investigation.Log
	// Bus is the only way to perform a write (BUILD-PLAN §4.5). It is nil
	// before M3 is wired in: an action then flashes instead of planning.
	Bus bus.Bus
	// Policy is the guard rail `:policy` shows and `:readonly off` refuses
	// against. It is the same value the Bus was built with; nil means the
	// built-in policy applies.
	Policy *bus.Policy
	// Knowledge is the index internal/app built once at startup over
	// knowledge.root. The side panel starts from it, so the first subject
	// does not wait for a full scan; it rebuilds on reindex. nil = no index.
	Knowledge *investigation.Index
	// Planners are the backends that can write, keyed by registration name.
	// The actions menu asks every one of them for its specs.
	Planners map[string]action.Planner
	// ResultsView builds the grouped results view of a finished rollout.
	// internal/app sets it to views.NewFanoutFromResults; package tui may not
	// import internal/tui/views itself. nil falls back to the plain listing.
	ResultsView func(env *Env, title string, results []action.StepResult) View
	// AlertOpeners let a backend take over `enter` on an alert row before the
	// built-in alert investigation runs. The first one that returns true owns
	// the key. The k8s backend opens the pods view this way once it exists.
	AlertOpeners []func(resource.Resource) (tea.Cmd, bool)
	// Execs are the transports a command may name with --via, keyed by name
	// ("ssh", "salt"). internal/app fills it; nil means --via is refused.
	Execs map[string]transport.Executor
	// Salt is what the `:salt` commands and the --via hint read. The zero
	// value means Salt is not configured.
	Salt SaltOptions
	// PluginDir is the plugins directory (<config-dir>/plugins). `S` in a
	// grouped results view saves the command there as a plugin (task 117);
	// empty means saving is refused and `:plugins` lists nothing.
	PluginDir string
}

// SaltOptions is the part of the Salt configuration the UI needs: the target
// count above which the Salt transport is suggested, and whether the backend
// is registered at all (salt.master set).
type SaltOptions struct {
	// SuggestOver is salt.suggest_over: a plan on more targets than this
	// suggests --via salt. Zero never suggests.
	SuggestOver int
	// Configured is true when the Salt backend is registered.
	Configured bool
}

// SessionState is the per-session context every pane shares. Only the root model mutates it.
type SessionState struct {
	Name          string
	Targets       targets.Set
	Window        resource.TimeWindow
	Investigation *investigation.Meta // nil = none open
	ReadOnly      bool
	// saltUsed records that the Salt transport ran something in this
	// session, so its job-cache warning is shown once.
	saltUsed bool
}
