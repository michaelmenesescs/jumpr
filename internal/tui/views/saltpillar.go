package views

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/action"
	"jumper/internal/backend/salt"
	"jumper/internal/errs"
	"jumper/internal/investigation"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/tui"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
)

// KindPillar is the factory key of the pillar screen. It is a view kind, not a
// backend kind: the salt backend serves salt-minion.
const KindPillar resource.KindName = "salt-pillar"

const (
	// pillarLoadTimeout bounds one pillar.items job.
	pillarLoadTimeout = 90 * time.Second
	// pillarRevealTimeout bounds the one pillar.get a reveal runs.
	pillarRevealTimeout = 45 * time.Second
	// pillarRevealKey is the key that reveals a leaf value; `r` is refresh.
	pillarRevealKey = "R"
	// pillarIndent is the cells one level of the tree costs.
	pillarIndent = 2
)

// PillarBackend is what the pillar screen needs from the Salt backend. The
// salt package implements it; tests use a fake.
type PillarBackend interface {
	// PillarKeys returns the pillar of one minion as types and lengths only.
	PillarKeys(ctx context.Context, minion string) (salt.PillarTree, error)
	// PillarValue reads exactly one value, for the reveal dialog.
	PillarValue(ctx context.Context, minion, keyPath string) (string, error)
}

// pillarMode is what the keyboard is doing.
type pillarMode int

const (
	pillarIdle pillarMode = iota
	pillarFilter
)

// pillarLoadMsg is one pillar read, tagged with Seq.
type pillarLoadMsg struct {
	Seq  int
	Tree salt.PillarTree
	Err  error
}

// pillarValueMsg is one revealed value. It carries the value to the dialog and
// nowhere else: the view keeps only the path.
type pillarValueMsg struct {
	Path  string
	Value string
	Err   error
}

// pillarRow is one visible row of the tree.
type pillarRow struct {
	Node     salt.PillarNode
	Depth    int
	Expanded bool
}

// SaltPillar is `:pillar <minion>` (SPEC §8.6 "Pillar"): the structure and the
// keys of a minion's pillar, with every value hidden until one is deliberately
// revealed. It is a tui.View; the master is read in tea.Cmds with deadlines.
type SaltPillar struct {
	env    *tui.Env
	b      PillarBackend
	minion string
	keys   *keymap.Map
	glyphs render.GlyphSet
	theme  *theme.Theme

	// base is the pane's context; every load derives from it.
	base context.Context

	seq      int
	cancel   context.CancelFunc
	tree     salt.PillarTree
	rows     []pillarRow
	expanded map[string]bool
	filter   string
	mode     pillarMode
	input    textinput.Model
	loading  bool
	loadErr  error
	cursor   int

	w, h int
}

var _ tui.View = (*SaltPillar)(nil)

// NewSaltPillar builds the pillar screen of one minion.
func NewSaltPillar(env *tui.Env, b PillarBackend, minion string) *SaltPillar {
	keys := keymap.Default()
	if env != nil && env.Keys != nil {
		keys = env.Keys
	}
	glyphs, _ := render.Glyphs("unicode")
	if env != nil && env.Glyphs.Name != "" {
		glyphs = env.Glyphs
	}
	th := theme.Dark()
	if env != nil && env.Theme != nil {
		th = env.Theme
	}
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 0
	return &SaltPillar{
		env:      env,
		b:        b,
		minion:   strings.TrimSpace(minion),
		keys:     keys,
		glyphs:   glyphs,
		theme:    th,
		expanded: make(map[string]bool),
		input:    in,
	}
}

// Init starts the pillar read.
func (s *SaltPillar) Init() tea.Cmd { return s.load() }

// SetContext makes every load derive from ctx, so closing the pane stops it.
func (s *SaltPillar) SetContext(ctx context.Context) {
	if ctx != nil {
		s.base = ctx
	}
}

// Close cancels a read in flight. The stack calls it when the view is popped.
func (s *SaltPillar) Close() error {
	s.stop()
	return nil
}

