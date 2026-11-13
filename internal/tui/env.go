package tui

import (
	"log/slog"
	"time"

	"jumper/internal/backend"
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
// Packages that do not exist yet (bus, transport, investigation) add their
// fields in the task that introduces them (BUILD-PLAN §4.8 "Growing structs").
type Env struct {
	Registry  *backend.Registry
	Inventory inventory.Inventory // nil until the app wires the inventory in
	Sets      *targets.Store      // saved target sets; nil = none configured
	Session   *SessionState       // targets, window, investigation, readonly
	Exec      transport.Executor  // remote reads for probes and fan-outs; nil = none
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
}

// SessionState is the per-session context every pane shares. Only the root model mutates it.
type SessionState struct {
	Name     string
	Targets  targets.Set
	Window   resource.TimeWindow
	ReadOnly bool
}
