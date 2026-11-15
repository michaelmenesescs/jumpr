// Package tui holds the small set of types every view and command is written
// against: the View interface, the shared Env, navigation and streaming
// messages, and the view stack.
package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/resource"
)

// Subject is what a view is "about". It drives the knowledge panel and investigation entries.
type Subject struct {
	Hosts []resource.HostID
	Unit  string
	Alert string
	Kind  resource.KindName
	Ref   resource.Ref
}

type KeyHint struct {
	Key  string // "enter", "u", "C-a %"
	Text string // "open host"
}

// View is one screen inside a view pane. Views are values owned by a ViewStack.
// Update and Render must never block or do I/O; start work by returning a tea.Cmd.
type View interface {
	Init() tea.Cmd
	Update(msg tea.Msg) (View, tea.Cmd)
	// Render returns exactly h lines, each exactly w cells wide (use render.Fit).
	Render(w, h int) string
	Crumb() string    // breadcrumb text, e.g. "hosts (24)"
	Hints() []KeyHint // shown in the hint bar while focused
	Subject() Subject
	// Capturing is true while the view owns the keyboard (filter input, text prompt).
	// While true, the pane sends every key except the prefix key to the view.
	Capturing() bool
	// CopyLines returns the full plain text of the view for copy mode (no ANSI).
	CopyLines() []string
}

// Factory builds the view for a query. Registered per kind; kinds without one get the generic table view.
type Factory func(env *Env, q resource.Query) (View, error)
