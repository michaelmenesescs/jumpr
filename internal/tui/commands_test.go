package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"sitrep/internal/backend"
	"sitrep/internal/backend/fake"
)

// newTestEnv builds an Env backed by the fake backend with a fixed clock.
func newTestEnv(t *testing.T) *Env {
	t.Helper()
	reg := backend.NewRegistry()
	if err := reg.Register(fake.New(fake.Options{})); err != nil {
		t.Fatalf("register fake backend: %v", err)
	}
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	return &Env{Registry: reg, Now: func() time.Time { return now }}
}

func newTestCommands(t *testing.T) *Commands {
	t.Helper()
	c := NewCommands()
	RegisterBuiltins(c)
	return c
}

func TestCommandsRegister(t *testing.T) {
	c := NewCommands()
	run := func(*Env, string) tea.Cmd { return nil }

	if err := c.Register(Command{Name: "alpha", Aliases: []string{"a"}, Run: run}); err != nil {
		t.Fatalf("register alpha: %v", err)
	}
	if err := c.Register(Command{Name: "beta", Run: run}); err != nil {
		t.Fatalf("register beta: %v", err)
	}

	cases := []struct {
		name string
		cmd  Command
	}{
		{"duplicate name", Command{Name: "alpha", Run: run}},
		{"duplicate alias", Command{Name: "gamma", Aliases: []string{"a"}, Run: run}},
		{"alias collides with name", Command{Name: "beta2", Aliases: []string{"beta"}, Run: run}},
		{"empty name", Command{Name: "  ", Run: run}},
		{"nil run", Command{Name: "noop"}},
	}
	for _, tc := range cases {
		if err := c.Register(tc.cmd); err == nil {
			t.Errorf("%s: Register(%+v) succeeded, want error", tc.name, tc.cmd)
		}
	}

	if _, ok := c.Lookup("A"); !ok {
		t.Error("Lookup(\"A\") missed the lower-case alias")
	}
	all := c.All()
	if len(all) != 2 || all[0].Name != "alpha" || all[1].Name != "beta" {
		t.Errorf("All() = %v, want alpha, beta in order", names(all))
	}
}

func TestCommandsAllSorted(t *testing.T) {
	c := NewCommands()
	run := func(*Env, string) tea.Cmd { return nil }
	for _, n := range []string{"zeta", "alpha", "mike"} {
		if err := c.Register(Command{Name: n, Run: run}); err != nil {
			t.Fatalf("register %s: %v", n, err)
		}
	}
	got := names(c.All())
	if strings.Join(got, ",") != "alpha,mike,zeta" {
		t.Errorf("All() = %v, want alphabetical", got)
	}
}

func TestExecKindWithSelector(t *testing.T) {
	env := newTestEnv(t)
	cmds := newTestCommands(t)

	msg := cmds.Exec(env, ":hosts role=web dc=ewr")()
	q, ok := msg.(OpenQueryMsg)
	if !ok {
		t.Fatalf("Exec returned %T, want OpenQueryMsg", msg)
	}
	if q.Query.Kind != "host" {
		t.Errorf("Kind = %q, want host", q.Query.Kind)
	}
	if got := len(q.Query.Selector.Terms); got != 2 {
		t.Fatalf("terms = %d, want 2 (%v)", got, q.Query.Selector.Terms)
	}
	if q.Query.Selector.Terms[0].Key != "role" || q.Query.Selector.Terms[0].Value != "web" {
		t.Errorf("first term = %+v, want role=web", q.Query.Selector.Terms[0])
	}
	if q.Query.Selector.Terms[1].Key != "dc" || q.Query.Selector.Terms[1].Value != "ewr" {
		t.Errorf("second term = %+v, want dc=ewr", q.Query.Selector.Terms[1])
	}
}

func TestExecKindAlias(t *testing.T) {
	env := newTestEnv(t)
	cmds := newTestCommands(t)

	msg := cmds.Exec(env, "h dc=ewr")()
	q, ok := msg.(OpenQueryMsg)
	if !ok {
		t.Fatalf("Exec returned %T, want OpenQueryMsg", msg)
	}
	if q.Query.Kind != "host" {
		t.Errorf("Kind = %q, want host (alias h)", q.Query.Kind)
	}
	if len(q.Query.Selector.Terms) != 1 {
		t.Errorf("terms = %d, want 1", len(q.Query.Selector.Terms))
	}
}

