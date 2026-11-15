package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/action"
	"jumpr/internal/bus"
	"jumpr/internal/mux"
	"jumpr/internal/resource"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
)

// The session UX of task 082: detaching, the end-session question, the sessions
// dialog, the rename and the while-detached summary.

// sessionRoot is a root model over one mux session, with the mux and session
// commands registered and a size set.
func sessionRoot(t *testing.T) *Root {
	t.Helper()
	theme.SetProfile("none")
	env := &Env{
		Session: &SessionState{Name: "main"},
		Theme:   theme.Dark(),
		Keys:    keymap.Default(),
		Now:     time.Now,
	}
	cmds := NewCommands()
	RegisterBuiltins(cmds)
	if err := RegisterMuxCommands(cmds); err != nil {
		t.Fatalf("register mux commands: %v", err)
	}
	if err := RegisterSessionCommands(cmds); err != nil {
		t.Fatalf("register session commands: %v", err)
	}
	root := NewRoot(env, cmds, nil, resource.Query{Kind: "host"}, "ctrl+a")
	root.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	return root
}

// runAll executes cmd, flattening batches, and returns every message it yields.
func runAll(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	var out []tea.Msg
	var walk func(tea.Cmd)
	walk = func(c tea.Cmd) {
		if c == nil {
			return
		}
		switch msg := c().(type) {
		case tea.BatchMsg:
			for _, sub := range msg {
				walk(sub)
			}
		case nil:
		default:
			out = append(out, msg)
		}
	}
	walk(cmd)
	return out
}

// firstMsg returns the first message of type T in msgs.
func firstMsg[T any](msgs []tea.Msg) (T, bool) {
	var zero T
	for _, msg := range msgs {
		if v, ok := msg.(T); ok {
			return v, true
		}
	}
	return zero, false
}

