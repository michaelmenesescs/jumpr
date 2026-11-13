package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/bus"
	"jumper/internal/mux"
)

// SessionSummary is one session as the sessions dialog shows it: its name, its
// window count, whether this client is on it, when it was last active and
// whether it is dormant — saved to disk and revived on first attach
// (BUILD-PLAN §6, task 082).
type SessionSummary struct {
	Name     string
	Windows  int
	Attached bool
	Dormant  bool
	Created  time.Time
	Activity time.Time
}

// SessionFuncs is what the root asks of the session server: everything a
// session cannot do for itself. It is zero in standalone mode, where there is
// no other session to switch to and nothing to detach from, so every action
// says so instead of pretending.
type SessionFuncs struct {
	List   func() ([]SessionSummary, error)
	Switch func(name string) error
	New    func(name string) error
	Kill   func(name string) error
	Rename func(name string) error
}

// Messages the root raises for the session server and the answers it gets
// back. All of them are handled by the root; the call itself always runs in a
// tea.Cmd, never in Update.

// SelectSessionMsg asks the root to hand this client to session Name.
type SelectSessionMsg struct{ Name string }

// NewSessionMsg asks the root to create session Name.
type NewSessionMsg struct{ Name string }

// KillSessionMsg asks the root to end session Name; a dormant one loses its
// file.
type KillSessionMsg struct{ Name string }

// RenameSessionMsg asks the root to rename this session to Name.
type RenameSessionMsg struct{ Name string }

// SessionsLoadedMsg is the session list, loaded out of Update.
type SessionsLoadedMsg struct {
	Items []SessionSummary
	Err   error
}

// sessionRenamedMsg reports a finished rename: Name is the new name, Err the
// reason it did not happen.
type sessionRenamedMsg struct {
	Name string
	Err  error
}

// sessionActionMsg is the outcome of creating or killing a session: Text is the
// flash line, and a nil Err means the list has to be loaded again.
type sessionActionMsg struct {
	Level FlashLevel
	Text  string
	Err   error
}

// endSessionMsg is the answer to the end-session question: the program quits.
type endSessionMsg struct{}

// newSessionPromptMsg asks the root to ask for the name of a new session.
type newSessionPromptMsg struct{}

// killSessionPromptMsg asks the root to confirm killing Name.
type killSessionPromptMsg struct {
	Name    string
	Dormant bool
}

// AttachMsg tells the root that a client has attached: the while-detached
// counters become one flash line and start again from zero. The counts are the
// prompts the server refused while nobody could answer (task 082 requirement 6).
type AttachMsg struct {
	HostKeySkipped    int
	PassphraseSkipped int
}

// DetachMsg tells the root that no client is attached any more: from now on
// what happens is counted for the next attach's summary (requirement 8).
type DetachMsg struct{}

// PrefixMsg re-points the session prefix key. Attaching a client is the only
// thing that changes it (a client inside tmux gets prefix-in-tmux), and an
// attach runs outside the event loop, so it arrives as a message instead of a
// method call: the root's fields belong to the UI goroutine (BUILD-PLAN §6.1).
type PrefixMsg struct{ Key string }

// SessionControlMsg installs the sessions dialog's server side. It is sent by
// app once the server has bound the session to its registry, which happens
// after the program is running.
type SessionControlMsg struct{ Funcs SessionFuncs }

// DetachHookMsg installs what `prefix d` and `:detach` do: a session server
// unplugs the terminal, a standalone run leaves it nil. Sent from Attach, which
// is not the UI goroutine.
type DetachHookMsg struct{ Hook func() }

// awayCounters is what happened while no client was attached.
type awayCounters struct {
	runs     []string // rollouts that reached the canary pause, in order
	fanouts  int      // fan-outs that finished
	errors   int      // journal errors that arrived
	hostKeys int      // host-key prompts nobody could answer
}

// SetSessionFuncs installs the session server's side of the sessions dialog.
// app calls it once per session; a standalone run leaves it zero.
func (r *Root) SetSessionFuncs(f SessionFuncs) {
	if r == nil {
		return
	}
	r.sessions = f
}

// SetSessionName renames the session in both places the model holds it.
func (r *Root) SetSessionName(name string) {
	if r == nil || name == "" {
		return
	}
	if r.mux != nil && r.mux.Session() != nil {
		r.mux.Session().Rename(name)
	}
	if r.env != nil && r.env.Session != nil {
		r.env.Session.Name = name
	}
}

// serverMode reports whether this session can be left and rejoined: the
// session server installs a detach hook, a standalone run has none.
func (r *Root) serverMode() bool { return r.OnDetach != nil }

