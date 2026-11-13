package views

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/action"
	"jumper/internal/backend"
	"jumper/internal/backend/linux"
	"jumper/internal/errs"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/tui"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
)

// Bounds of the drill-in's remote calls.
const (
	procLoadTimeout = 30 * time.Second
	procPlanTimeout = 15 * time.Second
)

// Layout and text constants.
const (
	procLabelWidth = 11 // "oom score  "
	procTimeLayout = "2006-01-02 15:04:05 MST"
	procDash       = "—"
)

// Section IDs the overview reports as unavailable. The backend owns the batch;
// these are the keys of ProcDetail.Failed.
const (
	procSecFDs     = "fds"
	procSecEnviron = "environ"
)

// procInfoMsg is the result of the detail batch, tagged with its Seq so a stale
// refresh is ignored.
type procInfoMsg struct {
	Seq     int
	Info    linux.ProcDetail
	Err     error
	Elapsed time.Duration
}

// procJournalMsg is the result of the process journal listing.
type procJournalMsg struct {
	Seq     int
	Entries []linux.Entry
	Err     error
}

// procInfoer is the part of the Linux backend the drill-in needs. It is an
// interface so the view is testable without a transport.
type procInfoer interface {
	ProcInfo(ctx context.Context, host resource.HostID, pid string) (linux.ProcDetail, error)
}

// procJournaler lists one PID's journal records. It is an interface for the
// same reason.
type procJournaler interface {
	ProcJournal(ctx context.Context, host resource.HostID, pid string, o linux.JournalOptions) ([]linux.Entry, error)
}

// Proc is the tabbed drill-in of one process on one host: the /proc overview,
// the limits table, the environment keys, this process's sockets and its
// journal records.
type Proc struct {
	env  *tui.Env
	b    backend.Backend
	host resource.HostID
	pid  string
	num  int
	keys *keymap.Map

	window resource.TimeWindow

	seq     int
	cancel  context.CancelFunc
	info    linux.ProcDetail
	err     error
	exited  bool
	done    bool
	elapsed time.Duration

	sockSeq    int
	sockCancel context.CancelFunc
	sockCh     <-chan backend.Result
	sockRows   []resource.Resource
	sockErr    error
	sockDone   bool

	jourSeq     int
	jourCancel  context.CancelFunc
	jourLoaded  bool
	jourLoading bool
	jourEntries []linux.Entry
	jourErr     error

	tabs     *Tabs
	overview int
	limits   int
	envKeys  int
	sockets  int
	journal  int

	overviewView *Text
	limitsView   *Text
	envView      *Text
	socketsView  *Text
	journalView  *Text

	w, h int
}

var _ tui.View = (*Proc)(nil)

// NewProc builds the drill-in of one process. b is the Linux backend (any
// backend that answers ProcInfo works) and env supplies the theme, keys, clock
// and session window.
func NewProc(env *tui.Env, b backend.Backend, host resource.HostID, pid string) *Proc {
	keys := keymap.Default()
	if env != nil && env.Keys != nil {
		keys = env.Keys
	}
	pid = strings.TrimSpace(pid)
	num, _ := strconv.Atoi(pid)
	p := &Proc{
		env:  env,
		b:    b,
		host: resource.NormalizeHostID(string(host)),
		pid:  pid,
		num:  num,
		keys: keys,
	}
	if env != nil && env.Session != nil {
		p.window = env.Session.Window
	}
	p.overviewView = NewText(env, "pid "+pid+" overview", nil)
	p.limitsView = NewText(env, "pid "+pid+" limits", nil)
	p.envView = NewText(env, "pid "+pid+" env", nil)
	p.socketsView = NewText(env, "pid "+pid+" sockets", nil)
	p.journalView = NewText(env, "pid "+pid+" journal", nil)

	p.overview, p.limits, p.envKeys, p.sockets, p.journal = 0, 1, 2, 3, 4
	p.tabs = NewTabs(env, []Tab{
		{Key: "1", Title: "overview", View: p.overviewView},
		{Key: "2", Title: "limits", View: p.limitsView},
		{Key: "3", Title: "env", View: p.envView},
		{Key: "4", Title: "sockets", View: p.socketsView},
		{Key: "5", Title: "journal", View: p.journalView},
	})
	return p
}

