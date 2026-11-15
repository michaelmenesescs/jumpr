package tui_test

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend"
	"jumpr/internal/backend/fake"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
	"jumpr/internal/transport/transporttest"
	"jumpr/internal/tui"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
)

// newSSHRoot builds a root model with an SSH opener wired into the env, the way
// internal/app will once it has a transport.
func newSSHRoot(t *testing.T, opener transport.ShellOpener) *tui.Root {
	t.Helper()
	theme.SetProfile("none")
	reg := backend.NewRegistry()
	if err := reg.Register(fake.New(fake.Options{})); err != nil {
		t.Fatalf("register fake backend: %v", err)
	}
	env := &tui.Env{
		Registry:  reg,
		Shells:    opener,
		Session:   &tui.SessionState{Name: "main"},
		Theme:     theme.Dark(),
		Keys:      keymap.Default(),
		Now:       time.Now,
		Factories: make(map[resource.KindName]tui.Factory),
	}
	for _, spec := range reg.Kinds() {
		env.Factories[spec.Name] = tableFactory
	}
	cmds := tui.NewCommands()
	tui.RegisterBuiltins(cmds)
	if err := tui.RegisterMuxCommands(cmds); err != nil {
		t.Fatalf("register mux commands: %v", err)
	}
	return tui.NewRoot(env, cmds, tableFactory, resource.Query{Kind: "host", Hosts: []resource.HostID{"web-ewr-001", "web-ewr-002", "web-ewr-007"}}, "ctrl+a")
}

func TestSSHCommandAddsPane(t *testing.T) {
	opener := transporttest.New()
	root := newSSHRoot(t, opener)
	tm := start(t, root)
	waitFor(t, tm, "web-ewr-007")

	tm.Type(":ssh web-ewr-001")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "2:ssh web-ewr-001")

	screen := finish(t, tm)
	if !strings.Contains(screen, "ssh web-ewr-001") {
		t.Fatalf("screen has no terminal pane titled for the host:\n%s", screen)
	}
	// The first pane is still the host list.
	if !strings.Contains(screen, "web-ewr-") {
		t.Fatalf("the host list pane is gone:\n%s", screen)
	}
}

func TestSSHCommandWithoutOpenerFlashes(t *testing.T) {
	root := newSSHRoot(t, nil)
	tm := start(t, root)
	waitFor(t, tm, "web-ewr-007")

	tm.Type(":ssh web-ewr-001")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "no SSH in demo mode")

	screen := finish(t, tm)
	if !strings.Contains(screen, "no SSH in demo mode") {
		t.Fatalf("screen has no demo-mode flash:\n%s", screen)
	}
}
