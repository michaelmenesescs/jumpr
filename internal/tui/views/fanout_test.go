package views

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend/linux"
	"jumper/internal/errs"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/targets"
	"jumper/internal/transport"
	"jumper/internal/transport/transporttest"
	"jumper/internal/tui"
)

// fanoutHostCount is the size of the fleet every fan-out test runs against.
const fanoutHostCount = 30

// fanoutHostIDs builds web-01 … web-30.
func fanoutHostIDs() []resource.HostID {
	out := make([]resource.HostID, fanoutHostCount)
	for i := range out {
		out[i] = resource.HostID(fmt.Sprintf("web-%02d", i+1))
	}
	return out
}

// fanoutFake scripts the acceptance fleet: 27 hosts with identical output, two
// that differ (from the majority and from each other's group only once) and one
// that times out.
func fanoutFake() *transporttest.Fake {
	ex := transporttest.New()
	ex.OnFunc(func(host resource.HostID, _ transport.Command) (transporttest.Reply, bool) {
		switch string(host) {
		case "web-01", "web-02":
			return transporttest.Reply{Stdout: "unit-a\nunit-c\n"}, true
		case "web-30":
			return transporttest.Reply{
				Err: errs.New(errs.Timeout, "fake.exec", "timed out").WithHost(string(host)),
			}, true
		}
		return transporttest.Reply{Stdout: "unit-a\nunit-b\n"}, true
	})
	return ex
}

// fanoutEnv is the environment a fan-out view runs in: a fixed clock, no colour
// and a transport that answers the scripted fleet.
func fanoutEnv(ex transport.Executor) *tui.Env {
	env := testEnv()
	env.Exec = ex
	env.Session = &tui.SessionState{Name: "main", Targets: targets.FromHosts("30 hosts", fanoutHostIDs())}
	return env
}

// newFanoutFixture builds one probe run over the scripted fleet.
func newFanoutFixture(t *testing.T) (*Fanout, *transporttest.Fake) {
	t.Helper()
	ex := fanoutFake()
	f := NewFanout(fanoutEnv(ex), "probe failed-units", transport.Command{Script: "true"}, fanoutHostIDs(), nil)
	return f, ex
}

// drainFanout runs Init and every follow-up command by hand, exactly as the
// root model would.
func drainFanout(t *testing.T, f *Fanout) *Fanout {
	t.Helper()
	return drainFanoutCmd(t, f, f.Init())
}

// drainFanoutCmd applies cmd and everything it asks for next.
func drainFanoutCmd(t *testing.T, f *Fanout, cmd tea.Cmd) *Fanout {
	t.Helper()
	for i := 0; cmd != nil; i++ {
		if i > 5000 {
			t.Fatal("fan-out did not finish")
		}
		msg := cmd()
		if msg == nil {
			t.Fatal("fan-out command returned nil")
		}
		v, next := f.Update(msg)
		if _, ok := v.(*Fanout); !ok {
			t.Fatalf("Update returned %T, want *Fanout", v)
		}
		cmd = next
	}
	return f
}

// fanoutScreen renders the view and splits it into lines.
func fanoutScreen(t *testing.T, f *Fanout, w, h int) []string {
	t.Helper()
	return strings.Split(f.Render(w, h), "\n")
}

// TestFanoutHeadline is requirement 3's banner: the summary of 30 hosts.
func TestFanoutHeadline(t *testing.T) {
	f, _ := newFanoutFixture(t)
	drainFanout(t, f)

	if !f.done {
		t.Fatal("fan-out not done after the stream closed")
	}
	screen := f.Render(100, 20)
	if !strings.Contains(screen, "27 identical · 2 differ · 1 errors") {
		t.Fatalf("banner does not carry the summary:\n%s", screen)
	}
	if got := f.Summary().Headline(); got != "27 identical · 2 differ · 1 errors" {
		t.Fatalf("Headline() = %q", got)
	}
}

