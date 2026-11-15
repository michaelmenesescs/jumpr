package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend"
	"jumpr/internal/bus"
	"jumpr/internal/fanout"
	"jumpr/internal/interact"
	"jumpr/internal/investigation"
	"jumpr/internal/resource"
	"jumpr/internal/targets"
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

// InvestigationChangedMsg reports the session's investigation: Meta is the one
// that is now open, nil means none is. Only the root stores it.
type InvestigationChangedMsg struct{ Meta *investigation.Meta }

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

// FanoutBatchMsg is one batch of a multi-host run (a probe, `:run`): the
// per-host results of internal/fanout, tagged with the run's Seq so batches of
// a superseded run are dropped.
type FanoutBatchMsg struct {
	Seq     int
	Results []fanout.HostResult
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

// Action flow (dialogs)

// ProposalMsg is the result of one bus.Propose round-trip: the planned,
// policy-checked proposal, or the error that planning returned.
type ProposalMsg struct {
	Proposal bus.Proposal
	Err      error
}

// RunEventsMsg is one batch of a rollout's events. The channel is closed after
// the final event, which arrives together with Closed.
type RunEventsMsg struct {
	Events []bus.Event
	Closed bool
}

// RefreshRequestMsg asks a view to reload its data. The root sends it to the
// focused view after a rollout finishes, so the rows on screen catch up.
type RefreshRequestMsg struct{}

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
