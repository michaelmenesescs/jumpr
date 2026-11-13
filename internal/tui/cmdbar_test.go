package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"sitrep/internal/render"
)

func typeRunes(b *CmdBar, s string) {
	b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
}

func press(b *CmdBar, t tea.KeyType) (tea.Cmd, bool) {
	return b.Update(tea.KeyMsg{Type: t})
}

func submit(b *CmdBar, line string) tea.Msg {
	b.Open(line)
	cmd, done := press(b, tea.KeyEnter)
	if !done {
		panic("enter did not close the command bar")
	}
	if cmd == nil {
		return nil
	}
	return cmd()
}

func TestCmdBarOpenClose(t *testing.T) {
	b := NewCmdBar(newTestEnv(t), newTestCommands(t))
	if b.Active() {
		t.Fatal("a fresh command bar is active")
	}
	b.Open("hosts ")
	if !b.Active() {
		t.Fatal("Open did not activate the bar")
	}
	if got := b.View(20); !strings.HasPrefix(got, ":hosts") {
		t.Errorf("View = %q, want the prefill after the colon", got)
	}

	cmd, done := press(b, tea.KeyEsc)
	if !done || cmd != nil {
		t.Errorf("esc = (%v, %v), want (nil, true)", cmd, done)
	}
	if b.Active() {
		t.Error("esc left the bar active")
	}
}

func TestCmdBarEnterRunsCommand(t *testing.T) {
	b := NewCmdBar(newTestEnv(t), newTestCommands(t))
	b.Open("")
	typeRunes(b, "hosts dc=ewr")

	cmd, done := press(b, tea.KeyEnter)
	if !done {
		t.Fatal("enter did not close the bar")
	}
	if cmd == nil {
		t.Fatal("enter returned no command")
	}
	msg, ok := cmd().(RunCommandMsg)
	if !ok {
		t.Fatalf("enter emitted %T, want RunCommandMsg", msg)
	}
	if msg.Line != "hosts dc=ewr" {
		t.Errorf("line = %q, want %q", msg.Line, "hosts dc=ewr")
	}
	if b.Active() {
		t.Error("enter left the bar active")
	}
	if got := b.History(); len(got) != 1 || got[0] != "hosts dc=ewr" {
		t.Errorf("history = %v, want the submitted line", got)
	}
}

func TestCmdBarEnterEmpty(t *testing.T) {
	b := NewCmdBar(newTestEnv(t), newTestCommands(t))
	b.Open("   ")
	cmd, done := press(b, tea.KeyEnter)
	if !done || cmd != nil {
		t.Errorf("empty enter = (%v, %v), want (nil, true)", cmd, done)
	}
	if len(b.History()) != 0 {
		t.Errorf("history = %v, want empty", b.History())
	}
}

func TestHistoryNoDuplicates(t *testing.T) {
	b := NewCmdBar(newTestEnv(t), newTestCommands(t))
	for _, line := range []string{"hosts", "hosts", "kinds", "hosts"} {
		submit(b, line)
	}
	want := []string{"hosts", "kinds", "hosts"}
	got := b.History()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("history = %v, want %v (consecutive duplicates dropped)", got, want)
	}

	n := len(b.History())
	submit(b, fmt.Sprintf("line %d", n))
	submit(b, fmt.Sprintf("line %d", n))
	if got := len(b.History()); got != n+1 {
		t.Errorf("history length = %d, want %d", got, n+1)
	}
}

func TestCmdBarHistoryCap(t *testing.T) {
	b := NewCmdBar(nil, nil)
	history := make([]string, 0, cmdBarHistoryMax+25)
	for i := 0; i < cmdBarHistoryMax+25; i++ {
		history = append(history, fmt.Sprintf("cmd %d", i))
	}
	b.SetHistory(history)

	got := b.History()
	if len(got) != cmdBarHistoryMax {
		t.Fatalf("history length = %d, want %d", len(got), cmdBarHistoryMax)
	}
	if got[len(got)-1] != history[len(history)-1] {
		t.Errorf("newest = %q, want %q", got[len(got)-1], history[len(history)-1])
	}
	if got[0] != history[len(history)-cmdBarHistoryMax] {
		t.Errorf("oldest kept = %q, want %q", got[0], history[len(history)-cmdBarHistoryMax])
	}
}

func TestCmdBarHistoryWalk(t *testing.T) {
	b := NewCmdBar(newTestEnv(t), newTestCommands(t))
	submit(b, "hosts")
	submit(b, "kinds")

	b.Open("")
	press(b, tea.KeyUp)
	if got := b.input.Value(); got != "kinds" {
		t.Fatalf("up = %q, want the newest entry", got)
	}
	press(b, tea.KeyUp)
	if got := b.input.Value(); got != "hosts" {
		t.Fatalf("up twice = %q, want the oldest entry", got)
	}
	press(b, tea.KeyUp)
	if got := b.input.Value(); got != "hosts" {
		t.Errorf("up past the oldest = %q, want it to stay", got)
	}
	press(b, tea.KeyDown)
	if got := b.input.Value(); got != "kinds" {
		t.Errorf("down = %q, want kinds", got)
	}
	press(b, tea.KeyDown)
	if got := b.input.Value(); got != "" {
		t.Errorf("down past the newest = %q, want the empty draft", got)
	}
}

