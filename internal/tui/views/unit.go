package views

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/action"
	"jumpr/internal/backend"
	"jumpr/internal/backend/elastic"
	"jumpr/internal/backend/linux"
	"jumpr/internal/errs"
	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
)

// Bounds the detail batch and the read-only plan preview.
const (
	unitLoadTimeout = 30 * time.Second
	unitPlanTimeout = 15 * time.Second
)

// Layout and text constants of the status tab.
const (
	unitLabelWidth = 12 // "unit file     "
	unitTimeLayout = "2006-01-02 15:04:05 MST"
	unitStatusHead = "systemctl status"
	unitEmptyValue = "—"
)

// unitInfoMsg is the result of the detail batch, tagged with its Seq so a stale
// refresh is ignored.
type unitInfoMsg struct {
	Seq     int
	Info    linux.UnitInfo
	Err     error
	Elapsed time.Duration
}

// unitInfoer is the part of the Linux backend the detail view needs. It is an
// interface so the view is testable without a transport.
type unitInfoer interface {
	UnitInfo(ctx context.Context, host resource.HostID, unit string) (linux.UnitInfo, error)
}

// Unit is the tabbed detail of one systemd unit: status, show, cat, journal and
// the dependency trees.
type Unit struct {
	env  *tui.Env
	b    backend.Backend
	host resource.HostID
	name string
	keys *keymap.Map

	window resource.TimeWindow

	seq     int
	cancel  context.CancelFunc
	info    linux.UnitInfo
	err     error
	done    bool
	started time.Time
	elapsed time.Duration

	tabs   *Tabs
	status int
	show   int
	cat    int
	jour   int
	dep    int

	journal *Journal
	deps    *unitDeps

	w, h int
}

var _ tui.View = (*Unit)(nil)

// NewUnit builds the detail of one unit on one host. b is the Linux backend
// (any backend that answers UnitInfo works) and env supplies the theme, keys,
// clock and session window.
func NewUnit(env *tui.Env, b backend.Backend, host resource.HostID, unit string) *Unit {
	keys := keymap.Default()
	if env != nil && env.Keys != nil {
		keys = env.Keys
	}
	u := &Unit{
		env:  env,
		b:    b,
		host: resource.NormalizeHostID(string(host)),
		name: strings.TrimSpace(unit),
		keys: keys,
	}
	if env != nil && env.Session != nil {
		u.window = env.Session.Window
	}
	u.journal = NewJournal(env, b, resource.Query{
		Kind:   linux.KindJournal,
		Hosts:  []resource.HostID{u.host},
		Args:   map[string]string{"unit": u.name},
		Window: u.window,
	})
	u.status, u.show, u.cat, u.jour = 0, 1, 2, 3
	u.deps = NewUnitDeps(env, b, u.host, u.name)
	u.dep = 4
	u.tabs = NewTabs(env, []Tab{
		{Key: "1", Title: "status", View: NewText(env, u.name+" status", nil)},
		{Key: "2", Title: "show", View: NewText(env, u.name+" show", nil)},
		{Key: "3", Title: "cat", View: NewText(env, u.name+" cat", nil)},
		{Key: "4", Title: "journal", View: u.journal},
		{Key: "5", Title: "deps", View: u.deps},
	})
	return u
}

// Init starts the detail batch. The journal tab loads on its first visit.
func (u *Unit) Init() tea.Cmd { return tea.Batch(u.load(), u.tabs.Init()) }

// now is the clock handed in by the environment.
func (u *Unit) now() time.Time {
	if u.env != nil && u.env.Now != nil {
		return u.env.Now()
	}
	return time.Now()
}

// style returns the theme, falling back to the default palette.
func (u *Unit) style() *theme.Theme {
	if u.env != nil && u.env.Theme != nil {
		return u.env.Theme
	}
	return theme.Dark()
}

// load runs the one-batch UnitInfo in a tea.Cmd.
func (u *Unit) load() tea.Cmd {
	if u.cancel != nil {
		u.cancel()
		u.cancel = nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), unitLoadTimeout)
	u.cancel = cancel

	u.seq++
	seq := u.seq
	u.info, u.err, u.done = linux.UnitInfo{}, nil, false
	u.elapsed = 0
	u.started = u.now()

	b, host, unit, started := u.b, u.host, u.name, u.started
	return func() tea.Msg {
		info, ok := b.(unitInfoer)
		if !ok {
			return unitInfoMsg{Seq: seq, Err: errs.New(errs.Config, "views.Unit", "this backend has no unit detail")}
		}
		res, err := info.UnitInfo(ctx, host, unit)
		return unitInfoMsg{Seq: seq, Info: res, Err: err, Elapsed: u.now().Sub(started)}
	}
}

