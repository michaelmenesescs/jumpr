package views

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/action"
	"jumper/internal/backend"
	"jumper/internal/backend/linux"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/tui"
)

// stubView is a minimal tui.View that records how often it was initialised.
type stubView struct {
	crumb string
	lines []string
	cap   bool
	inits int
}

func (s *stubView) Init() tea.Cmd                      { s.inits++; return nil }
func (s *stubView) Update(tea.Msg) (tui.View, tea.Cmd) { return s, nil }
func (s *stubView) Render(w, h int) string {
	return render.Fit(strings.Join(s.lines, "\n"), w, h)
}
func (s *stubView) Crumb() string        { return s.crumb }
func (s *stubView) Subject() tui.Subject { return tui.Subject{Unit: s.crumb} }
func (s *stubView) Capturing() bool      { return s.cap }
func (s *stubView) CopyLines() []string  { return s.lines }
func (s *stubView) Hints() []tui.KeyHint { return []tui.KeyHint{{Key: "z", Text: s.crumb}} }

// keyMsg builds the key message a terminal would deliver for key.
func keyMsg(key string) tea.KeyMsg {
	switch key {
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
}

func TestTabsSwitchAndWrap(t *testing.T) {
	a := &stubView{crumb: "one", lines: []string{"ALPHA"}}
	b := &stubView{crumb: "two", lines: []string{"BETA"}}
	c := &stubView{crumb: "three", lines: []string{"GAMMA"}}
	tabs := NewTabs(testEnv(), []Tab{
		{Key: "1", Title: "one", View: a},
		{Key: "2", Title: "two", View: b},
		{Key: "3", Title: "three", View: c},
	})
	if _, cmd := tabs.Update(keyMsg("tab")); cmd != nil {
		t.Errorf("tab returned a command, want none")
	}
	if tabs.Active() != 1 {
		t.Fatalf("after tab Active = %d, want 1", tabs.Active())
	}
	if got := stripHostANSI(tabs.Render(40, 4)); !strings.Contains(got, "BETA") {
		t.Errorf("frame does not show the second tab:\n%s", got)
	}
	tabs.Update(keyMsg("3"))
	if tabs.Active() != 2 {
		t.Errorf("after 3 Active = %d, want 2", tabs.Active())
	}
	tabs.Update(keyMsg("tab"))
	if tabs.Active() != 0 {
		t.Errorf("tab after the last tab Active = %d, want wrap to 0", tabs.Active())
	}
	tabs.Update(keyMsg("shift+tab"))
	if tabs.Active() != 2 {
		t.Errorf("shift+tab on the first tab Active = %d, want wrap to 2", tabs.Active())
	}
	tabs.Update(keyMsg("2"))
	if tabs.Active() != 1 {
		t.Errorf("number key Active = %d, want 1", tabs.Active())
	}
	if tabs.Crumb() != "two" || tabs.CopyLines()[0] != "BETA" {
		t.Errorf("container does not forward the active tab: crumb %q", tabs.Crumb())
	}
	if hints := tabs.Hints(); len(hints) != 1 || hints[0].Text != "two" {
		t.Errorf("Hints = %v, want the active tab's", hints)
	}
}

func TestTabsStripNamesAndLazyInit(t *testing.T) {
	a := &stubView{crumb: "one"}
	b := &stubView{crumb: "two"}
	tabs := NewTabs(testEnv(), []Tab{
		{Key: "1", Title: "status", View: a},
		{Key: "2", Title: "show", View: b},
	})
	tabs.Init()
	if a.inits != 1 || b.inits != 0 {
		t.Fatalf("inits after Init = %d/%d, want 1/0 (lazy)", a.inits, b.inits)
	}
	strip := stripHostANSI(strings.Split(tabs.Render(40, 3), "\n")[0])
	for _, want := range []string{"1 status", "2 show"} {
		if !strings.Contains(strip, want) {
			t.Errorf("strip does not contain %q: %q", want, strip)
		}
	}
	tabs.Update(keyMsg("2"))
	if b.inits != 1 {
		t.Errorf("second tab inits = %d, want 1 on first visit", b.inits)
	}
	tabs.Update(keyMsg("1"))
	tabs.Update(keyMsg("2"))
	if b.inits != 1 {
		t.Errorf("second tab inits = %d, want no re-init on revisit", b.inits)
	}
	if !tabs.Visited(1) || tabs.Visited(0) == false {
		t.Errorf("Visited = %v/%v, want both true", tabs.Visited(0), tabs.Visited(1))
	}
}

func TestTabsCapturingForwardsKeys(t *testing.T) {
	a := &stubView{crumb: "one", cap: true}
	b := &stubView{crumb: "two"}
	tabs := NewTabs(testEnv(), []Tab{
		{Key: "1", Title: "one", View: a},
		{Key: "2", Title: "two", View: b},
	})
	if !tabs.Capturing() {
		t.Fatal("Capturing = false, want the active tab's true")
	}
	tabs.Update(keyMsg("2"))
	if tabs.Active() != 0 {
		t.Errorf("number key switched while capturing, Active = %d", tabs.Active())
	}
}

func TestTabsRenderExactSize(t *testing.T) {
	tabs := NewTabs(testEnv(), []Tab{
		{Key: "1", Title: "status", View: &stubView{crumb: "one", lines: []string{"a", "b", "c"}}},
		{Key: "2", Title: "show", View: &stubView{crumb: "two", lines: []string{"d"}}},
	})
	tabs.Select(1)
	for _, size := range [][2]int{{40, 6}, {20, 3}, {80, 12}} {
		frame := tabs.Render(size[0], size[1])
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

// unitFake is a backend that answers the detail batch, the journal list, the
// planner and nothing else.
type unitFake struct {
	info    linux.UnitInfo
	err     error
	specs   []action.Spec
	plan    action.Plan
	planErr error
	planned []action.Request
}

var (
	_ backend.Backend = (*unitFake)(nil)
	_ action.Planner  = (*unitFake)(nil)
	_ unitInfoer      = (*unitFake)(nil)
)

func (f *unitFake) Name() string                            { return "linux" }
func (f *unitFake) Kinds() []resource.KindSpec              { return (&linux.Backend{}).Kinds() }
func (f *unitFake) Links(resource.Resource) []resource.Link { return nil }
func (f *unitFake) Health(context.Context) backend.Health   { return backend.Health{} }
func (f *unitFake) Get(context.Context, resource.Ref) (resource.Resource, error) {
	return resource.Resource{}, nil
}
func (f *unitFake) List(context.Context, resource.Query) (<-chan backend.Result, error) {
	ch := make(chan backend.Result)
	close(ch)
	return ch, nil
}

func (f *unitFake) UnitInfo(_ context.Context, host resource.HostID, unit string) (linux.UnitInfo, error) {
	if f.err != nil {
		return linux.UnitInfo{}, f.err
	}
	return f.info, nil
}

func (f *unitFake) Actions([]resource.Resource) []action.Spec { return f.specs }

func (f *unitFake) Plan(_ context.Context, req action.Request) (action.Plan, error) {
	f.planned = append(f.planned, req)
	if f.planErr != nil {
		return action.Plan{}, f.planErr
	}
	return f.plan, nil
}

// unitTestInfo is an active nginx.service with one drop-in.
func unitTestInfo() linux.UnitInfo {
	entered := time.Date(2026, 11, 12, 9, 12, 41, 0, time.UTC)
	return linux.UnitInfo{
		Status: "● nginx.service - A high performance web server\n     Active: active (running) since Thu 2026-11-12 09:12:41 UTC",
		Props: map[string]string{
			"ActiveState":   "active",
			"SubState":      "running",
			"Result":        "success",
			"NRestarts":     "0",
			"ExecMainPID":   "2231",
			"MemoryCurrent": "12890112",
			"TasksCurrent":  "3",
			"UnitFileState": "enabled",
		},
		Detail: linux.Detail{
			ActiveState:          "active",
			SubState:             "running",
			Result:               "success",
			ExecMainPID:          "2231",
			MemoryCurrent:        12890112,
			TasksCurrent:         3,
			ActiveEnterTimestamp: entered,
			FragmentPath:         "/usr/lib/systemd/system/nginx.service",
			UnitFileState:        "enabled",
			DropInPaths:          []string{"/etc/systemd/system/nginx.service.d/override.conf"},
		},
		Cat: "# /usr/lib/systemd/system/nginx.service\n[Unit]\nDescription=nginx\n\n" +
			"# /etc/systemd/system/nginx.service.d/override.conf\n[Service]\nLimitNOFILE=65535",
		DropIns: []string{"/etc/systemd/system/nginx.service.d/override.conf"},
	}
}

// newUnitFake is the fixture backend: restart/stop plus a one-step plan.
func newUnitFake() *unitFake {
	return &unitFake{
		info: unitTestInfo(),
		specs: []action.Spec{
			{ID: "linux.systemctl.restart", Title: "restart", HasDryRun: true},
			{ID: "linux.systemctl.stop", Title: "stop", Risk: action.RiskDestructive},
		},
		plan: action.Plan{
			Title: "restart nginx.service on 1 host",
			Steps: []action.Step{{
				Target:  action.Target{Host: "web-01"},
				Display: "sudo -n systemctl restart nginx.service",
			}},
		},
	}
}

// drainUnit runs a command and everything it asks for next, as the root would.
func drainUnit(t *testing.T, u *Unit, cmd tea.Cmd) {
	t.Helper()
	for i := 0; cmd != nil; i++ {
		if i > 5000 {
			t.Fatal("commands did not settle")
		}
		msg := cmd()
		if msg == nil {
			return
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				if c != nil {
					drainUnit(t, u, c)
				}
			}
			return
		}
		_, next := u.Update(msg)
		cmd = next
	}
}

// unitFixture builds a loaded view over the fake backend.
func unitFixture(t *testing.T) (*Unit, *unitFake) {
	t.Helper()
	f := newUnitFake()
	u := NewUnit(testEnv(), f, "web-01", "nginx.service")
	drainUnit(t, u, u.Init())
	return u, f
}

// sendUnitKey routes one key and returns its command.
func sendUnitKey(t *testing.T, u *Unit, key string) tea.Cmd {
	t.Helper()
	v, cmd := u.Update(keyMsg(key))
	if _, ok := v.(*Unit); !ok {
		t.Fatalf("Update returned %T, want *Unit", v)
	}
	return cmd
}

func TestUnitStatusTabShowsKeyFacts(t *testing.T) {
	u, _ := unitFixture(t)
	frame := stripHostANSI(u.Render(100, 24))

	for _, want := range []string{
		"state", "active", "(running)",
		"since", "2026-11-12 09:12:41 UTC",
		"restarts", "main pid", "2231", "memory", "12.3 MiB",
		"tasks", "result", "success", "unit file", "enabled", "/usr/lib/systemd/system/nginx.service",
		unitStatusHead, "A high performance web server",
		"1 status", "2 show", "3 cat", "4 journal",
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame does not contain %q:\n%s", want, frame)
		}
	}
	if u.Crumb() != "nginx.service" {
		t.Errorf("Crumb = %q, want the unit name", u.Crumb())
	}
	if sub := u.Subject(); sub.Unit != "nginx.service" || sub.Kind != linux.KindUnitDetail {
		t.Errorf("Subject = %+v", sub)
	}
}

func TestUnitTabsSwitch(t *testing.T) {
	u, _ := unitFixture(t)
	tabs := u.tabs

	sendUnitKey(t, u, "2")
	if tabs.Active() != u.show {
		t.Errorf("after 2 Active = %d, want the show tab", tabs.Active())
	}
	if frame := stripHostANSI(u.Render(100, 20)); !strings.Contains(frame, "MemoryCurrent") {
		t.Errorf("show tab does not list properties:\n%s", frame)
	}
	sendUnitKey(t, u, "tab")
	if tabs.Active() != u.cat {
		t.Errorf("after tab Active = %d, want the cat tab", tabs.Active())
	}
	sendUnitKey(t, u, "shift+tab")
	if tabs.Active() != u.show {
		t.Errorf("after shift+tab Active = %d, want the show tab", tabs.Active())
	}
	sendUnitKey(t, u, "4")
	if tabs.Active() != u.jour {
		t.Errorf("after 4 Active = %d, want the journal tab", tabs.Active())
	}
	if !tabs.Visited(u.jour) {
		t.Error("journal tab was not initialised on its first visit")
	}
	sendUnitKey(t, u, "1")
	if tabs.Active() != u.status {
		t.Errorf("after 1 Active = %d, want the status tab", tabs.Active())
	}
}

func TestUnitShowFilter(t *testing.T) {
	u, _ := unitFixture(t)
	sendUnitKey(t, u, "2")

	if got := stripHostANSI(u.Render(100, 20)); !strings.Contains(got, "ActiveState") {
		t.Fatalf("show tab does not list ActiveState:\n%s", got)
	}
	sendUnitKey(t, u, "/")
	if !u.Capturing() {
		t.Fatal("Capturing = false while filtering")
	}
	for _, r := range "memory" {
		sendUnitKey(t, u, string(r))
	}
	frame := stripHostANSI(u.Render(100, 20))
	if !strings.Contains(frame, "MemoryCurrent") {
		t.Errorf("filtered frame lost the match:\n%s", frame)
	}
	if strings.Contains(frame, "ActiveState") {
		t.Errorf("filtered frame still shows a non-match:\n%s", frame)
	}
	sendUnitKey(t, u, "esc")
	if u.Capturing() {
		t.Error("Capturing = true after esc")
	}
	if frame := stripHostANSI(u.Render(100, 20)); !strings.Contains(frame, "ActiveState") {
		t.Errorf("clearing the filter did not restore the list:\n%s", frame)
	}
}

func TestUnitCatMarksDropIns(t *testing.T) {
	u, _ := unitFixture(t)
	sendUnitKey(t, u, "3")
	frame := stripHostANSI(u.Render(100, 20))
	if !strings.Contains(frame, "# /usr/lib/systemd/system/nginx.service") {
		t.Errorf("cat tab lost the unit-file header:\n%s", frame)
	}
	if !strings.Contains(frame, "drop-in") {
		t.Errorf("cat tab does not mark the drop-in section:\n%s", frame)
	}
	if !strings.Contains(frame, "LimitNOFILE=65535") {
		t.Errorf("cat tab lost the drop-in body:\n%s", frame)
	}
}

func TestUnitActionsKey(t *testing.T) {
	u, _ := unitFixture(t)
	cmd := sendUnitKey(t, u, "x")
	if cmd == nil {
		t.Fatal("x returned no command")
	}
	msg, ok := cmd().(tui.OpenDialogMsg)
	if !ok {
		t.Fatalf("x returned %T, want tui.OpenDialogMsg", cmd())
	}
	if !strings.Contains(msg.Dialog.Title(), "nginx.service") {
		t.Errorf("dialog title = %q, want the unit name", msg.Dialog.Title())
	}
	body := stripHostANSI(msg.Dialog.View(60, 10))
	for _, want := range []string{"restart", "! stop"} {
		if !strings.Contains(body, want) {
			t.Errorf("actions menu does not contain %q:\n%s", want, body)
		}
	}
}

func TestUnitActionsPlanPreview(t *testing.T) {
	u, f := unitFixture(t)
	cmd := sendUnitKey(t, u, "x")
	msg, ok := cmd().(tui.OpenDialogMsg)
	if !ok {
		t.Fatalf("x returned %T, want tui.OpenDialogMsg", cmd())
	}
	// Pick the first spec the way the list dialog does and run its command.
	_, pick := msg.Dialog.Update(keyMsg("enter"))
	if pick == nil {
		t.Fatal("picking a spec returned no command")
	}
	pickMsg := pick()
	batch, ok := pickMsg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("pick returned %T, want tea.BatchMsg", pickMsg)
	}
	var preview tui.OpenDialogMsg
	for _, c := range batch {
		if c == nil {
			continue
		}
		if m, ok := c().(tui.OpenDialogMsg); ok {
			preview = m
		}
	}
	if preview.Dialog == nil {
		t.Fatal("picking a spec did not show the plan")
	}
	if body := stripHostANSI(preview.Dialog.View(60, 10)); !strings.Contains(body, "sudo -n systemctl restart nginx.service") {
		t.Errorf("plan preview does not show the command:\n%s", body)
	}
	if len(f.planned) != 1 {
		t.Fatalf("planner got %d requests, want 1", len(f.planned))
	}
	req := f.planned[0]
	if req.SpecID != "linux.systemctl.restart" || len(req.Targets) != 1 || req.Targets[0].Host != "web-01" {
		t.Errorf("request = %+v", req)
	}
	if req.Actor != action.Human {
		t.Errorf("actor = %q, want human", req.Actor)
	}
}

func TestUnitCommandLine(t *testing.T) {
	u, _ := unitFixture(t)
	want := "unit-detail --host web-01 --arg unit=nginx.service"
	if got := u.CommandLine(); got != want {
		t.Errorf("CommandLine = %q, want %q", got, want)
	}
}

func TestUnitRenderExactSize(t *testing.T) {
	u, _ := unitFixture(t)
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
