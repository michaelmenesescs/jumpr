package tui_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	teatest "github.com/charmbracelet/x/exp/teatest"

	"jumper/internal/backend"
	"jumper/internal/backend/fake"
	"jumper/internal/resource"
	"jumper/internal/tui"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
	"jumper/internal/tui/views"
)

// newAppRoot builds a root model the way internal/app does: only the views
// package's own factories are registered, so every other kind has to fall back
// to the default table factory the root was given.
func newAppRoot(t *testing.T, start resource.Query, extra map[resource.KindName]tui.Factory) *tui.Root {
	t.Helper()
	theme.SetProfile("none")
	reg := backend.NewRegistry()
	if err := reg.Register(fake.New(fake.Options{})); err != nil {
		t.Fatalf("register fake backend: %v", err)
	}
	factories := views.Factories()
	for kind, f := range extra {
		factories[kind] = f
	}
	env := &tui.Env{
		Registry:  reg,
		Session:   &tui.SessionState{Name: "main"},
		Theme:     theme.Dark(),
		Keys:      keymap.Default(),
		Now:       time.Now,
		Factories: factories,
	}
	cmds := tui.NewCommands()
	tui.RegisterBuiltins(cmds)
	if err := tui.RegisterMuxCommands(cmds); err != nil {
		t.Fatalf("register mux commands: %v", err)
	}
	return tui.NewRoot(env, cmds, tableFactory, start, "ctrl+a")
}

// startSmall launches a test program for root at the 110×18 size the task's
// manual check uses.
func startSmall(t *testing.T, root *tui.Root) *teatest.TestModel {
	t.Helper()
	return teatest.NewTestModel(t, root, teatest.WithInitialTermSize(110, 18))
}

// feed sends one message to the root and returns the command it produced.
func feed(t *testing.T, root *tui.Root, msg tea.Msg) tea.Cmd {
	t.Helper()
	m, cmd := root.Update(msg)
	if m != root {
		t.Fatalf("root.Update(%#v) returned a different model", msg)
	}
	return cmd
}

// crumbLine is the breadcrumb line of a rendered screen.
func crumbLine(screen string) string {
	lines := strings.Split(screen, "\n")
	if len(lines) < 2 {
		return ""
	}
	return lines[1]
}

// sendLine types a `:` command line into the command bar and submits it.
func sendLine(tm *teatest.TestModel, line string) {
	tm.Type(line)
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
}

// TestCmdNavKindFromCommandBar covers requirement 2 and 3: `:` + a kind or an
// alias + enter replaces the focused pane's view with that kind's view, and esc
// brings the old view back.
func TestCmdNavKindFromCommandBar(t *testing.T) {
	cases := []struct {
		line string
		want []string // text only the new view shows
	}{
		{line: ":units", want: []string{"sshd.service"}},
		{line: ":u", want: []string{"sshd.service"}},
		{line: ":alerts", want: []string{"HighErrorRate"}},
		{line: ":hosts", want: []string{"web-ewr-"}},
	}
	for _, tc := range cases {
		t.Run(tc.line, func(t *testing.T) {
			tm := startSmall(t, newAppRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(4)}, nil))
			waitFor(t, tm, "web-ewr-")

			sendLine(tm, tc.line)
			waitFor(t, tm, tc.want...)

			// The old view stays reachable: exactly one crumb separator while
			// the new view is on top.
			tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
			waitFor(t, tm, "4 hosts")

			screen := finish(t, tm)
			crumb := crumbLine(screen)
			if !strings.Contains(crumb, "host (4)") {
				t.Fatalf("crumb after esc is %q, want the host table back", crumb)
			}
			if strings.Contains(crumb, "›") {
				t.Fatalf("crumb after esc is %q, want a single crumb", crumb)
			}
		})
	}
}

// TestCmdNavCrumbAndBanner checks the crumb of the view a `:` kind opens, and
// that exactly one view was pushed.
func TestCmdNavCrumbAndBanner(t *testing.T) {
	tm := startSmall(t, newAppRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(4)}, nil))
	waitFor(t, tm, "web-ewr-")

	sendLine(tm, ":units")
	waitFor(t, tm, "sshd.service")

	screen := finish(t, tm)
	crumb := crumbLine(screen)
	if !strings.Contains(crumb, "unit (") {
		t.Fatalf("crumb is %q, want a unit view", crumb)
	}
	if n := strings.Count(crumb, "›"); n != 1 {
		t.Fatalf("crumb is %q with %d separators, want exactly one (one push)", crumb, n)
	}

	// The banner belongs to the unit table, not to the host table.
	if strings.Contains(screen, "4 hosts") {
		t.Fatalf("unit view still shows the host banner:\n%s", screen)
	}
}

// recordingFactory records the query it was handed and builds a stub view.
type recordingFactory struct {
	got []resource.Query
}

func (r *recordingFactory) factory(_ *tui.Env, q resource.Query) (tui.View, error) {
	r.got = append(r.got, q)
	return &crumbView{crumb: fmt.Sprintf("%s (recorded)", q.Kind)}, nil
}

// crumbView is the smallest possible view: a fixed crumb, body and subject.
type crumbView struct {
	crumb string
	subj  tui.Subject
}

func (v *crumbView) Init() tea.Cmd { return nil }
func (v *crumbView) Update(tea.Msg) (tui.View, tea.Cmd) {
	return v, nil
}
func (v *crumbView) Render(w, h int) string { return v.crumb }
func (v *crumbView) Crumb() string          { return v.crumb }
func (v *crumbView) Hints() []tui.KeyHint   { return nil }
func (v *crumbView) Subject() tui.Subject   { return v.subj }
func (v *crumbView) Capturing() bool        { return false }
func (v *crumbView) CopyLines() []string    { return nil }

