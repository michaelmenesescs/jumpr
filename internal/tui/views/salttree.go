package views

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend/salt"
	"jumpr/internal/errs"
	"jumpr/internal/group"
	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
)

const (
	// saltTreeTimeout bounds one state tree load; the backend's runner bounds
	// every Salt call inside it as well.
	saltTreeTimeout = 90 * time.Second
	// saltTreeDetailTimeout bounds one rendered show_sls.
	saltTreeDetailTimeout = 45 * time.Second
	// The left pane (the sls list) keeps between these cells.
	saltTreeLeftMin = 22
	saltTreeLeftMax = 48
)

// saltTreeMode is what the keyboard is doing.
type saltTreeMode int

const (
	saltTreeIdle saltTreeMode = iota
	saltTreeFilter
	saltTreePrompt
)

// SaltTreeBackend is what the state tree screen needs from the Salt backend.
// The salt package implements it; tests use a fake.
type SaltTreeBackend interface {
	// Tree reads the environments, the sls files and the assignment counts.
	Tree(ctx context.Context, q resource.Query) (salt.Tree, error)
	// ShowSls renders one sls for one minion.
	ShowSls(ctx context.Context, minion, sls string) (salt.ShowState, error)
	// TopFile is state.show_top for the given minions.
	TopFile(ctx context.Context, minions []string) (salt.ShowTop, error)
	// SlsSource reads one file of the state tree.
	SlsSource(ctx context.Context, minion, file string) (string, error)
	// MasterMinion is the minion ID that serves the master's file roots.
	MasterMinion() (string, bool)
	// MinionOf maps a session host onto its minion ID.
	MinionOf(host resource.HostID) (string, bool)
}

// saltTreeLoadMsg is one tree load's answer, tagged with Seq.
type saltTreeLoadMsg struct {
	Seq  int
	Tree salt.Tree
	Err  error
}

// saltTreeDetailMsg is one rendered sls, tagged with DetailSeq.
type saltTreeDetailMsg struct {
	Seq   int
	State salt.ShowState
	Err   error
}

// saltTreeSourceMsg is one raw sls source read.
type saltTreeSourceMsg struct {
	Seq    int
	Source string
	Err    error
}

// saltTreeTopMsg is the top file of the session's minions.
type saltTreeTopMsg struct {
	Top     salt.ShowTop
	Minions []string
	Err     error
}

// SaltTree is `:states` (SPEC §8.6): the environments and sls files of the
// master's state tree on the left, the rendered states of the selected sls on
// the right. It is a tui.View; all remote work runs in a tea.Cmd with a
// deadline.
type SaltTree struct {
	env *tui.Env
	b   SaltTreeBackend
	// envName narrows the browser to one environment; "" shows them all.
	envName string
	keys    *keymap.Map
	glyphs  render.GlyphSet
	theme   *theme.Theme

	// base is the pane's context; every load derives from it.
	base context.Context

	seq        int
	cancel     context.CancelFunc
	envs       []string
	files      []salt.SlsFile
	view       []salt.SlsFile
	assigned   map[string]int
	assignedOK bool
	minions    []string
	loadErr    error
	loading    bool

	cursor int
	filter string
	mode   saltTreeMode
	input  textinput.Model

	detailSeq     int
	detailSls     string
	detailMinion  string
	detail        salt.ShowState
	detailErr     error
	detailLoading bool

	sourceActive bool
	source       string
	sourceErr    error
	sourcePath   string

	w, h int
}

var _ tui.View = (*SaltTree)(nil)

// NewSaltTree builds the state tree screen. envName as "*" or "" shows every
// environment.
func NewSaltTree(env *tui.Env, b SaltTreeBackend, envName string) *SaltTree {
	keys := keymap.Default()
	if env != nil && env.Keys != nil {
		keys = env.Keys
	}
	glyphs, _ := render.Glyphs("unicode")
	if env != nil && env.Glyphs.Name != "" {
		glyphs = env.Glyphs
	}
	t := &SaltTree{env: env, b: b, keys: keys, glyphs: glyphs}
	if name := strings.TrimSpace(envName); name != "*" {
		t.envName = name
	}
	if env != nil && env.Theme != nil {
		t.theme = env.Theme
	} else {
		t.theme = theme.Dark()
	}
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 0
	t.input = in
	return t
}