// now returns the clock the environment handed in.
func (s *SaltPillar) now() time.Time {
	if s.env != nil && s.env.Now != nil {
		return s.env.Now()
	}
	return time.Now()
}

// baseCtx is the context every load derives from.
func (s *SaltPillar) baseCtx() context.Context {
	if s.base != nil {
		return s.base
	}
	return context.Background()
}

// stop cancels a read in flight.
func (s *SaltPillar) stop() {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

// readOnly reports the session-level read-only switch.
func (s *SaltPillar) readOnly() bool {
	return s.env != nil && s.env.Session != nil && s.env.Session.ReadOnly
}

// load abandons a read in flight and asks the master for the pillar of the
// minion, structure only.
func (s *SaltPillar) load() tea.Cmd {
	s.stop()
	s.seq++
	seq := s.seq
	s.tree = salt.PillarTree{}
	s.rows = nil
	s.loadErr = nil
	s.loading = true
	s.cursor = 0
	if s.b == nil {
		s.loading = false
		s.loadErr = errs.New(errs.Config, "views.saltpillar", "no salt backend configured")
		return nil
	}
	if s.minion == "" {
		s.loading = false
		s.loadErr = errs.New(errs.Config, "views.saltpillar", "pillar needs a minion")
		return nil
	}
	b, minion := s.b, s.minion
	ctx, cancel := context.WithCancel(s.baseCtx())
	s.cancel = cancel
	return func() tea.Msg {
		cctx, ccancel := context.WithTimeout(ctx, pillarLoadTimeout)
		defer ccancel()
		tree, err := b.PillarKeys(cctx, minion)
		return pillarLoadMsg{Seq: seq, Tree: tree, Err: err}
	}
}

// Update folds loads and revealed values, and routes keys. A superseded load
// is dropped.
func (s *SaltPillar) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	switch m := msg.(type) {
	case pillarLoadMsg:
		if m.Seq != s.seq {
			return s, nil
		}
		return s, s.applyLoad(m)
	case pillarValueMsg:
		return s, s.applyValue(m)
	case tui.RefreshRequestMsg:
		return s, s.load()
	case tea.KeyMsg:
		return s.handleKey(m)
	}
	return s, nil
}

// applyLoad keeps the tree, or the reason there is none, and opens the first
// level so the top-level keys are on screen at once.
func (s *SaltPillar) applyLoad(m pillarLoadMsg) tea.Cmd {
	s.loading = false
	if m.Err != nil {
		s.loadErr = m.Err
		s.rows = nil
		return nil
	}
	s.tree = m.Tree
	for _, n := range s.tree.Nodes {
		if len(n.Children) > 0 && len(s.expanded) == 0 {
			s.expanded[n.Key] = true
		}
	}
	s.rebuild()
	return nil
}

// rebuild flattens the tree into the visible rows.
func (s *SaltPillar) rebuild() {
	s.rows = s.rows[:0]
	if needle := strings.ToLower(strings.TrimSpace(s.filter)); needle != "" {
		// A filter shows every matching key path, whatever its depth.
		var walk func(nodes []salt.PillarNode)
		walk = func(nodes []salt.PillarNode) {
			for _, n := range nodes {
				if strings.Contains(strings.ToLower(n.Key), needle) {
					s.rows = append(s.rows, pillarRow{Node: n})
				}
				walk(n.Children)
			}
		}
		walk(s.tree.Nodes)
	} else {
		var walk func(nodes []salt.PillarNode, depth int)
		walk = func(nodes []salt.PillarNode, depth int) {
			for _, n := range nodes {
				expanded := s.expanded[n.Key]
				s.rows = append(s.rows, pillarRow{Node: n, Depth: depth, Expanded: expanded})
				if expanded && len(n.Children) > 0 {
					walk(n.Children, depth+1)
				}
			}
		}
		walk(s.tree.Nodes, 0)
	}
	s.clampCursor()
}

// setFilter installs the key-path filter and rebuilds the rows.
func (s *SaltPillar) setFilter(text string) {
	s.filter = text
	s.rebuild()
}

