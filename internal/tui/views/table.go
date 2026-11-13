// Package views holds the concrete screens every pane can show. The generic
// Table is the fallback for kinds that have no dedicated view: it streams any
// backend list, sorts and filters it, and renders only the visible window.
package views

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend"
	"jumper/internal/errs"
	"jumper/internal/fuzzy"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/targets"
	"jumper/internal/tui"
	"jumper/internal/tui/keymap"
)

// hostErr is one failed batch of a streaming List.
type hostErr struct {
	Host resource.HostID
	err  error
}

// Table is the k9s-style list for one kind. It is a tui.View; all remote work
// happens in tea.Cmd goroutines and Update never blocks.
type Table struct {
	env  *tui.Env
	b    backend.Backend
	spec resource.KindSpec
	q    resource.Query
	keys *keymap.Map

	// base is the pane's context; every listing derives from it, so closing
	// the pane stops the fan-out. Nil means context.Background.
	base context.Context

	// stream state
	seq     int
	cancel  context.CancelFunc
	ctx     context.Context // the listing's context; the watch derives from it
	ch      <-chan backend.Result
	started time.Time
	elapsed time.Duration
	done    bool
	doneN   int // units finished, as reported by the backend
	total   int // units expected; 0 = unknown

	// watch state: live events of a Watchable kind after the listing closed
	wseq     int
	wat      <-chan backend.Event
	watching bool

	rows []resource.Resource // every row received, in arrival order
	errs []hostErr           // per-host failures

	// view state (kept in sync with rows and filter; Render never sorts)
	view   []resource.Resource
	cursor int

	// marks are the hosts marked with space, * or cleared with U. Keyed by
	// host identity, so a mark survives a re-sort or a filter change.
	marks map[resource.HostID]bool

	sortID   string
	sortDesc bool

	filter    string
	filtering bool
	fuzzy     bool // filter text is a fuzzy subsequence, not a substring
	filterRe  *regexp.Regexp
	filterBad bool

	w, h int

	// glyphs is the active glyph set: every rune the view draws comes from it.
	glyphs render.GlyphSet

	// header cache: the column layout only changes with the width
	hdrWidth  int
	hdrCols   []render.TableColumn
	hdrWidths []int
	hdr       string

	// banner cache: recomputed only when the summary inputs change
	bnKey    bannerKey
	bnText   string
	bncached bool

	staleN int
}

// bannerKey identifies one banner state.
type bannerKey struct {
	done     bool
	doneN    int
	total    int
	rows     int
	errs     int
	filter   string
	fuzzy    bool
	elapsed  time.Duration
	stale    int
	watching bool
}

var _ tui.View = (*Table)(nil)

// NewTable builds the table view for one backend and query. The glyph set is
// the active one from env (resolved once at startup); WithGlyphs overrides it.
func NewTable(env *tui.Env, b backend.Backend, spec resource.KindSpec, q resource.Query) *Table {
	keys := keymap.Default()
	if env != nil && env.Keys != nil {
		keys = env.Keys
	}
	sortID, desc := sortSpec(spec.DefaultSort)
	glyphs, _ := render.Glyphs("unicode")
	if env != nil && env.Glyphs.Name != "" {
		glyphs = env.Glyphs
	}
	return &Table{
		env:      env,
		b:        b,
		spec:     spec,
		q:        q,
		keys:     keys,
		sortID:   sortID,
		sortDesc: desc,
		filterRe: nil,
		glyphs:   glyphs,
	}
}

// WithGlyphs sets the glyph set this table renders with (from ui.glyphs,
// resolved once at startup) and returns the table for chaining.
func (t *Table) WithGlyphs(set render.GlyphSet) *Table {
	if set.Name != "" {
		t.glyphs = set
		t.bncached = false
		t.hdrCols = nil
	}
	return t
}

// sortSpec splits a KindSpec.DefaultSort value ("-load") into column and
// direction.
func sortSpec(s string) (string, bool) {
	if rest, ok := strings.CutPrefix(s, "-"); ok {
		return rest, true
	}
	return s, false
}

// Init starts the first listing.
func (t *Table) Init() tea.Cmd { return t.restart() }

// SetContext makes every listing derive from ctx, so cancelling the pane's
// context stops the stream. The view pane calls it before Init.
func (t *Table) SetContext(ctx context.Context) {
	if ctx != nil {
		t.base = ctx
	}
}

