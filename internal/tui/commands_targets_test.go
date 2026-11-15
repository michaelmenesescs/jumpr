package tui_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend"
	"jumpr/internal/backend/fake"
	"jumpr/internal/inventory"
	"jumpr/internal/resource"
	"jumpr/internal/targets"
	"jumpr/internal/tui"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
)

// targetInventory is a fixed inventory of 504 ewr hosts and 96 lax hosts, the
// shape the fake backend uses for its data centres.
type targetInventory struct {
	hosts []inventory.Host
}

func newTargetInventory() *targetInventory {
	out := make([]inventory.Host, 0, 600)
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
	add("ewr", 504)
	add("lax", 96)
	return &targetInventory{hosts: out}
}

func (t *targetInventory) Hosts() []inventory.Host { return t.hosts }

func (t *targetInventory) Select(sel resource.Selector) []inventory.Host {
	out := make([]inventory.Host, 0, len(t.hosts))
	for _, h := range t.hosts {
		if sel.Matches(h.Labels) {
			out = append(out, h)
		}
	}
	return out
}

func (t *targetInventory) Resolve(name string) (inventory.Host, bool) {
	for _, h := range t.hosts {
		if string(h.ID) == name {
			return h, true
		}
	}
	return inventory.Host{}, false
}

func (t *targetInventory) Refresh(context.Context) map[string]error { return nil }

func (t *targetInventory) Age() time.Duration { return 0 }

// newTargetsRoot builds a root over the fake backend with an inventory and a
// saved-set store, so the `:targets` commands have something to resolve
// against.
func newTargetsRoot(t *testing.T, q resource.Query) (*tui.Root, *targets.Store) {
	t.Helper()
	theme.SetProfile("none")
	reg := backend.NewRegistry()
	if err := reg.Register(fake.New(fake.Options{})); err != nil {
		t.Fatalf("register fake backend: %v", err)
	}
	store := targets.NewStore(t.TempDir())
	env := &tui.Env{
		Registry:  reg,
		Inventory: newTargetInventory(),
		Sets:      store,
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
	return tui.NewRoot(env, cmds, tableFactory, q, "ctrl+a"), store
}

// TestHostsCommandSetsTargets checks that opening a host list with a selector
// makes that selector the session target set: the status line counts the
// session targets, not the focused table.
func TestHostsCommandSetsTargets(t *testing.T) {
	root, _ := newTargetsRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)})
	tm := start(t, root)
	waitFor(t, tm, "web-ewr-007")

	tm.Type(":hosts dc=ewr")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "targets:504")

	screen := finish(t, tm)
	status := strings.Split(screen, "\n")[0]
	if !strings.Contains(status, "targets:504") {
		t.Fatalf("status line is %q, want targets:504", status)
	}
}

// TestMarkedRowsBecomeTargets marks two rows with space and turns them into the
// session target set with `:targets marked`.
func TestMarkedRowsBecomeTargets(t *testing.T) {
	root, _ := newTargetsRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)})
	tm := start(t, root)
	waitFor(t, tm, "web-ewr-007")

	tm.Send(tea.KeyMsg{Type: tea.KeySpace})
	tm.Send(tea.KeyMsg{Type: tea.KeyDown})
	tm.Send(tea.KeyMsg{Type: tea.KeySpace})
	tm.Type(":targets marked")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "targets: 2 hosts (2 marked)")

	screen := finish(t, tm)
	status := strings.Split(screen, "\n")[0]
	if !strings.Contains(status, "targets:2") {
		t.Fatalf("status line is %q, want targets:2", status)
	}
	if !strings.Contains(screen, "▸") {
		t.Fatalf("marked rows have no ▸ gutter:\n%s", screen)
	}
}

// TestTargetsViewListsSet checks that a bare `:targets` opens a text view of
// the current set instead of changing it.
func TestTargetsViewListsSet(t *testing.T) {
	root, _ := newTargetsRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)})
	tm := start(t, root)
	waitFor(t, tm, "web-ewr-007")

	tm.Type(":hosts dc=ewr")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "targets:504")

	tm.Type(":targets")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "hosts: 504")

	screen := finish(t, tm)
	if !strings.Contains(screen, "label: dc=ewr") {
		t.Fatalf("targets view has no label line:\n%s", screen)
	}
	if !strings.Contains(screen, "web-ewr-001") {
		t.Fatalf("targets view does not list the hosts:\n%s", screen)
	}
}

// TestTargetsSetScopesHostList checks the last rule of requirement 5: a
// host-scoped list opened with no hosts and no selector uses the session
// target set. The targets view is focused first so no row host wins.
func TestTargetsSetScopesHostList(t *testing.T) {
	root, _ := newTargetsRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)})
	tm := start(t, root)
	waitFor(t, tm, "web-ewr-007")

	tm.Type(":targets dc=ewr")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "targets: 504 hosts (dc=ewr)")

	tm.Type(":targets")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "hosts: 504")

	tm.Type(":units")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	// The fake backend lists five units per host, so 504 hosts are 2,520 rows.
	waitFor(t, tm, "unit (2520)")
}

// TestTargetsSaveLoad saves the current set, clears it and loads it back.
func TestTargetsSaveLoad(t *testing.T) {
	root, store := newTargetsRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(24)})
	tm := start(t, root)
	waitFor(t, tm, "web-ewr-007")

	tm.Type(":hosts dc=ewr")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "targets:504")

	tm.Type(":targets save canary")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "saved 504 hosts as canary")

	tm.Type(":targets clear")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "targets: 0 hosts")

	tm.Type(":targets load canary")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, "targets: 504 hosts (@canary)")

	screen := finish(t, tm)
	status := strings.Split(screen, "\n")[0]
	if !strings.Contains(status, "targets:504") {
		t.Fatalf("status line is %q, want targets:504 after the load", status)
	}

	hosts, err := store.Load("canary")
	if err != nil {
		t.Fatalf("load saved set: %v", err)
	}
	if len(hosts) != 504 {
		t.Fatalf("saved set has %d hosts, want 504", len(hosts))
	}
}