// Init starts the tree load.
func (s *SaltTree) Init() tea.Cmd { return s.load() }

// SetContext makes every load derive from ctx, so closing the pane stops the
// remote calls.
func (s *SaltTree) SetContext(ctx context.Context) {
	if ctx != nil {
		s.base = ctx
	}
}

// Close cancels a load in flight. The stack calls it when the view is popped.
func (s *SaltTree) Close() error {
	s.stop()
	return nil
}

// baseCtx is the context every load derives from.
func (s *SaltTree) baseCtx() context.Context {
	if s.base != nil {
		return s.base
	}
	return context.Background()
}

// load abandons any load in flight and reads the tree again.
func (s *SaltTree) load() tea.Cmd {
	s.stop()
	s.seq++
	seq := s.seq
	s.files, s.view = nil, nil
	s.assigned = nil
	s.assignedOK = false
	s.envs, s.minions = nil, nil
	s.loadErr = nil
	s.loading = true
	s.cursor = 0
	s.detailSls, s.detailMinion = "", ""
	s.detail, s.detailErr = salt.ShowState{}, nil
	s.sourceActive, s.source, s.sourceErr, s.sourcePath = false, "", nil, ""
	if s.b == nil {
		s.loading = false
		s.loadErr = fmt.Errorf("no salt backend configured")
		return nil
	}
	q := s.query()
	b := s.b
	ctx, cancel := context.WithCancel(s.baseCtx())
	s.cancel = cancel
	return func() tea.Msg {
		cctx, ccancel := context.WithTimeout(ctx, saltTreeTimeout)
		defer ccancel()
		tree, err := b.Tree(cctx, q)
		return saltTreeLoadMsg{Seq: seq, Tree: tree, Err: err}
	}
}

// query is the tree query: the session's hosts, so the assignment counts cover
// exactly the targets.
func (s *SaltTree) query() resource.Query {
	q := resource.Query{Kind: salt.KindSls, Hosts: s.sessionHosts()}
	if s.envName != "" {
		q.Args = map[string]string{"env": s.envName}
	}
	return q
}

// stop cancels a load in flight.
func (s *SaltTree) stop() {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

// sessionHosts are the hosts of the session target set.
func (s *SaltTree) sessionHosts() []resource.HostID {
	if s.env == nil || s.env.Session == nil {
		return nil
	}
	return append([]resource.HostID(nil), s.env.Session.Targets.Hosts...)
}

// Update folds loads and routes keys.
func (s *SaltTree) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	switch m := msg.(type) {
	case saltTreeLoadMsg:
		return s, s.applyLoad(m)
	case saltTreeDetailMsg:
		if m.Seq != s.detailSeq {
			return s, nil
		}
		s.detailLoading = false
		s.detail, s.detailErr = m.State, m.Err
		return s, nil
	case saltTreeSourceMsg:
		if m.Seq != s.detailSeq {
			return s, nil
		}
		s.source, s.sourceErr = m.Source, m.Err
		return s, nil
	case saltTreeTopMsg:
		if m.Err != nil {
			return s, tui.Flash(tui.FlashError, "top file: %v", m.Err)
		}
		return s, tui.Push(NewText(s.env, "top file", topFileLines(m.Top, m.Minions)))
	case tui.RefreshRequestMsg:
		return s, s.load()
	case tea.KeyMsg:
		return s.handleKey(m)
	}
	return s, nil
}

// applyLoad keeps the tree, or the reason there is none.
func (s *SaltTree) applyLoad(m saltTreeLoadMsg) tea.Cmd {
	if m.Seq != s.seq {
		return nil
	}
	s.loading = false
	if m.Err != nil {
		s.loadErr = m.Err
		return nil
	}
	s.envs = m.Tree.Envs
	s.files = m.Tree.Files
	s.assigned = m.Tree.Assigned
	s.assignedOK = m.Tree.AssignedOK
	s.minions = m.Tree.Minions
	s.refilter()
	return nil
}

