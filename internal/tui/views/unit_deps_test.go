package views

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend/linux"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/tui"
)

// depsFake is the unit fake plus the dependency batch.
type depsFake struct {
	*unitFake
	deps  linux.Dependencies
	err   error
	calls int
}

func (f *depsFake) UnitDependencies(_ context.Context, _ resource.HostID, _ string) (linux.Dependencies, error) {
	f.calls++
	if f.err != nil {
		return linux.Dependencies{}, f.err
	}
	return f.deps, nil
}

// depsTestTree is a two-level forward tree and a two-level reverse tree.
func depsTestTree() linux.Dependencies {
	return linux.Dependencies{
		Forward: []linux.DepNode{
			{Unit: "nginx.service", Depth: 0},
			{Unit: "system.slice", Depth: 1},
			{Unit: "systemd-journald.service", Depth: 2},
			{Unit: "nginx.service", Depth: 1},
			{Unit: "local-fs.target", Depth: 1},
		},
		Reverse: []linux.DepNode{
			{Unit: "nginx.service", Depth: 0},
			{Unit: "multi-user.target", Depth: 1},
			{Unit: "graphical.target", Depth: 2},
		},
	}
}

// depsFixture loads a Unit view whose deps tab has been opened once.
func depsFixture(t *testing.T) (*Unit, *depsFake) {
	t.Helper()
	f := &depsFake{unitFake: newUnitFake(), deps: depsTestTree()}
	u := NewUnit(testEnv(), f, "web-01", "nginx.service")
	drainUnit(t, u, u.Init())
	drainUnit(t, u, sendUnitKey(t, u, "5"))
	return u, f
}

func TestDepsTabShowsBothSectionsCollapsed(t *testing.T) {
	u, f := depsFixture(t)
	if f.calls != 1 {
		t.Errorf("dependency batch ran %d times, want 1", f.calls)
	}
	if u.tabs.Active() != u.dep {
		t.Fatalf("after 5 Active = %d, want the deps tab (%d)", u.tabs.Active(), u.dep)
	}
	if !u.tabs.Visited(u.dep) {
		t.Fatal("the deps tab was not initialised on its first visit")
	}

	frame := stripHostANSI(u.Render(100, 30))
	for _, want := range []string{
		depsForwardTitle, depsReverseTitle,
		"system.slice", "local-fs.target", "multi-user.target",
		"5 deps",
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("deps frame does not contain %q:\n%s", want, frame)
		}
	}
	// Everything below depth 2 is collapsed until its parent is expanded.
	for _, hidden := range []string{"systemd-journald.service", "graphical.target"} {
		if strings.Contains(frame, hidden) {
			t.Errorf("collapsed tree still shows %q:\n%s", hidden, frame)
		}
	}
}

func TestDepsTabExpandRevealsDeeperNodes(t *testing.T) {
	u, _ := depsFixture(t)
	rows := u.deps.rows
	// Move to system.slice, the first depth-1 node, and expand it.
	drainUnit(t, u, sendUnitKey(t, u, "down"))
	if got := u.deps.rows[u.deps.cursor].node.Unit; got != "system.slice" {
		t.Fatalf("cursor on %q after one down, want system.slice (rows %d)", got, len(rows))
	}
	drainUnit(t, u, sendUnitKey(t, u, " "))
	frame := stripHostANSI(u.Render(100, 30))
	if !strings.Contains(frame, "systemd-journald.service") {
		t.Errorf("expanded node did not reveal its child:\n%s", frame)
	}
	if strings.Contains(frame, "graphical.target") {
		t.Errorf("expanding one node revealed the other section:\n%s", frame)
	}
	// Expand the reverse tree's only depth-1 node too.
	for i := 0; i < 5; i++ {
		drainUnit(t, u, sendUnitKey(t, u, "down"))
	}
	drainUnit(t, u, sendUnitKey(t, u, " "))
	if frame := stripHostANSI(u.Render(100, 30)); !strings.Contains(frame, "graphical.target") {
		t.Errorf("reverse expansion did not reveal the child:\n%s", frame)
	}
	// Collapsing again hides it.
	drainUnit(t, u, sendUnitKey(t, u, " "))
	if frame := stripHostANSI(u.Render(100, 30)); strings.Contains(frame, "graphical.target") {
		t.Errorf("collapse left the child visible:\n%s", frame)
	}
}

func TestDepsTabEnterOpensUnitDetail(t *testing.T) {
	u, _ := depsFixture(t)
	drainUnit(t, u, sendUnitKey(t, u, "down"))
	drainUnit(t, u, sendUnitKey(t, u, "down"))
	if got := u.deps.rows[u.deps.cursor].node.Unit; got != "nginx.service" {
		t.Fatalf("cursor on %q, want nginx.service", got)
	}
	cmd := sendUnitKey(t, u, "enter")
	if cmd == nil {
		t.Fatal("enter returned no command")
	}
	msg, ok := cmd().(tui.OpenQueryMsg)
	if !ok {
		t.Fatalf("enter returned %T, want tui.OpenQueryMsg", cmd())
	}
	if msg.Query.Kind != linux.KindUnitDetail {
		t.Errorf("query kind = %q, want %q", msg.Query.Kind, linux.KindUnitDetail)
	}
	if got := msg.Query.Args["unit"]; got != "nginx.service" {
		t.Errorf("query unit = %q, want nginx.service", got)
	}
	if len(msg.Query.Hosts) != 1 || msg.Query.Hosts[0] != "web-01" {
		t.Errorf("query hosts = %v", msg.Query.Hosts)
	}
}

func TestDepsTabRenderExactSize(t *testing.T) {
	u, _ := depsFixture(t)
	for _, size := range [][2]int{{60, 15}, {120, 30}, {30, 8}} {
		frame := u.Render(size[0], size[1])
		lines := strings.Split(frame, "\n")
		if len(lines) != size[1] {
			t.Fatalf("Render(%d,%d) gave %d lines", size[0], size[1], len(lines))
		}
		for i, line := range lines {
			if got := render.Width(line); got != size[0] {
				t.Fatalf("Render(%d,%d) line %d is %d cells", size[0], size[1], i, got)
			}
		}
	}
}

func TestDepsTabWithoutBackendSupport(t *testing.T) {
	// The plain unit fake has no dependency batch: the tab says so, it does not
	// panic or hang.
	u := NewUnit(testEnv(), newUnitFake(), "web-01", "nginx.service")
	drainUnit(t, u, u.Init())
	drainUnit(t, u, sendUnitKey(t, u, "5"))
	tab, ok := u.deps, u.tabs.Active() == u.dep
	if !ok {
		t.Fatalf("deps tab is not active")
	}
	if !strings.Contains(stripHostANSI(tab.Render(80, 10)), "no dependency tree") {
		t.Errorf("deps tab did not report the missing batch:\n%s", stripHostANSI(tab.Render(80, 10)))
	}
}

func TestDepsTabCopyLines(t *testing.T) {
	u, _ := depsFixture(t)
	lines := u.deps.CopyLines()
	if len(lines) == 0 {
		t.Fatal("no copy lines")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, depsForwardTitle) || !strings.Contains(joined, "▸ system.slice") {
		t.Errorf("copy lines lost the tree:\n%s", joined)
	}
	var _ tui.View = u.deps
	var _ tea.Cmd = u.deps.Init()
}
