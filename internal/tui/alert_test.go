package tui_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	teatest "github.com/charmbracelet/x/exp/teatest"

	"jumpr/internal/backend"
	"jumpr/internal/backend/fake"
	"jumpr/internal/inventory"
	"jumpr/internal/investigation"
	"jumpr/internal/redact"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
	"jumpr/internal/tui/views"
)

// alertNow is the fixed clock of the alert-opening test.
var alertNow = time.Date(2026, 11, 12, 14, 2, 11, 0, time.FixedZone("EST", -5*60*60))

// alertInventory is three ewr hosts and two lax hosts, with the label keys the
// fake alerts carry (dc, role, host).
type alertInventory struct {
	hosts []inventory.Host
}

func newAlertInventory() *alertInventory {
	out := make([]inventory.Host, 0, 5)
	add := func(dc string, n int) {
		for i := 1; i <= n; i++ {
			id := resource.HostID(fmt.Sprintf("web-%s-%03d", dc, i))
			out = append(out, inventory.Host{
				ID:     id,
				Addr:   string(id),
				Labels: map[string]string{"host": string(id), "role": "web", "dc": dc},
			})
		}
	}
	add("ewr", 3)
	add("lax", 2)
	return &alertInventory{hosts: out}
}

func (a *alertInventory) Hosts() []inventory.Host { return a.hosts }

func (a *alertInventory) Select(sel resource.Selector) []inventory.Host {
	out := make([]inventory.Host, 0, len(a.hosts))
	for _, h := range a.hosts {
		if sel.Matches(h.Labels) {
			out = append(out, h)
		}
	}
	return out
}

func (a *alertInventory) Resolve(name string) (inventory.Host, bool) {
	for _, h := range a.hosts {
		if string(h.ID) == strings.ToLower(strings.TrimSpace(name)) {
			return h, true
		}
	}
	return inventory.Host{}, false
}

func (a *alertInventory) Refresh(context.Context) map[string]error { return nil }

func (a *alertInventory) Age() time.Duration { return 0 }

// alertHarness is a root over the fake backend (which serves alerts and hosts),
// a five-host inventory and a real investigation record on disk.
type alertHarness struct {
	root *tui.Root
	env  *tui.Env
	dir  string
}

func newAlertHarness(t *testing.T) *alertHarness {
	t.Helper()
	theme.SetProfile("none")
	dir := t.TempDir()
	red, err := redact.New(nil)
	if err != nil {
		t.Fatalf("redact.New: %v", err)
	}
	log := investigation.NewLog(dir, red, func() time.Time { return alertNow })

	reg := backend.NewRegistry()
	if err := reg.Register(fake.New(fake.Options{Now: func() time.Time { return alertNow }})); err != nil {
		t.Fatalf("register fake backend: %v", err)
	}
	env := &tui.Env{
		Registry:  reg,
		Inventory: newAlertInventory(),
		Session:   &tui.SessionState{Name: "main"},
		Theme:     theme.Dark(),
		Keys:      keymap.Default(),
		Now:       func() time.Time { return alertNow },
		Factories: make(map[resource.KindName]tui.Factory),
		Recorder:  log,
	}
	for _, spec := range reg.Kinds() {
		env.Factories[spec.Name] = tableFactory
	}
	// The timeline is not served by a backend: the views package registers its
	// factory under the synthetic kind, as internal/app does.
	for kind, f := range views.Factories() {
		env.Factories[kind] = f
	}
	cmds := tui.NewCommands()
	tui.RegisterBuiltins(cmds)
	if err := tui.RegisterMuxCommands(cmds); err != nil {
		t.Fatalf("register mux commands: %v", err)
	}
	if err := tui.RegisterInvestigationCommands(cmds); err != nil {
		t.Fatalf("register investigation commands: %v", err)
	}
	root := tui.NewRoot(env, cmds, tableFactory, resource.Query{Kind: "alert"}, "ctrl+a")
	return &alertHarness{root: root, env: env, dir: dir}
}

// record returns the one investigation file the harness wrote.
func (h *alertHarness) record(t *testing.T) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(h.dir, "investigations", "*.md"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("investigation files = %v, %v; want exactly one", paths, err)
	}
	return paths[0]
}

// openAlertRow filters to the named alert and presses enter on it.
func openAlertRow(t *testing.T, tm *teatest.TestModel, name string) {
	t.Helper()
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	tm.Type(name)
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter}) // keep the filter
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter}) // open the alert
}

// TestAlertOpenInvestigation is SPEC W1: enter on a firing alert opens an
// investigation with the alert's window and hosts, pushes the host list and
// records an `open` entry.
func TestAlertOpenInvestigation(t *testing.T) {
	h := newAlertHarness(t)
	tm := start(t, h.root)
	waitFor(t, tm, "HighErrorRate")

	openAlertRow(t, tm, "HighErrorRate")
	waitFor(t, tm, "inv:2026-11-12-higherrorrate-dc-ewr-service-api", "targets:3", "host (3)")

	file := waitFile(t, h.record(t), "**open**", "HighErrorRate")
	if !strings.Contains(file, "dc=ewr") {
		t.Fatalf("investigation title has no main label:\n%s", file)
	}
	if !strings.Contains(file, "web-ewr-001") {
		t.Fatalf("investigation frontmatter has no targets:\n%s", file)
	}
	if !strings.Contains(file, "window:") {
		t.Fatalf("investigation has no window:\n%s", file)
	}

	cur := h.env.Session.Investigation
	if cur == nil {
		t.Fatalf("session has no investigation")
	}
	if len(cur.Targets) != 3 {
		t.Fatalf("investigation targets = %v, want the three ewr hosts", cur.Targets)
	}

	// The timeline shows the `open` bullet the record carries.
	sendKey(tm, "T")
	waitFor(t, tm, "## Timeline", "**open**", "HighErrorRate")

	screen := finish(t, tm)
	for _, want := range []string{"inv:", "targets:3", "**open**"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("final screen is missing %q:\n%s", want, screen)
		}
	}
}

// TestAlertOpenNoHostsOpensEmpty: an alert whose labels match no inventory host
// still opens a record, with no targets and a flash saying why.
func TestAlertOpenNoHostsOpensEmpty(t *testing.T) {
	h := newAlertHarness(t)
	tm := start(t, h.root)
	waitFor(t, tm, "CertificateExpiry")

	openAlertRow(t, tm, "CertificateExpiry")
	waitFor(t, tm, "inv:2026-11-12-certificateexpiry-domain-www-example-internal", "targets:0")

	file := waitFile(t, h.record(t), "**open**", "CertificateExpiry")
	if strings.Contains(file, "on 0 hosts") {
		t.Fatalf("record claims a host count of zero:\n%s", file)
	}
}
