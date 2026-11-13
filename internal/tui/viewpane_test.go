package tui

import (
	"context"
	"sort"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/mux"
	"jumper/internal/render"
	"jumper/internal/resource"
)

// paneView is a View that can describe itself as a `:` command line and can
// take the pane's context, like views.Table.
type paneView struct {
	crumb     string
	line      string
	filter    string
	body      string
	initCmd   tea.Cmd
	onUpdate  func(tea.Msg) tea.Cmd
	hints     []KeyHint
	subject   Subject
	capturing bool
	copies    []string
	ctx       context.Context
}

func (v *paneView) Init() tea.Cmd { return v.initCmd }

func (v *paneView) Update(msg tea.Msg) (View, tea.Cmd) {
	if v.onUpdate != nil {
		return v, v.onUpdate(msg)
	}
	return v, nil
}

func (v *paneView) Render(w, h int) string {
	body := v.body
	if body == "" {
		body = v.crumb
	}
	return render.Fit(body, w, h)
}

func (v *paneView) Crumb() string                  { return v.crumb }
func (v *paneView) Hints() []KeyHint               { return v.hints }
func (v *paneView) Subject() Subject               { return v.subject }
func (v *paneView) Capturing() bool                { return v.capturing }
func (v *paneView) CopyLines() []string            { return v.copies }
func (v *paneView) CommandLine() string            { return v.line }
func (v *paneView) FilterState() string            { return v.filter }
func (v *paneView) SetContext(ctx context.Context) { v.ctx = ctx }

var _ View = (*paneView)(nil)

// queryLine renders a query the way views.Table.CommandLine does, so the tests
// can assert on a stable text.
func queryLine(q resource.Query) string {
	parts := []string{string(q.Kind)}
	if !q.Selector.IsZero() {
		parts = append(parts, q.Selector.String())
	}
	line := strings.Join(parts, " ")
	if len(q.Hosts) > 0 {
		hosts := make([]string, len(q.Hosts))
		for i, h := range q.Hosts {
			hosts[i] = string(h)
		}
		line += " --host " + strings.Join(hosts, ",")
	}
	if len(q.Args) > 0 {
		keys := make([]string, 0, len(q.Args))
		for k := range q.Args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			line += " --arg " + k + "=" + q.Args[k]
		}
	}
	return line
}

// queryLineFactory builds paneViews that echo the query as crumb and command
// line.
func queryLineFactory(_ *Env, q resource.Query) (View, error) {
	line := queryLine(q)
	return &paneView{crumb: line, line: line}, nil
}

// runCmd runs one command and returns its message, failing on a nil command.
func runCmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command, got nil")
	}
	return cmd()
}

func TestViewPaneWrapsCmds(t *testing.T) {
	root := &paneView{
		crumb:   "hosts",
		initCmd: func() tea.Msg { return ListBatchMsg{Seq: 1} },
		onUpdate: func(tea.Msg) tea.Cmd {
			return func() tea.Msg { return ListBatchMsg{Seq: 2} }
		},
	}
	p := NewViewPane(nil, 7, context.Background(), root)
	defer p.Close()

	// Init's command comes back wrapped for this pane.
	got := runCmd(t, p.Init())
	pm, ok := got.(mux.PaneMsg)
	if !ok {
		t.Fatalf("Init command returned %T, want mux.PaneMsg", got)
	}
	if pm.Pane != 7 {
		t.Fatalf("PaneMsg.Pane = %d, want 7", pm.Pane)
	}
	batch, ok := pm.Msg.(ListBatchMsg)
	if !ok || batch.Seq != 1 {
		t.Fatalf("Init message = %#v, want ListBatchMsg{Seq: 1}", pm.Msg)
	}

	// So does a command a view returns from Update.
	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	got = runCmd(t, cmd)
	pm, ok = got.(mux.PaneMsg)
	if !ok {
		t.Fatalf("Update command returned %T, want mux.PaneMsg", got)
	}
	if pm.Pane != 7 {
		t.Fatalf("PaneMsg.Pane = %d, want 7", pm.Pane)
	}
	batch, ok = pm.Msg.(ListBatchMsg)
	if !ok || batch.Seq != 2 {
		t.Fatalf("Update message = %#v, want ListBatchMsg{Seq: 2}", pm.Msg)
	}

	// A nil command stays nil: nothing is delivered to whichever pane is
	// focused later.
	quiet := NewViewPane(nil, 8, context.Background(), &paneView{})
	defer quiet.Close()
	if cmd := quiet.Init(); cmd != nil {
		t.Fatalf("Init of a view with no work = %v, want nil", cmd)
	}
	if _, cmd := quiet.Update(tea.KeyMsg{}); cmd != nil {
		t.Fatalf("Update of a view with no work = %v, want nil", cmd)
	}
}