// setFilter installs the local filter and rebuilds the visible rows.
func (s *SaltTree) setFilter(text string) {
	s.filter = text
	s.refilter()
}

// refilter rebuilds the visible rows, keeping the cursor inside them.
func (s *SaltTree) refilter() {
	s.view = s.view[:0]
	needle := strings.ToLower(strings.TrimSpace(s.filter))
	for _, f := range s.files {
		if needle != "" {
			hay := strings.ToLower(f.Name + " " + f.Env + " " + f.Path)
			if !strings.Contains(hay, needle) {
				continue
			}
		}
		s.view = append(s.view, f)
	}
	sort.SliceStable(s.view, func(i, j int) bool {
		a, b := s.view[i], s.view[j]
		if a.Env != b.Env {
			return a.Env < b.Env
		}
		return a.Name < b.Name
	})
	if s.cursor >= len(s.view) {
		s.cursor = len(s.view) - 1
	}
	if s.cursor < 0 {
		s.cursor = 0
	}
}

// handleKey routes one key press.
func (s *SaltTree) handleKey(m tea.KeyMsg) (tui.View, tea.Cmd) {
	if s.mode != saltTreeIdle {
		return s, s.inputKey(m)
	}
	key := m.String()
	switch {
	case s.keys.Filter.Matches(key):
		return s, s.beginInput(saltTreeFilter, "")
	case s.keys.Up.Matches(key):
		s.move(-1)
		return s, nil
	case s.keys.Down.Matches(key):
		s.move(1)
		return s, nil
	case s.keys.PageUp.Matches(key):
		s.move(-max(1, s.h-2))
		return s, nil
	case s.keys.PageDown.Matches(key):
		s.move(max(1, s.h-2))
		return s, nil
	case s.keys.Top.Matches(key):
		s.cursor = 0
		return s, nil
	case s.keys.Bottom.Matches(key):
		if len(s.view) > 0 {
			s.cursor = len(s.view) - 1
		}
		return s, nil
	case s.keys.Open.Matches(key):
		return s, s.openSls()
	case s.keys.Refresh.Matches(key):
		return s, s.load()
	case s.keys.Back.Matches(key):
		if s.filter != "" {
			s.setFilter("")
			return s, nil
		}
		return s, tui.Pop()
	}
	switch key {
	case "t":
		return s, s.topCmd()
	case "c":
		return s, s.sourceCmd()
	case "d":
		return s, s.dryRunCmd()
	}
	return s, nil
}

// beginInput opens the filter or the minion prompt.
func (s *SaltTree) beginInput(mode saltTreeMode, value string) tea.Cmd {
	s.mode = mode
	s.input.SetValue(value)
	s.input.CursorEnd()
	s.input.Focus()
	return nil
}

// inputKey edits the active prompt. enter applies it, esc drops it.
func (s *SaltTree) inputKey(m tea.KeyMsg) tea.Cmd {
	switch m.String() {
	case "enter":
		value := strings.TrimSpace(s.input.Value())
		mode := s.mode
		s.mode = saltTreeIdle
		s.input.Blur()
		switch mode {
		case saltTreeFilter:
			s.setFilter(value)
		case saltTreePrompt:
			f, ok := s.selected()
			if value == "" || !ok {
				return tui.Flash(tui.FlashInfo, "type a minion id")
			}
			return s.loadDetail(value, f)
		}
		return nil
	case "esc":
		mode := s.mode
		s.mode = saltTreeIdle
		s.input.Blur()
		if mode == saltTreeFilter {
			s.setFilter("")
		}
		return nil
	}
	var cmd tea.Cmd
	s.input, cmd = s.input.Update(m)
	if s.mode == saltTreeFilter {
		s.setFilter(s.input.Value())
	}
	return cmd
}