// key is one key press.
func key(s string) tea.KeyMsg {
	if s == "esc" {
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	if s == "enter" {
		return tea.KeyMsg{Type: tea.KeyEnter}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// TestDetachStandaloneFlash is requirement 1's other half: a standalone run has
// nothing to attach back to, so `prefix d` says so instead of pretending.
func TestDetachStandaloneFlash(t *testing.T) {
	root := sessionRoot(t)
	if root.OnDetach != nil {
		t.Fatal("a fresh root already has a detach hook")
	}
	msgs := runAll(t, root.handleOp(mux.OpMsg{Op: mux.OpDetach}))
	if len(msgs) != 1 {
		t.Fatalf("detach yielded %d messages, want one flash", len(msgs))
	}
	flash, ok := msgs[0].(FlashMsg)
	if !ok {
		t.Fatalf("detach yielded %T, want FlashMsg", msgs[0])
	}
	want := "standalone mode: nothing to detach from; :q quits"
	if flash.Text != want {
		t.Fatalf("flash = %q, want %q", flash.Text, want)
	}
}

// TestQuitAsksInServerMode is requirement 2: `:q` on the session server asks
// before ending the session, and says that detaching is the safe answer.
func TestQuitAsksInServerMode(t *testing.T) {
	root := sessionRoot(t)
	detached := 0
	root.OnDetach = func() { detached++ }

	if cmd := root.handleMsg(tea.QuitMsg{}); cmd != nil {
		t.Fatalf(":q went straight to a command (%T), want the question", cmd)
	}
	d, ok := root.dialog.(*endSessionDialog)
	if !ok {
		t.Fatalf(":q opened %T, want *endSessionDialog", root.dialog)
	}
	view := d.View(100, 8)
	for _, want := range []string{
		`end session "main"? shells and follows will stop (y/N)`,
		"d = detach instead",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("question %q missing %q", view, want)
		}
	}

	// d detaches and leaves the session running.
	msgs := runAll(t, root.handleKey(key("d")))
	for _, msg := range msgs {
		if _, ok := msg.(endSessionMsg); ok {
			t.Fatal("d ended the session instead of detaching")
		}
	}
	if detached != 1 {
		t.Fatalf("detach hook ran %d times, want 1", detached)
	}

	// y does end it, and the program's quit is not asked about again.
	if cmd := root.handleMsg(tea.QuitMsg{}); cmd != nil {
		t.Fatalf("second :q did not ask")
	}
	msgs = runAll(t, root.handleKey(key("y")))
	var answer endSessionMsg
	found := false
	for _, msg := range msgs {
		if a, ok := msg.(endSessionMsg); ok {
			answer, found = a, true
		}
	}
	if !found {
		t.Fatal("y did not answer the question")
	}
	quit := root.handleMsg(answer)
	if quit == nil {
		t.Fatal("the answer did not end the program")
	}
	if _, ok := quit().(tea.QuitMsg); !ok {
		t.Fatal("the answer did not produce a quit")
	}
}

// TestQuitStandaloneDoesNotAsk is the other side of requirement 2: a standalone
// run ends on `:q` without a question.
func TestQuitStandaloneDoesNotAsk(t *testing.T) {
	root := sessionRoot(t)
	cmd := root.handleMsg(tea.QuitMsg{})
	if cmd == nil {
		t.Fatal("a standalone :q did not quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("standalone :q produced %T, want a quit", cmd())
	}
	if root.dialog != nil {
		t.Fatalf("a standalone :q opened %T", root.dialog)
	}
}

// TestSwitchSessionDialog is requirement 3: `:sessions` lists the registry and
// enter hands the client to the highlighted session.
func TestSwitchSessionDialog(t *testing.T) {
	root := sessionRoot(t)
	var switched []string
	root.SetSessionFuncs(SessionFuncs{
		List: func() ([]SessionSummary, error) {
			now := time.Now()
			return []SessionSummary{
				{Name: "main", Windows: 2, Attached: true, Activity: now},
				{Name: "db-incident", Windows: 1, Dormant: true, Activity: now.Add(-3 * time.Minute)},
			}, nil
		},
		Switch: func(name string) error {
			switched = append(switched, name)
			return nil
		},
	})

	msgs := runAll(t, root.handleOp(mux.OpMsg{Op: mux.OpSessionList}))
	if len(msgs) != 1 {
		t.Fatalf(":sessions yielded %d messages, want the list", len(msgs))
	}
	loaded, ok := msgs[0].(SessionsLoadedMsg)
	if !ok {
		t.Fatalf(":sessions yielded %T, want SessionsLoadedMsg", msgs[0])
	}
	if loaded.Err != nil {
		t.Fatalf("list: %v", loaded.Err)
	}
	if cmd := root.handleMsg(loaded); cmd != nil {
		t.Fatalf("loading the list ran %T, want the dialog", cmd)
	}
	d, ok := root.dialog.(*sessionsDialog)
	if !ok {
		t.Fatalf(":sessions opened %T, want *sessionsDialog", root.dialog)
	}
	view := d.View(100, 8)
	for _, want := range []string{"main  2 windows  (attached)", "db-incident  1 window  (saved)", "3m ago"} {
		if !strings.Contains(view, want) {
			t.Fatalf("list %q missing %q", view, want)
		}
	}

	// Down, then enter: the second session is the one asked for.
	root.handleKey(key("j"))
	msgs = runAll(t, root.handleKey(key("enter")))
	var pick SelectSessionMsg
	found := false
	for _, msg := range msgs {
		if p, ok := msg.(SelectSessionMsg); ok {
			pick, found = p, true
		}
	}
	if !found {
		t.Fatal("enter did not pick a session")
	}
	if pick.Name != "db-incident" {
		t.Fatalf("picked %q, want db-incident", pick.Name)
	}
	switchMsgs := runAll(t, root.handleMsg(pick))
	if len(switched) != 1 || switched[0] != "db-incident" {
		t.Fatalf("switch calls = %v, want [db-incident]", switched)
	}
	found = false
	for _, msg := range switchMsgs {
		if f, ok := msg.(FlashMsg); ok && f.Level == FlashOK && strings.Contains(f.Text, "switched to session db-incident") {
			found = true
		}
	}
	if !found {
		t.Fatalf("switch did not confirm: %v", switchMsgs)
	}
}

// TestSessionsDialogCreateAndKill is requirement 3's other two keys: n asks for
// a name, x confirms before killing.
func TestSessionsDialogCreateAndKill(t *testing.T) {
	root := sessionRoot(t)
	var created, killed []string
	root.SetSessionFuncs(SessionFuncs{
		List: func() ([]SessionSummary, error) {
			return []SessionSummary{{Name: "old", Windows: 1}}, nil
		},
		New:  func(name string) error { created = append(created, name); return nil },
		Kill: func(name string) error { killed = append(killed, name); return nil },
	})
	root.handleMsg(root.handleOp(mux.OpMsg{Op: mux.OpSessionList})())

	msgs := runAll(t, root.handleKey(key("n")))
	prompt, found := firstMsg[newSessionPromptMsg](msgs)
	if !found {
		t.Fatalf("n yielded %v, want newSessionPromptMsg", msgs)
	}
	root.handleMsg(prompt)
	if _, ok := root.dialog.(*promptDialog); !ok {
		t.Fatalf("n opened %T, want a prompt", root.dialog)
	}

	// x asks before killing, and the confirmation carries the highlighted name.
	root.dialog = nil
	root.handleMsg(root.handleOp(mux.OpMsg{Op: mux.OpSessionList})())
	msgs = runAll(t, root.handleKey(key("x")))
	ask, found := firstMsg[killSessionPromptMsg](msgs)
	if !found {
		t.Fatalf("x yielded %v, want killSessionPromptMsg", msgs)
	}
	root.handleMsg(ask)
	if _, ok := root.dialog.(*confirmDialog); !ok {
		t.Fatalf("x opened %T, want a confirmation", root.dialog)
	}
	msgs = runAll(t, root.handleKey(key("y")))
	kill, found := firstMsg[KillSessionMsg](msgs)
	if !found || kill.Name != "old" {
		t.Fatalf("confirmation answered %v, want kill of old", msgs)
	}
	root.handleMsg(NewSessionMsg{Name: "fresh"})
	actionMsgs := runAll(t, root.newSessionCmd("fresh"))
	if len(created) != 1 || created[0] != "fresh" {
		t.Fatalf("created = %v, want [fresh]", created)
	}
	found = false
	for _, msg := range actionMsgs {
		if a, ok := msg.(sessionActionMsg); ok && a.Err == nil && strings.Contains(a.Text, "created session fresh") {
			found = true
		}
	}
	if !found {
		t.Fatalf("create did not confirm: %v", actionMsgs)
	}
	if len(killed) != 0 {
		t.Fatalf("killed = %v before the answer was executed", killed)
	}
	runAll(t, root.killSessionCmd("old"))
	if len(killed) != 1 || killed[0] != "old" {
		t.Fatalf("killed = %v, want [old]", killed)
	}
}

// TestWhileDetachedSummary is requirement 8: on attach, one line says what
// happened while nobody was looking, and the counters start again.
func TestWhileDetachedSummary(t *testing.T) {
	root := sessionRoot(t)
	d := newRunDialog(root.env, bus.Proposal{Plan: action.Plan{Title: "restart nginx"}}, 1)
	d.p.Plan.Spec.Title = "restart nginx"
	root.runs = append(root.runs, d)

	root.Update(DetachMsg{})
	if !root.detached {
		t.Fatal("the root did not record the detach")
	}
	// A rollout that paused at its canary while the client was away.
	root.Update(runEventsForMsg{ID: d.id, Msg: RunEventsMsg{Events: []bus.Event{{Phase: bus.PhasePaused}}}})
	// A fan-out that finished with nobody looking.
	root.Update(FanoutBatchMsg{Closed: true})

	msgs := runAll(t, root.handleMsg(AttachMsg{}))
	if len(msgs) != 1 {
		t.Fatalf("attach yielded %d messages, want one summary", len(msgs))
	}
	flash, ok := msgs[0].(FlashMsg)
	if !ok {
		t.Fatalf("attach yielded %T, want FlashMsg", msgs[0])
	}
	for _, want := range []string{
		"while detached:",
		`rollout "restart nginx" paused at canary`,
		"1 fan-out finished",
	} {
		if !strings.Contains(flash.Text, want) {
			t.Fatalf("summary %q missing %q", flash.Text, want)
		}
	}
	if root.detached {
		t.Fatal("attach did not clear the detached state")
	}
	// The counters are gone: a second attach with nothing to report flashes
	// nothing.
	if msgs := runAll(t, root.handleMsg(AttachMsg{})); len(msgs) != 0 {
		t.Fatalf("second attach flashed %v, want nothing", msgs)
	}
}

// TestSkippedPromptsInSummary is requirement 6's flash: what could not be asked
// while detached is listed on the next attach.
func TestSkippedPromptsInSummary(t *testing.T) {
	root := sessionRoot(t)
	root.Update(DetachMsg{})
	msgs := runAll(t, root.handleMsg(AttachMsg{HostKeySkipped: 2}))
	if len(msgs) != 1 {
		t.Fatalf("attach yielded %v, want one line", msgs)
	}
	flash := msgs[0].(FlashMsg)
	want := "2 hosts need a host-key decision: :health"
	if !strings.Contains(flash.Text, want) {
		t.Fatalf("summary %q missing %q", flash.Text, want)
	}
}

// TestRenameSessionUsesServer is requirement 4: `:rename-session` goes through
// the server, which renames the registry entry and the autosave file, and the
// root follows with the new name.
func TestRenameSessionUsesServer(t *testing.T) {
	root := sessionRoot(t)
	var renamed []string
	root.SetSessionFuncs(SessionFuncs{
		Rename: func(name string) error { renamed = append(renamed, name); return nil },
	})
	msgs := runAll(t, root.handleOp(mux.OpMsg{Op: mux.OpRenameSession, Arg: "db-incident"}))
	if len(renamed) != 1 || renamed[0] != "db-incident" {
		t.Fatalf("rename calls = %v, want [db-incident]", renamed)
	}
	var answer sessionRenamedMsg
	found := false
	for _, msg := range msgs {
		if a, ok := msg.(sessionRenamedMsg); ok {
			answer, found = a, true
		}
	}
	if !found || answer.Err != nil {
		t.Fatalf("rename answered %v", msgs)
	}
	root.handleMsg(answer)
	if got := root.mux.Session().Name; got != "db-incident" {
		t.Fatalf("session name = %q, want db-incident", got)
	}
	if root.env.Session.Name != "db-incident" {
		t.Fatalf("env session name = %q", root.env.Session.Name)
	}
}

// TestEndSessionOnLastPane is requirement 2's other half: closing the last pane
// of a server session asks before ending it instead of quitting outright.
func TestEndSessionOnLastPane(t *testing.T) {
	root := sessionRoot(t)
	root.OnDetach = func() {}
	if cmd := root.handleOp(mux.OpMsg{Op: mux.OpKillPane}); cmd != nil {
		t.Fatalf("the last kill-pane ran %T, want the question", cmd)
	}
	if _, ok := root.dialog.(*endSessionDialog); !ok {
		t.Fatalf("last kill-pane opened %T, want the end-session question", root.dialog)
	}
}