// TestFanoutGroupsSorted is requirement 3's row order: the majority first, then
// the differing group, then one row per error kind.
func TestFanoutGroupsSorted(t *testing.T) {
	f, _ := newFanoutFixture(t)
	drainFanout(t, f)

	sum := f.Summary()
	if len(sum.Groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(sum.Groups))
	}
	if got := len(sum.Groups[0].Hosts); got != 27 {
		t.Fatalf("majority group = %d hosts, want 27", got)
	}
	if got := len(sum.Groups[1].Hosts); got != 2 {
		t.Fatalf("differing group = %d hosts, want 2", got)
	}

	rows := fanoutScreen(t, f, 100, 20)
	if !strings.Contains(rows[1], "HOSTS") || !strings.Contains(rows[1], "SHARE") ||
		!strings.Contains(rows[1], "OUTPUT") {
		t.Fatalf("header row is missing columns: %q", rows[1])
	}
	if !strings.Contains(rows[2], "majority") || !strings.Contains(rows[2], "27") {
		t.Fatalf("first row is not the majority group: %q", rows[2])
	}
	if !strings.Contains(rows[3], "differs") || !strings.Contains(rows[3], "2") {
		t.Fatalf("second row is not the differing group: %q", rows[3])
	}
	if !strings.Contains(rows[2], "(+1 lines)") {
		t.Fatalf("row does not summarise extra output lines: %q", rows[2])
	}
	if !strings.Contains(rows[4], "timeout") || !strings.Contains(rows[4], "1 host") {
		t.Fatalf("error row is missing: %q", rows[4])
	}
}

// TestFanoutMembers is requirement 3's drill-in and requirement 4's list.
func TestFanoutMembers(t *testing.T) {
	f, _ := newFanoutFixture(t)
	drainFanout(t, f)

	_, cmd := f.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on the majority group returned no command")
	}
	push, ok := cmd().(tui.PushViewMsg)
	if !ok {
		t.Fatalf("enter returned %T, want tui.PushViewMsg", cmd())
	}
	m, ok := push.View.(*FanoutMembers)
	if !ok {
		t.Fatalf("enter pushed %T, want *FanoutMembers", push.View)
	}
	if got := len(m.g.Hosts); got != 27 {
		t.Fatalf("members = %d hosts, want 27", got)
	}
	if m.g.Hosts[0] != "web-03" {
		t.Fatalf("first member = %q, want web-03", m.g.Hosts[0])
	}
	screen := m.Render(80, 10)
	if !strings.Contains(screen, "web-03") || !strings.Contains(screen, "unit-a") {
		t.Fatalf("member list does not show host and output line:\n%s", screen)
	}

	_, openCmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if openCmd == nil {
		t.Fatal("enter on a member returned no command")
	}
	q, ok := openCmd().(tui.OpenQueryMsg)
	if !ok {
		t.Fatalf("enter on a member returned %T, want tui.OpenQueryMsg", openCmd())
	}
	if q.Query.Kind != linux.KindHostOverview {
		t.Fatalf("member opens kind %q, want %q", q.Query.Kind, linux.KindHostOverview)
	}
	if len(q.Query.Hosts) != 1 || q.Query.Hosts[0] != "web-03" {
		t.Fatalf("member opens hosts %v, want [web-03]", q.Query.Hosts)
	}

	_, backCmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if _, ok := backCmd().(tui.PopViewMsg); !ok {
		t.Fatalf("esc in the member list did not pop")
	}
}