// now returns the clock the environment handed us.
func (t *Table) now() time.Time {
	if t.env != nil && t.env.Now != nil {
		return t.env.Now()
	}
	return time.Now()
}

// restart cancels any running stream, bumps Seq (so late batches are ignored),
// clears the rows and starts a fresh List.
func (t *Table) restart() tea.Cmd {
	if t.cancel != nil {
		t.cancel()
	}
	parent := t.base
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	t.cancel = cancel
	t.ctx = ctx
	t.seq++
	t.wseq++
	t.ch = nil
	t.wat = nil
	t.watching = false
	t.rows = nil
	t.errs = nil
	t.view = nil
	t.done = false
	t.doneN = 0
	t.total = 0
	t.cursor = 0
	t.elapsed = 0
	t.staleN = 0
	t.started = t.now()

	ch, err := t.b.List(ctx, t.q)
	if err != nil {
		t.done = true
		t.elapsed = t.now().Sub(t.started)
		return tui.Flash(tui.FlashError, "%v", err)
	}
	t.ch = ch
	return tui.ListBatch(t.seq, ch)
}

// Update handles stream batches and keys. Stale batches (another Seq) are
// ignored entirely.
func (t *Table) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	switch m := msg.(type) {
	case tui.ListBatchMsg:
		if m.Seq != t.seq {
			return t, nil
		}
		return t, t.applyBatch(m)
	case watchBatchMsg:
		if m.seq != t.wseq {
			return t, nil
		}
		return t, t.applyEvents(m)
	case tui.MarkedTargetsRequestMsg:
		return t, t.markedTargetsCmd()
	case tui.RefreshRequestMsg:
		return t, t.restart()
	case tea.KeyMsg:
		return t.handleKey(m)
	}
	return t, nil
}

// applyBatch appends one batch, keeps the cursor on the same Ref and asks for
// the next batch unless the stream is closed.
func (t *Table) applyBatch(m tui.ListBatchMsg) tea.Cmd {
	ref, hadRef := t.currentRef()

	for _, r := range m.Results {
		if r.Done > t.doneN {
			t.doneN = r.Done
		}
		if r.Total > t.total {
			t.total = r.Total
		}
		if r.Err != nil {
			t.errs = append(t.errs, hostErr{Host: r.Host, err: r.Err})
			continue
		}
		t.rows = append(t.rows, r.Items...)
	}

	t.refilter()
	t.staleN = staleCount(t.rows)
	if hadRef {
		t.cursorOn(ref)
	}

	if m.Closed {
		t.done = true
		t.elapsed = t.now().Sub(t.started)
		return t.startWatch()
	}
	return tui.ListBatch(t.seq, t.ch)
}

// watchBatchMax is the most events one watch message carries.
const watchBatchMax = 200

// watchBatchMsg is one batch of live events for one listing. It is local to
// the view: nothing outside this package sends or receives it.
type watchBatchMsg struct {
	seq    int
	events []backend.Event
	closed bool
}

// watchBatch reads the next batch of events from ch, or reports the channel
// closed. It is the watch twin of tui.ListBatch.
func watchBatch(seq int, ch <-chan backend.Event) tea.Cmd {
	return func() tea.Msg {
		events, closed := tui.NextBatch(ch, watchBatchMax)
		return watchBatchMsg{seq: seq, events: events, closed: closed}
	}
}

// startWatch subscribes to the live events of a Watchable kind once the
// listing has closed. A backend that cannot watch, or a kind that is not
// watchable, simply leaves the table as it is.
func (t *Table) startWatch() tea.Cmd {
	w, ok := t.b.(backend.Watcher)
	if !ok || !t.spec.Watchable || t.ctx == nil {
		return nil
	}
	ch, err := w.Watch(t.ctx, t.q)
	if err != nil {
		return tui.Flash(tui.FlashWarn, "watch: %v", err)
	}
	t.wat = ch
	t.wseq++
	return watchBatch(t.wseq, ch)
}

