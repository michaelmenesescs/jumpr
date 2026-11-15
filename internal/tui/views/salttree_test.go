package views

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend/salt"
	"jumpr/internal/errs"
	"jumpr/internal/redact"
	"jumpr/internal/resource"
	"jumpr/internal/targets"
	"jumpr/internal/tui"
	"jumpr/internal/tui/keymap"
)

// ---- fixtures ----

// fakeTree is a Salt state tree without a master.
type fakeTree struct {
	tree      salt.Tree
	treeErr   error
	state     salt.ShowState
	stateErr  error
	top       salt.ShowTop
	topErr    error
	source    string
	sourceErr error
	master    string

	treeQueries []resource.Query
	showMinion  string
	showSls     string
	topMinions  []string
	srcMinion   string
	srcFile     string
}

func (f *fakeTree) Tree(_ context.Context, q resource.Query) (salt.Tree, error) {
	f.treeQueries = append(f.treeQueries, q)
	return f.tree, f.treeErr
}

func (f *fakeTree) ShowSls(_ context.Context, minion, sls string) (salt.ShowState, error) {
	f.showMinion, f.showSls = minion, sls
	return f.state, f.stateErr
}

func (f *fakeTree) TopFile(_ context.Context, minions []string) (salt.ShowTop, error) {
	f.topMinions = append([]string(nil), minions...)
	return f.top, f.topErr
}

func (f *fakeTree) SlsSource(_ context.Context, minion, file string) (string, error) {
	f.srcMinion, f.srcFile = minion, file
	return f.source, f.sourceErr
}

func (f *fakeTree) MasterMinion() (string, bool) {
	return f.master, f.master != ""
}

func (f *fakeTree) MinionOf(host resource.HostID) (string, bool) {
	id := strings.TrimSuffix(string(host), ".lab.example.internal")
	if id == "" || id == string(host) {
		return "", false
	}
	for _, m := range f.tree.Minions {
		if m == id {
			return id, true
		}
	}
	return "", false
}

// treeFixture is the default state tree: three sls in base and one in dev.
func treeFixture() *fakeTree {
	return &fakeTree{
		tree: salt.Tree{
			Envs:    []string{"base", "dev"},
			Minions: []string{"db-01", "web-01", "web-02"},
			Files: []salt.SlsFile{
				{Name: "common", Env: "base", Path: "common/init.sls"},
				{Name: "nginx", Env: "base", Path: "nginx/init.sls"},
				{Name: "nginx.conf", Env: "base", Path: "nginx/conf.sls"},
				{Name: "site", Env: "dev", Path: "site/init.sls"},
			},
			Assigned: map[string]int{
				salt.SlsKey("base", "common"): 3,
				salt.SlsKey("base", "nginx"):  2,
			},
			AssignedOK: true,
		},
		state: salt.ShowState{
			Minions: []string{"web-01"},
			Defs: []salt.StateDef{
				{Minion: "web-01", ID: "nginx", Module: "pkg", Fun: "installed", SLS: "nginx", Env: "base", Name: "nginx"},
				{Minion: "web-01", ID: "nginx-conf", Module: "file", Fun: "managed", SLS: "nginx", Env: "base",
					Name: "password=hunter2", Requisites: "require pkg:nginx"},
			},
		},
		top: salt.ShowTop{
			Minions: []string{"db-01", "web-01", "web-02"},
			Top: map[string]map[string][]string{
				"web-01": {"base": {"common", "nginx"}},
				"web-02": {"base": {"common", "nginx"}},
				"db-01":  {"base": {"common"}},
			},
		},
		source: "nginx:\n  pkg.installed:\n    - name: nginx\n",
		master: "saltmaster",
	}
}

// treeEnv is a session targeting the given hosts.
func treeEnv(hosts ...resource.HostID) *tui.Env {
	return &tui.Env{
		Session: &tui.SessionState{Name: "s1", Targets: targets.FromHosts("all", hosts)},
		Keys:    keymap.Default(),
	}
}

// webHost is the inventory host of a minion in these tests.
func webHost(m string) resource.HostID { return resource.HostID(m + ".lab.example.internal") }

// treeHostList is the host list of the given minions.
func treeHostList(minions ...string) []resource.HostID {
	out := make([]resource.HostID, 0, len(minions))
	for _, m := range minions {
		out = append(out, webHost(m))
	}
	return out
}

// driveTree runs a view command until nothing is left to do.
func driveTree(t *testing.T, v *SaltTree, cmd tea.Cmd) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for cmd != nil {
		if time.Now().After(deadline) {
			t.Fatal("the view did not settle")
		}
		_, next := v.Update(cmd())
		cmd = next
	}
}

