package views

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend"
	"jumper/internal/backend/linux"
	"jumper/internal/errs"
	"jumper/internal/investigation"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/tui"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
)

// journalMaxRows caps the rows a following journal keeps in memory.
const journalMaxRows = 20000

// journalFollowMax is the most entries one follow batch carries.
const journalFollowMax = 200

// journalFollower is the part of the Linux backend the journal view needs. It
// is an interface so the view stays testable without a transport.
type journalFollower interface {
	FollowJournal(ctx context.Context, host resource.HostID, o linux.JournalOptions) (<-chan linux.Entry, error)
}

// journalBatchMsg is one batch of followed entries, tagged with the follow
// stream's Seq so a canceled stream's late batch is ignored.
type journalBatchMsg struct {
	Seq     int
	Entries []linux.Entry
	Closed  bool
}

// Journal is the journal screen for one host or a set of hosts. It loads the
// session time window through backend.List, renders it like a table and can
// follow a single host live with `w`.
type Journal struct {
	env  *tui.Env
	b    backend.Backend
	spec resource.KindSpec
	q    resource.Query
	keys *keymap.Map

	// window stream state
	seq     int
	cancel  context.CancelFunc
	ch      <-chan backend.Result
	done    bool
	multi   bool
	maxPri  int
	rows    []resource.Resource
	errs    []hostErr
	view    []resource.Resource
	cursor  int
	started time.Time
	elapsed time.Duration

	// filter state
	filter    string
	filtering bool
	filterRe  *regexp.Regexp
	filterBad bool

	// follow state
	following    bool
	wantFollow   bool
	followSeq    int
	followCancel context.CancelFunc
	followCh     <-chan linux.Entry

	w, h int

	// header cache, keyed by width
	hdrWidth  int
	hdrCols   []render.TableColumn
	hdrWidths []int
	hdr       string
}

var _ tui.View = (*Journal)(nil)