// Update handles the batch, the actions and refresh keys, and routes everything
// else to the tab container.
func (u *Unit) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	if m, ok := msg.(unitInfoMsg); ok {
		if m.Seq != u.seq {
			return u, nil
		}
		u.info, u.err = m.Info, m.Err
		u.elapsed, u.done = m.Elapsed, true
		if u.cancel != nil {
			u.cancel()
			u.cancel = nil
		}
		if m.Err == nil {
			u.build()
		}
		return u, nil
	}
	if k, ok := msg.(tea.KeyMsg); ok && !u.tabs.Capturing() {
		if cmd, handled := u.handleKey(k); handled {
			return u, cmd
		}
	}
	v, cmd := u.tabs.Update(msg)
	if t, ok := v.(*Tabs); ok {
		u.tabs = t
	}
	return u, cmd
}

// handleKey handles the detail's own keys: `x` actions, `r` refresh, `l` logs
// for this unit and `E` drop-in preview from the cat tab (task 087).
func (u *Unit) handleKey(m tea.KeyMsg) (tea.Cmd, bool) {
	key := m.String()
	switch {
	case u.keys.Actions.Matches(key):
		return u.actions(), true
	case u.keys.Refresh.Matches(key):
		return u.refresh(), true
	case key == "l":
		return u.logs(), true
	case key == "E" && u.tabs.Active() == u.cat:
		return u.openDropIn(), true
	}
	return nil, false
}

// logs opens the central logs filtered by this unit. The filter needs a log
// source with a configured unit field; without one the backend ignores it and
// the screen says so in its banner.
func (u *Unit) logs() tea.Cmd {
	return tui.OpenQuery(resource.Query{
		Kind:   elastic.KindLog,
		Hosts:  []resource.HostID{u.host},
		Args:   map[string]string{elastic.ArgUnit: u.name},
		Window: u.window,
	})
}

// refresh reloads the batch, the journal and the deps tab when they have been
// visited.
func (u *Unit) refresh() tea.Cmd {
	cmds := []tea.Cmd{u.load()}
	if u.tabs.Visited(u.jour) {
		cmds = append(cmds, u.journal.Init())
	}
	if u.tabs.Visited(u.dep) {
		cmds = append(cmds, u.deps.Init())
	}
	return tea.Batch(cmds...)
}

// build turns a loaded batch into the two text tabs.
func (u *Unit) build() {
	u.tabs.SetView(u.status, NewText(u.env, u.name+" status", u.statusLines()))
	u.tabs.SetView(u.show, newShowTab(u.env, u.name+" show", u.showPairs()))
	u.tabs.SetView(u.cat, NewText(u.env, u.name+" cat", u.catLines()))
}

// statusLines is the status tab: the key facts first, then the raw text.
func (u *Unit) statusLines() []string {
	d := u.info.Detail
	st := u.style()
	lines := []string{
		u.fact("state", u.stateValue(d)),
		u.fact("since", u.sinceValue(d)),
		u.fact("restarts", render.Int(int64(d.NRestarts))),
		u.fact("main pid", orEmpty(d.ExecMainPID)),
		u.fact("memory", u.memoryValue(d)),
		u.fact("tasks", render.Int(int64(d.TasksCurrent))),
		u.fact("result", orEmpty(d.Result)),
		u.fact("unit file", u.unitFileValue(d)),
	}
	lines = append(lines, "", st.Dim.Render("── "+unitStatusHead+" ──"))
	raw := strings.Split(u.info.Status, "\n")
	if len(raw) == 1 && raw[0] == "" {
		raw = []string{st.Faint.Render("no status output")}
	}
	return append(lines, raw...)
}

// fact renders one labelled key fact.
func (u *Unit) fact(label, value string) string {
	return u.style().Dim.Render(render.PadRight(label, unitLabelWidth)) + "  " + value
}

// stateValue renders "active (running)" with the active state coloured.
func (u *Unit) stateValue(d linux.Detail) string {
	if d.ActiveState == "" {
		return unitEmptyValue
	}
	st := u.style()
	style := st.Base
	switch d.ActiveState {
	case "active":
		style = st.OK
	case "failed":
		style = st.Crit
	case "activating", "deactivating", "reloading":
		style = st.Warn
	}
	out := style.Render(d.ActiveState)
	if d.SubState != "" {
		out += " " + st.Dim.Render("("+d.SubState+")")
	}
	return out
}

// sinceValue renders the timestamp the unit entered its current state and how
// long ago that was.
func (u *Unit) sinceValue(d linux.Detail) string {
	t := d.ActiveEnterTimestamp
	if t.IsZero() {
		t = d.InactiveEnterTimestamp
	}
	if t.IsZero() {
		return unitEmptyValue
	}
	return t.Format(unitTimeLayout) + " " + u.style().Dim.Render("("+render.Age(t, u.now())+")")
}

