package views

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"jumpr/internal/backend"
	"jumpr/internal/backend/linux"
	"jumpr/internal/errs"
	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
)

// depsLoadTimeout bounds the dependency batch.
const depsLoadTimeout = 30 * time.Second

// depsCollapseDepth is the first depth that is hidden until its parent is
// expanded: the tree shows the unit and its direct dependencies, and collapses
// everything below level 2.
const depsCollapseDepth = 2

// Section titles of the deps tab.
const (
	depsForwardTitle = "requires / wants"
	depsReverseTitle = "required by"
	depsEmptyValue   = "—"
)

// unitDepsMsg is the result of the dependency batch, tagged with its Seq so a
// stale refresh is ignored.
type unitDepsMsg struct {
	Seq  int
	Deps linux.Dependencies
	Err  error
}

// unitDepser is the part of the Linux backend the deps tab needs. It is an
// interface so the tab is testable without a transport.
type unitDepser interface {
	UnitDependencies(ctx context.Context, host resource.HostID, unit string) (linux.Dependencies, error)
}

// depRef names one node of one section: the expansion key of the tree.
type depRef struct {
	section int
	index   int
}

// depRow is one rendered line: either a section header, a note or a node.
type depRow struct {
	title string // non-empty for a header or a note
	note  bool
	node  linux.DepNode
	ref   depRef
}

// unitDeps is the "deps" tab of the unit detail: the forward dependency tree and
// the reverse tree, collapsed below depth 2, with enter opening a dependency's
// own unit detail.
type unitDeps struct {
	env  *tui.Env
	b    backend.Backend
	host resource.HostID
	unit string
	keys *keymap.Map

	seq    int
	cancel context.CancelFunc
	deps   linux.Dependencies
	err    error
	done   bool

	expanded map[depRef]bool
	rows     []depRow
	cursor   int
	offset   int

	w, h int
}

var _ tui.View = (*unitDeps)(nil)

// NewUnitDeps builds the deps tab of one unit on one host.
func NewUnitDeps(env *tui.Env, b backend.Backend, host resource.HostID, unit string) *unitDeps {
	keys := keymap.Default()
	if env != nil && env.Keys != nil {
		keys = env.Keys
	}
	return &unitDeps{
		env:      env,
		b:        b,
		host:     resource.NormalizeHostID(string(host)),
		unit:     strings.TrimSpace(unit),
		keys:     keys,
		expanded: make(map[depRef]bool),
	}
}

// Init loads the dependency trees in a tea.Cmd.
func (d *unitDeps) Init() tea.Cmd {
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), depsLoadTimeout)
	d.cancel = cancel
	d.seq++
	seq := d.seq
	d.deps, d.err, d.done = linux.Dependencies{}, nil, false

	b, host, unit := d.b, d.host, d.unit
	return func() tea.Msg {
		depser, ok := b.(unitDepser)
		if !ok {
			return unitDepsMsg{Seq: seq, Err: errs.New(errs.Config, "views.unitDeps", "this backend has no dependency tree")}
		}
		deps, err := depser.UnitDependencies(ctx, host, unit)
		return unitDepsMsg{Seq: seq, Deps: deps, Err: err}
	}
}

// style returns the theme, falling back to the default palette.
func (d *unitDeps) style() *theme.Theme {
	if d.env != nil && d.env.Theme != nil {
		return d.env.Theme
	}
	return theme.Dark()
}

// Update handles the batch and the tree's keys.
func (d *unitDeps) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	if m, ok := msg.(unitDepsMsg); ok {
		if m.Seq != d.seq {
			return d, nil
		}
		d.deps, d.err, d.done = m.Deps, m.Err, true
		if d.cancel != nil {
			d.cancel()
			d.cancel = nil
		}
		d.rebuild()
		return d, nil
	}
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return d, nil
	}
	return d, d.key(k)
}

// key handles movement, expansion and drill-in.
func (d *unitDeps) key(m tea.KeyMsg) tea.Cmd {
	key := m.String()
	switch {
	case d.keys.Up.Matches(key):
		d.move(-1)
	case d.keys.Down.Matches(key):
		d.move(1)
	case d.keys.PageUp.Matches(key):
		d.move(-d.pageSize())
	case d.keys.PageDown.Matches(key):
		d.move(d.pageSize())
	case d.keys.Top.Matches(key):
		d.moveTo(0, 1)
	case d.keys.Bottom.Matches(key):
		d.moveTo(len(d.rows)-1, -1)
	case d.keys.Open.Matches(key):
		return d.open()
	case key == " " || key == "space":
		d.toggle()
	}
	return nil
}