// ProcFactory builds the drill-in for a query: the query names the host and
// either Args["pid"] or the Args["process"] the process row's `enter` link
// sets.
func ProcFactory(env *tui.Env, q resource.Query) (tui.View, error) {
	if env == nil || env.Registry == nil {
		return nil, errs.New(errs.Config, "views.ProcFactory", "no backends configured")
	}
	b, _, ok := env.Registry.Lookup(string(linux.KindProcess))
	if !ok {
		return nil, errs.New(errs.Config, "views.ProcFactory", "no backend serves the process kind")
	}
	host := hostFromQuery(q)
	if host == "" {
		return nil, errs.New(errs.Config, "views.ProcFactory", "process detail needs a host")
	}
	pid := strings.TrimSpace(q.Args["pid"])
	if pid == "" {
		pid = strings.TrimSpace(q.Args["process"])
	}
	if pid == "" {
		return nil, errs.New(errs.Config, "views.ProcFactory", "process detail needs a pid")
	}
	return NewProc(env, b, host, pid), nil
}

// Init starts the /proc batch and the socket listing in parallel. The journal
// tab loads on its first visit.
func (p *Proc) Init() tea.Cmd { return tea.Batch(p.load(), p.loadSockets()) }

// now is the clock handed in by the environment.
func (p *Proc) now() time.Time {
	if p.env != nil && p.env.Now != nil {
		return p.env.Now()
	}
	return time.Now()
}

// style returns the theme, falling back to the default palette.
func (p *Proc) style() *theme.Theme {
	if p.env != nil && p.env.Theme != nil {
		return p.env.Theme
	}
	return theme.Dark()
}

// load runs the one-batch ProcInfo in a tea.Cmd.
func (p *Proc) load() tea.Cmd {
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), procLoadTimeout)
	p.cancel = cancel

	p.seq++
	seq := p.seq
	p.info, p.err, p.exited, p.done = linux.ProcDetail{}, nil, false, false
	p.elapsed = 0
	started := p.now()

	b, host, pid := p.b, p.host, p.pid
	return func() tea.Msg {
		in, ok := b.(procInfoer)
		if !ok {
			return procInfoMsg{Seq: seq, Err: errs.New(errs.Config, "views.Proc", "this backend has no process detail")}
		}
		res, err := in.ProcInfo(ctx, host, pid)
		return procInfoMsg{Seq: seq, Info: res, Err: err, Elapsed: p.now().Sub(started)}
	}
}