// clampCursor keeps the cursor inside the rows.
func (s *SaltPillar) clampCursor() {
	last := len(s.rows) - 1
	if last < 0 {
		s.cursor = 0
		return
	}
	if s.cursor > last {
		s.cursor = last
	}
	if s.cursor < 0 {
		s.cursor = 0
	}
}

// move moves the cursor by delta.
func (s *SaltPillar) move(delta int) {
	s.cursor += delta
	s.clampCursor()
}

// setCursor puts the cursor on n.
func (s *SaltPillar) setCursor(n int) {
	s.cursor = n
	s.clampCursor()
}

// selectedRow returns the row under the cursor.
func (s *SaltPillar) selectedRow() (pillarRow, bool) {
	if s.cursor < 0 || s.cursor >= len(s.rows) {
		return pillarRow{}, false
	}
	return s.rows[s.cursor], true
}

// handleKey routes one key press.
func (s *SaltPillar) handleKey(m tea.KeyMsg) (tui.View, tea.Cmd) {
	if s.mode != pillarIdle {
		return s, s.inputKey(m)
	}
	key := m.String()
	switch {
	case s.keys.Filter.Matches(key):
		return s, s.beginFilter()
	case s.keys.Up.Matches(key):
		s.move(-1)
		return s, nil
	case s.keys.Down.Matches(key):
		s.move(1)
		return s, nil
	case s.keys.PageUp.Matches(key):
		s.move(-max(1, s.h-3))
		return s, nil
	case s.keys.PageDown.Matches(key):
		s.move(max(1, s.h-3))
		return s, nil
	case s.keys.Top.Matches(key):
		s.setCursor(0)
		return s, nil
	case s.keys.Bottom.Matches(key):
		s.setCursor(len(s.rows) - 1)
		return s, nil
	case s.keys.Open.Matches(key):
		s.toggle()
		return s, nil
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
	case pillarRevealKey:
		return s, s.reveal()
	case " ":
		s.toggle()
		return s, nil
	}
	return s, nil
}

// beginFilter opens the key-path filter.
func (s *SaltPillar) beginFilter() tea.Cmd {
	s.mode = pillarFilter
	s.input.SetValue(s.filter)
	s.input.CursorEnd()
	s.input.Focus()
	return nil
}

// inputKey edits the filter. enter keeps it, esc drops it.
func (s *SaltPillar) inputKey(m tea.KeyMsg) tea.Cmd {
	switch m.String() {
	case "enter":
		s.mode = pillarIdle
		s.input.Blur()
		s.setFilter(strings.TrimSpace(s.input.Value()))
		return nil
	case "esc":
		s.mode = pillarIdle
		s.input.Blur()
		s.setFilter("")
		return nil
	}
	var cmd tea.Cmd
	s.input, cmd = s.input.Update(m)
	s.setFilter(s.input.Value())
	return cmd
}

// toggle expands or collapses the selected node.
func (s *SaltPillar) toggle() {
	row, ok := s.selectedRow()
	if !ok || len(row.Node.Children) == 0 {
		return
	}
	if s.expanded[row.Node.Key] {
		delete(s.expanded, row.Node.Key)
	} else {
		s.expanded[row.Node.Key] = true
	}
	s.rebuild()
}

// reveal asks for confirmation and then reads exactly one value. It is refused
// in a read-only session.
func (s *SaltPillar) reveal() tea.Cmd {
	row, ok := s.selectedRow()
	if !ok {
		return tui.Flash(tui.FlashInfo, "select a key to reveal")
	}
	if len(row.Node.Children) > 0 || row.Node.Type == "dict" || row.Node.Type == "list" {
		return tui.Flash(tui.FlashInfo, "%s is a %s; reveal a leaf key", row.Node.Key, row.Node.Type)
	}
	if s.readOnly() {
		return tui.Flash(tui.FlashWarn, "read-only mode: pillar values cannot be revealed")
	}
	path, minion := row.Node.Key, s.minion
	text := fmt.Sprintf("reveal pillar value %s on %s? it will be shown on screen", path, minion) +
		"\nthe value stays in the master's job cache either way; jumper never records or copies it"
	yes := s.revealCmd(path)
	th := s.theme
	return func() tea.Msg {
		return tui.OpenDialogMsg{Dialog: tui.NewConfirmDialog(th, "reveal pillar value", text, yes)}
	}
}