func TestViewPaneRootMsgsDetected(t *testing.T) {
	root := []tea.Msg{
		FlashMsg{Level: FlashError, Text: "boom"},
		OpenDialogMsg{},
		CloseDialogMsg{},
		RunCommandMsg{Line: "hosts"},
		WindowChangedMsg{},
		mux.OpMsg{Op: "split-h"},
		tea.QuitMsg{},
	}
	for _, msg := range root {
		if !IsRootMsg(msg) {
			t.Errorf("IsRootMsg(%#v) = false, want true", msg)
		}
	}
	pane := []tea.Msg{
		nil,
		ListBatchMsg{Seq: 1},
		LinesMsg{Seq: 1},
		PushViewMsg{},
		PopViewMsg{},
		ReplaceViewMsg{},
		OpenQueryMsg{},
		HealthMsg{},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")},
		tea.WindowSizeMsg{Width: 80, Height: 24},
	}
	for _, msg := range pane {
		if IsRootMsg(msg) {
			t.Errorf("IsRootMsg(%#v) = true, want false", msg)
		}
	}
}

func TestViewPaneOpenQueryPushes(t *testing.T) {
	env := newTestEnv(t)
	env.Factories = map[resource.KindName]Factory{"host": queryLineFactory}
	root := &paneView{crumb: "hosts", line: "host"}
	p := NewViewPane(env, 1, context.Background(), root)
	defer p.Close()

	sel, err := resource.ParseSelector("role=web")
	if err != nil {
		t.Fatalf("parse selector: %v", err)
	}
	q := resource.Query{Kind: "host", Selector: sel, Hosts: []resource.HostID{"web-01"}}
	_, cmd := p.Update(OpenQueryMsg{Query: q})
	if got := p.stack.Len(); got != 2 {
		t.Fatalf("stack length after OpenQueryMsg = %d, want 2", got)
	}
	want := "host role=web --host web-01"
	if got := p.Title(); got != want {
		t.Fatalf("Title() = %q, want %q", got, want)
	}
	// The pushed view was given the pane context before Init ran.
	if pv, ok := p.stack.Top().(*paneView); !ok || pv.ctx != p.ctx {
		t.Fatalf("pushed view context = %v, want the pane context", p.stack.Top())
	}
	// Its Init command (nil here) is still wrapped: nil stays nil.
	if cmd != nil {
		if msg := cmd(); msg != nil {
			t.Fatalf("pushed view Init returned %#v, want nil", msg)
		}
	}

	// An unknown kind is a flash, not a new view.
	before := p.stack.Len()
	_, cmd = p.Update(OpenQueryMsg{Query: resource.Query{Kind: "nope"}})
	msg := runCmd(t, cmd)
	pm, ok := msg.(mux.PaneMsg)
	if !ok || pm.Pane != 1 {
		t.Fatalf("unknown kind message = %#v, want a PaneMsg for pane 1", msg)
	}
	flash, ok := pm.Msg.(FlashMsg)
	if !ok {
		t.Fatalf("unknown kind message = %#v, want a FlashMsg", pm.Msg)
	}
	if flash.Level != FlashError || !strings.Contains(flash.Text, "unknown kind") {
		t.Fatalf("flash = %#v, want an error mentioning the unknown kind", flash)
	}
	if !IsRootMsg(pm.Msg) {
		t.Fatal("the flash must be a root message")
	}
	if got := p.stack.Len(); got != before {
		t.Fatalf("stack length after a failed query = %d, want %d", got, before)
	}

	// A kind without a factory flashes too.
	delete(env.Factories, "host")
	_, cmd = p.Update(OpenQueryMsg{Query: resource.Query{Kind: "unit"}})
	pm, ok = runCmd(t, cmd).(mux.PaneMsg)
	if !ok {
		t.Fatal("missing factory did not produce a pane message")
	}
	if flash, ok := pm.Msg.(FlashMsg); !ok || !strings.Contains(flash.Text, "no view factory") {
		t.Fatalf("missing factory message = %#v, want a flash about the factory", pm.Msg)
	}
}

func TestViewPaneStateRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	env.Factories = map[resource.KindName]Factory{"host": queryLineFactory}
	cmds := newTestCommands(t)
	ctx := context.Background()

	root := &paneView{crumb: "hosts", line: "host"}
	p := NewViewPane(env, 1, ctx, root)
	defer p.Close()

	// The last word of the line lands in the selector and the flags in the
	// query fields; the pane resolves it through the registry.
	line := "hosts role=web --host web-01,web-02 --arg unit=nginx.service"
	if _, cmd := p.Update(OpenQueryMsg{Query: queryOf(t, env, cmds, line)}); cmd != nil {
		cmd()
	}
	if got := p.stack.Len(); got != 2 {
		t.Fatalf("stack length = %d, want 2", got)
	}

	st := p.State()
	if st.Type != "view" {
		t.Fatalf("State().Type = %q, want \"view\"", st.Type)
	}
	want := []string{"host", "host role=web --host web-01,web-02 --arg unit=nginx.service"}
	if len(st.Commands) != len(want) {
		t.Fatalf("State().Commands = %v, want %v", st.Commands, want)
	}
	for i := range want {
		if st.Commands[i] != want[i] {
			t.Fatalf("State().Commands[%d] = %q, want %q", i, st.Commands[i], want[i])
		}
	}

	// Feeding those command lines back through Commands.Exec rebuilds a pane
	// with the same crumbs.
	p2 := NewViewPane(env, 2, ctx, &paneView{crumb: "hosts", line: st.Commands[0]})
	defer p2.Close()
	for _, c := range st.Commands[1:] {
		if _, cmd := p2.Update(OpenQueryMsg{Query: queryOf(t, env, cmds, c)}); cmd != nil {
			cmd()
		}
	}
	if got, want := p2.stack.Crumbs(), p.stack.Crumbs(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("rebuilt crumbs = %v, want %v", got, want)
	}
	if got, want := p2.State().Commands, st.Commands; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("rebuilt State().Commands = %v, want %v", got, want)
	}

	// An entry that cannot describe itself stops the list there.
	plain := NewViewPane(env, 3, ctx, &stubView{crumb: "root"})
	defer plain.Close()
	if got := plain.State().Commands; len(got) != 0 {
		t.Fatalf("State().Commands for a plain root = %v, want none", got)
	}
}

// queryOf runs one kind command line through Commands.Exec and returns the
// query it produced, failing when it flashes instead.
func queryOf(t *testing.T, env *Env, cmds *Commands, line string) resource.Query {
	t.Helper()
	msg := runCmd(t, cmds.Exec(env, line))
	oq, ok := msg.(OpenQueryMsg)
	if !ok {
		t.Fatalf("%q produced %#v, want an OpenQueryMsg", line, msg)
	}
	return oq.Query
}