// applyEvents folds a batch of live events into the rows: an add or a modify
// replaces the row with the same Ref, a delete drops it. The cursor stays on
// the resource it was on.
func (t *Table) applyEvents(m watchBatchMsg) tea.Cmd {
	ref, hadRef := t.currentRef()
	t.watching = true
	for _, ev := range m.events {
		t.applyEvent(ev)
	}
	t.refilter()
	t.staleN = staleCount(t.rows)
	if hadRef {
		t.cursorOn(ref)
	}
	if m.closed {
		t.watching = false
		return nil
	}
	return watchBatch(t.wseq, t.wat)
}

// applyEvent is one add, modify or delete.
func (t *Table) applyEvent(ev backend.Event) {
	switch ev.Type {
	case backend.EventDeleted:
		for i, r := range t.rows {
			if r.Ref == ev.Resource.Ref {
				t.rows = append(t.rows[:i], t.rows[i+1:]...)
				return
			}
		}
	case backend.EventAdded, backend.EventModified:
		for i, r := range t.rows {
			if r.Ref == ev.Resource.Ref {
				t.rows[i] = ev.Resource
				return
			}
		}
		t.rows = append(t.rows, ev.Resource)
	}
}

// currentRef returns the Ref under the cursor, if there is one.
func (t *Table) currentRef() (resource.Ref, bool) {
	if t.cursor < 0 || t.cursor >= len(t.view) {
		return resource.Ref{}, false
	}
	return t.view[t.cursor].Ref, true
}

// cursorOn moves the cursor back onto ref, then clamps it.
func (t *Table) cursorOn(ref resource.Ref) {
	for i, r := range t.view {
		if r.Ref == ref {
			t.cursor = i
			return
		}
	}
	t.clampCursor()
}

// handleKey routes one key press. While the filter is capturing, every key
// belongs to the filter text (j, k, q, : are text too).
func (t *Table) handleKey(m tea.KeyMsg) (tui.View, tea.Cmd) {
	if t.filtering {
		return t, t.filterKey(m)
	}

	key := m.String()
	switch {
	case t.keys.FuzzyFind.Matches(key):
		t.fuzzy = true
		t.filtering = true
		t.setFilter("")
		t.refilter()
		return t, nil
	case t.keys.Filter.Matches(key):
		t.fuzzy = false
		t.filtering = true
		t.setFilter(t.filter)
		t.refilter()
		return t, nil
	case t.keys.Back.Matches(key):
		if t.filter != "" {
			t.fuzzy = false
			t.setFilter("")
			t.refilter()
			return t, nil
		}
		return t, tui.Pop()
	case t.keys.Up.Matches(key):
		t.moveCursor(-1)
		return t, nil
	case t.keys.Down.Matches(key):
		t.moveCursor(1)
		return t, nil
	case t.keys.PageUp.Matches(key):
		t.moveCursor(-t.pageSize())
		return t, nil
	case t.keys.PageDown.Matches(key):
		t.moveCursor(t.pageSize())
		return t, nil
	case t.keys.Top.Matches(key):
		t.cursor = 0
		return t, nil
	case t.keys.Bottom.Matches(key):
		if len(t.view) > 0 {
			t.cursor = len(t.view) - 1
		}
		return t, nil
	case t.keys.Refresh.Matches(key):
		return t, t.restart()
	case t.keys.Open.Matches(key):
		return t, t.open()
	case t.keys.Errors.Matches(key):
		if len(t.errs) == 0 {
			return t, tui.Flash(tui.FlashInfo, "no errors")
		}
		return t, tui.Flash(tui.FlashError, "%v", t.errs[0].err)
	case t.keys.Finding.Matches(key):
		return t, t.finding()
	case t.keys.Actions.Matches(key):
		return t, t.actionsCmd()
	}

	if link, ok := t.linkForKey(key); ok {
		if refuser, ok := t.b.(linkRefuser); ok {
			// A pivot that cannot be taken (a node the inventory does not
			// know, task 111) explains itself instead of opening a view that
			// would fail to dial.
			if msg := refuser.RefuseLink(t.view[t.cursor], key); msg != "" {
				return t, tui.Flash(tui.FlashWarn, "%s", msg)
			}
		}
		return t, tui.OpenQuery(link.To)
	}
	switch key {
	case " ", "space":
		t.toggleMark()
		return t, nil
	case "*":
		t.markAll()
		return t, nil
	case "U":
		t.clearMarks()
		return t, nil
	case "s":
		t.nextSortColumn()
		return t, nil
	case "S":
		ref, had := t.currentRef()
		t.sortDesc = !t.sortDesc
		t.refilter()
		if had {
			t.cursorOn(ref)
		}
		return t, nil
	}
	return t, nil
}