// NewJournal builds the journal view for b and q. q.Window is the session
// window, q.Hosts the target hosts and q.Args["unit"]/["priority"] the journal
// arguments.
func NewJournal(env *tui.Env, b backend.Backend, q resource.Query) *Journal {
	keys := keymap.Default()
	if env != nil && env.Keys != nil {
		keys = env.Keys
	}
	j := &Journal{
		env:    env,
		b:      b,
		spec:   journalSpec(b, q.Kind),
		q:      q,
		keys:   keys,
		multi:  len(q.Hosts) > 1,
		maxPri: 7,
	}
	if v := strings.TrimSpace(q.Args["priority"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 7 {
			j.maxPri = n
		}
	}
	return j
}

// journalSpec finds the journal KindSpec of b, falling back to the canonical
// name when q carries an alias.
func journalSpec(b backend.Backend, kind resource.KindName) resource.KindSpec {
	if b != nil {
		for _, spec := range b.Kinds() {
			if spec.Name == kind || spec.Name == linux.KindJournal {
				return spec
			}
		}
	}
	return resource.KindSpec{Name: linux.KindJournal}
}

// Init starts the windowed listing.
func (j *Journal) Init() tea.Cmd { return j.restart() }

// now is the clock handed in by the environment.
func (j *Journal) now() time.Time {
	if j.env != nil && j.env.Now != nil {
		return j.env.Now()
	}
	return time.Now()
}

// unit is the unit argument the query carries, if any.
func (j *Journal) unit() string { return strings.TrimSpace(j.q.Args["unit"]) }

// restart cancels any running window stream and starts a fresh listing. A
// follow stream in progress is left to its caller.
func (j *Journal) restart() tea.Cmd {
	if j.cancel != nil {
		j.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	j.cancel = cancel

	j.seq++
	j.ch = nil
	j.rows = nil
	j.errs = nil
	j.view = nil
	j.done = false
	j.cursor = 0
	j.elapsed = 0
	j.started = j.now()

	ch, err := j.b.List(ctx, j.q)
	if err != nil {
		j.done = true
		j.elapsed = j.now().Sub(j.started)
		return tui.Flash(tui.FlashError, "%v", err)
	}
	j.ch = ch
	return tui.ListBatch(j.seq, ch)
}

// reload reloads the window, remembering whether follow was on so it can be
// resumed once the window is in.
func (j *Journal) reload() tea.Cmd {
	wasFollowing := j.following
	j.stopFollow()
	j.wantFollow = wasFollowing
	return j.restart()
}

// Update handles batches, keys and the session window. Stale batches from a
// canceled stream are ignored.
func (j *Journal) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	switch m := msg.(type) {
	case tui.ListBatchMsg:
		if m.Seq != j.seq {
			return j, nil
		}
		return j, j.applyBatch(m)
	case journalBatchMsg:
		if m.Seq != j.followSeq {
			return j, nil
		}
		return j, j.applyFollow(m)
	case tui.WindowChangedMsg:
		j.q.Window = m.Window
		return j, j.reload()
	case tea.KeyMsg:
		return j, j.handleKey(m)
	}
	return j, nil
}

// applyBatch appends one window batch, keeps the cursor at the last line while
// the window loads, and asks for the next batch until the stream closes.
func (j *Journal) applyBatch(m tui.ListBatchMsg) tea.Cmd {
	for _, r := range m.Results {
		if r.Err != nil {
			j.errs = append(j.errs, hostErr{Host: r.Host, err: r.Err})
			continue
		}
		j.rows = append(j.rows, r.Items...)
	}
	j.refilter()
	if !j.done {
		j.cursor = len(j.view) - 1
	}
	j.clampCursor()

	if m.Closed {
		j.done = true
		j.elapsed = j.now().Sub(j.started)
		cmd := j.recordLoaded()
		if j.wantFollow {
			j.wantFollow = false
			return tea.Batch(cmd, j.startFollow())
		}
		return cmd
	}
	return tui.ListBatch(j.seq, j.ch)
}

// finding records the selected journal line as an investigation finding.
func (j *Journal) finding() tea.Cmd {
	if j.cursor < 0 || j.cursor >= len(j.view) {
		return tui.Flash(tui.FlashInfo, "no lines")
	}
	return j.env.RecordFinding(j.rowText(j.view[j.cursor]))
}

// rowText is the plain text of one row: every rendered cell, untruncated.
func (j *Journal) rowText(row resource.Resource) string {
	specCols := j.specColumns()
	now := j.now()
	cells := make([]string, len(specCols))
	for i, c := range specCols {
		cells[i] = cellText(c, row, now, c.Width)
	}
	return strings.Join(cells, "  ")
}

// recordLoaded appends the automatic `journal` entry for a loaded window. Only
// the events of the task are recorded, never a refresh of a table.
func (j *Journal) recordLoaded() tea.Cmd {
	if j.env == nil || j.env.Recorder == nil {
		return nil
	}
	if _, open := j.env.Recorder.Current(); !open {
		return nil
	}
	unit := j.unit()
	if unit == "" {
		unit = "all units"
	}
	return j.env.AppendInvestigation(investigation.EntryJournal,
		fmt.Sprintf("%s on %s (%d lines in window)", unit, j.hostText(), len(j.rows)), "")
}

// hostText names what the journal is scoped to: one host, N hosts, or the
// session when the query carried none.
func (j *Journal) hostText() string {
	switch len(j.q.Hosts) {
	case 0:
		return "no host"
	case 1:
		return string(j.q.Hosts[0])
	}
	return fmt.Sprintf("%d hosts", len(j.q.Hosts))
}

// applyFollow appends one live batch. The cursor stays pinned to the end only
// when it was already there, so scrolling back is not disturbed.
func (j *Journal) applyFollow(m journalBatchMsg) tea.Cmd {
	prevEnd := j.cursor == len(j.view)-1
	host := j.followHost()
	for _, e := range m.Entries {
		j.rows = append(j.rows, e.Resource(host))
	}
	j.trimRows()
	j.refilter()
	if prevEnd && len(j.view) > 0 {
		j.cursor = len(j.view) - 1
	}
	j.clampCursor()

	if m.Closed {
		j.following = false
		return nil
	}
	return j.nextFollow()
}

// trimRows keeps at most journalMaxRows entries while following, dropping the
// oldest.
func (j *Journal) trimRows() {
	if len(j.rows) <= journalMaxRows {
		return
	}
	drop := len(j.rows) - journalMaxRows
	j.rows = append([]resource.Resource(nil), j.rows[drop:]...)
}

// handleKey routes one key press. While the filter is capturing every key is
// text.
func (j *Journal) handleKey(m tea.KeyMsg) tea.Cmd {
	if j.filtering {
		return j.filterKey(m)
	}

	key := m.String()
	switch {
	case j.keys.Filter.Matches(key):
		j.filtering = true
		return nil
	case j.keys.Back.Matches(key):
		if j.filter != "" {
			j.setFilter("")
			j.refilter()
			return nil
		}
		return tui.Pop()
	case j.keys.Follow.Matches(key):
		return j.toggleFollow()
	case j.keys.Up.Matches(key):
		j.moveCursor(-1)
		return nil
	case j.keys.Down.Matches(key):
		j.moveCursor(1)
		return nil
	case j.keys.PageUp.Matches(key):
		j.moveCursor(-j.pageSize())
		return nil
	case j.keys.PageDown.Matches(key):
		j.moveCursor(j.pageSize())
		return nil
	case j.keys.Top.Matches(key):
		j.cursor = 0
		return nil
	case j.keys.Bottom.Matches(key):
		if len(j.view) > 0 {
			j.cursor = len(j.view) - 1
		}
		return nil
	case j.keys.Refresh.Matches(key):
		return j.reload()
	case j.keys.Finding.Matches(key):
		return j.finding()
	}

	// 1–7 set the maximum priority and reload.
	if len(key) == 1 && key[0] >= '1' && key[0] <= '7' {
		j.maxPri = int(key[0] - '0')
		if j.q.Args == nil {
			j.q.Args = make(map[string]string, 1)
		}
		j.q.Args["priority"] = key
		return j.reload()
	}
	return nil
}

// filterKey edits the filter text. Enter keeps it, Esc clears it.
func (j *Journal) filterKey(m tea.KeyMsg) tea.Cmd {
	switch m.Type {
	case tea.KeyEnter:
		j.filtering = false
		return nil
	case tea.KeyEsc:
		j.filtering = false
		if j.filter != "" {
			j.setFilter("")
			j.refilter()
		}
		return nil
	case tea.KeyBackspace:
		if r := []rune(j.filter); len(r) > 0 {
			j.setFilter(string(r[:len(r)-1]))
			j.refilter()
		}
		return nil
	case tea.KeyCtrlU:
		j.setFilter("")
		j.refilter()
		return nil
	case tea.KeyRunes:
		j.setFilter(j.filter + string(m.Runes))
		j.refilter()
		return nil
	}
	return nil
}

// setFilter installs a new filter text; a "~" prefix makes it a regex.
func (j *Journal) setFilter(s string) {
	j.filter = s
	j.filterBad = false
	j.filterRe = nil
	if pat, ok := strings.CutPrefix(s, "~"); ok {
		re, err := regexp.Compile("(?i)" + pat)
		if err != nil {
			j.filterBad = true
			return
		}
		j.filterRe = re
	}
}

// matches reports whether row passes the filter. It looks at the source and the
// message only.
func (j *Journal) matches(row resource.Resource) bool {
	if j.filter == "" {
		return true
	}
	if j.filterBad {
		return false
	}
	text := row.Str("source") + " " + row.Str("message")
	if j.filterRe != nil {
		return j.filterRe.MatchString(text)
	}
	return strings.Contains(strings.ToLower(text), strings.ToLower(j.filter))
}

// refilter rebuilds the visible rows. A multi-host list is merge-sorted by time
// here, in the view; the backend never sorts a journal.
func (j *Journal) refilter() {
	out := make([]resource.Resource, 0, len(j.rows))
	for _, r := range j.rows {
		if j.matches(r) {
			out = append(out, r)
		}
	}
	if j.multi {
		sort.SliceStable(out, func(a, b int) bool {
			return rowTime(out[a]).Before(rowTime(out[b]))
		})
	}
	j.view = out
}

// rowTime reads a row's time field, or the zero time.
func rowTime(r resource.Resource) time.Time {
	if t, ok := r.Fields["time"].(time.Time); ok {
		return t
	}
	return time.Time{}
}

// moveCursor moves the cursor by delta and clamps it.
func (j *Journal) moveCursor(delta int) {
	j.cursor += delta
	j.clampCursor()
}

// pageSize is one screen of rows.
func (j *Journal) pageSize() int {
	if n := j.h - 2; n > 1 {
		return n
	}
	return 10
}

// clampCursor keeps the cursor inside the visible rows.
func (j *Journal) clampCursor() {
	if len(j.view) == 0 {
		j.cursor = 0
		return
	}
	if j.cursor < 0 {
		j.cursor = 0
	}
	if j.cursor >= len(j.view) {
		j.cursor = len(j.view) - 1
	}
}

// toggleFollow starts or stops the live stream. Follow needs exactly one host.
func (j *Journal) toggleFollow() tea.Cmd {
	if j.following {
		j.stopFollow()
		return tui.Flash(tui.FlashInfo, "follow off")
	}
	if j.followHost() == "" {
		return tui.Flash(tui.FlashWarn, "follow needs a single host")
	}
	return j.startFollow()
}

// followHost is the one host follow can run on, or "".
func (j *Journal) followHost() resource.HostID {
	if len(j.q.Hosts) != 1 {
		return ""
	}
	return resource.NormalizeHostID(string(j.q.Hosts[0]))
}

// startFollow begins FollowJournal and returns the first batch command.
func (j *Journal) startFollow() tea.Cmd {
	host := j.followHost()
	if host == "" {
		return tui.Flash(tui.FlashWarn, "follow needs a single host")
	}
	f, ok := j.b.(journalFollower)
	if !ok {
		return tui.Flash(tui.FlashError, "this backend has no journal follow")
	}
	ctx, cancel := context.WithCancel(context.Background())
	j.followCancel = cancel
	j.followSeq++
	seq := j.followSeq
	ch, err := f.FollowJournal(ctx, host, linux.JournalOptions{
		Unit:        j.unit(),
		MaxPriority: j.maxPri,
		Window:      j.q.Window,
		Now:         j.now(),
		Follow:      true,
		Sudo:        true,
	})
	if err != nil {
		cancel()
		j.followCancel = nil
		j.following = false
		return tui.Flash(tui.FlashError, "%v", err)
	}
	j.followCh = ch
	j.following = true
	return journalFollow(seq, ch)
}

// nextFollow asks for the next batch of the current stream.
func (j *Journal) nextFollow() tea.Cmd {
	if j.followCh == nil {
		return nil
	}
	return journalFollow(j.followSeq, j.followCh)
}

// journalFollow reads the next batch off ch and emits it tagged with seq.
func journalFollow(seq int, ch <-chan linux.Entry) tea.Cmd {
	return func() tea.Msg {
		entries, closed := tui.NextBatch(ch, journalFollowMax)
		return journalBatchMsg{Seq: seq, Entries: entries, Closed: closed}
	}
}

// stopFollow cancels the live stream and forgets it.
func (j *Journal) stopFollow() {
	if j.followCancel != nil {
		j.followCancel()
		j.followCancel = nil
	}
	j.followCh = nil
	j.following = false
}

// Close cancels both streams. The view stack calls it when the view is popped
// or replaced.
func (j *Journal) Close() error {
	if j.cancel != nil {
		j.cancel()
		j.cancel = nil
	}
	j.wantFollow = false
	j.stopFollow()
	return nil
}

// columns returns the render columns for the given width, prepending the host
// column when more than one host is listed.
func (j *Journal) columns(total int) ([]render.TableColumn, []int) {
	specCols := j.spec.Columns
	if j.multi {
		specCols = append([]resource.Column{{ID: "host", Title: "HOST", Width: 16}}, specCols...)
	}
	cols := make([]render.TableColumn, len(specCols))
	for i, c := range specCols {
		cols[i] = render.TableColumn{
			Title:      c.Title,
			Width:      c.Width,
			AlignRight: c.Align == resource.AlignRight,
		}
	}
	return cols, render.ColumnWidths(cols, total)
}

// header returns the cached header line and layout for width w.
func (j *Journal) header(w int) ([]render.TableColumn, []int) {
	if j.hdrCols != nil && j.hdrWidth == w {
		return j.hdrCols, j.hdrWidths
	}
	cols, widths := j.columns(w)
	j.hdrCols, j.hdrWidths, j.hdrWidth = cols, widths, w
	j.hdr = j.env.Theme.Header.Render(render.Header(cols, widths))
	return cols, widths
}

// specColumns is the column list matching the rendered header.
func (j *Journal) specColumns() []resource.Column {
	if !j.multi {
		return j.spec.Columns
	}
	out := make([]resource.Column, 0, len(j.spec.Columns)+1)
	out = append(out, resource.Column{ID: "host", Title: "HOST", Width: 16})
	return append(out, j.spec.Columns...)
}

// rowLine renders one journal row: time dim, priority coloured by status,
// source in the accent colour and an error message in Crit.
func (j *Journal) rowLine(row resource.Resource, cols []render.TableColumn, widths []int, selected bool) string {
	theme := j.env.Theme
	specCols := j.specColumns()
	status := theme.StatusStyle(row.Status)
	cells := make([]render.Cell, len(specCols))
	now := j.now()
	for i, c := range specCols {
		text := cellText(c, row, now, widths[i])
		var style func(string) string
		switch {
		case row.Fields[c.ID] == nil && c.Kind != resource.ColStatus:
			faint := theme.Faint
			style = func(s string) string { return faint.Render(s) }
		case c.ID == "time" || c.ID == "host":
			dim := theme.Dim
			style = func(s string) string { return dim.Render(s) }
		case c.ID == "source":
			accent := theme.Accent
			style = func(s string) string { return accent.Render(s) }
		case c.ID == "message" && row.Status == resource.StatusCrit:
			crit := theme.Crit
			style = func(s string) string { return crit.Render(s) }
		default:
			style = func(s string) string { return status.Render(s) }
		}
		cells[i] = render.Cell{Text: text, Style: style}
	}
	line := render.Row(cells, widths, cols)
	if selected {
		line = theme.Selected.Render(line)
	}
	return line
}

// banner is the one-line summary: "<n> lines · -1h → now · -u unit · p≤4".
func (j *Journal) banner() string {
	parts := []string{render.Int(int64(len(j.view))) + " lines"}
	parts = append(parts, windowText(j.q.Window))
	if unit := j.unit(); unit != "" {
		parts = append(parts, "-u "+unit)
	}
	if j.maxPri < 7 {
		parts = append(parts, fmt.Sprintf("p≤%d", j.maxPri))
	}
	if j.following {
		parts = append(parts, "following")
	}
	if j.filter != "" {
		parts = append(parts, "/filter: "+j.filter)
	}
	return j.env.Theme.Dim.Render(strings.Join(parts, " · "))
}

// windowText renders a time window for the banner.
func windowText(w resource.TimeWindow) string {
	if w.IsZero() {
		return "all"
	}
	label := w.Label
	if label == "" {
		label = w.From.Format("15:04")
	}
	if w.ToNow {
		return label + " → now"
	}
	if w.To.IsZero() {
		return label
	}
	return label + " → " + w.To.Format("15:04")
}

// Render returns exactly h lines of exactly w cells.
func (j *Journal) Render(w, h int) string {
	j.w, j.h = w, h
	if w <= 0 || h <= 0 {
		return ""
	}
	lines := make([]string, 0, h)
	lines = append(lines, j.banner())
	if h > 1 {
		cols, widths := j.header(w)
		lines = append(lines, j.hdr)

		visible := h - 2
		if visible > 0 {
			errShow := len(j.errs)
			if errShow > visible-1 {
				errShow = visible - 1
			}
			if errShow < 0 {
				errShow = 0
			}
			dataLines := visible - errShow
			if dataLines > 0 {
				start := 0
				if j.cursor >= dataLines {
					start = j.cursor - dataLines + 1
				}
				for i := start; i < start+dataLines && i < len(j.view); i++ {
					lines = append(lines, j.rowLine(j.view[i], cols, widths, i == j.cursor))
				}
			}
			for i := 0; i < errShow && i < len(j.errs); i++ {
				lines = append(lines, journalErrorLine(j.env.Theme, j.errs[i], widths))
			}
		}
	}
	return render.Fit(strings.Join(lines, "\n"), w, h)
}

// journalErrorLine renders one failed host in the message column.
func journalErrorLine(theme *theme.Theme, he hostErr, widths []int) string {
	if len(widths) == 0 {
		return ""
	}
	kind := string(errs.KindOf(he.err))
	line := render.PadRight(string(he.Host), widths[0])
	if len(widths) == 1 {
		line = render.Truncate(render.PadRight(line+" "+kind, widths[0]), widths[0])
	} else {
		line += " " + kind
	}
	return theme.Crit.Render(line)
}

// Crumb is "journal (n)".
func (j *Journal) Crumb() string {
	return fmt.Sprintf("journal (%d)", len(j.view))
}

// Hints lists follow, filter, priority, refresh and back.
func (j *Journal) Hints() []tui.KeyHint {
	follow := "follow"
	if j.following {
		follow = "unfollow"
	}
	return []tui.KeyHint{
		{Key: firstKey(j.keys.Follow), Text: follow},
		{Key: firstKey(j.keys.Filter), Text: "filter"},
		{Key: "1-7", Text: "priority"},
		{Key: firstKey(j.keys.Refresh), Text: "refresh"},
		{Key: firstKey(j.keys.Back), Text: "back"},
	}
}

// Subject is the listed hosts and unit.
func (j *Journal) Subject() tui.Subject {
	return tui.Subject{Hosts: j.q.Hosts, Unit: j.unit(), Kind: j.spec.Name}
}

// Capturing is true while the filter owns the keyboard.
func (j *Journal) Capturing() bool { return j.filtering }

// CommandLine is the `:` command line that reopens this screen.
func (j *Journal) CommandLine() string {
	parts := []string{"journal"}
	if len(j.q.Hosts) > 0 {
		hosts := make([]string, 0, len(j.q.Hosts))
		for _, h := range j.q.Hosts {
			hosts = append(hosts, string(h))
		}
		parts = append(parts, "--host", strings.Join(hosts, ","))
	}
	if unit := j.unit(); unit != "" {
		parts = append(parts, "--arg", "unit="+unit)
	}
	return strings.Join(parts, " ")
}

// CopyLines returns the plain text of the visible list.
func (j *Journal) CopyLines() []string {
	specCols := j.specColumns()
	now := j.now()
	lines := make([]string, 0, len(j.view)+len(j.errs)+1)
	titles := make([]string, len(specCols))
	for i, c := range specCols {
		titles[i] = c.Title
	}
	lines = append(lines, strings.Join(titles, "  "))
	for _, r := range j.view {
		cells := make([]string, len(specCols))
		for i, c := range specCols {
			cells[i] = cellText(c, r, now, c.Width)
		}
		lines = append(lines, strings.Join(cells, "  "))
	}
	for _, he := range j.errs {
		lines = append(lines, string(he.Host)+"  "+string(errs.KindOf(he.err)))
	}
	return lines
}
