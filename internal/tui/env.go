package tui

import (
	"log/slog"
	"time"

	"jumper/internal/backend"
	"jumper/internal/resource"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
)

// Env is the shared, read-mostly environment handed to every view and command.
// Packages that do not exist yet (inventory, bus, transport, targets,
// investigation) add their fields in the task that introduces them
// (BUILD-PLAN §4.8 "Growing structs").
type Env struct {
	Registry  *backend.Registry
	Session   *SessionState // targets, window, investigation, readonly
	Theme     *theme.Theme
	Keys      *keymap.Map
	Now       func() time.Time
	Factories map[resource.KindName]Factory
	Log       *slog.Logger
}

// SessionState is the per-session context every pane shares. Only the root model mutates it.
type SessionState struct {
	Name     string
	Window   resource.TimeWindow
	ReadOnly bool
}