// memoryValue renders MemoryCurrent, or a dash when systemd did not report it.
func (u *Unit) memoryValue(d linux.Detail) string {
	if d.MemoryCurrent < 0 {
		return unitEmptyValue
	}
	return render.Bytes(d.MemoryCurrent)
}

// unitFileValue renders the unit-file state and, dimmed, the fragment path.
func (u *Unit) unitFileValue(d linux.Detail) string {
	out := orEmpty(d.UnitFileState)
	if d.FragmentPath != "" {
		out += "  " + u.style().Dim.Render(d.FragmentPath)
	}
	return out
}

// showPairs is every `systemctl show` property, sorted by name.
func (u *Unit) showPairs() [][2]string {
	keys := make([]string, 0, len(u.info.Props))
	for k := range u.info.Props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([][2]string, len(keys))
	for i, k := range keys {
		pairs[i] = [2]string{k, u.info.Props[k]}
	}
	return pairs
}

// catLines is the cat tab: `# /path` headers accented and drop-ins marked.
func (u *Unit) catLines() []string {
	st := u.style()
	raw := strings.Split(u.info.Cat, "\n")
	if len(raw) == 1 && raw[0] == "" {
		return []string{st.Faint.Render("no unit file")}
	}
	lines := make([]string, len(raw))
	for i, line := range raw {
		lines[i] = line
		path, ok := catHeader(line)
		if !ok {
			continue
		}
		head := st.Accent.Render("# " + path)
		if strings.Contains(path, ".d/") {
			head += "  " + st.Warn.Render("drop-in")
		}
		lines[i] = head
	}
	return lines
}

// catHeader is the path of a `systemctl cat` file header line ("# /path").
func catHeader(line string) (string, bool) {
	line = strings.TrimRight(line, "\r")
	if !strings.HasPrefix(line, "# /") {
		return "", false
	}
	p := strings.TrimSpace(strings.TrimPrefix(line, "#"))
	return p, p != ""
}

// orEmpty renders an empty string as a dash.
func orEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return unitEmptyValue
	}
	return s
}

// ref is this unit's resource reference.
func (u *Unit) ref() resource.Ref {
	return resource.Ref{Backend: "linux", Kind: linux.KindUnit, Host: u.host, Name: u.name}
}

// selection is the one-row selection the actions menu plans for. The systemctl
// planner reads the unit's active state from it.
func (u *Unit) selection() []resource.Resource {
	return []resource.Resource{{
		Ref: u.ref(),
		Fields: map[string]any{
			"host":   string(u.host),
			"unit":   u.name,
			"active": u.info.Detail.ActiveState,
			"sub":    u.info.Detail.SubState,
		},
	}}
}

// actions opens the actions menu for this unit (task 062's planner). Choosing a
// spec shows the read-only plan: the exact commands, before anything runs.
func (u *Unit) actions() tea.Cmd {
	p, ok := u.b.(action.Planner)
	if !ok {
		return tui.Flash(tui.FlashWarn, "this backend has no actions")
	}
	if !u.done || u.err != nil {
		return tui.Flash(tui.FlashWarn, "still loading %s", u.name)
	}
	specs := p.Actions(u.selection())
	if len(specs) == 0 {
		return tui.Flash(tui.FlashWarn, "no actions for %s", u.name)
	}
	items := make([]tui.ListItem, len(specs))
	for i, spec := range specs {
		label := spec.Title
		if spec.Risk == action.RiskDestructive {
			label = "! " + label
		}
		items[i] = tui.ListItem{Label: label, Detail: spec.ID}
	}
	th := u.style()
	title := "actions · " + u.name
	picks := make([]func(int) tea.Cmd, len(specs))
	for i, spec := range specs {
		picks[i] = u.planPick(p, spec)
	}
	return func() tea.Msg {
		return tui.OpenDialogMsg{Dialog: tui.NewListDialog(th, title, items, func(i int) tea.Cmd {
			if i < 0 || i >= len(picks) {
				return nil
			}
			return picks[i](i)
		})}
	}
}

// planPick plans one spec on this unit and shows the steps it would run. Plan
// is read-only, so nothing reaches a node except the dry run.
func (u *Unit) planPick(p action.Planner, spec action.Spec) func(int) tea.Cmd {
	target := action.Target{Host: u.host, Ref: u.ref()}
	params := make(map[string]string, len(spec.Params))
	for _, param := range spec.Params {
		params[param.Name] = param.Default
	}
	req := action.Request{SpecID: spec.ID, Targets: []action.Target{target}, Params: params, Actor: action.Human}
	th := u.style()
	return func(int) tea.Cmd {
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), unitPlanTimeout)
			defer cancel()
			plan, err := p.Plan(ctx, req)
			if err != nil {
				return tui.FlashMsg{Level: tui.FlashError, Text: err.Error()}
			}
			items := make([]tui.ListItem, 0, len(plan.Steps))
			for _, step := range plan.Steps {
				items = append(items, tui.ListItem{Label: step.Display, Detail: string(step.Target.Host)})
			}
			if len(items) == 0 {
				items = append(items, tui.ListItem{Label: "nothing to run"})
			}
			return tui.OpenDialogMsg{Dialog: tui.NewListDialog(th, plan.Title, items, nil)}
		}
	}
}