// revealCmd reads one value for the dialog that asked for it.
func (s *SaltPillar) revealCmd(path string) tea.Cmd {
	b, minion, base := s.b, s.minion, s.baseCtx()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, pillarRevealTimeout)
		defer cancel()
		value, err := b.PillarValue(ctx, minion, path)
		return pillarValueMsg{Path: path, Value: value, Err: err}
	}
}

// applyValue shows the revealed value in a dialog and records the path — never
// the value — in the investigation.
func (s *SaltPillar) applyValue(m pillarValueMsg) tea.Cmd {
	if m.Err != nil {
		return tui.Flash(tui.FlashError, "reveal %s: %v", m.Path, m.Err)
	}
	s.recordReveal(m.Path)
	dialog := newPillarValueDialog(s.theme, fmt.Sprintf("pillar value %s · %s", m.Path, s.minion), m.Value)
	return func() tea.Msg { return tui.OpenDialogMsg{Dialog: dialog} }
}

// recordReveal appends the timeline entry of a reveal: the path, no value.
func (s *SaltPillar) recordReveal(path string) {
	if s.env == nil || s.env.Recorder == nil {
		return
	}
	rec := s.env.Recorder
	if _, open := rec.Current(); !open {
		return
	}
	text := "pillar value revealed: " + path
	if s.env.Redact != nil {
		text = s.env.Redact.String(text)
	}
	_ = rec.Append(investigation.Entry{
		Time:  s.now(),
		Kind:  investigation.EntryView,
		Actor: action.Human,
		Text:  text,
	})
}

// Render draws the banner, the header and the visible rows, exactly w × h.
func (s *SaltPillar) Render(w, h int) string {
	s.w, s.h = w, h
	if w <= 0 || h <= 0 {
		return ""
	}
	lines := []string{s.banner(w)}
	if h > 1 {
		lines = append(lines, s.header(w))
		if visible := h - 2; visible > 0 {
			start := 0
			if s.cursor >= visible {
				start = s.cursor - visible + 1
			}
			for i := start; i < start+visible && i < len(s.rows); i++ {
				lines = append(lines, s.rowLine(i, w))
			}
			for i := len(s.rows); i < start+visible; i++ {
				lines = append(lines, render.PadRight("", w))
			}
		}
	}
	return render.FitWith(s.glyphs, strings.Join(lines, "\n"), w, h)
}

// banner is the summary line: the minion, the key count and, while it is open,
// the filter prompt.
func (s *SaltPillar) banner(w int) string {
	var text string
	switch {
	case s.mode == pillarFilter:
		text = "filter> " + s.input.View()
	case s.loadErr != nil:
		text = "error: " + s.loadErr.Error()
	case s.loading:
		text = "reading the pillar of " + s.minion + " " + string(s.glyphs.Ellipsis)
	default:
		text = fmt.Sprintf("pillar %s · %d keys · values hidden", s.minion, len(flattenKeys(s.tree)))
		if s.filter != "" {
			text += " · filter: " + s.filter
		}
	}
	text = render.Truncate(text, w)
	if s.theme != nil {
		if s.loadErr != nil {
			text = s.theme.Flash.Error.Render(text)
		} else {
			text = s.theme.Dim.Render(text)
		}
	}
	return render.PadRight(text, w)
}

// header is the column title row.
func (s *SaltPillar) header(w int) string {
	line := render.Truncate("KEY PATH", w)
	if s.theme != nil {
		line = s.theme.Header.Render(line)
	}
	return render.PadRight(line, w)
}

// rowLine renders one tree row: the indent, the expand marker, the key and its
// type and length.
func (s *SaltPillar) rowLine(i int, w int) string {
	row := s.rows[i]
	line := render.PadRight(render.Truncate(s.rowText(row), w), w)
	if s.theme == nil {
		return line
	}
	switch {
	case i == s.cursor:
		return s.theme.Selected.Render(line)
	case row.Node.Type == "dict":
		return s.theme.Base.Render(line)
	}
	return line
}