// move moves the cursor by delta, keeping it inside the rows.
func (s *SaltTree) move(delta int) {
	last := len(s.view) - 1
	if last < 0 {
		s.cursor = 0
		return
	}
	s.cursor += delta
	if s.cursor < 0 {
		s.cursor = 0
	}
	if s.cursor > last {
		s.cursor = last
	}
}

// selected returns the sls under the cursor.
func (s *SaltTree) selected() (salt.SlsFile, bool) {
	if s.cursor < 0 || s.cursor >= len(s.view) {
		return salt.SlsFile{}, false
	}
	return s.view[s.cursor], true
}

// openSls renders the selected sls for the first session minion, or asks for
// one when the session has no minion at all.
func (s *SaltTree) openSls() tea.Cmd {
	f, ok := s.selected()
	if !ok {
		return tui.Flash(tui.FlashInfo, "no sls selected")
	}
	minion := s.defaultMinion()
	if minion == "" {
		s.beginInput(saltTreePrompt, "")
		return nil
	}
	return s.loadDetail(minion, f)
}

// defaultMinion is the minion of the first session target, so enter renders
// the sls for the host the operator selected first.
func (s *SaltTree) defaultMinion() string {
	if s.b != nil {
		for _, h := range s.sessionHosts() {
			if m, ok := s.b.MinionOf(h); ok && m != "" {
				return m
			}
		}
	}
	if len(s.minions) > 0 {
		return s.minions[0]
	}
	return ""
}

// loadDetail renders one sls for one minion.
func (s *SaltTree) loadDetail(minion string, f salt.SlsFile) tea.Cmd {
	if s.b == nil {
		return tui.Flash(tui.FlashError, "no salt backend configured")
	}
	s.detailSeq++
	seq := s.detailSeq
	s.detailSls, s.detailMinion = f.Name, minion
	s.detail, s.detailErr = salt.ShowState{}, nil
	s.detailLoading = true
	s.sourceActive, s.source, s.sourceErr = false, "", nil
	b, base := s.b, s.baseCtx()
	name := f.Name
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, saltTreeDetailTimeout)
		defer cancel()
		st, err := b.ShowSls(ctx, minion, name)
		return saltTreeDetailMsg{Seq: seq, State: st, Err: err}
	}
}

// topCmd reads the top file of the session's minions and groups them by
// identical sls list.
func (s *SaltTree) topCmd() tea.Cmd {
	if len(s.minions) == 0 {
		return tui.Flash(tui.FlashWarn, "no target of this session maps to a salt minion")
	}
	if s.b == nil {
		return tui.Flash(tui.FlashError, "no salt backend configured")
	}
	b, base := s.b, s.baseCtx()
	minions := append([]string(nil), s.minions...)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, saltTreeDetailTimeout)
		defer cancel()
		top, err := b.TopFile(ctx, minions)
		return saltTreeTopMsg{Top: top, Minions: minions, Err: err}
	}
}

// sourceCmd reads the raw source of the selected sls from the master's own
// minion.
func (s *SaltTree) sourceCmd() tea.Cmd {
	f, ok := s.selected()
	if !ok {
		return tui.Flash(tui.FlashInfo, "no sls selected")
	}
	if s.b == nil {
		return tui.Flash(tui.FlashError, "no salt backend configured")
	}
	minion, ok := s.b.MasterMinion()
	if !ok {
		return tui.Flash(tui.FlashError, "the salt master has no minion of its own")
	}
	s.detailSeq++
	seq := s.detailSeq
	s.sourceActive = true
	s.source, s.sourceErr = "", nil
	s.sourcePath = f.Path
	b, base := s.b, s.baseCtx()
	file := f.Path
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, saltTreeDetailTimeout)
		defer cancel()
		src, err := b.SlsSource(ctx, minion, file)
		return saltTreeSourceMsg{Seq: seq, Source: src, Err: err}
	}
}