func TestCmdBarHistoryKeepsDraft(t *testing.T) {
	b := NewCmdBar(nil, nil)
	b.SetHistory([]string{"hosts"})
	b.Open("half-typed")
	press(b, tea.KeyUp)
	if got := b.input.Value(); got != "hosts" {
		t.Fatalf("up = %q, want hosts", got)
	}
	press(b, tea.KeyDown)
	if got := b.input.Value(); got != "half-typed" {
		t.Errorf("down = %q, want the draft restored", got)
	}
}

func TestTabCompletesKind(t *testing.T) {
	b := NewCmdBar(newTestEnv(t), newTestCommands(t))
	b.Open("ho")

	press(b, tea.KeyTab)
	if got := b.input.Value(); got != "host " {
		t.Fatalf("first tab = %q, want %q", got, "host ")
	}
	press(b, tea.KeyTab)
	if got := b.input.Value(); got != "hosts " {
		t.Errorf("second tab = %q, want it to cycle to %q", got, "hosts ")
	}
	press(b, tea.KeyTab)
	if got := b.input.Value(); got != "host " {
		t.Errorf("third tab = %q, want it to cycle back", got)
	}
}

func TestTabCompletesCommand(t *testing.T) {
	cmds := NewCommands()
	RegisterBuiltins(cmds)
	b := NewCmdBar(nil, cmds)
	b.Open("he")
	press(b, tea.KeyTab)
	if got := b.input.Value(); got != "help " {
		t.Errorf("tab = %q, want %q", got, "help ")
	}
}

func TestTabCompletesAlias(t *testing.T) {
	cmds := NewCommands()
	run := func(*Env, string) tea.Cmd { return nil }
	if err := cmds.Register(Command{Name: "quit", Aliases: []string{"q"}, Run: run}); err != nil {
		t.Fatal(err)
	}
	b := NewCmdBar(nil, cmds)
	b.Open("q")
	press(b, tea.KeyTab)
	if got := b.input.Value(); got != "q " {
		t.Errorf("tab = %q, want %q", got, "q ")
	}
}

func TestTabCompletesArgument(t *testing.T) {
	cmds := NewCommands()
	run := func(*Env, string) tea.Cmd { return nil }
	if err := cmds.Register(Command{
		Name:     "open",
		Run:      run,
		Complete: func(_ *Env, args string) []string { return []string{"bar", "baz", "qux"} },
	}); err != nil {
		t.Fatal(err)
	}
	b := NewCmdBar(nil, cmds)
	b.Open("open ba")

	press(b, tea.KeyTab)
	if got := b.input.Value(); got != "open bar" {
		t.Fatalf("tab = %q, want %q", got, "open bar")
	}
	press(b, tea.KeyTab)
	if got := b.input.Value(); got != "open baz" {
		t.Errorf("second tab = %q, want %q", got, "open baz")
	}
}

func TestTabNoCandidatesLeavesValue(t *testing.T) {
	b := NewCmdBar(newTestEnv(t), newTestCommands(t))
	b.Open("zzz")
	press(b, tea.KeyTab)
	if got := b.input.Value(); got != "zzz" {
		t.Errorf("tab = %q, want the value untouched", got)
	}
	if len(b.matches) != 0 {
		t.Errorf("matches = %v, want none", b.matches)
	}
}

func TestTabShowsCandidates(t *testing.T) {
	b := NewCmdBar(newTestEnv(t), newTestCommands(t))
	b.Open("ho")
	press(b, tea.KeyTab)

	view := b.View(40)
	if got := render.Width(view); got != 40 {
		t.Errorf("View width = %d, want 40", got)
	}
	if !strings.Contains(view, "hosts") {
		t.Errorf("View %q does not list the other candidate", view)
	}
}

func TestCmdBarViewWidth(t *testing.T) {
	b := NewCmdBar(newTestEnv(t), newTestCommands(t))
	b.Open("hosts role=web dc=ewr")
	for _, w := range []int{1, 2, 5, 10, 24, 80} {
		if got := render.Width(b.View(w)); got != w {
			t.Errorf("View(%d) width = %d", w, got)
		}
	}
	if got := b.View(0); got != "" {
		t.Errorf("View(0) = %q, want empty", got)
	}
}

func TestCmdBarInactiveViewIsBlank(t *testing.T) {
	b := NewCmdBar(nil, nil)
	if got := b.View(12); got != strings.Repeat(" ", 12) {
		t.Errorf("inactive View = %q, want blank", got)
	}
}

func TestCmdBarTypingUpdatesValue(t *testing.T) {
	b := NewCmdBar(newTestEnv(t), newTestCommands(t))
	b.Open("")
	typeRunes(b, "kind")
	press(b, tea.KeyBackspace)
	typeRunes(b, "ds")
	if got := b.input.Value(); got != "kinds" {
		t.Errorf("value = %q, want kinds", got)
	}
}