// rowText is the plain text of one row: "  ▸ tls:  ‹dict, 2 keys›".
func (s *SaltPillar) rowText(row pillarRow) string {
	marker := "  "
	if len(row.Node.Children) > 0 {
		switch {
		case row.Expanded && s.glyphs.ASCII():
			marker = "v "
		case row.Expanded:
			marker = "▾ "
		case s.glyphs.ASCII():
			marker = "> "
		default:
			marker = "▸ "
		}
	}
	name := pillarLeafName(row.Node.Key)
	if row.Node.Type == "dict" {
		name += ":"
	}
	indent := strings.Repeat(" ", row.Depth*pillarIndent)
	return indent + marker + name + "  " + pillarTypeLabel(row.Node)
}

// pillarLeafName is the last segment of a key path.
func pillarLeafName(key string) string {
	if i := strings.LastIndexByte(key, ':'); i >= 0 {
		return key[i+1:]
	}
	return key
}

// pillarTypeLabel is the type and size cell of one node: never a value.
func pillarTypeLabel(n salt.PillarNode) string {
	switch n.Type {
	case "dict":
		return fmt.Sprintf("‹dict, %s %s›", render.Int(int64(len(n.Children))), pluralOf(len(n.Children), "key", "keys"))
	case "list":
		return fmt.Sprintf("‹list, %s %s›", render.Int(int64(n.Len)), pluralOf(n.Len, "item", "items"))
	case "str":
		return fmt.Sprintf("‹str, %s %s›", render.Int(int64(n.Len)), pluralOf(n.Len, "char", "chars"))
	default:
		return "‹" + n.Type + "›"
	}
}

// pluralOf picks between the singular and plural form of a noun for n.
func pluralOf(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// flattenKeys is every key path of the tree, containers included.
func flattenKeys(t salt.PillarTree) []string {
	var out []string
	var walk func(nodes []salt.PillarNode)
	walk = func(nodes []salt.PillarNode) {
		for _, n := range nodes {
			out = append(out, n.Key)
			walk(n.Children)
		}
	}
	walk(t.Nodes)
	return out
}

// Crumb is "pillar <minion>".
func (s *SaltPillar) Crumb() string { return "pillar " + s.minion }

// Hints are the keys the screen answers.
func (s *SaltPillar) Hints() []tui.KeyHint {
	if s.mode != pillarIdle {
		return []tui.KeyHint{{Key: "enter", Text: "apply"}, {Key: "esc", Text: "cancel"}}
	}
	return []tui.KeyHint{
		{Key: firstKey(s.keys.Open), Text: "expand"},
		{Key: pillarRevealKey, Text: "reveal"},
		{Key: firstKey(s.keys.Filter), Text: "filter"},
		{Key: firstKey(s.keys.Refresh), Text: "reload"},
		{Key: firstKey(s.keys.Back), Text: "back"},
	}
}

// Subject is the minion the pillar belongs to.
func (s *SaltPillar) Subject() tui.Subject {
	return tui.Subject{
		Hosts: []resource.HostID{resource.HostID(s.minion)},
		Kind:  salt.KindMinion,
		Ref:   resource.Ref{Backend: "salt", Kind: salt.KindMinion, Name: s.minion},
	}
}

// Capturing is true while the filter owns the keyboard.
func (s *SaltPillar) Capturing() bool { return s.mode != pillarIdle }

// CommandLine is the `:` command line that reopens this screen.
func (s *SaltPillar) CommandLine() string {
	return "salt-pillar --arg minion=" + s.minion
}

// CopyLines returns the plain text of the screen. It holds key paths, types
// and lengths: a value is never part of this view, revealed or not.
func (s *SaltPillar) CopyLines() []string {
	head := fmt.Sprintf("pillar %s: %d keys, values hidden", s.minion, len(s.tree.Nodes))
	if s.loadErr != nil {
		head = "pillar " + s.minion + ": error: " + s.loadErr.Error()
	}
	out := []string{head}
	for _, row := range s.rows {
		out = append(out, row.Node.Key+"  "+pillarTypeLabel(row.Node))
	}
	if s.filter != "" {
		out = append(out, "filter: "+s.filter)
	}
	return out
}

// pillarValueDialog shows one revealed value. It is a modal of the root, so
// copy mode never sees it, and esc closes it.
type pillarValueDialog struct {
	t     *theme.Theme
	title string
	lines []string
	top   int
	h     int
}

var _ tui.Dialog = (*pillarValueDialog)(nil)

// newPillarValueDialog builds the value box. The text lives here and nowhere
// else: the view does not keep it.
func newPillarValueDialog(t *theme.Theme, title, value string) *pillarValueDialog {
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(value, "\r\n", "\n"), "\n"), "\n")
	if len(lines) == 0 {
		lines = []string{""}
	}
	return &pillarValueDialog{t: t, title: title, lines: lines}
}