func TestViewPaneCloseCancelsContext(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	root := &paneView{crumb: "hosts"}
	p := NewViewPane(nil, 1, parent, root)
	if root.ctx != p.ctx {
		t.Fatal("NewViewPane did not hand the pane context to the root view")
	}
	select {
	case <-p.ctx.Done():
		t.Fatal("the pane context was cancelled before Close")
	default:
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	select {
	case <-p.ctx.Done():
	default:
		t.Fatal("Close did not cancel the pane context")
	}

	// Cancelling the session context cancels the pane's too.
	p2 := NewViewPane(nil, 2, parent, &paneView{})
	defer p2.Close()
	cancelParent()
	select {
	case <-p2.ctx.Done():
	default:
		t.Fatal("cancelling the parent did not cancel the pane context")
	}
}

func TestViewPaneRenderCrumbsAndFilter(t *testing.T) {
	p := NewViewPane(&Env{}, 1, context.Background(), &paneView{crumb: "hosts (24)"})
	defer p.Close()
	p.stack.Push(&paneView{crumb: "web-01"})
	top := &paneView{crumb: "units", filter: "nginx", body: "one\ntwo"}
	p.stack.Push(top)

	const w, h = 40, 6
	out := p.View(w, h)
	lines := strings.Split(out, "\n")
	if len(lines) != h {
		t.Fatalf("View returned %d lines, want %d", len(lines), h)
	}
	for i, line := range lines {
		if got := render.Width(line); got != w {
			t.Fatalf("line %d is %d cells wide, want %d", i, got, w)
		}
	}
	want := "hosts (24) › web-01 › units · /nginx"
	if got := strings.TrimRight(lines[0], " "); got != want {
		t.Fatalf("crumb line = %q, want %q", got, want)
	}
	if !strings.HasPrefix(lines[1], "one") {
		t.Fatalf("body line 1 = %q, want the view's first line", lines[1])
	}

	// Too long: cut from the left, so the innermost crumb survives.
	narrow := p.View(14, 3)
	first := strings.Split(narrow, "\n")[0]
	if got := render.Width(first); got != 14 {
		t.Fatalf("narrow crumb line is %d cells wide, want 14", got)
	}
	if !strings.HasPrefix(first, "…") {
		t.Fatalf("narrow crumb line = %q, want a leading ellipsis", first)
	}
	if !strings.HasSuffix(first, "nginx") {
		t.Fatalf("narrow crumb line = %q, want it to keep the filter", first)
	}

	// A single-line pane is only the crumb line; none of it is body.
	one := p.View(w, 1)
	if strings.Contains(one, "\n") || !strings.HasPrefix(one, "hosts (24)") {
		t.Fatalf("View(w, 1) = %q, want just the crumb line", one)
	}
}

func TestViewPaneDelegates(t *testing.T) {
	top := &paneView{
		crumb:     "units",
		hints:     []KeyHint{{Key: "enter", Text: "open"}},
		subject:   Subject{Unit: "nginx.service"},
		capturing: true,
		copies:    []string{"a", "b"},
	}
	p := NewViewPane(&Env{}, 1, context.Background(), top)
	defer p.Close()

	if got := p.Title(); got != "units" {
		t.Fatalf("Title() = %q, want \"units\"", got)
	}
	if p.RawKeys() {
		t.Fatal("RawKeys() = true, want false for a view pane")
	}
	if !p.Capturing() {
		t.Fatal("Capturing() = false, want the view's true")
	}
	if got := p.Hints(); len(got) != 1 || got[0].Key != "enter" {
		t.Fatalf("Hints() = %#v, want the view's hints", got)
	}
	if got := p.Subject(); got.Unit != "nginx.service" {
		t.Fatalf("Subject() = %#v, want the view's subject", got)
	}
	if got := p.CopyLines(); len(got) != 2 || got[1] != "b" {
		t.Fatalf("CopyLines() = %v, want the view's lines", got)
	}

	// SetFocus is accepted and changes nothing the view sees.
	p.SetFocus(true)
	if got := p.stack.Top(); got != View(top) {
		t.Fatal("SetFocus replaced the top view")
	}
}

func TestViewPaneQueryFlags(t *testing.T) {
	env := newTestEnv(t)
	cmds := newTestCommands(t)

	queries := []struct {
		line string
		want resource.Query
	}{
		{line: "hosts", want: resource.Query{Kind: "host"}},
		{
			line: "hosts role=web",
			want: resource.Query{Kind: "host", Selector: mustSelector(t, "role=web")},
		},
		{
			line: "hosts --host web-02,web-01",
			want: resource.Query{Kind: "host", Hosts: []resource.HostID{"web-02", "web-01"}},
		},
		{
			line: "hosts dc=ewr --arg unit=nginx.service --host web-01",
			want: resource.Query{
				Kind:     "host",
				Selector: mustSelector(t, "dc=ewr"),
				Hosts:    []resource.HostID{"web-01"},
				Args:     map[string]string{"unit": "nginx.service"},
			},
		},
	}

	for _, tc := range queries {
		got := queryOf(t, env, cmds, tc.line)
		if got.Kind != tc.want.Kind || got.Selector.String() != tc.want.Selector.String() {
			t.Errorf("%q: query = %#v, want %#v", tc.line, got, tc.want)
		}
		if strings.Join(hostsOf(got.Hosts), ",") != strings.Join(hostsOf(tc.want.Hosts), ",") {
			t.Errorf("%q: hosts = %v, want %v", tc.line, got.Hosts, tc.want.Hosts)
		}
		if len(got.Args) != len(tc.want.Args) {
			t.Errorf("%q: args = %v, want %v", tc.line, got.Args, tc.want.Args)
		}
		for k, v := range tc.want.Args {
			if got.Args[k] != v {
				t.Errorf("%q: args[%s] = %q, want %q", tc.line, k, got.Args[k], v)
			}
		}
	}

	// A flag without a value is an error flash, never a panic.
	for _, line := range []string{"hosts --host", "hosts --arg novalue"} {
		msg := runCmd(t, cmds.Exec(env, line))
		flash, ok := msg.(FlashMsg)
		if !ok || flash.Level != FlashError {
			t.Fatalf("%q produced %#v, want an error flash", line, msg)
		}
	}
}

func hostsOf(hs []resource.HostID) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = string(h)
	}
	return out
}

func mustSelector(t *testing.T, s string) resource.Selector {
	t.Helper()
	sel, err := resource.ParseSelector(s)
	if err != nil {
		t.Fatalf("parse selector %q: %v", s, err)
	}
	return sel
}