// move steps the cursor by delta rows, skipping headers and notes.
func (d *unitDeps) move(delta int) {
	if delta == 0 {
		return
	}
	step := 1
	if delta < 0 {
		step = -1
	}
	i := d.cursor
	for n := 0; n < abs(delta); n++ {
		j := i + step
		for j >= 0 && j < len(d.rows) && d.rows[j].title != "" {
			j += step
		}
		if j < 0 || j >= len(d.rows) {
			return
		}
		i = j
	}
	d.cursor = i
}

// moveTo moves the cursor from start towards the end looking for a node.
func (d *unitDeps) moveTo(start, step int) {
	for j := start; j >= 0 && j < len(d.rows); j += step {
		if d.rows[j].title == "" {
			d.cursor = j
			return
		}
	}
}

// toggle expands or collapses the selected node's children.
func (d *unitDeps) toggle() {
	row, ok := d.selected()
	if !ok {
		return
	}
	d.expanded[row.ref] = !d.expanded[row.ref]
	d.rebuild()
}

// open pushes the selected dependency's own unit detail.
func (d *unitDeps) open() tea.Cmd {
	row, ok := d.selected()
	if !ok {
		return nil
	}
	return tui.OpenQuery(resource.Query{
		Kind:  linux.KindUnitDetail,
		Hosts: []resource.HostID{d.host},
		Args:  map[string]string{"unit": row.node.Unit},
	})
}

// selected is the node row under the cursor.
func (d *unitDeps) selected() (depRow, bool) {
	if d.cursor < 0 || d.cursor >= len(d.rows) {
		return depRow{}, false
	}
	row := d.rows[cursorClamp(d.cursor, len(d.rows))]
	if row.title != "" {
		return depRow{}, false
	}
	return row, true
}

// pageSize is how many rows a page key moves.
func (d *unitDeps) pageSize() int {
	if d.h <= 3 {
		return 1
	}
	return d.h - 3
}

// rebuild recomputes the visible rows after a load or an expansion change and
// keeps the cursor on a node.
func (d *unitDeps) rebuild() {
	prev, hadPrev := d.selected()
	d.rows = d.visibleRows()
	if hadPrev {
		for i, row := range d.rows {
			if row.title == "" && row.ref == prev.ref {
				d.cursor = i
				return
			}
		}
	}
	for i, row := range d.rows {
		if row.title == "" {
			d.cursor = i
			return
		}
	}
	d.cursor = 0
}

// visibleRows is the tree as it is shown: section headers, the nodes down to
// depth 1, and deeper nodes only under an expanded parent.
func (d *unitDeps) visibleRows() []depRow {
	rows := make([]depRow, 0, len(d.deps.Forward)+len(d.deps.Reverse)+4)
	rows = append(rows, d.sectionRows(0, depsForwardTitle, d.deps.Forward)...)
	rows = append(rows, d.sectionRows(1, depsReverseTitle, d.deps.Reverse)...)
	return rows
}

// sectionRows renders one section, hiding every node at or below
// depsCollapseDepth unless its parent was expanded.
func (d *unitDeps) sectionRows(section int, title string, nodes []linux.DepNode) []depRow {
	rows := []depRow{{title: title}}
	if len(nodes) == 0 {
		rows = append(rows, depRow{title: "  " + depsEmptyValue, note: true})
		return rows
	}
	shown := make(map[int]bool, len(nodes))
	parentAt := make(map[int]int, depsCollapseDepth+2)
	for i, node := range nodes {
		depth := node.Depth
		if depth < 0 {
			depth = 0
		}
		parentAt[depth] = i
		show := depth < depsCollapseDepth
		if !show {
			parent, ok := parentAt[depth-1]
			show = ok && shown[parent] && d.expanded[depRef{section: section, index: parent}]
		}
		shown[i] = show
		if !show {
			continue
		}
		rows = append(rows, depRow{node: node, ref: depRef{section: section, index: i}})
	}
	return rows
}

// Render returns exactly h lines of exactly w cells.
func (d *unitDeps) Render(w, h int) string {
	d.w, d.h = w, h
	if w <= 0 || h <= 0 {
		return ""
	}
	lines := d.lines(w)
	d.scroll()
	visible := lines[d.offset:]
	return render.Fit(strings.Join(visible, "\n"), w, h)
}

