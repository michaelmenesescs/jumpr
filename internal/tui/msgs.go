package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend"
	"jumper/internal/interact"
	"jumper/internal/resource"
	"jumper/internal/targets"
)

// Navigation (emitted by views and commands; handled by the focused view pane's stack)

// PushViewMsg asks the view stack to push View on top of the current view.
type PushViewMsg struct{ View View }

// PopViewMsg asks the view stack to drop the top view (never the root).
type PopViewMsg struct{}

// ReplaceViewMsg asks the view stack to replace the top view.
type ReplaceViewMsg struct{ View View }

// OpenQueryMsg asks the root to resolve a query through the registry and
// factories and then push the resulting view.
type OpenQueryMsg struct{ Query resource.Query }

// Feedback

// FlashLevel is the severity of a FlashMsg.
type FlashLevel int

const (
	FlashInfo FlashLevel = iota
	FlashOK
	FlashWarn
	FlashError
)

// FlashMsg shows one line of feedback in the status area.
type FlashMsg struct {
	Level FlashLevel
	Text  string
}

// Dialogs (modal, owned by the root)

// CloseDialogMsg asks the root to close the current dialog.
type CloseDialogMsg struct{}

// Command bar

// RunCommandMsg executes a `:` command line as if typed.
type RunCommandMsg struct{ Line string }

// Session state changes (root updates Env.Session, then rebroadcasts to all panes)

// TargetsChangedMsg reports a new target set for the session. The root stores
// it in SessionState.Targets, flashes the new count and rebroadcasts it.
type TargetsChangedMsg struct{ Set targets.Set }

// WindowChangedMsg reports a new time window for the session.
type WindowChangedMsg struct{ Window resource.TimeWindow }

// Health polling (root → status line)

// HealthMsg carries the latest health of one backend.
type HealthMsg struct {
	Backend string
	Health  backend.Health
}

// Streaming data into a view. Seq guards against stale streams after a refresh.

// ListBatchMsg is one batch of a backend List stream.
type ListBatchMsg struct {
	Seq     int
	Results []backend.Result
	Closed  bool
}

// LinesMsg is one batch of a line stream (journal, logs, command output).
type LinesMsg struct {
	Seq    int
	Lines  []string
	Closed bool
	Err    error
}

// Interaction requests from non-UI code (host key, passphrase). The root shows
// a dialog above everything else and replies on Reply, which is buffered by
// one so the answer never blocks.
type PromptMsg struct {
	Prompt interact.Prompt // package internal/interact
	Reply  chan<- interact.Answer
}

// Constructors returning tea.Cmd.

// Push returns a command that pushes v onto the focused view stack.
func Push(v View) tea.Cmd {
	return func() tea.Msg { return PushViewMsg{View: v} }
}

// Pop returns a command that pops the focused view stack.
func Pop() tea.Cmd {
	return func() tea.Msg { return PopViewMsg{} }
}

// Flash returns a command that shows a formatted one-line message.
func Flash(level FlashLevel, format string, args ...any) tea.Cmd {
	text := format
	if len(args) > 0 {
		text = fmt.Sprintf(format, args...)
	}
	return func() tea.Msg { return FlashMsg{Level: level, Text: text} }
}

// OpenQuery returns a command that asks the root to open q.
func OpenQuery(q resource.Query) tea.Cmd {
	return func() tea.Msg { return OpenQueryMsg{Query: q} }
}

// OpenDialogMsg asks the root to show Dialog modally.
type OpenDialogMsg struct{ Dialog Dialog }