// dryRunCmd opens the dry run of the selected sls on the session targets
// through `:salt state` (task 075).
func (s *SaltTree) dryRunCmd() tea.Cmd {
	f, ok := s.selected()
	if !ok {
		return tui.Flash(tui.FlashInfo, "no sls selected")
	}
	line := "salt state " + f.Name
	return func() tea.Msg { return tui.RunCommandMsg{Line: line} }
}

// topFileLines renders the grouped top file: the fan-out headline, then one
// line per group with its sls list.
func topFileLines(top salt.ShowTop, minions []string) []string {
	g := group.NewGrouper(len(minions), nil)
	for _, m := range minions {
		list := top.SLS(m)
		text := strings.Join(list, "\n")
		if len(list) == 0 {
			if errsOf := top.Errors[m]; len(errsOf) > 0 {
				text = "error: " + strings.Join(errsOf, "; ")
			} else {
				text = "(no top entry)"
			}
		}
		g.Add(resource.HostID(m), text, 0)
	}
	sum := g.Summary()
	lines := []string{sum.Headline()}
	for _, grp := range sum.Groups {
		detail := strings.Join(strings.Split(strings.TrimRight(grp.Output, "\n"), "\n"), ", ")
		lines = append(lines, fmt.Sprintf("%4d  %s", len(grp.Hosts), detail))
	}
	lines = append(lines, "", fmt.Sprintf("minions: %s", strings.Join(minions, ", ")))
	return lines
}

// redact passes text through the session redactor before it is shown. Rendered
// states can carry pillar-derived secrets (passwords in file.managed contents).
func (s *SaltTree) redact(text string) string {
	if s.env != nil && s.env.Redact != nil {
		return s.env.Redact.String(text)
	}
	return text
}

// Render draws the banner, the sls list and the detail pane, exactly w × h.
func (s *SaltTree) Render(w, h int) string {
	s.w, s.h = w, h
	if w <= 0 || h <= 0 {
		return ""
	}
	lines := []string{s.banner(w)}
	if h > 1 {
		leftW, rightW := s.split(w)
		lines = append(lines, s.headerLine(leftW, rightW))
		visible := h - 2
		left := s.leftLines(leftW, visible)
		right := s.rightLines(rightW, visible)
		for i := 0; i < visible; i++ {
			lines = append(lines, render.PadRight(left[i], leftW)+" "+render.PadRight(right[i], rightW))
		}
	}
	return render.FitWith(s.glyphs, strings.Join(lines, "\n"), w, h)
}

// split divides the width into the list pane and the detail pane.
func (s *SaltTree) split(w int) (int, int) {
	left := w * 2 / 5
	if left < saltTreeLeftMin {
		left = saltTreeLeftMin
	}
	if left > saltTreeLeftMax {
		left = saltTreeLeftMax
	}
	if left > w-8 {
		left = w / 3
	}
	right := w - left - 1
	if right < 1 {
		right = 1
	}
	return left, right
}

// banner is the summary line and, while it is open, the input prompt.
func (s *SaltTree) banner(w int) string {
	var text string
	switch {
	case s.mode == saltTreeFilter:
		text = "filter> " + s.input.View()
	case s.mode == saltTreePrompt:
		text = "minion> " + s.input.View()
	case s.loadErr != nil:
		text = "error: " + s.loadErr.Error()
	case s.loading:
		text = "reading the state tree " + string(s.glyphs.Ellipsis)
	default:
		text = fmt.Sprintf("%d envs · %d sls · %d minions", len(s.envs), len(s.files), len(s.minions))
		if s.assignedOK {
			text += fmt.Sprintf(" · assigned on %d minions", len(s.minions))
		} else if len(s.minions) > 0 {
			text += " · assignment unknown"
		}
		if s.filter != "" {
			text += " · filter: " + s.filter
		}
	}
	text = render.Truncate(text, w)
	if s.loadErr != nil && s.mode == saltTreeIdle && s.theme != nil {
		text = s.theme.Flash.Error.Render(text)
	} else if s.theme != nil {
		text = s.theme.Dim.Render(text)
	}
	return render.PadRight(text, w)
}

