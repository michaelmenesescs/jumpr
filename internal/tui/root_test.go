package tui_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	teatest "github.com/charmbracelet/x/exp/teatest"

	"jumper/internal/backend"
	"jumper/internal/backend/fake"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/tui"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
	"jumper/internal/tui/views"
)

// newRoot builds a root model over the fake backend with the table view as the
// default factory. The colour profile is "none", so assertions can look for
// plain text.
func newRoot(t *testing.T, q resource.Query) *tui.Root {
	t.Helper()
	theme.SetProfile("none")
	reg := backend.NewRegistry()
	if err := reg.Register(fake.New(fake.Options{})); err != nil {
		t.Fatalf("register fake backend: %v", err)
	}
	env := &tui.Env{
		Registry:  reg,
		Session:   &tui.SessionState{Name: "main"},
		Theme:     theme.Dark(),
		Keys:      keymap.Default(),
		Now:       time.Now,
		Factories: make(map[resource.KindName]tui.Factory),
	}
	cmds := tui.NewCommands()
	tui.RegisterBuiltins(cmds)
	return tui.NewRoot(env, cmds, tableFactory, q)
}

// tableFactory is the same closure internal/app passes to the root.
func tableFactory(env *tui.Env, q resource.Query) (tui.View, error) {
	b, spec, ok := env.Registry.Lookup(string(q.Kind))
	if !ok {
		return nil, fmt.Errorf("unknown kind %q", q.Kind)
	}
	return views.NewTable(env, b, spec, q), nil
}

// start launches a test program for root at 120×36.
func start(t *testing.T, root *tui.Root) *teatest.TestModel {
	t.Helper()
	return teatest.NewTestModel(t, root, teatest.WithInitialTermSize(120, 36))
}

// waitFor waits until the output produced from now on contains every want.
func waitFor(t *testing.T, tm *teatest.TestModel, want ...string) {
	t.Helper()
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		for _, w := range want {
			if !bytes.Contains(b, []byte(w)) {
				return false
			}
		}
		return true
	}, teatest.WithDuration(3*time.Second))
}

// finish quits the program and returns the final model's rendered screen.
func finish(t *testing.T, tm *teatest.TestModel) string {
	t.Helper()
	tm.Quit()
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second))
	root, ok := m.(*tui.Root)
	if !ok {
		t.Fatalf("final model is %T, want *tui.Root", m)
	}
	return root.View()
}

// ewrHosts returns the first n hosts of the ewr data centre.
func ewrHosts(n int) []resource.HostID {
	out := make([]resource.HostID, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, resource.HostID(fmt.Sprintf("web-ewr-%03d", i)))
	}
	return out
}

func TestRootDemoShowsHosts(t *testing.T) {
	t.Run("explicit host list", func(t *testing.T) {
		tm := start(t, newRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)}))
		waitFor(t, tm, "web-ewr-007", "(24)")
		screen := finish(t, tm)
		if !strings.Contains(screen, "web-ewr-007") {
			t.Fatalf("final screen has no web-ewr-007:\n%s", screen)
		}
		if !strings.Contains(screen, "host (24)") {
			t.Fatalf("final screen has no \"host (24)\" crumb:\n%s", screen)
		}
	})

	t.Run("selector", func(t *testing.T) {
		sel, err := resource.ParseSelector("dc=ewr")
		if err != nil {
			t.Fatalf("parse selector: %v", err)
		}
		tm := start(t, newRoot(t, resource.Query{Kind: "host", Selector: sel}))
		waitFor(t, tm, "(504)")
		screen := finish(t, tm)
		if !strings.Contains(screen, "host dc=ewr (504)") {
			t.Fatalf("final screen has no \"host dc=ewr (504)\" crumb:\n%s", screen)
		}
	})
}

func TestRootCommandNavigates(t *testing.T) {
	tm := start(t, newRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)}))
	waitFor(t, tm, "web-ewr-007")
	tm.Type(":alerts")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "HighErrorRate")
	screen := finish(t, tm)
	if !strings.Contains(screen, "HighErrorRate") {
		t.Fatalf("final screen has no HighErrorRate alert:\n%s", screen)
	}
}

func TestRootFilter(t *testing.T) {
	tm := start(t, newRoot(t, resource.Query{Kind: "host"}))
	waitFor(t, tm, "web-ewr-")
	tm.Type("/007")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "filter: 007")
	screen := finish(t, tm)
	if !strings.Contains(screen, "web-ewr-007") {
		t.Fatalf("filtered screen has no web-ewr-007:\n%s", screen)
	}
	if strings.Contains(screen, "web-ewr-008") {
		t.Fatalf("filtered screen still shows web-ewr-008:\n%s", screen)
	}
}

func TestRootEnterEscStack(t *testing.T) {
	tm := start(t, newRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)}))
	waitFor(t, tm, "web-ewr-007")

	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "›")

	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	waitFor(t, tm, "24 hosts")
	screen := finish(t, tm)

	crumb := strings.Split(screen, "\n")[1]
	if !strings.Contains(crumb, "host (24)") {
		t.Fatalf("crumb after esc is %q, want it to contain \"host (24)\"", crumb)
	}
	if strings.Contains(crumb, "›") {
		t.Fatalf("crumb after esc is %q, want a single crumb", crumb)
	}
}

func TestRootHelp(t *testing.T) {
	tm := start(t, newRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)}))
	waitFor(t, tm, "web-ewr-007")

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	waitFor(t, tm, "Global")

	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	screen := finish(t, tm)
	if strings.Contains(screen, "Global") {
		t.Fatalf("help dialog still shown after esc:\n%s", screen)
	}
}

func TestRootUnknownCommandFlashes(t *testing.T) {
	tm := start(t, newRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)}))
	waitFor(t, tm, "web-ewr-007")

	tm.Type(":nope")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "unknown command: nope")
	screen := finish(t, tm)
	if !strings.Contains(screen, "unknown command: nope") {
		t.Fatalf("final screen has no unknown-command flash:\n%s", screen)
	}
}

func TestRootQuit(t *testing.T) {
	tm := start(t, newRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)}))
	waitFor(t, tm, "web-ewr-007")

	tm.Type(":q")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))
}

func TestRootViewSize(t *testing.T) {
	root := newRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(4)})
	if got := root.View(); got != "" {
		t.Fatalf("View before the first WindowSizeMsg is %q, want \"\"", got)
	}

	_, _ = root.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	screen := root.View()
	lines := strings.Split(screen, "\n")
	if len(lines) != 24 {
		t.Fatalf("View has %d lines, want 24", len(lines))
	}
	for i, line := range lines {
		if w := render.Width(line); w != 80 {
			t.Fatalf("line %d is %d cells wide, want 80", i, w)
		}
	}

	// The layout is status (1), crumbs (1), body, flash/cmd (1), hints (1).
	if !strings.Contains(lines[0], "main") {
		t.Fatalf("status line is %q, want the session name", lines[0])
	}
	if !strings.Contains(lines[1], "host (0)") {
		t.Fatalf("crumb line is %q, want \"host (0)\" before the first stream", lines[1])
	}
}