// filterKey edits the filter text. Enter keeps it, Esc clears it.
func (t *Table) filterKey(m tea.KeyMsg) tea.Cmd {
	switch m.Type {
	case tea.KeyEnter:
		t.filtering = false
		return nil
	case tea.KeyEsc:
		t.filtering = false
		t.fuzzy = false
		if t.filter != "" {
			t.setFilter("")
			t.refilter()
		}
		return nil
	case tea.KeyBackspace:
		if r := []rune(t.filter); len(r) > 0 {
			t.setFilter(string(r[:len(r)-1]))
			t.refilter()
		}
		return nil
	case tea.KeyCtrlU:
		t.setFilter("")
		t.refilter()
		return nil
	case tea.KeyRunes:
		t.setFilter(t.filter + string(m.Runes))
		t.refilter()
		return nil
	}
	return nil
}

// finding records the selected row as an investigation finding: the row's text
// is what the user was looking at when they pressed F.
func (t *Table) finding() tea.Cmd {
	if t.cursor < 0 || t.cursor >= len(t.view) {
		return tui.Flash(tui.FlashInfo, "no rows")
	}
	return t.env.RecordFinding(t.rowText(t.view[t.cursor]))
}

// open drills into the selected row through its first link, or explains why
// there is nothing to open.
func (t *Table) open() tea.Cmd {
	if t.cursor < 0 || t.cursor >= len(t.view) {
		return tui.Flash(tui.FlashInfo, "no rows")
	}
	links := t.b.Links(t.view[t.cursor])
	if len(links) == 0 {
		return tui.Flash(tui.FlashInfo, "no drill-in for %s", t.spec.Name)
	}
	return tui.OpenQuery(links[0].To)
}

// linkRefuser is a backend that explains why a row's pivot cannot be taken.
// The table flashes the text instead of opening the link's (empty) query.
type linkRefuser interface {
	RefuseLink(r resource.Resource, key string) string
}

// linkForKey finds the link of the selected row bound to key.
func (t *Table) linkForKey(key string) (resource.Link, bool) {
	if t.cursor < 0 || t.cursor >= len(t.view) {
		return resource.Link{}, false
	}
	for _, l := range t.b.Links(t.view[t.cursor]) {
		if l.Key != "" && l.Key == key {
			return l, true
		}
	}
	return resource.Link{}, false
}