// loadSockets starts the socket listing for this host. The stream is filtered
// to the rows that name this PID.
func (p *Proc) loadSockets() tea.Cmd {
	if p.sockCancel != nil {
		p.sockCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.sockCancel = cancel

	p.sockSeq++
	p.sockCh = nil
	p.sockRows = nil
	p.sockErr = nil
	p.sockDone = false

	q := resource.Query{Kind: linux.KindSocket, Hosts: []resource.HostID{p.host}}
	ch, err := p.b.List(ctx, q)
	if err != nil {
		p.sockDone = true
		p.sockErr = err
		p.socketsView.SetLines(p.socketLines())
		return nil
	}
	p.sockCh = ch
	return tui.ListBatch(p.sockSeq, ch)
}

// loadJournal lists this PID's journal records with the remote _PID= match.
func (p *Proc) loadJournal() tea.Cmd {
	p.jourLoaded = true
	p.jourLoading = true
	p.jourSeq++
	seq := p.jourSeq
	if p.jourCancel != nil {
		p.jourCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.jourCancel = cancel

	j, ok := p.b.(procJournaler)
	if !ok {
		p.jourLoading = false
		p.jourErr = errs.New(errs.Config, "views.Proc", "this backend has no process journal")
		p.journalView.SetLines(p.journalLines())
		return nil
	}
	host, pid, window, now := p.host, p.pid, p.window, p.now()
	return func() tea.Msg {
		entries, err := j.ProcJournal(ctx, host, pid, linux.JournalOptions{
			Window: window,
			Now:    now,
			Sudo:   true,
		})
		return procJournalMsg{Seq: seq, Entries: entries, Err: err}
	}
}

// Update handles the batch, the socket stream, the journal, the session window
// and the keys. Nothing here waits for anything else.
func (p *Proc) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	switch m := msg.(type) {
	case procInfoMsg:
		if m.Seq != p.seq {
			return p, nil
		}
		p.info, p.err = m.Info, m.Err
		p.elapsed, p.done = m.Elapsed, true
		p.exited = m.Err != nil && linux.ProcessExited(m.Err)
		if p.cancel != nil {
			p.cancel()
			p.cancel = nil
		}
		if m.Err == nil {
			p.build()
		}
		return p, nil
	case tui.ListBatchMsg:
		if m.Seq != p.sockSeq {
			return p, nil
		}
		return p, p.applySockets(m)
	case procJournalMsg:
		if m.Seq != p.jourSeq {
			return p, nil
		}
		p.jourLoading = false
		p.jourEntries, p.jourErr = m.Entries, m.Err
		p.journalView.SetLines(p.journalLines())
		if p.jourCancel != nil {
			p.jourCancel()
			p.jourCancel = nil
		}
		return p, nil
	case tui.WindowChangedMsg:
		p.window = m.Window
		return p, tea.Batch(p.load(), p.loadJournal())
	case tea.KeyMsg:
		if cmd, handled := p.handleKey(m); handled {
			return p, cmd
		}
	}

	v, cmd := p.tabs.Update(msg)
	if t, ok := v.(*Tabs); ok {
		p.tabs = t
	}
	if p.tabs.Active() == p.journal && !p.jourLoaded {
		cmd = tea.Batch(cmd, p.loadJournal())
	}
	return p, cmd
}

// handleKey handles the drill-in's own keys: `x` actions, `r` refresh and, once
// the process is gone, the way back.
func (p *Proc) handleKey(m tea.KeyMsg) (tea.Cmd, bool) {
	key := m.String()
	if p.exited {
		switch {
		case p.keys.Back.Matches(key), key == "enter":
			return tui.Pop(), true
		case p.keys.Refresh.Matches(key):
			return p.refresh(), true
		}
		// A gone process has nothing else to do; swallow the key rather than
		// scrolling an empty tab.
		return nil, true
	}
	if p.tabs.Capturing() {
		return nil, false
	}
	switch {
	case p.keys.Actions.Matches(key):
		return p.actions(), true
	case p.keys.Refresh.Matches(key):
		return p.refresh(), true
	}
	return nil, false
}

// refresh reloads the batch and the socket listing, and the journal when it has
// been visited.
func (p *Proc) refresh() tea.Cmd {
	cmds := []tea.Cmd{p.load(), p.loadSockets()}
	if p.jourLoaded {
		cmds = append(cmds, p.loadJournal())
	}
	return tea.Batch(cmds...)
}

// applySockets keeps the rows that name this PID and asks for the next batch.
func (p *Proc) applySockets(m tui.ListBatchMsg) tea.Cmd {
	for _, r := range m.Results {
		if r.Err != nil {
			if p.sockErr == nil {
				p.sockErr = r.Err
			}
			continue
		}
		for _, row := range r.Items {
			if p.holdsPID(row) {
				p.sockRows = append(p.sockRows, row)
			}
		}
	}
	p.socketsView.SetLines(p.socketLines())
	if m.Closed {
		p.sockDone = true
		if p.sockCancel != nil {
			p.sockCancel()
			p.sockCancel = nil
		}
		return nil
	}
	return tui.ListBatch(p.sockSeq, p.sockCh)
}

// holdsPID reports whether a socket row lists this process.
func (p *Proc) holdsPID(r resource.Resource) bool {
	s, ok := r.Raw.(linux.Socket)
	if !ok {
		return false
	}
	for _, proc := range s.Procs {
		if proc.PID == p.num {
			return true
		}
	}
	return false
}

// build turns a loaded batch into the overview, limits and env tabs.
func (p *Proc) build() {
	p.overviewView.SetLines(p.overviewLines())
	p.limitsView.SetLines(p.limitLines())
	p.envView.SetLines(p.envLines())
}

// overviewLines is the overview tab: the key facts of the process.
func (p *Proc) overviewLines() []string {
	d := p.info
	st := p.style()
	lines := []string{
		p.fact("pid", strconv.Itoa(d.PID)),
		p.fact("name", procOrDash(d.Name)),
		p.fact("state", p.stateValue(d.State)),
		p.fact("threads", render.Int(int64(d.Threads))),
		p.fact("rss", procBytes(d.RSS)),
		p.fact("virtual", procBytes(d.VMSize)),
		p.fact("fds", p.fdsValue()),
		p.fact("oom score", render.Int(int64(d.OOMScore))),
		p.fact("started", p.startValue()),
		p.fact("exe", procOrDash(d.Exe)),
		p.fact("cwd", procOrDash(d.Cwd)),
		p.fact("cgroup", p.cgroupValue()),
	}
	if len(d.Failed) > 0 {
		lines = append(lines, "", st.Header.Render("UNAVAILABLE"))
		for _, id := range sortedFailures(d.Failed) {
			lines = append(lines, "  "+id+"  "+st.Crit.Render(string(errs.KindOf(d.Failed[id]))))
		}
	}
	return lines
}

// fact renders one labelled key fact.
func (p *Proc) fact(label, value string) string {
	return p.style().Dim.Render(render.PadRight(label, procLabelWidth)) + "  " + value
}

// stateValue colours the process state.
func (p *Proc) stateValue(state string) string {
	if state == "" {
		return procDash
	}
	st := p.style()
	style := st.Base
	switch {
	case strings.HasPrefix(state, "R"):
		style = st.OK
	case strings.HasPrefix(state, "D"), strings.HasPrefix(state, "Z"):
		style = st.Crit
	case strings.HasPrefix(state, "T"), strings.HasPrefix(state, "S"):
		style = st.Base
	}
	return style.Render(state)
}

// fdsValue is "used / soft limit", with a warning above 80 % of the limit.
func (p *Proc) fdsValue() string {
	if p.info.Failed[procSecFDs] != nil {
		return p.style().Crit.Render(string(errs.KindOf(p.info.Failed[procSecFDs])))
	}
	value := strconv.Itoa(p.info.FDs) + " / " + p.info.FDLimitText()
	if p.info.FDWarn {
		return p.style().Warn.Render(value) + "  " + p.style().Warn.Render("above 80 %")
	}
	return value
}

// startValue is the process start time and how long ago that was.
func (p *Proc) startValue() string {
	if p.info.Start.IsZero() {
		return procDash
	}
	return p.info.Start.Format(procTimeLayout) + " " +
		p.style().Dim.Render("("+render.Age(p.info.Start, p.now())+")")
}

// cgroupValue is the cgroup path and the systemd unit it names.
func (p *Proc) cgroupValue() string {
	cg := p.info.Cgroup
	if cg.Path == "" {
		return procDash
	}
	out := cg.Path
	if cg.Unit != "" {
		out += "  " + p.style().Dim.Render("→") + "  " + p.style().Accent.Render(cg.Unit)
	}
	return out
}

// limitLines is the limits tab: one row per /proc/PID/limits entry.
func (p *Proc) limitLines() []string {
	st := p.style()
	if len(p.info.Limits) == 0 {
		return []string{st.Faint.Render("no limits readable")}
	}
	hdr := render.PadRight("LIMIT", 22) + render.PadRight("SOFT", 14) + render.PadRight("HARD", 14) + "UNITS"
	lines := []string{st.Header.Render(hdr)}
	for _, lim := range p.info.Limits {
		lines = append(lines, render.PadRight(lim.Name, 22)+render.PadRight(lim.Soft, 14)+render.PadRight(lim.Hard, 14)+lim.Units)
	}
	return lines
}

// envLines is the env tab: the variable names only, never the values.
func (p *Proc) envLines() []string {
	st := p.style()
	if p.info.Failed[procSecEnviron] != nil {
		return []string{st.Crit.Render("environment not readable (" + string(errs.KindOf(p.info.Failed[procSecEnviron])) + ")")}
	}
	if len(p.info.EnvKeys) == 0 {
		return []string{st.Faint.Render("no environment keys")}
	}
	lines := []string{
		st.Dim.Render(fmt.Sprintf("%d keys · values are cut off on the remote side", len(p.info.EnvKeys))),
		"",
	}
	return append(lines, p.info.EnvKeys...)
}

// socketLines is the sockets tab: this PID's rows from the socket list.
func (p *Proc) socketLines() []string {
	st := p.style()
	hdr := render.PadRight("PROTO", 6) + render.PadRight("STATE", 10) + render.PadRight("LOCAL", 26) +
		render.PadRight("PEER", 26) + "PROCESS"
	lines := []string{st.Header.Render(hdr)}
	for _, row := range p.sockRows {
		line := render.PadRight(row.Str("proto"), 6) + render.PadRight(row.Str("state"), 10) +
			render.PadRight(row.Str("local"), 26) + render.PadRight(row.Str("peer"), 26) + row.Str("process")
		lines = append(lines, line)
	}
	switch {
	case !p.sockDone && len(p.sockRows) == 0:
		lines = append(lines, st.Faint.Render("loading…"))
	case p.sockErr != nil:
		lines = append(lines, st.Crit.Render("sockets failed: "+errorLabel(p.sockErr)))
	case len(p.sockRows) == 0:
		lines = append(lines, st.Faint.Render("no sockets for this pid"))
	}
	return lines
}

// journalLines is the journal tab: journalctl's records for this PID.
func (p *Proc) journalLines() []string {
	st := p.style()
	switch {
	case p.jourErr != nil:
		return []string{st.Crit.Render("journal failed: " + errorLabel(p.jourErr))}
	case p.jourLoading:
		return []string{st.Faint.Render("loading journal for pid " + p.pid + "…")}
	case len(p.jourEntries) == 0:
		return []string{st.Faint.Render("no journal records for this pid")}
	}
	lines := make([]string, 0, len(p.jourEntries)+1)
	lines = append(lines, st.Dim.Render(fmt.Sprintf("%d records · _PID=%s", len(p.jourEntries), p.pid)))
	for _, e := range p.jourEntries {
		msg := e.Message
		if e.Priority <= 3 {
			msg = st.Crit.Render(msg)
		}
		lines = append(lines, st.Dim.Render(e.Time.Format("15:04:05"))+" "+
			render.PadRight(linux.PriorityName(e.Priority), 6)+" "+
			st.Accent.Render(e.Source)+": "+msg)
	}
	return lines
}

// procBytes renders a byte count, or a dash when the kernel did not report it.
func procBytes(n int64) string {
	if n <= 0 {
		return procDash
	}
	return render.Bytes(n)
}

// procOrDash renders an empty value as a dash.
func procOrDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return procDash
	}
	return s
}