// Close cancels the batch and the journal.
func (u *Unit) Close() error {
	if u.cancel != nil {
		u.cancel()
		u.cancel = nil
	}
	return u.tabs.Close()
}

// Render returns exactly h lines of exactly w cells.
func (u *Unit) Render(w, h int) string {
	u.w, u.h = w, h
	return u.tabs.Render(w, h)
}

// Crumb is the unit name.
func (u *Unit) Crumb() string { return u.name }

// Hints are the active tab's hints plus actions and refresh.
func (u *Unit) Hints() []tui.KeyHint {
	hints := u.tabs.Hints()
	hints = append(hints,
		tui.KeyHint{Key: "l", Text: "logs"},
		tui.KeyHint{Key: firstKey(u.keys.Actions), Text: "actions"},
		tui.KeyHint{Key: firstKey(u.keys.Refresh), Text: "refresh"},
	)
	return hints
}

// Subject is this unit on this host.
func (u *Unit) Subject() tui.Subject {
	return tui.Subject{
		Hosts: []resource.HostID{u.host},
		Unit:  u.name,
		Kind:  linux.KindUnitDetail,
		Ref:   u.ref(),
	}
}

// Capturing is true while the active tab owns the keyboard.
func (u *Unit) Capturing() bool { return u.tabs.Capturing() }

// CommandLine is the `:` command line that reopens this screen.
func (u *Unit) CommandLine() string {
	return fmt.Sprintf("unit-detail --host %s --arg unit=%s", u.host, u.name)
}

// CopyLines is the plain text of the active tab.
func (u *Unit) CopyLines() []string { return u.tabs.CopyLines() }

// showTab is the `show` tab: a KV list whose `/` filter hides the properties
// that do not match, instead of only jumping to them.
type showTab struct {
	*Text
	env       *tui.Env
	crumb     string
	pairs     [][2]string
	filter    string
	filtering bool
}

var _ tui.View = (*showTab)(nil)

// newShowTab builds the property list.
func newShowTab(env *tui.Env, crumb string, pairs [][2]string) *showTab {
	s := &showTab{Text: NewKV(env, crumb, pairs), env: env, crumb: crumb, pairs: pairs}
	return s
}

// Update intercepts the filter keys and forwards everything else.
func (s *showTab) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		v, cmd := s.Text.Update(msg)
		if t, ok := v.(*Text); ok {
			s.Text = t
		}
		return s, cmd
	}
	key := k.String()
	if s.filtering {
		switch k.Type {
		case tea.KeyEnter:
			s.filtering = false
			return s, nil
		case tea.KeyEsc:
			s.filtering = false
			s.setFilter("")
			return s, nil
		case tea.KeyBackspace:
			if r := []rune(s.filter); len(r) > 0 {
				s.setFilter(string(r[:len(r)-1]))
			}
			return s, nil
		case tea.KeyCtrlU:
			s.setFilter("")
			return s, nil
		case tea.KeyRunes:
			s.setFilter(s.filter + string(k.Runes))
			return s, nil
		}
		return s, nil
	}
	if key == "/" {
		s.filtering = true
		return s, nil
	}
	v, cmd := s.Text.Update(msg)
	if t, ok := v.(*Text); ok {
		s.Text = t
	}
	return s, cmd
}

// setFilter installs the filter and rebuilds the visible pairs.
func (s *showTab) setFilter(f string) {
	s.filter = f
	kept := make([][2]string, 0, len(s.pairs))
	for _, p := range s.pairs {
		if s.matches(p) {
			kept = append(kept, p)
		}
	}
	s.Text.SetLines(NewKV(s.env, s.crumb, kept).lines)
}

// matches reports whether a pair passes the filter. It looks at the key and the
// value, case-insensitively.
func (s *showTab) matches(p [2]string) bool {
	if s.filter == "" {
		return true
	}
	needle := strings.ToLower(s.filter)
	return strings.Contains(strings.ToLower(p[0]), needle) || strings.Contains(strings.ToLower(p[1]), needle)
}

// Capturing is true while the filter owns the keyboard.
func (s *showTab) Capturing() bool { return s.filtering }

// Crumb is the caller-supplied breadcrumb.
func (s *showTab) Crumb() string { return s.crumb }