// TestFanoutDiffDialogOpens is requirement 3's `d`: a unified diff against the
// majority, and a warning when the majority itself is selected.
func TestFanoutDiffDialogOpens(t *testing.T) {
	f, _ := newFanoutFixture(t)
	drainFanout(t, f)

	// The majority itself: refuse, with a warning.
	_, cmd := f.Update(runes("d"))
	flash, ok := cmd().(tui.FlashMsg)
	if !ok {
		t.Fatalf("diff on the majority returned %T, want tui.FlashMsg", cmd())
	}
	if flash.Level != tui.FlashWarn {
		t.Fatalf("diff on the majority flashed level %v, want warn", flash.Level)
	}

	// The differing group: a dialog with both sides of the diff.
	f.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, cmd = f.Update(runes("d"))
	open, ok := cmd().(tui.OpenDialogMsg)
	if !ok {
		t.Fatalf("diff on a differing group returned %T, want tui.OpenDialogMsg", cmd())
	}
	if open.Dialog == nil {
		t.Fatal("diff dialog is nil")
	}
	body := open.Dialog.View(80, 20)
	for _, want := range []string{"-unit-b", "+unit-c"} {
		if !strings.Contains(body, want) {
			t.Fatalf("diff dialog is missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(open.Dialog.Title(), "diff") {
		t.Fatalf("diff dialog title = %q", open.Dialog.Title())
	}
}

// TestFanoutRerun is requirement 3's `r`: the same command runs again from a
// clean grouping.
func TestFanoutRerun(t *testing.T) {
	f, ex := newFanoutFixture(t)
	drainFanout(t, f)
	first := len(ex.Calls())
	if first != fanoutHostCount {
		t.Fatalf("first run made %d calls, want %d", first, fanoutHostCount)
	}

	_, cmd := f.Update(runes("r"))
	if cmd == nil {
		t.Fatal("rerun returned no command")
	}
	if f.done {
		t.Fatal("rerun did not restart the run")
	}
	drainFanoutCmd(t, f, cmd)

	if got := len(ex.Calls()); got != 2*fanoutHostCount {
		t.Fatalf("after rerun %d calls, want %d", got, 2*fanoutHostCount)
	}
	if !f.done {
		t.Fatal("rerun did not finish")
	}
	if got := f.Summary().Headline(); got != "27 identical · 2 differ · 1 errors" {
		t.Fatalf("rerun summary = %q", got)
	}
}

// TestFanoutCancelOnPop is requirement 3's `esc`: pop the view and stop a run
// that is still in flight.
func TestFanoutCancelOnPop(t *testing.T) {
	f, _ := newFanoutFixture(t)
	cmd := f.Init()
	if cmd == nil {
		t.Fatal("Init returned no command")
	}
	_, popCmd := f.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if popCmd == nil {
		t.Fatal("esc returned no command")
	}
	msg := popCmd()
	if _, ok := msg.(tui.PopViewMsg); !ok {
		t.Fatalf("esc returned %T, want tui.PopViewMsg", msg)
	}
	if !f.done {
		t.Fatal("esc did not mark the run stopped")
	}
	if f.ctx == nil || f.ctx.Err() == nil {
		t.Fatal("esc did not cancel the run's context")
	}
}

// TestFanoutExactSize is requirement 3's contract with the view stack: exactly
// w cells per line, exactly h lines.
func TestFanoutExactSize(t *testing.T) {
	f, _ := newFanoutFixture(t)
	drainFanout(t, f)

	for _, size := range [][2]int{{40, 10}, {100, 20}, {12, 3}, {1, 1}} {
		w, h := size[0], size[1]
		out := f.Render(w, h)
		lines := strings.Split(out, "\n")
		if len(lines) != h {
			t.Fatalf("%dx%d: %d lines, want %d", w, h, len(lines), h)
		}
		for i, line := range lines {
			if got := render.Width(line); got != w {
				t.Fatalf("%dx%d: line %d is %d cells, want %d: %q", w, h, i, got, w, line)
			}
		}
	}
}

// TestFanoutMarkGroupTargets is requirement 3's `space` plus `:targets marked`.
func TestFanoutMarkGroupTargets(t *testing.T) {
	f, _ := newFanoutFixture(t)
	drainFanout(t, f)

	f.Update(tea.KeyMsg{Type: tea.KeySpace})
	hosts := f.MarkedHosts()
	if len(hosts) != 27 {
		t.Fatalf("marked hosts = %d, want 27", len(hosts))
	}
	if hosts[0] != "web-03" {
		t.Fatalf("first marked host = %q, want web-03", hosts[0])
	}

	_, cmd := f.Update(tui.MarkedTargetsRequestMsg{})
	if cmd == nil {
		t.Fatal("marked-targets request returned no command")
	}
	changed, ok := cmd().(tui.TargetsChangedMsg)
	if !ok {
		t.Fatalf("marked-targets request returned %T, want tui.TargetsChangedMsg", cmd())
	}
	if got := len(changed.Set.Hosts); got != 27 {
		t.Fatalf("new target set = %d hosts, want 27", got)
	}

	// The copy is the headline, then the marked group's hosts and output.
	copied := strings.Join(f.CopyLines(), "\n")
	for _, want := range []string{"27 identical · 2 differ · 1 errors", "web-03", "unit-a"} {
		if !strings.Contains(copied, want) {
			t.Fatalf("CopyLines is missing %q:\n%s", want, copied)
		}
	}
}