func TestExecCommandReceivesArgs(t *testing.T) {
	cmds := NewCommands()
	var got string
	if err := cmds.Register(Command{
		Name: "echo",
		Run: func(_ *Env, args string) tea.Cmd {
			got = args
			return nil
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if cmd := cmds.Exec(nil, ":echo  a b  "); cmd != nil {
		cmd()
	}
	if got != "a b" {
		t.Errorf("args = %q, want %q", got, "a b")
	}
}

func TestExecUnknown(t *testing.T) {
	cmds := newTestCommands(t)

	msg := cmds.Exec(newTestEnv(t), ":nope arg")()
	f, ok := msg.(FlashMsg)
	if !ok {
		t.Fatalf("Exec returned %T, want FlashMsg", msg)
	}
	if f.Level != FlashError {
		t.Errorf("level = %v, want FlashError", f.Level)
	}
	if !strings.Contains(f.Text, "unknown command: nope") {
		t.Errorf("flash %q, want it to name the command", f.Text)
	}
}

func TestExecBadSelectorFlashes(t *testing.T) {
	cmds := newTestCommands(t)

	msg := cmds.Exec(newTestEnv(t), ":hosts role~[")()
	f, ok := msg.(FlashMsg)
	if !ok {
		t.Fatalf("Exec returned %T, want FlashMsg", msg)
	}
	if f.Level != FlashError {
		t.Errorf("level = %v, want FlashError", f.Level)
	}
	if !strings.Contains(f.Text, "bad selector") {
		t.Errorf("flash %q, want a bad-selector message", f.Text)
	}
}

func TestExecEmptyIsNoop(t *testing.T) {
	cmds := newTestCommands(t)
	if cmd := cmds.Exec(newTestEnv(t), "   :  "); cmd != nil {
		t.Errorf("Exec(empty) = %v, want nil", cmd())
	}
}

func TestExecNoRegistryIsUnknown(t *testing.T) {
	cmds := newTestCommands(t)
	msg := cmds.Exec(&Env{}, ":hosts dc=ewr")()
	if f, ok := msg.(FlashMsg); !ok || f.Level != FlashError {
		t.Fatalf("Exec = %T %v, want an error flash", msg, msg)
	}
}

func TestTimeCommand(t *testing.T) {
	env := newTestEnv(t)
	cmds := newTestCommands(t)

	for _, line := range []string{":time -1h", ":t -1h"} {
		msg := cmds.Exec(env, line)()
		w, ok := msg.(WindowChangedMsg)
		if !ok {
			t.Fatalf("%s: got %T, want WindowChangedMsg", line, msg)
		}
		if w.Window.IsZero() {
			t.Errorf("%s: window is zero", line)
		}
		if w.Window.Label != "-1h" {
			t.Errorf("%s: label = %q, want -1h", line, w.Window.Label)
		}
		if !w.Window.ToNow || !w.Window.From.Equal(env.Now().Add(-time.Hour)) {
			t.Errorf("%s: window = %+v, want the last hour ending now", line, w.Window)
		}
	}
}

func TestTimeCommandBadWindow(t *testing.T) {
	cmds := newTestCommands(t)
	msg := cmds.Exec(newTestEnv(t), ":time whenever")()
	f, ok := msg.(FlashMsg)
	if !ok || f.Level != FlashError {
		t.Fatalf("got %T, want an error flash", msg)
	}
	if !strings.Contains(f.Text, "bad time window") {
		t.Errorf("flash %q, want a bad-time-window message", f.Text)
	}
}

func TestBuiltinQuitHelpKinds(t *testing.T) {
	cmds := newTestCommands(t)

	if _, ok := cmds.Exec(nil, ":q")().(tea.QuitMsg); !ok {
		t.Error(":q did not emit tea.QuitMsg")
	}
	if _, ok := cmds.Exec(nil, ":quit")().(tea.QuitMsg); !ok {
		t.Error(":quit did not emit tea.QuitMsg")
	}
	if _, ok := cmds.Exec(nil, ":help")().(HelpRequestedMsg); !ok {
		t.Error(":help did not emit HelpRequestedMsg")
	}
	if _, ok := cmds.Exec(nil, ":kinds")().(KindsRequestedMsg); !ok {
		t.Error(":kinds did not emit KindsRequestedMsg")
	}
}

func names(cs []Command) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}