// Title is the box's border title.
func (d *pillarValueDialog) Title() string { return d.title }

// Update scrolls the value or closes the box.
func (d *pillarValueDialog) Update(msg tea.Msg) (tui.Dialog, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return d, nil
	}
	switch k.String() {
	case "esc", "q":
		return d, tui.CloseDialog()
	case "up", "k":
		d.top--
	case "down", "j":
		d.top++
	case "pgup":
		d.top -= max(1, d.page())
	case "pgdown":
		d.top += max(1, d.page())
	case "home", "g":
		d.top = 0
	case "end", "G":
		d.top = len(d.lines) - 1
	}
	d.clamp()
	return d, nil
}

// page is the visible line count, one line of which is the footer.
func (d *pillarValueDialog) page() int {
	if d.h-1 < 1 {
		return 1
	}
	return d.h - 1
}

// clamp keeps the scroll position inside the value.
func (d *pillarValueDialog) clamp() {
	if d.top < 0 {
		d.top = 0
	}
	if last := len(d.lines) - d.page(); d.top > last {
		d.top = max(0, last)
	}
	if d.top > len(d.lines)-1 {
		d.top = max(0, len(d.lines)-1)
	}
}

// View renders the visible value lines and the footer, exactly maxW cells
// wide.
func (d *pillarValueDialog) View(maxW, maxH int) string {
	if maxW < 1 || maxH < 1 {
		return ""
	}
	d.h = maxH
	d.clamp()
	rows := maxH - 1
	if rows < 1 {
		rows = 1
	}
	lines := make([]string, 0, rows+1)
	for i := 0; i < rows; i++ {
		idx := d.top + i
		if idx >= len(d.lines) {
			lines = append(lines, "")
			continue
		}
		lines = append(lines, d.lines[idx])
	}
	footer := "esc close · not recorded, not copied"
	if d.t != nil {
		footer = d.t.Dim.Render(footer)
	}
	lines = append(lines, footer)
	for i := range lines {
		lines[i] = render.PadRight(render.Truncate(lines[i], maxW), maxW)
	}
	return strings.Join(lines, "\n")
}

// pillarFactory builds the pillar screen of the query's minion.
func pillarFactory(env *tui.Env, q resource.Query) (tui.View, error) {
	if env == nil || env.Registry == nil {
		return nil, errs.New(errs.Config, "views.pillarFactory", "no backends configured")
	}
	minion := strings.TrimSpace(q.Args["minion"])
	if minion == "" {
		return nil, errs.New(errs.Config, "views.pillarFactory", "pillar needs a minion")
	}
	name := strings.TrimSpace(q.Backend)
	if name == "" {
		name = "salt"
	}
	b, ok := env.Registry.Get(name)
	if !ok {
		return nil, errs.New(errs.Config, "views.pillarFactory", "no salt backend registered")
	}
	pb, ok := b.(PillarBackend)
	if !ok {
		return nil, errs.New(errs.Config, "views.pillarFactory", "backend cannot read pillar data")
	}
	return NewSaltPillar(env, pb, minion), nil
}