// sortedFailures is the Failed keys in a stable order.
func sortedFailures(failed map[string]error) []string {
	ids := make([]string, 0, len(failed))
	for id := range failed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Render returns exactly h lines of exactly w cells.
func (p *Proc) Render(w, h int) string {
	p.w, p.h = w, h
	if w <= 0 || h <= 0 {
		return ""
	}
	switch {
	case p.exited:
		return p.renderExited(w, h)
	case p.err != nil:
		return p.renderError(w, h)
	}
	return p.tabs.Render(w, h)
}

// renderExited is the screen of a process that disappeared between the list
// and the drill-in.
func (p *Proc) renderExited(w, h int) string {
	st := p.style()
	lines := []string{
		st.Crit.Render("process exited"),
		st.Dim.Render("pid " + p.pid + " on " + string(p.host) + " is gone"),
		"",
		"press " + firstKey(p.keys.Back) + " to go back or " + firstKey(p.keys.Refresh) + " to retry",
	}
	return render.Fit(strings.Join(lines, "\n"), w, h)
}

// renderError is the screen of a drill-in that could not run.
func (p *Proc) renderError(w, h int) string {
	st := p.style()
	lines := []string{
		st.Crit.Render("process detail failed: " + errorLabel(p.err)),
		st.Dim.Render(string(p.host) + " · pid " + p.pid),
		"",
		"press " + firstKey(p.keys.Back) + " to go back or " + firstKey(p.keys.Refresh) + " to retry",
	}
	return render.Fit(strings.Join(lines, "\n"), w, h)
}

// Close cancels the three loads. The view stack calls it when the view is
// popped.
func (p *Proc) Close() error {
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	if p.sockCancel != nil {
		p.sockCancel()
		p.sockCancel = nil
	}
	if p.jourCancel != nil {
		p.jourCancel()
		p.jourCancel = nil
	}
	return p.tabs.Close()
}

// ref is this process's resource reference.
func (p *Proc) ref() resource.Ref {
	return resource.Ref{Backend: "linux", Kind: linux.KindProcess, Host: p.host, Name: p.pid}
}

// selection is the one-row selection the actions menu plans for. The kill
// planner reads the PID from the ref name and the host from the fields.
func (p *Proc) selection() []resource.Resource {
	return []resource.Resource{{
		Ref: p.ref(),
		Fields: map[string]any{
			"host": string(p.host),
			"pid":  int64(p.num),
		},
	}}
}

// actions opens the actions menu for this process (the kill planner of task
// 062). Choosing a spec shows the read-only plan: the exact commands, before
// anything runs.
func (p *Proc) actions() tea.Cmd {
	planner, ok := p.b.(action.Planner)
	if !ok {
		return tui.Flash(tui.FlashWarn, "this backend has no actions")
	}
	if !p.done || p.err != nil {
		return tui.Flash(tui.FlashWarn, "still loading pid %s", p.pid)
	}
	specs := planner.Actions(p.selection())
	if len(specs) == 0 {
		return tui.Flash(tui.FlashWarn, "no actions for pid %s", p.pid)
	}
	items := make([]tui.ListItem, len(specs))
	for i, spec := range specs {
		label := spec.Title
		if spec.Risk == action.RiskDestructive {
			label = "! " + label
		}
		items[i] = tui.ListItem{Label: label, Detail: spec.ID}
	}
	th := p.style()
	title := "actions · pid " + p.pid
	picks := make([]func(int) tea.Cmd, len(specs))
	for i, spec := range specs {
		picks[i] = p.planPick(planner, spec)
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

// planPick plans one spec on this process and shows the steps it would run.
// Plan is read-only, so nothing reaches a node except a dry run.
func (p *Proc) planPick(planner action.Planner, spec action.Spec) func(int) tea.Cmd {
	target := action.Target{Host: p.host, Ref: p.ref()}
	params := make(map[string]string, len(spec.Params))
	for _, param := range spec.Params {
		params[param.Name] = param.Default
	}
	req := action.Request{SpecID: spec.ID, Targets: []action.Target{target}, Params: params, Actor: action.Human}
	th := p.style()
	return func(int) tea.Cmd {
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), procPlanTimeout)
			defer cancel()
			plan, err := planner.Plan(ctx, req)
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

// Crumb is the PID.
func (p *Proc) Crumb() string { return "pid " + p.pid }

// Hints are the active tab's hints plus actions and refresh; a gone process
// offers only the way back and a retry.
func (p *Proc) Hints() []tui.KeyHint {
	if p.exited {
		return []tui.KeyHint{
			{Key: firstKey(p.keys.Back), Text: "back"},
			{Key: firstKey(p.keys.Refresh), Text: "retry"},
		}
	}
	hints := p.tabs.Hints()
	return append(hints,
		tui.KeyHint{Key: firstKey(p.keys.Actions), Text: "actions"},
		tui.KeyHint{Key: firstKey(p.keys.Refresh), Text: "refresh"},
	)
}

// Subject is this process on this host.
func (p *Proc) Subject() tui.Subject {
	return tui.Subject{
		Hosts: []resource.HostID{p.host},
		Kind:  linux.KindProcessDetail,
		Ref:   p.ref(),
	}
}

// Capturing is true while the active tab owns the keyboard.
func (p *Proc) Capturing() bool { return p.tabs.Capturing() }

// CommandLine is the `:` command line that reopens this screen.
func (p *Proc) CommandLine() string {
	return fmt.Sprintf("process-detail --host %s --arg pid=%s", p.host, p.pid)
}

// CopyLines is the plain text of the visible screen.
func (p *Proc) CopyLines() []string {
	if p.exited || p.err != nil {
		return strings.Split(p.Render(p.w, p.h), "\n")
	}
	return p.tabs.CopyLines()
}