// MarkedHosts returns the marked hosts, sorted and de-duplicated. Rows without
// a host identity (global kinds such as alerts) cannot be marked.
func (t *Table) MarkedHosts() []resource.HostID {
	if len(t.marks) == 0 {
		return nil
	}
	out := make([]resource.HostID, 0, len(t.marks))
	for h, on := range t.marks {
		if on && h != "" {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	if len(out) == 0 {
		return nil
	}
	return out
}

// toggleMark marks or unmarks the row under the cursor.
func (t *Table) toggleMark() {
	if t.cursor < 0 || t.cursor >= len(t.view) {
		return
	}
	host := hostOf(t.view[t.cursor])
	if host == "" {
		return
	}
	if t.marks == nil {
		t.marks = make(map[resource.HostID]bool)
	}
	if t.marks[host] {
		delete(t.marks, host)
		return
	}
	t.marks[host] = true
}

// markAll marks every host of the rows the filter currently shows.
func (t *Table) markAll() {
	if t.marks == nil {
		t.marks = make(map[resource.HostID]bool)
	}
	for _, r := range t.view {
		if host := hostOf(r); host != "" {
			t.marks[host] = true
		}
	}
}

// clearMarks drops every mark.
func (t *Table) clearMarks() { t.marks = nil }

// markedTargetsCmd answers a request for the marked rows: the marks become the
// session target set, or the user is told there are none.
func (t *Table) markedTargetsCmd() tea.Cmd {
	hosts := t.MarkedHosts()
	if len(hosts) == 0 {
		return tui.Flash(tui.FlashInfo, "no marked rows")
	}
	set := targets.FromHosts(fmt.Sprintf("%d marked", len(hosts)), hosts)
	return func() tea.Msg { return tui.TargetsChangedMsg{Set: set} }
}

// actionsCmd asks the root for the actions menu over the marked rows, or over
// the row under the cursor when nothing is marked (task 061).
func (t *Table) actionsCmd() tea.Cmd {
	sel := t.selection()
	if len(sel) == 0 {
		return tui.Flash(tui.FlashInfo, "no row selected")
	}
	kind := t.spec.Name
	return func() tea.Msg {
		return tui.ActionsRequestMsg{Kind: kind, Selection: sel}
	}
}

// selection is what the actions menu acts on: every marked row, in list order,
// else the row under the cursor.
func (t *Table) selection() []resource.Resource {
	if len(t.marks) == 0 {
		if t.cursor < 0 || t.cursor >= len(t.view) {
			return nil
		}
		return []resource.Resource{t.view[t.cursor]}
	}
	out := make([]resource.Resource, 0, len(t.marks))
	seen := make(map[resource.HostID]bool, len(t.marks))
	for _, row := range t.rows {
		host := hostOf(row)
		if host == "" || !t.marks[host] || seen[host] {
			continue
		}
		seen[host] = true
		out = append(out, row)
	}
	return out
}

// markGlyph is the one-cell mark in the row gutter: "▸" for the unicode and
// nerd sets, ">" for the ascii set, whose frame holds no rune above 0x7f.
func (t *Table) markGlyph() rune {
	if t.glyphs.ASCII() {
		return '>'
	}
	return '▸'
}

// markCell is the row gutter: the mark glyph for a marked host, a space
// otherwise.
func (t *Table) markCell(row resource.Resource) string {
	if host := hostOf(row); host != "" && t.marks[host] {
		return string(t.markGlyph())
	}
	return " "
}

// nextSortColumn advances to the next column and sorts ascending, keeping the
// cursor on its resource.
func (t *Table) nextSortColumn() {
	if len(t.spec.Columns) == 0 {
		return
	}
	ref, had := t.currentRef()
	next := 0
	for i, c := range t.spec.Columns {
		if c.ID == t.sortID {
			next = (i + 1) % len(t.spec.Columns)
			break
		}
	}
	t.sortID = t.spec.Columns[next].ID
	t.sortDesc = false
	t.refilter()
	if had {
		t.cursorOn(ref)
	}
}

// moveCursor moves the cursor by delta and keeps it inside the view.
func (t *Table) moveCursor(delta int) {
	t.cursor += delta
	t.clampCursor()
}

// pageSize is the number of rows one page key moves.
func (t *Table) pageSize() int {
	if n := t.h - 2; n > 1 {
		return n
	}
	return 10
}

// clampCursor keeps the cursor inside [0, len(view)).
func (t *Table) clampCursor() {
	if len(t.view) == 0 {
		t.cursor = 0
		return
	}
	if t.cursor < 0 {
		t.cursor = 0
	}
	if t.cursor >= len(t.view) {
		t.cursor = len(t.view) - 1
	}
}

// setFilter installs a new filter text. In fuzzy mode no regex is compiled; in
// substring mode a ~ prefix becomes a case-insensitive regex. An invalid regex
// matches nothing and is flagged.
func (t *Table) setFilter(s string) {
	t.filter = s
	t.filterBad = false
	t.filterRe = nil
	if t.fuzzy {
		return
	}
	if pat, ok := strings.CutPrefix(s, "~"); ok {
		re, err := regexp.Compile("(?i)" + pat)
		if err != nil {
			t.filterBad = true
			return
		}
		t.filterRe = re
	}
}

// matches reports whether row passes the current filter. Fuzzy mode is a
// subsequence match over the row's name and its label values; the substring
// form is case-insensitive over the row's rendered cells; ~ means a regex.
func (t *Table) matches(row resource.Resource) bool {
	if t.filter == "" {
		return true
	}
	if t.fuzzy {
		return t.fuzzyMatch(row)
	}
	if t.filterBad {
		return false
	}
	text := t.rowText(row)
	if t.filterRe != nil {
		return t.filterRe.MatchString(text)
	}
	return strings.Contains(strings.ToLower(text), strings.ToLower(t.filter))
}

// fuzzyMatch reports whether the filter is a fuzzy subsequence of the row's
// name or of any label value.
func (t *Table) fuzzyMatch(row resource.Resource) bool {
	if name := t.rowName(row); name != "" {
		if _, _, ok := fuzzy.Match(t.filter, name); ok {
			return true
		}
	}
	for _, v := range row.Labels {
		if _, _, ok := fuzzy.Match(t.filter, v); ok {
			return true
		}
	}
	return false
}

// rowName is the row's name column: its Ref name, else a "name" field, else
// the first column's text.
func (t *Table) rowName(row resource.Resource) string {
	if row.Ref.Name != "" {
		return row.Ref.Name
	}
	if _, ok := t.spec.Column("name"); ok {
		return row.Str("name")
	}
	if len(t.spec.Columns) > 0 {
		return cellTextWith(t.glyphs, t.spec.Columns[0], row, t.now(), 0)
	}
	return ""
}

// rowText joins every rendered cell of row into one searchable string.
func (t *Table) rowText(row resource.Resource) string {
	now := t.now()
	parts := make([]string, len(t.spec.Columns))
	for i, c := range t.spec.Columns {
		parts[i] = cellTextWith(t.glyphs, c, row, now, c.Width)
	}
	return strings.Join(parts, " ")
}

// refilter rebuilds the visible slice from rows, then re-sorts and clamps the
// cursor. It runs on data and filter changes, never on Render.
func (t *Table) refilter() {
	out := make([]resource.Resource, 0, len(t.rows))
	for _, r := range t.rows {
		if t.matches(r) {
			out = append(out, r)
		}
	}
	if t.sortID != "" {
		id, desc := t.sortID, t.sortDesc
		sort.SliceStable(out, func(i, j int) bool {
			c := resource.Compare(out[i].Fields[id], out[j].Fields[id])
			if desc {
				return c > 0
			}
			return c < 0
		})
	}
	t.view = out
	t.clampCursor()
}

// columns converts the kind's columns into render.TableColumns and computes
// their widths for the given total.
func (t *Table) columns(total int) ([]render.TableColumn, []int) {
	cols := make([]render.TableColumn, len(t.spec.Columns))
	for i, c := range t.spec.Columns {
		cols[i] = render.TableColumn{
			Title:      c.Title,
			Width:      c.Width,
			AlignRight: c.Align == resource.AlignRight,
		}
	}
	return cols, render.ColumnWidths(cols, total)
}

// header returns the column layout for width w and caches the rendered header
// line, so Render does no layout work when only the data changed. One cell is
// reserved on the left for the mark gutter.
func (t *Table) header(w int) ([]render.TableColumn, []int) {
	if t.hdrCols != nil && t.hdrWidth == w {
		return t.hdrCols, t.hdrWidths
	}
	cols, widths := t.columns(max(w-1, 0))
	t.hdrCols = cols
	t.hdrWidths = widths
	t.hdrWidth = w
	t.hdr = t.env.Theme.Header.Render(" " + render.HeaderWith(t.glyphs, cols, widths))
	return cols, widths
}

// rowLine renders one data row, styled by its status (and inverted when
// selected). gutter is the one-cell mark column in front of it.
func (t *Table) rowLine(row resource.Resource, gutter string, cols []render.TableColumn, widths []int, selected bool) string {
	theme := t.env.Theme
	status := theme.StatusStyle(row.Status)
	cells := make([]render.Cell, len(t.spec.Columns))
	now := t.now()
	for i, c := range t.spec.Columns {
		text := cellTextWith(t.glyphs, c, row, now, widths[i])
		if row.Fields[c.ID] == nil && c.Kind != resource.ColStatus {
			faint := theme.Faint
			cells[i] = render.Cell{Text: text, Style: func(s string) string { return faint.Render(s) }}
			continue
		}
		cells[i] = render.Cell{Text: text, Style: func(s string) string { return status.Render(s) }}
	}
	line := gutter + render.RowWith(t.glyphs, cells, widths, cols)
	if selected {
		line = theme.Selected.Render(line)
	}
	return line
}

// errorLine renders a failed host at the bottom of the table. The host sits in
// the name column and the error kind starts at the first non-name column,
// spilling into the blank remainder so a narrow column never cuts it. The row
// is styled Crit, and sits behind the same one-cell mark gutter as the data.
func (t *Table) errorLine(he hostErr, widths []int) string {
	crit := t.env.Theme.Crit
	if len(widths) == 0 {
		return ""
	}
	kind := string(errs.KindOf(he.err))
	line := render.PadRight(string(he.Host), widths[0])
	if len(widths) == 1 {
		line = render.TruncateWith(t.glyphs, render.PadRight(line+" "+kind, widths[0]), widths[0])
	} else {
		line += " " + kind
	}
	return crit.Render(" " + line)
}

// banner is the one-line summary above the header. It is cached: Render runs
// many times per second and the summary only changes when data arrives.
func (t *Table) banner() string {
	key := bannerKey{
		done:     t.done,
		doneN:    t.doneN,
		total:    t.total,
		rows:     len(t.rows),
		errs:     len(t.errs),
		filter:   t.filter,
		fuzzy:    t.fuzzy,
		elapsed:  t.elapsed,
		stale:    t.staleN,
		watching: t.watching,
	}
	if key == t.bnKey && t.bncached {
		return t.bnText
	}

	var b strings.Builder
	sep := " " + string(t.glyphs.Separator) + " "
	dots := " " + string(t.glyphs.Ellipsis)
	if !t.done {
		if t.total > 0 {
			fmt.Fprintf(&b, "loading %s/%s%s", render.Int(int64(t.doneN)), render.Int(int64(t.total)), dots)
		} else {
			fmt.Fprintf(&b, "loading %s%s", render.Int(int64(len(t.rows))), dots)
		}
	} else {
		n := len(t.rows) + len(t.errs)
		fmt.Fprintf(&b, "%s %s%s%d %s%s%s",
			render.Int(int64(n)), plural(t.spec.Name), sep, len(t.errs), pluralWord(len(t.errs), "error"),
			sep, seconds(t.elapsed))
		if key.stale > 0 {
			b.WriteString(sep + "stale")
		}
		if key.watching {
			b.WriteString(sep + "live")
		}
	}
	if t.filter != "" {
		label := "/filter: "
		if t.fuzzy {
			label = "fuzzy: "
		}
		b.WriteString(sep + label)
		b.WriteString(t.filter)
	}
	t.bnKey = key
	t.bncached = true
	t.bnText = t.env.Theme.Dim.Render(b.String())
	return t.bnText
}

// staleCount counts rows the backend served from cache or a failed refresh.
func staleCount(rows []resource.Resource) int {
	n := 0
	for _, r := range rows {
		if r.Stale {
			n++
		}
	}
	return n
}

// plural names a kind: host → hosts.
func plural(kind resource.KindName) string {
	s := string(kind)
	if s == "" || strings.HasSuffix(s, "s") {
		return s
	}
	return s + "s"
}

// pluralWord returns the singular form for exactly one.
func pluralWord(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// seconds renders an elapsed time with one decimal: 2.1s.
func seconds(d time.Duration) string {
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// Render returns exactly h lines of exactly w cells: the banner, the header
// and the visible window only.
func (t *Table) Render(w, h int) string {
	t.w, t.h = w, h
	if w <= 0 || h <= 0 {
		return ""
	}

	lines := make([]string, 0, h)
	lines = append(lines, t.banner())
	if h > 1 {
		cols, widths := t.header(w)
		lines = append(lines, t.hdr)

		visible := h - 2
		if visible > 0 {
			errShow := len(t.errs)
			if errShow > visible-1 {
				errShow = visible - 1
			}
			if errShow < 0 {
				errShow = 0
			}
			dataLines := visible - errShow
			if dataLines > 0 {
				start := 0
				if t.cursor >= dataLines {
					start = t.cursor - dataLines + 1
				}
				for i := start; i < start+dataLines && i < len(t.view); i++ {
					lines = append(lines, t.rowLine(t.view[i], t.markCell(t.view[i]), cols, widths, i == t.cursor))
				}
			}
			for i := 0; i < errShow && i < len(t.errs); i++ {
				lines = append(lines, t.errorLine(t.errs[i], widths))
			}
		}
	}
	return render.FitWith(t.glyphs, strings.Join(lines, "\n"), w, h)
}

// Crumb is "<kind> (<n>)", with the selector when the query has one.
func (t *Table) Crumb() string {
	kind := string(t.spec.Name)
	if !t.q.Selector.IsZero() {
		return fmt.Sprintf("%s %s (%d)", kind, t.q.Selector.String(), len(t.view))
	}
	return fmt.Sprintf("%s (%d)", kind, len(t.view))
}

// CommandLine is the `:` command that opens this table again:
// "<kind> <selector>", then "--host a,b" and one sorted "--arg k=v" per arg.
func (t *Table) CommandLine() string {
	parts := []string{string(t.spec.Name)}
	if !t.q.Selector.IsZero() {
		parts = append(parts, t.q.Selector.String())
	}
	line := strings.Join(parts, " ")
	if len(t.q.Hosts) > 0 {
		hosts := make([]string, len(t.q.Hosts))
		for i, h := range t.q.Hosts {
			hosts[i] = string(h)
		}
		line += " --host " + strings.Join(hosts, ",")
	}
	if len(t.q.Args) > 0 {
		keys := make([]string, 0, len(t.q.Args))
		for k := range t.q.Args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			line += " --arg " + k + "=" + t.q.Args[k]
		}
	}
	return line
}

// FilterState is the filter text, "" when there is none. The view pane shows it
// next to the breadcrumbs.
func (t *Table) FilterState() string { return t.filter }

// Hints lists open, the selected row's links, filter, refresh and back.
func (t *Table) Hints() []tui.KeyHint {
	hints := make([]tui.KeyHint, 0, 5)
	hints = append(hints, tui.KeyHint{Key: firstKey(t.keys.Open), Text: "open"})
	if t.cursor >= 0 && t.cursor < len(t.view) {
		for _, l := range t.b.Links(t.view[t.cursor]) {
			if l.Key != "" {
				hints = append(hints, tui.KeyHint{Key: l.Key, Text: l.Label})
			}
		}
	}
	hints = append(hints,
		tui.KeyHint{Key: firstKey(t.keys.Filter), Text: "filter"},
		tui.KeyHint{Key: firstKey(t.keys.FuzzyFind), Text: "fuzzy"},
		tui.KeyHint{Key: firstKey(t.keys.Refresh), Text: "refresh"},
		tui.KeyHint{Key: firstKey(t.keys.Back), Text: "back"},
	)
	return hints
}

// firstKey renders the first key of a binding for humans.
func firstKey(b keymap.Binding) string {
	if len(b.Keys) == 0 {
		return ""
	}
	return keymap.DisplayKey(b.Keys[0])
}

// Subject is the selected row's host, kind and ref.
func (t *Table) Subject() tui.Subject {
	subj := tui.Subject{Kind: t.spec.Name}
	if t.cursor < 0 || t.cursor >= len(t.view) {
		return subj
	}
	row := t.view[t.cursor]
	subj.Ref = row.Ref
	// An alert is known by its alertname, not by its fingerprint ref: the
	// knowledge panel and the investigation both ask for the name.
	if name := strings.TrimSpace(row.Labels["alertname"]); name != "" {
		subj.Alert = name
	}
	if host := hostOf(row); host != "" {
		subj.Hosts = []resource.HostID{host}
	}
	return subj
}

// hostOf derives the host identity of a row.
func hostOf(r resource.Resource) resource.HostID {
	if r.Ref.Host != "" {
		return r.Ref.Host
	}
	if v, ok := r.Fields["host"].(string); ok && v != "" {
		return resource.NormalizeHostID(v)
	}
	if r.Ref.Kind == "host" && r.Ref.Name != "" {
		return resource.NormalizeHostID(r.Ref.Name)
	}
	return ""
}

// Capturing is true while the filter owns the keyboard.
func (t *Table) Capturing() bool { return t.filtering }

// CopyLines returns the plain text of the visible list: a header line and one
// line per row, with no styling.
func (t *Table) CopyLines() []string {
	now := t.now()
	lines := make([]string, 0, len(t.view)+len(t.errs)+1)

	titles := make([]string, len(t.spec.Columns))
	for i, c := range t.spec.Columns {
		titles[i] = c.Title
	}
	lines = append(lines, strings.Join(titles, "  "))

	for _, r := range t.view {
		cells := make([]string, len(t.spec.Columns))
		for i, c := range t.spec.Columns {
			cells[i] = cellTextWith(t.glyphs, c, r, now, c.Width)
		}
		lines = append(lines, strings.Join(cells, "  "))
	}
	for _, he := range t.errs {
		lines = append(lines, string(he.Host)+"  "+string(errs.KindOf(he.err)))
	}
	return lines
}