// headerLine is the two column titles.
func (s *SaltTree) headerLine(leftW, rightW int) string {
	nameW := max(1, leftW-14)
	left := render.PadRight(render.Truncate("SLS", nameW), nameW) + " " +
		render.PadRight("ENV", 5) + render.PadLeft("ASSIGNED", 8)
	left = render.Truncate(left, leftW)
	right := s.detailTitle()
	right = render.Truncate(right, rightW)
	if s.theme != nil {
		left = s.theme.Header.Render(left)
		right = s.theme.Header.Render(right)
	}
	return render.PadRight(left, leftW) + " " + render.PadRight(right, rightW)
}

// detailTitle names what the right pane shows.
func (s *SaltTree) detailTitle() string {
	switch {
	case s.sourceActive:
		return "SOURCE " + s.sourcePath
	case s.detailSls != "":
		return fmt.Sprintf("SLS %s · %s", s.detailSls, s.detailMinion)
	case s.loading:
		return "reading…"
	default:
		return "SLS ID / FUNCTION / NAME / REQUISITES"
	}
}

// leftLines renders the sls list, scrolled so the cursor stays visible.
func (s *SaltTree) leftLines(w, h int) []string {
	out := make([]string, h)
	if h <= 0 {
		return out
	}
	start := 0
	if s.cursor >= h {
		start = s.cursor - h + 1
	}
	for i := 0; i < h; i++ {
		idx := start + i
		if idx >= len(s.view) {
			out[i] = render.PadRight("", w)
			continue
		}
		f := s.view[idx]
		assigned := "-"
		if s.assignedOK {
			assigned = strconv.Itoa(s.assigned[salt.SlsKey(f.Env, f.Name)])
		}
		line := render.PadRight(render.Truncate(f.Name, max(1, w-14)), max(1, w-14)) + " " +
			render.PadRight(render.Truncate(f.Env, 5), 5) + render.PadLeft(assigned, 8)
		line = render.PadRight(render.Truncate(line, w), w)
		if s.theme != nil {
			switch {
			case idx == s.cursor:
				line = s.theme.Selected.Render(line)
			case f.Env != "base" && f.Env != "":
				line = s.theme.Dim.Render(line)
			}
		}
		out[i] = line
	}
	return out
}

// rightLines renders the detail pane: source, rendered states or the hints.
func (s *SaltTree) rightLines(w, h int) []string {
	out := make([]string, h)
	if h <= 0 {
		return out
	}
	var src []string
	switch {
	case s.sourceActive:
		src = s.sourceLines()
	case s.detailErr != nil:
		src = []string{"error: " + s.detailErr.Error()}
	case s.detailSls != "":
		src = s.stateLines()
	default:
		src = []string{
			"select an sls and press enter to render it",
			"t top file · c source · d dry run · / filter · r reload",
		}
	}
	for i := 0; i < h; i++ {
		if i < len(src) {
			out[i] = render.PadRight(render.Truncate(src[i], w), w)
			continue
		}
		out[i] = render.PadRight("", w)
	}
	return out
}

// sourceLines renders the raw sls source, redacted, or why it is unreadable.
func (s *SaltTree) sourceLines() []string {
	if s.sourceErr != nil {
		return []string{"source not readable", "(" + s.sourceErr.Error() + ")"}
	}
	text := s.redact(s.source)
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []string{"source not readable"}
	}
	return lines
}

// stateLines renders the show_sls rows and the render errors.
func (s *SaltTree) stateLines() []string {
	var out []string
	if s.detailLoading {
		out = append(out, "rendering "+string(s.glyphs.Ellipsis))
	}
	for _, def := range s.detail.Defs {
		cells := []string{
			def.ID,
			def.Function(),
			def.SLS,
			def.Name,
			def.Requisites,
		}
		for i, c := range cells {
			cells[i] = s.redact(c)
		}
		out = append(out, strings.Join(cells, "  "))
	}
	if len(s.detail.Defs) == 0 && !s.detailLoading {
		out = append(out, "(no states)")
	}
	for _, e := range s.detail.Errors {
		text := s.redact(e.Text)
		if e.Minion != "" {
			text = e.Minion + ": " + text
		}
		out = append(out, text)
	}
	return out
}

