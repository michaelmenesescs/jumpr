package tui_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend"
	"jumper/internal/backend/fake"
	"jumper/internal/mux"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/tui"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
)

// keyLogView records every key it receives and renders them, so a test can see
// on screen whether a key reached the view.
type keyLogView struct {
	keys []string
}

var _ tui.View = (*keyLogView)(nil)

func (v *keyLogView) Init() tea.Cmd { return nil }

func (v *keyLogView) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		v.keys = append(v.keys, k.String())
	}
	return v, nil
}

func (v *keyLogView) Render(w, h int) string {
	return render.Fit("keys: "+strings.Join(v.keys, " "), w, h)
}

func (v *keyLogView) Crumb() string        { return "keys" }
func (v *keyLogView) Hints() []tui.KeyHint { return nil }
func (v *keyLogView) Subject() tui.Subject { return tui.Subject{} }
func (v *keyLogView) Capturing() bool      { return false }
func (v *keyLogView) CopyLines() []string {
	return []string{"keys: " + strings.Join(v.keys, " ")}
}

// newKeyRoot builds a root whose every pane shows one shared keyLogView.
func newKeyRoot(t *testing.T, prefix string) *tui.Root {
	t.Helper()
	theme.SetProfile("none")
	reg := backend.NewRegistry()
	if err := reg.Register(fake.New(fake.Options{})); err != nil {
		t.Fatalf("register fake backend: %v", err)
	}
	env := &tui.Env{
		Registry: reg,
		Session:  &tui.SessionState{Name: "main"},
		Theme:    theme.Dark(),
		Keys:     keymap.Default(),
		Now:      time.Now,
	}
	v := &keyLogView{}
	factory := func(*tui.Env, resource.Query) (tui.View, error) { return v, nil }
	cmds := tui.NewCommands()
	return tui.NewRoot(env, cmds, factory, resource.Query{Kind: "host"}, prefix)
}

func TestMuxCommandsRegistered(t *testing.T) {
	c := tui.NewCommands()
	if err := tui.RegisterMuxCommands(c); err != nil {
		t.Fatalf("register mux commands: %v", err)
	}
	for _, name := range []string{
		"split", "vsplit", "kill-pane", "zoom", "focus", "resize", "layout",
		"window", "rename", "rename-session", "detach", "copy", "sync",
	} {
		cmd, ok := c.Lookup(name)
		if !ok {
			t.Errorf("command %q is not registered", name)
			continue
		}
		if !strings.HasPrefix(cmd.Usage, ":") {
			t.Errorf("command %q has usage %q, want it to start with \":\"", name, cmd.Usage)
		}
		if strings.TrimSpace(cmd.Help) == "" {
			t.Errorf("command %q has no Help", name)
		}
		if cmd.Complete == nil {
			t.Errorf("command %q has no argument completion", name)
		}
	}
}

// runCmd runs one command and returns the message it produced, unwrapping a
// batch to its first message.
func runCmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c != nil {
				return c()
			}
		}
		return nil
	}
	return msg
}

func TestMuxCommandOps(t *testing.T) {
	c := tui.NewCommands()
	if err := tui.RegisterMuxCommands(c); err != nil {
		t.Fatalf("register mux commands: %v", err)
	}
	cases := []struct {
		line string
		op   string
		arg  string
	}{
		{"split", mux.OpSplitH, ""},
		{"vsplit", mux.OpSplitV, ""},
		{"kill-pane", mux.OpKillPane, ""},
		{"zoom", mux.OpZoom, ""},
		{"focus up", mux.OpFocus, "up"},
		{"resize left", mux.OpResize, "left"},
		{"layout", mux.OpLayoutNext, ""},
		{"layout next", mux.OpLayoutNext, ""},
		{"window new", mux.OpNewWindow, ""},
		{"window 2", mux.OpSelectWindow, "2"},
		{"rename web", mux.OpRenameWindow, "web"},
		{"rename-session two", mux.OpRenameSession, "two"},
		{"detach", mux.OpDetach, ""},
		{"copy", mux.OpCopyMode, ""},
		{"sync", mux.OpSync, ""},
	}
	for _, tc := range cases {
		msg := runCmd(t, c.Exec(nil, tc.line))
		op, ok := msg.(mux.OpMsg)
		if !ok {
			t.Errorf(":%s produced %T, want mux.OpMsg", tc.line, msg)
			continue
		}
		if op.Op != tc.op || op.Arg != tc.arg {
			t.Errorf(":%s produced %+v, want {%s %s}", tc.line, op, tc.op, tc.arg)
		}
	}

	// Bad arguments flash instead of emitting an operation.
	msg := runCmd(t, c.Exec(nil, "focus sideways"))
	if _, ok := msg.(tui.FlashMsg); !ok {
		t.Fatalf(":focus sideways produced %T, want a flash", msg)
	}
}