// RegisterSessionCommands registers the `:` commands of the sessions dialog.
// Every one plans a mux operation the root executes, like the mux family.
func RegisterSessionCommands(c *Commands) error {
	if c == nil {
		return nil
	}
	return c.Register(Command{
		Name:     "sessions",
		Aliases:  []string{"session-list"},
		Usage:    ":sessions",
		Help:     "list sessions: enter switch, n new, x kill",
		Run:      func(*Env, string) tea.Cmd { return opCmd(mux.OpSessionList, "") },
		Complete: func(*Env, string) []string { return nil },
	})
}

// loadSessions asks the session server for the list, out of Update.
func (r *Root) loadSessions() tea.Cmd {
	list := r.sessions.List
	if list == nil {
		return Flash(FlashInfo, "standalone mode: no session server; :q quits")
	}
	return func() tea.Msg {
		items, err := list()
		return SessionsLoadedMsg{Items: items, Err: err}
	}
}

// openSessions loads the list; the dialog opens when it arrives, so Update
// never waits on the server.
func (r *Root) openSessions() tea.Cmd { return r.loadSessions() }

// sessionsDialogFor builds the dialog over the list, wiring the four actions to
// what the session server offers. The dialog never touches the root's modal
// itself: it asks for the next step with a message, because the root replaces
// its dialog with whatever the dialog's Update returned.
func (r *Root) sessionsDialogFor(items []SessionSummary) Dialog {
	return NewSessionsDialog(themeOf(r.env), items,
		func(i int) tea.Cmd {
			if i < 0 || i >= len(items) {
				return nil
			}
			return func() tea.Msg { return SelectSessionMsg{Name: items[i].Name} }
		},
		func() tea.Cmd {
			return func() tea.Msg { return newSessionPromptMsg{} }
		},
		func(i int) tea.Cmd {
			if i < 0 || i >= len(items) {
				return nil
			}
			msg := killSessionPromptMsg{Name: items[i].Name, Dormant: items[i].Dormant}
			return func() tea.Msg { return msg }
		},
	)
}

// openNewSessionPrompt asks for the name of a new session.
func (r *Root) openNewSessionPrompt() tea.Cmd {
	if r.sessions.New == nil {
		r.dialog = nil
		return Flash(FlashInfo, "standalone mode: no session server; :q quits")
	}
	r.dialog = NewPromptDialog(themeOf(r.env), "new session", "session name:", "",
		func(name string) tea.Cmd { return func() tea.Msg { return NewSessionMsg{Name: name} } })
	return nil
}

// openKillSessionPrompt confirms killing the session the dialog highlighted.
func (r *Root) openKillSessionPrompt(name string, dormant bool) tea.Cmd {
	if r.sessions.Kill == nil {
		r.dialog = nil
		return Flash(FlashInfo, "standalone mode: no session server; :q quits")
	}
	text := "kill session \"" + name + "\"?\nshells and follows will stop"
	if dormant {
		text = "kill session \"" + name + "\"?\nits saved layout is deleted"
	}
	r.dialog = NewConfirmDialog(themeOf(r.env), "kill session", text,
		func() tea.Msg { return KillSessionMsg{Name: name} })
	return nil
}

// switchCmd asks the server to hand this client to another session. The switch
// itself is the server's detach/attach pair; this side only asks.
func (r *Root) switchCmd(name string) tea.Cmd {
	sw := r.sessions.Switch
	if sw == nil {
		return Flash(FlashInfo, "standalone mode: no session server; :q quits")
	}
	return func() tea.Msg {
		if err := sw(name); err != nil {
			return FlashMsg{Level: FlashError, Text: "switch: " + err.Error()}
		}
		return FlashMsg{Level: FlashOK, Text: "switched to session " + name}
	}
}

// newSessionCmd creates a session. The new session is created detached: this
// client stays where it is, and enter on the fresh row switches to it (or
// `:attach -t name` from another terminal).
func (r *Root) newSessionCmd(name string) tea.Cmd {
	newFn := r.sessions.New
	return func() tea.Msg {
		if newFn == nil {
			return sessionActionMsg{Level: FlashInfo, Text: "standalone mode: no session server; :q quits", Err: errNoServer}
		}
		if err := newFn(name); err != nil {
			return sessionActionMsg{Level: FlashError, Text: "session not created: " + err.Error(), Err: err}
		}
		return sessionActionMsg{Level: FlashOK, Text: "created session " + name}
	}
}

// killSessionCmd ends a session (or deletes a dormant one).
func (r *Root) killSessionCmd(name string) tea.Cmd {
	kill := r.sessions.Kill
	return func() tea.Msg {
		if kill == nil {
			return sessionActionMsg{Level: FlashInfo, Text: "standalone mode: no session server; :q quits", Err: errNoServer}
		}
		if err := kill(name); err != nil {
			return sessionActionMsg{Level: FlashError, Text: "session not killed: " + err.Error(), Err: err}
		}
		return sessionActionMsg{Level: FlashOK, Text: "killed session " + name}
	}
}

