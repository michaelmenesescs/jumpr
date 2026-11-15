package tui_test

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	teatest "github.com/charmbracelet/x/exp/teatest"

	"jumpr/internal/backend"
	"jumpr/internal/backend/fake"
	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/testload"
	"jumpr/internal/tui"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
	"jumpr/internal/tui/views"
)

// newRoot builds a root model over the fake backend with the table view as the
// default factory. The colour profile is "none", so assertions can look for
// plain text.
func newRoot(t *testing.T, q resource.Query) *tui.Root {
	t.Helper()
	return newRootWithPrefix(t, q, "ctrl+a")
}

// newRootWithPrefix is newRoot with an explicit prefix key.
func newRootWithPrefix(t *testing.T, q resource.Query, prefix string) *tui.Root {
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
	for _, spec := range reg.Kinds() {
		env.Factories[spec.Name] = tableFactory
	}
	cmds := tui.NewCommands()
	tui.RegisterBuiltins(cmds)
	if err := tui.RegisterMuxCommands(cmds); err != nil {
		t.Fatalf("register mux commands: %v", err)
	}
	return tui.NewRoot(env, cmds, tableFactory, q, prefix)
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
	}, teatest.WithDuration(testload.Scale(3*time.Second)))
}

// finish quits the program and returns the final model's rendered screen.
func finish(t *testing.T, tm *teatest.TestModel) string {
	t.Helper()
	tm.Quit()
	m := tm.FinalModel(t, teatest.WithFinalTimeout(testload.Scale(3*time.Second)))
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

	// The layout is status (1), body, flash/cmd (1), hints (1).
	if !strings.Contains(lines[0], "main") {
		t.Fatalf("status line is %q, want the session name", lines[0])
	}
	if !strings.Contains(lines[1], "host (0)") {
		t.Fatalf("crumb line is %q, want \"host (0)\" before the first stream", lines[1])
	}
}

// sendPrefix sends the prefix key and then one key after it.
func sendPrefix(t *testing.T, tm *teatest.TestModel, key string) {
	t.Helper()
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlA})
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
}

func TestRootSplitShowsTwoPanes(t *testing.T) {
	tm := start(t, newRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)}))
	waitFor(t, tm, "web-ewr-007")

	sendPrefix(t, tm, "%")
	waitFor(t, tm, "1:host", "2:host")

	screen := finish(t, tm)
	if !strings.Contains(screen, "1:host") {
		t.Fatalf("split screen has no first pane title:\n%s", screen)
	}
	if !strings.Contains(screen, "2:host") {
		t.Fatalf("split screen has no second pane title:\n%s", screen)
	}
}

func TestRootCommandInSecondPane(t *testing.T) {
	tm := start(t, newRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)}))
	waitFor(t, tm, "web-ewr-007")

	sendPrefix(t, tm, "%")
	waitFor(t, tm, "2:host")

	tm.Type(":alerts")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "HighErrorRate")

	screen := finish(t, tm)
	if !strings.Contains(screen, "HighErrorRate") {
		t.Fatalf("focused pane did not switch to alerts:\n%s", screen)
	}
	if !strings.Contains(screen, "web-ewr-") {
		t.Fatalf("the other pane no longer shows hosts:\n%s", screen)
	}
}

func TestRootNewWindowTabs(t *testing.T) {
	tm := start(t, newRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)}))
	waitFor(t, tm, "web-ewr-007", "0:*")

	sendPrefix(t, tm, "c")
	waitFor(t, tm, "1:*", "0:")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlA})
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("0")})
	waitFor(t, tm, "0:*")

	screen := finish(t, tm)
	status := strings.Split(screen, "\n")[0]
	if !strings.Contains(status, "0:*") {
		t.Fatalf("status line is %q, want window 0 active", status)
	}
}

func TestRootZoom(t *testing.T) {
	tm := start(t, newRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)}))
	waitFor(t, tm, "web-ewr-007")

	sendPrefix(t, tm, "%")
	waitFor(t, tm, "2:host")

	sendPrefix(t, tm, "z")
	waitFor(t, tm, "0: Z")

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlA})
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})
	waitFor(t, tm, "host (")
	screen := finish(t, tm)
	if strings.Contains(screen, "0: Z") {
		t.Fatalf("status line still marks the window zoomed:\n%s", screen)
	}
}

// fakeClip records every text the root hands to the clipboard.
type fakeClip struct {
	mu   sync.Mutex
	text []string
}

func (f *fakeClip) Copy(text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.text = append(f.text, text)
	return nil
}

func (f *fakeClip) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.text...)
}

func TestRootCopyModeYank(t *testing.T) {
	fc := &fakeClip{}
	root := newRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)})
	root.SetClipboard(fc)
	tm := start(t, root)
	waitFor(t, tm, "web-ewr-007")

	sendPrefix(t, tm, "[")
	waitFor(t, tm, "[copy]")

	// V selects lines, k grows the selection by one line, y yanks it.
	key := func(s string) {
		tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
	}
	key("V")
	key("k")
	key("y")
	waitFor(t, tm, "yanked 2 lines")

	screen := finish(t, tm)
	if strings.Contains(screen, "[copy]") {
		t.Fatalf("copy mode is still drawn after the yank:\n%s", screen)
	}
	got := fc.got()
	if len(got) != 1 {
		t.Fatalf("clipboard received %d copies, want 1: %q", len(got), got)
	}
	if n := strings.Count(got[0], "\n") + 1; n != 2 {
		t.Fatalf("clipboard received %d lines, want 2: %q", n, got[0])
	}
}

// waitForGone is deliberately not used: teatest accumulates output, so an
// absence can only be asserted on the final screen.
func TestRootKillPaneReturnsToOne(t *testing.T) {
	tm := start(t, newRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)}))
	waitFor(t, tm, "web-ewr-007")

	sendPrefix(t, tm, "%")
	waitFor(t, tm, "2:host")

	sendPrefix(t, tm, "x")

	// Keys are handled in order, so waiting for the help dialog that "?" opens
	// proves the kill key before it was processed.
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	waitFor(t, tm, "Panes and windows")
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})

	screen := finish(t, tm)
	if strings.Contains(screen, "2:host") {
		t.Fatalf("killed pane is still drawn:\n%s", screen)
	}
	if !strings.Contains(screen, "host (") {
		t.Fatalf("surviving pane is gone:\n%s", screen)
	}
}