// lines is the whole tree as styled lines, cursor included.
func (d *unitDeps) lines(w int) []string {
	st := d.style()
	out := make([]string, 0, len(d.rows)+1)
	out = append(out, st.Accent.Render(render.Truncate(d.unit+" deps", w)))
	switch {
	case d.err != nil:
		out = append(out, st.Crit.Render(render.Truncate(d.err.Error(), w)))
	case !d.done:
		out = append(out, st.Faint.Render("loading dependencies…"))
	}
	for i, row := range d.rows {
		text := d.rowText(row)
		switch {
		case row.note:
			out = append(out, st.Faint.Render(render.Truncate(text, w)))
		case row.title != "":
			out = append(out, st.Dim.Render(render.Truncate("── "+text+" ──", w)))
		case i == d.cursor:
			out = append(out, st.Selected.Render(render.PadRight(render.Truncate(text, w), w)))
		default:
			out = append(out, d.nodeStyle(row).Render(render.Truncate(text, w)))
		}
	}
	return out
}

// rowText is the plain text of one row.
func (d *unitDeps) rowText(row depRow) string {
	switch {
	case row.title != "":
		return row.title
	}
	depth := row.node.Depth
	if depth < 0 {
		depth = 0
	}
	// Only nodes below the root carry an expansion marker: the root's own
	// children are always shown.
	marker := "  "
	if depth > 0 && d.hasChildren(row) {
		if d.expanded[row.ref] {
			marker = "▾ "
		} else {
			marker = "▸ "
		}
	}
	return strings.Repeat("  ", depth) + marker + row.node.Unit
}

// hasChildren reports whether a node has a visible-by-expansion child.
func (d *unitDeps) hasChildren(row depRow) bool {
	nodes := d.deps.Forward
	if row.ref.section == 1 {
		nodes = d.deps.Reverse
	}
	if row.ref.index+1 >= len(nodes) {
		return false
	}
	return nodes[row.ref.index+1].Depth > row.node.Depth
}

// nodeStyle dims the deeper levels so the top of the tree stands out.
func (d *unitDeps) nodeStyle(row depRow) lipgloss.Style {
	st := d.style()
	if row.node.Depth == 0 {
		return st.Bold
	}
	if row.node.Depth >= depsCollapseDepth {
		return st.Dim
	}
	return st.Base
}

// scroll keeps the cursor inside the visible window.
func (d *unitDeps) scroll() {
	if d.h <= 0 {
		return
	}
	if d.cursor < d.offset {
		d.offset = d.cursor
	}
	if d.cursor >= d.offset+d.h {
		d.offset = d.cursor - d.h + 1
	}
	if d.offset < 0 {
		d.offset = 0
	}
}

// Close cancels the batch.
func (d *unitDeps) Close() error {
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	return nil
}

// Crumb is the tab's breadcrumb.
func (d *unitDeps) Crumb() string { return d.unit + " deps" }

// Hints are the deps tab's own keys.
func (d *unitDeps) Hints() []tui.KeyHint {
	return []tui.KeyHint{
		{Key: firstKey(d.keys.Open), Text: "detail"},
		{Key: "space", Text: "expand"},
		{Key: firstKey(d.keys.Top), Text: "top"},
	}
}

// Subject is this unit on this host.
func (d *unitDeps) Subject() tui.Subject {
	return tui.Subject{
		Hosts: []resource.HostID{d.host},
		Unit:  d.unit,
		Kind:  linux.KindUnitDetail,
		Ref:   resource.Ref{Backend: "linux", Kind: linux.KindUnitDetail, Host: d.host, Name: d.unit},
	}
}

// Capturing is false: the tab never owns the keyboard as a whole.
func (d *unitDeps) Capturing() bool { return false }

// CopyLines is the plain text of the tree.
func (d *unitDeps) CopyLines() []string {
	out := make([]string, 0, len(d.rows))
	for _, row := range d.rows {
		out = append(out, d.rowText(row))
	}
	return out
}

// cursorClamp keeps an index inside a slice of length n.
func cursorClamp(i, n int) int {
	if i < 0 {
		return 0
	}
	if i >= n {
		return n - 1
	}
	return i
}

// abs is the absolute value of an int, without math.
func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