// newTreeView builds the view over the fake tree and settles its first load.
func newTreeView(t *testing.T, f *fakeTree, minions ...string) *SaltTree {
	t.Helper()
	v := NewSaltTree(treeEnv(treeHostList(minions...)...), f, "")
	driveTree(t, v, v.Init())
	return v
}

// pressTree sends one key press to the view.
func pressTree(v *SaltTree, key string) tea.Cmd {
	var msg tea.KeyMsg
	switch key {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	_, cmd := v.Update(msg)
	return cmd
}

// typeTree types text into the view, one key at a time.
func typeTree(v *SaltTree, text string) tea.Cmd {
	var cmd tea.Cmd
	for _, r := range text {
		_, cmd = v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return cmd
}

// ---- tests ----

// TestSaltTreeListAndFilter: the left pane lists every sls, and `/` narrows it.
func TestSaltTreeListAndFilter(t *testing.T) {
	f := treeFixture()
	v := newTreeView(t, f, "web-01", "web-02", "db-01")

	if len(f.treeQueries) != 1 {
		t.Fatalf("tree loads = %d, want 1", len(f.treeQueries))
	}
	if got := len(f.treeQueries[0].Hosts); got != 3 {
		t.Errorf("the tree query carried %d hosts, want the session's 3", got)
	}
	frame := v.Render(110, 20)
	for _, want := range []string{"nginx", "common", "nginx.conf", "site", "SLS", "ENV"} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame does not contain %q:\n%s", want, frame)
		}
	}
	if len(v.view) != 4 {
		t.Fatalf("rows = %d, want 4", len(v.view))
	}

	// Filtering.
	pressTree(v, "/")
	if !v.Capturing() {
		t.Fatal("the filter does not capture the keyboard")
	}
	typeTree(v, "ngin")
	if len(v.view) != 2 {
		t.Fatalf("filtered rows = %v, want nginx and nginx.conf", v.view)
	}
	pressTree(v, "esc")
	if v.Capturing() {
		t.Error("esc did not leave the filter")
	}
	if len(v.view) != 4 {
		t.Errorf("rows after clearing the filter = %d, want 4", len(v.view))
	}
}