// collect runs a command and returns every message it produces, flattening
// batches.
func collect(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	var out []tea.Msg
	var walk func(c tea.Cmd)
	walk = func(c tea.Cmd) {
		if c == nil {
			return
		}
		switch m := c().(type) {
		case nil:
		case tea.BatchMsg:
			for _, inner := range m {
				walk(inner)
			}
		default:
			out = append(out, m)
		}
	}
	walk(cmd)
	return out
}

// msgOp returns the first mux operation among msgs.
func msgOp(msgs []tea.Msg) (mux.OpMsg, bool) {
	for _, m := range msgs {
		if op, ok := m.(mux.OpMsg); ok {
			return op, true
		}
	}
	return mux.OpMsg{}, false
}

func TestMuxDialogFlows(t *testing.T) {
	t.Run("prompt submits and closes", func(t *testing.T) {
		d := tui.NewPromptDialog(theme.Dark(), "rename window", "window name:", "", func(name string) tea.Cmd {
			return func() tea.Msg { return mux.OpMsg{Op: mux.OpRenameWindow, Arg: name} }
		})
		for _, r := range "web" {
			d, _ = d.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
		_, cmd := d.Update(tea.KeyMsg{Type: tea.KeyEnter})
		msgs := collect(t, cmd)
		op, ok := msgOp(msgs)
		if !ok || op.Op != mux.OpRenameWindow || op.Arg != "web" {
			t.Fatalf("enter produced %v, want a rename-window op for \"web\"", msgs)
		}
		closed := false
		for _, m := range msgs {
			if _, ok := m.(tui.CloseDialogMsg); ok {
				closed = true
			}
		}
		if !closed {
			t.Fatalf("prompt did not close itself: %v", msgs)
		}
	})

	t.Run("confirm runs the yes command", func(t *testing.T) {
		yes := func() tea.Msg { return mux.OpMsg{Op: mux.OpKillPane} }
		d := tui.NewConfirmDialog(theme.Dark(), "kill pane", "Kill it?", yes)
		_, cmd := d.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
		op, ok := msgOp(collect(t, cmd))
		if !ok || op.Op != mux.OpKillPane {
			t.Fatalf("y produced %v, want a kill-pane op", op)
		}
	})

	t.Run("list picks the selection", func(t *testing.T) {
		items := []tui.ListItem{{Label: "0: hosts", Detail: "active"}, {Label: "1: alerts"}}
		d := tui.NewListDialog(theme.Dark(), "windows", items, func(i int) tea.Cmd {
			return func() tea.Msg { return mux.OpMsg{Op: mux.OpSelectWindow, Arg: strconv.Itoa(i)} }
		})
		d, _ = d.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
		_, cmd := d.Update(tea.KeyMsg{Type: tea.KeyEnter})
		op, ok := msgOp(collect(t, cmd))
		if !ok || op.Op != mux.OpSelectWindow || op.Arg != "1" {
			t.Fatalf("enter produced %v, want select-window 1", op)
		}
	})
}

func TestPrefixInTmux(t *testing.T) {
	inTmux, err := tui.PrefixKey("C-a", "C-b", true)
	if err != nil {
		t.Fatalf("PrefixKey in tmux: %v", err)
	}
	if inTmux != "ctrl+b" {
		t.Fatalf("prefix inside tmux is %q, want ctrl+b", inTmux)
	}
	outside, err := tui.PrefixKey("C-a", "C-b", false)
	if err != nil {
		t.Fatalf("PrefixKey outside tmux: %v", err)
	}
	if outside != "ctrl+a" {
		t.Fatalf("prefix outside tmux is %q, want ctrl+a", outside)
	}

	tm := start(t, newKeyRoot(t, inTmux))
	waitFor(t, tm, "keys:")

	// ctrl+a is not the prefix inside tmux, so the view receives it.
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlA})
	waitFor(t, tm, "keys: ctrl+a")

	// ctrl+b is: c opens a second window.
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlB})
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	waitFor(t, tm, "1:*")

	screen := finish(t, tm)
	if !strings.Contains(screen, "1:*") {
		t.Fatalf("ctrl+b c did not open a window:\n%s", screen)
	}
}