// focusHost is the host the recording test's focused view is about; the real
// host table reports its selected row the same way.
const focusHost = resource.HostID("web-ewr-005")

// focusedHostFactory stands in for the host table: a view whose subject names
// one host, exactly what views.Table.Subject reports for its cursor row.
func focusedHostFactory(_ *tui.Env, q resource.Query) (tui.View, error) {
	return &crumbView{
		crumb: "host (1)",
		subj:  tui.Subject{Kind: q.Kind, Hosts: []resource.HostID{focusHost}, Ref: resource.Ref{Kind: q.Kind, Name: string(focusHost)}},
	}, nil
}

// TestOpenQueryRoutingHostScope checks that a host-scoped kind opened without
// an explicit host inherits the focused view's host, while an explicit --host
// always wins. It drives the message through the root exactly as the command
// bar does.
func TestOpenQueryRoutingHostScope(t *testing.T) {
	rec := &recordingFactory{}
	root := newAppRoot(t, resource.Query{Kind: "host"},
		map[resource.KindName]tui.Factory{"host": focusedHostFactory, "unit": rec.factory})
	feed(t, root, tea.WindowSizeMsg{Width: 110, Height: 18})

	open := func(line string) {
		t.Helper()
		cmd := feed(t, root, tui.RunCommandMsg{Line: line})
		if cmd == nil {
			t.Fatalf("%q produced no command", line)
		}
		msg := cmd()
		if _, ok := msg.(tui.OpenQueryMsg); !ok {
			t.Fatalf("%q produced %#v, want an OpenQueryMsg", line, msg)
		}
		if next := feed(t, root, msg); next != nil {
			_ = next()
		}
	}

	open(":units")
	if len(rec.got) != 1 {
		t.Fatalf("factory called %d times, want 1", len(rec.got))
	}
	q := rec.got[0]
	if q.Kind != "unit" {
		t.Fatalf("query kind = %q, want unit", q.Kind)
	}
	if len(q.Hosts) != 1 || q.Hosts[0] != focusHost {
		t.Fatalf("query hosts = %v, want the focused view's host %q", q.Hosts, focusHost)
	}
	crumb := crumbLine(root.View())
	if !strings.Contains(crumb, "unit (recorded)") {
		t.Fatalf("crumb is %q, want the unit view on top", crumb)
	}
	if n := strings.Count(crumb, "›"); n != 1 {
		t.Fatalf("crumb is %q with %d separators, want exactly one push", crumb, n)
	}

	// A selector travels with the query untouched.
	open(":units role=web")
	if len(rec.got) != 2 {
		t.Fatalf("factory called %d times, want 2", len(rec.got))
	}
	if got := rec.got[1].Selector.String(); got != "role=web" {
		t.Fatalf("selector = %q, want role=web", got)
	}

	// An explicit host list wins over the focused host.
	open(":units --host web-ewr-002,web-ewr-003")
	if len(rec.got) != 3 {
		t.Fatalf("factory called %d times, want 3", len(rec.got))
	}
	got := rec.got[2].Hosts
	if len(got) != 2 || got[0] != "web-ewr-002" || got[1] != "web-ewr-003" {
		t.Fatalf("explicit hosts = %v, want web-ewr-002 and web-ewr-003", got)
	}
}

// TestOpenQueryRoutingGlobalKind leaves global kinds alone: an alert query is
// not scoped to the focused view's host.
func TestOpenQueryRoutingGlobalKind(t *testing.T) {
	rec := &recordingFactory{}
	root := newAppRoot(t, resource.Query{Kind: "host"},
		map[resource.KindName]tui.Factory{"host": focusedHostFactory, "alert": rec.factory})
	feed(t, root, tea.WindowSizeMsg{Width: 110, Height: 18})

	cmd := feed(t, root, tui.RunCommandMsg{Line: ":alerts severity=critical"})
	msg := cmd()
	if next := feed(t, root, msg); next != nil {
		_ = next()
	}
	if len(rec.got) != 1 {
		t.Fatalf("factory called %d times, want 1", len(rec.got))
	}
	if hosts := rec.got[0].Hosts; len(hosts) != 0 {
		t.Fatalf("alert query hosts = %v, want none", hosts)
	}
	if got := rec.got[0].Selector.String(); got != "severity=critical" {
		t.Fatalf("selector = %q, want severity=critical", got)
	}
}

// TestCmdNavBadSelector checks requirement 4: a bad selector flashes an error
// and leaves the current view alone.
func TestCmdNavBadSelector(t *testing.T) {
	tm := startSmall(t, newAppRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(4)}, nil))
	waitFor(t, tm, "web-ewr-")

	sendLine(tm, ":units dc~[")
	waitFor(t, tm, "bad selector")

	screen := finish(t, tm)
	if crumb := crumbLine(screen); strings.Contains(crumb, "›") || !strings.Contains(crumb, "host (4)") {
		t.Fatalf("crumb after a bad selector is %q, want the host table untouched", crumb)
	}
}

// TestCmdNavUnknownCommand checks the unknown-word case: an error flash and no
// navigation.
func TestCmdNavUnknownCommand(t *testing.T) {
	tm := startSmall(t, newAppRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(4)}, nil))
	waitFor(t, tm, "web-ewr-")

	sendLine(tm, ":bogus")
	waitFor(t, tm, "unknown command: bogus")

	screen := finish(t, tm)
	if crumb := crumbLine(screen); strings.Contains(crumb, "›") || !strings.Contains(crumb, "host (4)") {
		t.Fatalf("crumb after an unknown command is %q, want the host table untouched", crumb)
	}
}