// Crumb is "states (n)".
func (s *SaltTree) Crumb() string { return fmt.Sprintf("states (%d)", len(s.view)) }

// Hints are the keys the screen answers.
func (s *SaltTree) Hints() []tui.KeyHint {
	if s.mode != saltTreeIdle {
		return []tui.KeyHint{{Key: "enter", Text: "apply"}, {Key: "esc", Text: "cancel"}}
	}
	return []tui.KeyHint{
		{Key: firstKey(s.keys.Open), Text: "render"},
		{Key: "t", Text: "top file"},
		{Key: "c", Text: "source"},
		{Key: "d", Text: "dry run"},
		{Key: firstKey(s.keys.Filter), Text: "filter"},
		{Key: firstKey(s.keys.Refresh), Text: "reload"},
		{Key: firstKey(s.keys.Back), Text: "back"},
	}
}

// Subject is the slice of the tree the session targets cover.
func (s *SaltTree) Subject() tui.Subject {
	hosts := make([]resource.HostID, 0, len(s.minions))
	for _, m := range s.minions {
		hosts = append(hosts, resource.HostID(m))
	}
	return tui.Subject{Hosts: hosts, Kind: salt.KindSls}
}

// Capturing is true while the filter or the minion prompt owns the keyboard.
func (s *SaltTree) Capturing() bool { return s.mode != saltTreeIdle }

// saltTreeFactory builds the state tree browser of the query's environment.
// It is keyed by the real salt-sls kind: the salt backend serves the rows and
// the view.
func saltTreeFactory(env *tui.Env, q resource.Query) (tui.View, error) {
	if env == nil || env.Registry == nil {
		return nil, errs.New(errs.Config, "views.saltTreeFactory", "no backends configured")
	}
	name := strings.TrimSpace(q.Backend)
	if name == "" {
		name = "salt"
	}
	b, ok := env.Registry.Get(name)
	if !ok {
		return nil, errs.New(errs.Config, "views.saltTreeFactory", "no salt backend registered")
	}
	sb, ok := b.(SaltTreeBackend)
	if !ok {
		return nil, errs.New(errs.Config, "views.saltTreeFactory", "backend cannot read the state tree")
	}
	return NewSaltTree(env, sb, q.Args["env"]), nil
}

// NewSaltTreeFromEnv builds the state tree screen for a query, resolving the
// Salt backend through the registry.
func NewSaltTreeFromEnv(env *tui.Env, q resource.Query) (*SaltTree, error) {
	v, err := saltTreeFactory(env, q)
	if err != nil {
		return nil, err
	}
	tree, ok := v.(*SaltTree)
	if !ok {
		return nil, errs.New(errs.Internal, "views.saltTree", "factory did not return the state tree view")
	}
	return tree, nil
}

// CopyLines is the plain text of the screen, without colour.
func (s *SaltTree) CopyLines() []string {
	header := fmt.Sprintf("states: %d envs, %d sls, %d minions", len(s.envs), len(s.files), len(s.minions))
	if s.loadErr != nil {
		header = "states: error: " + s.loadErr.Error()
	}
	out := []string{header}
	for _, f := range s.view {
		assigned := ""
		if s.assignedOK {
			assigned = "  " + strconv.Itoa(s.assigned[salt.SlsKey(f.Env, f.Name)])
		}
		out = append(out, f.Env+"/"+f.Name+"  "+f.Path+assigned)
	}
	if s.detailSls != "" {
		out = append(out, "", s.detailSls+" · "+s.detailMinion)
		out = append(out, s.stateLines()...)
	}
	if s.sourceActive {
		out = append(out, "", "source "+s.sourcePath)
		out = append(out, s.sourceLines()...)
	}
	return out
}