// TestSaltTreeEnterShowsSlsRows: enter renders the selected sls for the first
// session minion, and every displayed string is redacted.
func TestSaltTreeEnterShowsSlsRows(t *testing.T) {
	red, err := redact.New(nil)
	if err != nil {
		t.Fatalf("redact.New: %v", err)
	}
	f := treeFixture()
	env := treeEnv(treeHostList("web-01", "web-02", "db-01")...)
	env.Redact = red
	v := NewSaltTree(env, f, "")
	driveTree(t, v, v.Init())

	// Select nginx (rows are sorted by env, then name).
	for i, row := range v.view {
		if row.Name == "nginx" {
			v.cursor = i
		}
	}
	driveTree(t, v, pressTree(v, "enter"))

	// The session set is sorted, so its first target is db-01.
	if f.showMinion != "db-01" || f.showSls != "nginx" {
		t.Fatalf("show_sls asked for %q/%q, want db-01/nginx", f.showMinion, f.showSls)
	}
	frame := v.Render(200, 20)
	for _, want := range []string{"pkg.installed", "file.managed", "require pkg:nginx", "db-01"} {
		if !strings.Contains(frame, want) {
			t.Errorf("frame does not contain %q:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "hunter2") {
		t.Errorf("the rendered state leaked a secret:\n%s", frame)
	}
	if !strings.Contains(frame, "redacted") {
		t.Errorf("the rendered state shows no redaction marker:\n%s", frame)
	}
}

// TestSaltTreeEnterErrorsAreShown: a render error is shown, not treated as a
// failure of the screen.
func TestSaltTreeEnterErrorsAreShown(t *testing.T) {
	f := treeFixture()
	f.state = salt.ShowState{
		Minions: []string{"web-01"},
		Errors:  []salt.ShowError{{Minion: "web-01", Text: "Rendering SLS 'base:nginx' failed"}},
	}
	v := newTreeView(t, f, "web-01")

	for i, row := range v.view {
		if row.Name == "nginx" {
			v.cursor = i
		}
	}
	driveTree(t, v, pressTree(v, "enter"))
	frame := v.Render(120, 20)
	if !strings.Contains(frame, "Rendering SLS") {
		t.Fatalf("the render error is missing:\n%s", frame)
	}
	if v.loadErr != nil {
		t.Errorf("the view failed on a render error: %v", v.loadErr)
	}
}

// TestSaltTreeTopGroups: t groups the session targets by identical sls list.
func TestSaltTreeTopGroups(t *testing.T) {
	f := treeFixture()
	v := newTreeView(t, f, "web-01", "web-02", "db-01")

	cmd := pressTree(v, "t")
	if cmd == nil {
		t.Fatal("t returned no command")
	}
	_, push := v.Update(cmd())
	if push == nil {
		t.Fatal("t did not push a view")
	}
	msg, ok := push().(tui.PushViewMsg)
	if !ok {
		t.Fatalf("t produced %T, want a pushed view", push())
	}
	if f.topMinions == nil || strings.Join(f.topMinions, ",") != "db-01,web-01,web-02" {
		t.Errorf("show_top minions = %v, want all three session minions", f.topMinions)
	}
	lines := msg.View.CopyLines()
	if len(lines) == 0 {
		t.Fatal("the top view has no lines")
	}
	if lines[0] != "2 identical · 1 differ" {
		t.Errorf("headline = %q, want 2 identical · 1 differ", lines[0])
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "common, nginx") || !strings.Contains(joined, "common") {
		t.Errorf("the top view does not show the sls lists:\n%s", joined)
	}
}

// TestSaltTreeSource: c reads the raw sls source from the master's own minion.
func TestSaltTreeSource(t *testing.T) {
	f := treeFixture()
	v := newTreeView(t, f, "web-01")

	for i, row := range v.view {
		if row.Name == "nginx" {
			v.cursor = i
		}
	}
	driveTree(t, v, pressTree(v, "c"))
	if f.srcMinion != "saltmaster" || f.srcFile != "nginx/init.sls" {
		t.Fatalf("source asked for %q/%q, want saltmaster/nginx/init.sls", f.srcMinion, f.srcFile)
	}
	frame := v.Render(120, 20)
	if !strings.Contains(frame, "pkg.installed") {
		t.Errorf("the source is not shown:\n%s", frame)
	}
}

// TestSaltTreeSourceNotReadable: a failed read says so.
func TestSaltTreeSourceNotReadable(t *testing.T) {
	f := treeFixture()
	f.sourceErr = errs.New(errs.NotAvailable, "salt.source", "source not readable")
	v := newTreeView(t, f, "web-01")

	for i, row := range v.view {
		if row.Name == "nginx" {
			v.cursor = i
		}
	}
	driveTree(t, v, pressTree(v, "c"))
	frame := v.Render(120, 20)
	if !strings.Contains(frame, "source not readable") {
		t.Fatalf("the frame does not say the source is unreadable:\n%s", frame)
	}
}

// TestSaltTreeDryRunCommand: d opens the dry run of the selected sls.
func TestSaltTreeDryRunCommand(t *testing.T) {
	f := treeFixture()
	v := newTreeView(t, f, "web-01")

	for i, row := range v.view {
		if row.Name == "nginx" {
			v.cursor = i
		}
	}
	cmd := pressTree(v, "d")
	if cmd == nil {
		t.Fatal("d returned no command")
	}
	msg, ok := cmd().(tui.RunCommandMsg)
	if !ok {
		t.Fatalf("d produced %T, want a RunCommandMsg", msg)
	}
	if msg.Line != "salt state nginx" {
		t.Errorf("command = %q, want salt state nginx", msg.Line)
	}
}

// TestSaltTreePromptWithoutTargets: with no session minion, enter asks for one.
func TestSaltTreePromptWithoutTargets(t *testing.T) {
	f := treeFixture()
	f.tree.Minions = nil
	v := NewSaltTree(treeEnv(), f, "")
	driveTree(t, v, v.Init())

	pressTree(v, "enter")
	if !v.Capturing() {
		t.Fatal("enter without a target did not open a prompt")
	}
	typeTree(v, "web-09")
	driveTree(t, v, pressTree(v, "enter"))
	if f.showMinion != "web-09" {
		t.Errorf("show_sls minion = %q, want the typed web-09", f.showMinion)
	}
}

// TestSaltTreeLoadError: a failed tree load is shown, not swallowed.
func TestSaltTreeLoadError(t *testing.T) {
	f := treeFixture()
	f.treeErr = errs.New(errs.Unreachable, "salt.tree", "master unreachable")
	v := NewSaltTree(treeEnv(treeHostList("web-01")...), f, "")
	driveTree(t, v, v.Init())
	frame := v.Render(100, 10)
	if !strings.Contains(frame, "master unreachable") {
		t.Fatalf("the load error is missing:\n%s", frame)
	}
}
