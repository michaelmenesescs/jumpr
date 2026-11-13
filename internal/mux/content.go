package mux

import tea "github.com/charmbracelet/bubbletea"

// Content is what lives in a pane: a view stack or a terminal.
type Content interface {
	Init() tea.Cmd
	Update(msg tea.Msg) (Content, tea.Cmd)
	// View returns exactly h lines of exactly w cells (the pane interior, borders excluded).
	View(w, h int) string
	Title() string
	SetFocus(focused bool)
	// RawKeys is true for terminals: every key except the prefix key is forwarded to Update untouched,
	// and the global keys (":", "?", "/") are not intercepted.
	RawKeys() bool
	CopyLines() []string // plain text incl. scrollback, oldest first
	State() ContentState // for persistence
	Close() error        // release remote sessions, cancel contexts
}

// ContentState is what is needed to recreate a pane after a restart.
type ContentState struct {
	Type     string   `json:"type"`     // "view" | "term"
	Commands []string `json:"commands"` // view: the `:` command line of each stack entry, bottom first
	Host     string   `json:"host"`     // term: host to reconnect to
}

// PaneMsg routes Msg to one pane's Content. Use mux.ForPane(id, cmd) to wrap the result of a tea.Cmd.
type PaneMsg struct {
	Pane PaneID
	Msg  tea.Msg
}

// ForPane wraps cmd so that every message it produces is routed to pane id.
// A nil command stays nil. A tea.BatchMsg is kept a batch: each inner command
// is wrapped in turn, so the runtime runs them and every result reaches the
// same pane.
func ForPane(id PaneID, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		switch msg := cmd().(type) {
		case nil:
			return nil
		case tea.BatchMsg:
			batch := make(tea.BatchMsg, 0, len(msg))
			for _, c := range msg {
				if c == nil {
					continue
				}
				batch = append(batch, ForPane(id, c))
			}
			return batch
		default:
			return PaneMsg{Pane: id, Msg: msg}
		}
	}
}