// renameCmd renames the session through the server, which also renames the
// autosave file, then puts the new name on screen.
func (r *Root) renameCmd(name string) tea.Cmd {
	rename := r.sessions.Rename
	return func() tea.Msg {
		if rename != nil {
			if err := rename(name); err != nil {
				return sessionRenamedMsg{Err: err}
			}
		}
		return sessionRenamedMsg{Name: name}
	}
}

// noteAwayRun counts a rollout that reached the canary pause while nobody was
// looking (requirement 8).
func (r *Root) noteAwayRun(id int, msg tea.Msg) {
	if !r.detached {
		return
	}
	ev, ok := msg.(RunEventsMsg)
	if !ok {
		return
	}
	paused := false
	for _, e := range ev.Events {
		if e.Phase == bus.PhasePaused {
			paused = true
			break
		}
	}
	if !paused {
		return
	}
	title := "run"
	if d := r.runByID(id); d != nil {
		title = d.Title()
	}
	r.away.runs = append(r.away.runs, title)
}

// noteAwayStream counts a finished fan-out or a new journal error that arrived
// while nobody was looking. It is called for the batch messages the panes
// receive, before they are routed (requirement 8).
func (r *Root) noteAwayStream(msg tea.Msg) {
	if !r.detached {
		return
	}
	switch m := msg.(type) {
	case FanoutBatchMsg:
		if m.Closed {
			r.away.fanouts++
		}
	case LinesMsg:
		if m.Err != nil {
			r.away.errors++
		}
	}
}

// attachSummary is the one line the next attach shows: what happened while the
// client was away, then what could not be asked (requirements 6 and 8). An
// empty string means there is nothing to say.
func (r *Root) attachSummary(m AttachMsg) string {
	parts := make([]string, 0, 4)
	if len(r.away.runs) > 0 {
		parts = append(parts, fmt.Sprintf("rollout %q paused at canary", r.away.runs[len(r.away.runs)-1]))
		if n := len(r.away.runs) - 1; n > 0 {
			parts = append(parts, fmt.Sprintf("%d more paused", n))
		}
	}
	if r.away.fanouts > 0 {
		parts = append(parts, fmt.Sprintf("%d fan-out%s finished", r.away.fanouts, plural(r.away.fanouts)))
	}
	if r.away.errors > 0 {
		parts = append(parts, fmt.Sprintf("%d new journal error%s", r.away.errors, plural(r.away.errors)))
	}
	if m.HostKeySkipped > 0 {
		verb, noun := "need", "hosts"
		if m.HostKeySkipped == 1 {
			verb, noun = "needs", "host"
		}
		parts = append(parts, fmt.Sprintf("%d %s %s a host-key decision: :health",
			m.HostKeySkipped, noun, verb))
	}
	if m.PassphraseSkipped > 0 {
		noun := "prompts"
		if m.PassphraseSkipped == 1 {
			noun = "prompt"
		}
		parts = append(parts, fmt.Sprintf("%d passphrase %s skipped: :health",
			m.PassphraseSkipped, noun))
	}
	if len(parts) == 0 {
		return ""
	}
	return "while detached: " + strings.Join(parts, " · ")
}

// resetAway starts the while-detached counters again.
func (r *Root) resetAway() { r.away = awayCounters{} }

// plural is "" for one and "s" otherwise.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// errNoServer is what a session action reports in standalone mode, where there
// is no registry to act on. It is never shown: the flash line is.
var errNoServer = errors.New("standalone mode: no session server")

// quitOrAsk ends the program. On the session server that ends the session, so
// it asks first; a standalone run just quits (requirement 2).
func (r *Root) quitOrAsk() tea.Cmd {
	if !r.serverMode() {
		return tea.Quit
	}
	return r.openEndSession()
}

// openEndSession asks whether to end this session, saying that detaching is the
// non-destructive default (requirement 2).
func (r *Root) openEndSession() tea.Cmd {
	name := "main"
	if r.mux != nil && r.mux.Session() != nil && r.mux.Session().Name != "" {
		name = r.mux.Session().Name
	}
	r.dialog = NewEndSessionDialog(themeOf(r.env), name,
		func() tea.Msg { return endSessionMsg{} },
		r.detachCmd())
	return nil
}

// detachCmd unplugs the terminal through the hook the session server installed.
func (r *Root) detachCmd() tea.Cmd {
	detach := r.OnDetach
	if detach == nil {
		return nil
	}
	return func() tea.Msg {
		detach()
		return nil
	}
}

// wouldEndSession reports whether op would close the last pane of the last
// window, which ends the session (and with it, in server mode, the client).
func (r *Root) wouldEndSession(op string) bool {
	if r.mux == nil || r.mux.Session() == nil {
		return false
	}
	sess := r.mux.Session()
	w := sess.ActiveWindow()
	if w == nil {
		return false
	}
	switch op {
	case mux.OpKillPane:
		return len(sess.Windows) == 1 && w.Layout.Count() == 1
	case mux.OpKillWindow:
		return len(sess.Windows) == 1
	}
	return false
}
