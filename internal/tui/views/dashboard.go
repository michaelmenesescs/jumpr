package views

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend/grafana"
	"jumpr/internal/errs"
	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
)

const (
	// dashboardMaxInFlight is how many panels query at once. The datasource
	// behind Grafana is the bottleneck, so a burst of panels is capped.
	dashboardMaxInFlight = 6
	// dashboardGap is the blank columns between two panel cells.
	dashboardGap = 1
	// dashboardMinPanelH is the shortest cell the grid draws: a title and one
	// line of body.
	dashboardMinPanelH = 4
	// dashboardDefaultWindow is the range used when the session has none.
	dashboardDefaultWindow = time.Hour
	// dashboardBatchMax caps the results one batch message carries.
	dashboardBatchMax = 32
	// dashboardSpinTick is how often the loading spinner advances.
	dashboardSpinTick = 100 * time.Millisecond
)

// dashboardSpinnerUnicode and dashboardSpinnerASCII are the loading spinner in
// each glyph family the UI declares (render.GlyphSet has no spinner field).
var (
	dashboardSpinnerUnicode = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
	dashboardSpinnerASCII   = []rune("|/-\\")
)

// DashboardQuerier is the part of the Grafana client the grid runs panel
// queries through. *grafana.Client implements it.
type DashboardQuerier interface {
	QueryPanel(ctx context.Context, p grafana.Panel, iv grafana.Interpolation, from, to time.Time, maxDataPoints int64) ([]grafana.Series, error)
}

// dashboardOpener is the browser hook of the Grafana backend: `o` sends the
// panel's URL there (BUILD-PLAN §6.1 rule 6 keeps the process handling out of
// the view). *grafana.Backend implements it.
type dashboardOpener interface {
	OpenURL(url string) error
}

// dashboardPanel is one leaf panel of the grid and its load state.
type dashboardPanel struct {
	panel   grafana.Panel
	section string
	series  []grafana.Series
	queries []string
	err     error
	loading bool
}

// queryable reports whether the TUI runs this panel's queries itself. A panel
// whose type jumpr cannot draw, or one without queries, is opened in a
// browser instead.
func (p *dashboardPanel) queryable() bool {
	return grafana.SupportedPanelType(p.panel.Type) && len(p.panel.Targets) > 0
}

// title is the panel's heading: its title, then its row section, then its type.
func (p *dashboardPanel) title() string {
	if t := strings.TrimSpace(p.panel.Title); t != "" {
		return t
	}
	if t := strings.TrimSpace(p.section); t != "" {
		return t
	}
	if t := strings.TrimSpace(p.panel.Type); t != "" {
		return t
	}
	return "panel"
}

// panelJob is one panel to load: its index in the grid and its definition.
type panelJob struct {
	index int
	panel grafana.Panel
}

// dashboardResultMsg is one panel's finished load.
type dashboardResultMsg struct {
	seq     int
	index   int
	series  []grafana.Series
	queries []string
	err     error
}

// dashboardBatchMsg is one batch of finished loads, as tui.NextBatch delivers
// it (BUILD-PLAN §5: the only streaming pattern).
type dashboardBatchMsg struct {
	seq     int
	results []dashboardResultMsg
	closed  bool
}

// Dashboard is the panel grid of one Grafana dashboard: timeseries panels as
// braille charts, stat panels as a number and a sparkline, gauge panels as a
// bar, everything else as its title and "open in browser (o)". Panels load
// concurrently with at most dashboardMaxInFlight in flight and each keeps its
// own spinner and error kind (SPEC §8.4).
type Dashboard struct {
	env    *tui.Env
	src    DashboardQuerier
	server string
	dash   *grafana.Dashboard
	window resource.TimeWindow

	panels []*dashboardPanel
	cursor int
	zoom   bool

	err     error
	loading bool

	seq     int
	cancel  context.CancelFunc
	results chan dashboardResultMsg
	// loadedMDP is the data-point budget the panel series were fetched with;
	// a width change that moves it reloads the grid.
	loadedMDP int64

	w, h int
}

var _ tui.View = (*Dashboard)(nil)

// NewDashboard builds the grid for d, loaded from src (one configured Grafana
// server, named server). The dashboard is already fetched: the grid only runs
// panel queries.
func NewDashboard(env *tui.Env, src DashboardQuerier, server string, d *grafana.Dashboard, window resource.TimeWindow) *Dashboard {
	v := &Dashboard{
		env:    env,
		src:    src,
		server: strings.TrimSpace(server),
		dash:   d,
		window: window,
	}
	if d != nil {
		v.panels = dashboardPanels(d.Panels, "")
	}
	return v
}

// dashboardPanels flattens the dashboard's panels: row panels become the
// section of the panels they hold, so every entry is a leaf the grid draws.
func dashboardPanels(panels []grafana.Panel, section string) []*dashboardPanel {
	var out []*dashboardPanel
	for _, p := range panels {
		if p.IsRow() {
			title := strings.TrimSpace(p.Title)
			if title == "" {
				title = section
			}
			out = append(out, dashboardPanels(p.Panels, title)...)
			continue
		}
		out = append(out, &dashboardPanel{panel: p, section: section})
		if len(p.Panels) > 0 {
			out = append(out, dashboardPanels(p.Panels, section)...)
		}
	}
	return out
}

// Init loads every queryable panel.
func (d *Dashboard) Init() tea.Cmd {
	if d.dash == nil {
		d.err = errs.New(errs.Config, "views.dashboard", "no dashboard to show")
		return nil
	}
	return d.load()
}

// now is the clock the environment hands in.
func (d *Dashboard) now() time.Time {
	if d.env != nil && d.env.Now != nil {
		return d.env.Now()
	}
	return time.Now()
}

// bounds is the range every panel queries: the session window, or the last
// hour when the session has none.
func (d *Dashboard) bounds() (time.Time, time.Time) {
	if d.window.IsZero() {
		to := d.now()
		return to.Add(-dashboardDefaultWindow), to
	}
	return d.window.Bounds(d.now())
}

// cols is the number of panel columns the width allows: three on a wide pane,
// two in the middle, one on a narrow one.
func (d *Dashboard) cols(w int) int {
	switch {
	case w >= 120:
		return 3
	case w >= 80:
		return 2
	}
	return 1
}

// cellWidth is the width of one panel cell at the current pane width.
func (d *Dashboard) cellWidth(w int) int {
	cols := d.cols(w)
	cw := (w - (cols-1)*dashboardGap) / cols
	if cw < 1 {
		cw = 1
	}
	return cw
}

// load cancels the previous load, marks the panels loading and starts the
// queries. The queries themselves run in goroutines, never in Update; the
// results arrive as dashboardBatchMsg through tui.NextBatch.
func (d *Dashboard) load() tea.Cmd {
	if d.cancel != nil {
		d.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel

	d.seq++
	seq := d.seq
	d.err = nil

	for _, p := range d.panels {
		p.series, p.queries, p.err = nil, nil, nil
		p.loading = p.queryable()
	}
	from, to := d.bounds()
	mdp := grafana.MaxDataPointsFor(d.cellWidth(d.paneWidth()))
	d.loadedMDP = mdp
	iv := grafana.NewInterpolation(d.vars(), from, to, mdp)

	var jobs []panelJob
	for i, p := range d.panels {
		if p.queryable() {
			jobs = append(jobs, panelJob{index: i, panel: p.panel})
		}
	}
	d.loading = len(jobs) > 0

	out := make(chan dashboardResultMsg)
	d.results = out
	src := d.src
	go runDashboardPanels(ctx, src, jobs, iv, from, to, mdp, out, seq)

	return d.nextBatch(seq, out)
}

// nextBatch asks for the next batch of panel results.
func (d *Dashboard) nextBatch(seq int, ch <-chan dashboardResultMsg) tea.Cmd {
	return func() tea.Msg {
		items, closed := tui.NextBatch(ch, dashboardBatchMax)
		return dashboardBatchMsg{seq: seq, results: items, closed: closed}
	}
}

// runDashboardPanels runs every job with at most dashboardMaxInFlight in
// flight and closes out when they are all done or ctx is canceled. It touches
// no view state: the results travel on the channel.
func runDashboardPanels(ctx context.Context, src DashboardQuerier, jobs []panelJob, iv grafana.Interpolation, from, to time.Time, mdp int64, out chan<- dashboardResultMsg, seq int) {
	defer close(out)
	sem := make(chan struct{}, dashboardMaxInFlight)
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func(j panelJob) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			series, queries, err := queryDashboardPanel(ctx, src, j.panel, iv, from, to, mdp)
			msg := dashboardResultMsg{seq: seq, index: j.index, series: series, queries: queries, err: err}
			select {
			case out <- msg:
			case <-ctx.Done():
			}
		}(job)
	}
	wg.Wait()
}

// queryDashboardPanel runs one panel's queries and computes the expressions
// that went into them, for the finding text.
func queryDashboardPanel(ctx context.Context, src DashboardQuerier, p grafana.Panel, iv grafana.Interpolation, from, to time.Time, mdp int64) ([]grafana.Series, []string, error) {
	if src == nil {
		return nil, nil, errs.New(errs.Config, "views.dashboard", "no grafana client")
	}
	pq, err := grafana.BuildPanelQuery(p, iv, from, to, mdp)
	if err != nil {
		return nil, nil, err
	}
	series, err := src.QueryPanel(ctx, p, iv, from, to, mdp)
	if err != nil {
		return nil, pq.Expressions, err
	}
	return series, pq.Expressions, nil
}

// paneWidth is the last rendered width, or a default before the first frame.
func (d *Dashboard) paneWidth() int {
	if d.w <= 0 {
		return 100
	}
	return d.w
}

// vars are the dashboard's template variables in the form the interpolation
// consumes.
func (d *Dashboard) vars() grafana.Vars {
	if d.dash == nil {
		return nil
	}
	return d.dash.QueryVars()
}

// Update handles panel loads, the session window, a resize and the keys. A
// load of an older sequence is dropped.
func (d *Dashboard) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	switch m := msg.(type) {
	case dashboardBatchMsg:
		if m.seq != d.seq {
			return d, nil
		}
		d.applyBatch(m)
		if !m.closed {
			return d, d.nextBatch(d.seq, d.results)
		}
		d.loading = d.anyLoading()
		return d, nil
	case tui.WindowChangedMsg:
		d.window = m.Window
		return d, d.load()
	case tea.WindowSizeMsg:
		d.w, d.h = m.Width, m.Height
		// The data-point budget is the cell width in braille cells; a resize
		// that changes it reloads, so the charts match the new width.
		if !d.loading && len(d.panels) > 0 {
			if mdp := grafana.MaxDataPointsFor(d.cellWidth(m.Width)); mdp != d.loadedMDP {
				return d, d.load()
			}
		}
		return d, nil
	case tea.KeyMsg:
		return d, d.handleKey(m)
	}
	return d, nil
}

// applyBatch stores one batch of finished loads.
func (d *Dashboard) applyBatch(m dashboardBatchMsg) {
	for _, res := range m.results {
		if res.seq != d.seq || res.index < 0 || res.index >= len(d.panels) {
			continue
		}
		p := d.panels[res.index]
		p.loading = false
		p.series, p.queries, p.err = res.series, res.queries, res.err
	}
}

// anyLoading reports whether any panel is still loading.
func (d *Dashboard) anyLoading() bool {
	for _, p := range d.panels {
		if p.loading {
			return true
		}
	}
	return false
}

// loadingCount is how many panels are loading, for the header.
func (d *Dashboard) loadingCount() int {
	n := 0
	for _, p := range d.panels {
		if p.loading {
			n++
		}
	}
	return n
}

// keys is the key map the view binds through.
func (d *Dashboard) keys() *keymap.Map {
	if d.env != nil && d.env.Keys != nil {
		return d.env.Keys
	}
	return keymap.Default()
}

// handleKey routes one key.
func (d *Dashboard) handleKey(k tea.KeyMsg) tea.Cmd {
	keys := d.keys()
	key := k.String()
	cols := d.cols(d.paneWidth())
	switch {
	case keys.Finding.Matches(key):
		return d.recordFinding()
	case keys.Refresh.Matches(key):
		return d.load()
	case keys.Open.Matches(key):
		if len(d.panels) == 0 {
			return tui.Flash(tui.FlashInfo, "the dashboard has no panels")
		}
		d.zoom = !d.zoom
		return nil
	case keys.Back.Matches(key):
		if d.zoom {
			d.zoom = false
			return nil
		}
		return tui.Pop()
	case keys.Up.Matches(key):
		d.move(-cols)
	case keys.Down.Matches(key):
		d.move(cols)
	case key == "left" || key == "h":
		d.moveInRow(-1, cols)
	case key == "right" || key == "l":
		d.moveInRow(1, cols)
	case keys.Top.Matches(key):
		d.cursor = 0
	case keys.Bottom.Matches(key):
		if len(d.panels) > 0 {
			d.cursor = len(d.panels) - 1
		}
	case keys.PageUp.Matches(key):
		d.move(-cols * d.visibleRows())
	case keys.PageDown.Matches(key):
		d.move(cols * d.visibleRows())
	case key == "o":
		return d.openSelected()
	}
	return nil
}

// move shifts the cursor by delta panels, clamped to the grid.
func (d *Dashboard) move(delta int) {
	if len(d.panels) == 0 {
		return
	}
	d.cursor += delta
	if d.cursor < 0 {
		d.cursor = 0
	}
	if d.cursor >= len(d.panels) {
		d.cursor = len(d.panels) - 1
	}
}

// moveInRow shifts the cursor within its grid row, so left on the first
// column and right on the last do nothing instead of wrapping.
func (d *Dashboard) moveInRow(delta, cols int) {
	if len(d.panels) == 0 {
		return
	}
	last := len(d.panels) - 1
	next := d.cursor + delta
	if next < 0 || next > last {
		return
	}
	if cols > 1 && next/cols != d.cursor/cols {
		return
	}
	d.cursor = next
}

// visibleRows is how many grid rows fit in the last frame.
func (d *Dashboard) visibleRows() int {
	bodyH := d.h - 2
	if bodyH < dashboardMinPanelH {
		return 1
	}
	return bodyH / dashboardMinPanelH
}

// selected is the panel under the cursor, or nil.
func (d *Dashboard) selected() *dashboardPanel {
	if d.cursor < 0 || d.cursor >= len(d.panels) {
		return nil
	}
	return d.panels[d.cursor]
}

// recordFinding writes the selected panel's title, queries and min/max/last
// values to the investigation as one finding (task 120 requirement 4).
func (d *Dashboard) recordFinding() tea.Cmd {
	p := d.selected()
	if p == nil {
		return tui.Flash(tui.FlashInfo, "no panel selected")
	}
	text := dashboardFinding(p)
	if d.env == nil {
		return tui.Flash(tui.FlashWarn, "no investigation configured")
	}
	if cmd := d.env.RecordFinding(text); cmd != nil {
		return cmd
	}
	return tui.Flash(tui.FlashInfo, "finding is empty")
}

// dashboardFinding is the finding text of one panel: the first line names the
// panel, the rest carries the queries and the series statistics.
func dashboardFinding(p *dashboardPanel) string {
	if p == nil {
		return ""
	}
	lines := []string{fmt.Sprintf("grafana panel %s (%s)", p.title(), dashboardPanelType(p))}
	for _, q := range p.queries {
		lines = append(lines, "  query "+q)
	}
	if p.err != nil {
		lines = append(lines, "  error "+string(errs.KindOf(p.err))+": "+dashboardErrDetail(p.err))
		return strings.Join(lines, "\n")
	}
	if len(p.series) == 0 {
		lines = append(lines, "  no data")
		return strings.Join(lines, "\n")
	}
	for _, s := range p.series {
		lines = append(lines, "  "+s.Title()+" "+dashboardStats(s))
	}
	return strings.Join(lines, "\n")
}

// dashboardStats renders "min 1 max 3 last 2" for one series.
func dashboardStats(s grafana.Series) string {
	min, max, ok := s.Bounds()
	if !ok {
		return "min — max — last —"
	}
	last, hasLast := s.Last()
	lastText := "—"
	if hasLast {
		lastText = dashboardValue(last)
	}
	return "min " + dashboardValue(min) + " max " + dashboardValue(max) + " last " + lastText
}

// dashboardPanelType is the panel's type, or "unknown" when it has none.
func dashboardPanelType(p *dashboardPanel) string {
	if t := strings.TrimSpace(p.panel.Type); t != "" {
		return t
	}
	return "unknown"
}

// openSelected sends the selected panel's browser URL to the app's opener.
func (d *Dashboard) openSelected() tea.Cmd {
	p := d.selected()
	if p == nil {
		return tui.Flash(tui.FlashInfo, "no panel selected")
	}
	url := d.panelURL(p)
	if url == "" {
		return tui.Flash(tui.FlashWarn, "the panel has no browser URL")
	}
	opener := d.browser()
	if opener == nil {
		return tui.Flash(tui.FlashWarn, "no browser opener configured")
	}
	return func() tea.Msg {
		if err := opener.OpenURL(url); err != nil {
			return tui.FlashMsg{Level: tui.FlashError, Text: err.Error()}
		}
		return tui.FlashMsg{Level: tui.FlashOK, Text: "opened " + url}
	}
}

// browser finds the Grafana backend's opener in the registry. Nil when there
// is none, which the caller flashes instead of guessing.
func (d *Dashboard) browser() dashboardOpener {
	if d.env == nil || d.env.Registry == nil {
		return nil
	}
	b, ok := d.env.Registry.Get("grafana")
	if !ok {
		return nil
	}
	opener, ok := b.(dashboardOpener)
	if !ok {
		return nil
	}
	return opener
}

// panelURL is the browser URL of one panel: the dashboard's URL with
// ?viewPanel=<id>.
func (d *Dashboard) panelURL(p *dashboardPanel) string {
	if p == nil || d.dash == nil {
		return ""
	}
	base := strings.TrimSpace(d.dash.URL)
	if base == "" {
		return ""
	}
	if i := strings.IndexByte(base, '?'); i >= 0 {
		base = base[:i]
	}
	if p.panel.ID > 0 {
		return base + "?viewPanel=" + strconv.Itoa(p.panel.ID)
	}
	return base
}

// style is the theme, or a fresh dark one in a bare test.
func (d *Dashboard) style() *theme.Theme {
	if d.env != nil && d.env.Theme != nil {
		return d.env.Theme
	}
	return theme.Dark()
}

// glyphs is the active glyph set, unicode when none was resolved.
func (d *Dashboard) glyphs() render.GlyphSet {
	if d.env != nil && d.env.Glyphs.Name != "" {
		return d.env.Glyphs
	}
	set, err := render.Glyphs("unicode")
	if err != nil {
		return render.GlyphSet{}
	}
	return set
}

// spinner is the current loading frame. The frame follows the clock instead of
// a tick loop: a panel's spinner moves as the batches of the other panels
// repaint the frame, and no timer keeps the program busy after the last result.
func (d *Dashboard) spinner() string {
	set := d.glyphs()
	frames := dashboardSpinnerUnicode
	if set.ASCII() {
		frames = dashboardSpinnerASCII
	}
	if len(frames) == 0 {
		return ""
	}
	frame := int(d.now().UnixNano()/int64(dashboardSpinTick)) % len(frames)
	if frame < 0 {
		frame += len(frames)
	}
	return string(frames[frame])
}

// Render returns exactly h lines of exactly w cells: a header, the grid (or
// the zoomed panel) and a footer.
func (d *Dashboard) Render(w, h int) string {
	d.w, d.h = w, h
	if w <= 0 || h <= 0 {
		return ""
	}
	st := d.style()
	if d.dash == nil {
		return render.Fit(st.Dim.Render(render.Truncate("no dashboard to show", w)), w, h)
	}
	head := d.header(w, st)
	foot := st.Dim.Render(render.Truncate(d.footerText(), w))
	body := ""
	if bodyH := h - 2; bodyH > 0 {
		if d.zoom {
			body = d.renderPanel(d.selected(), w, bodyH, st, d.selected() != nil)
		} else {
			body = d.grid(w, bodyH, st)
		}
	}
	return render.Fit(strings.Join([]string{head, body, foot}, "\n"), w, h)
}

// header is the dashboard line: title, server, window, panel count, progress.
func (d *Dashboard) header(w int, st *theme.Theme) string {
	sep := " " + string(d.glyphs().Separator) + " "
	parts := []string{"dashboard", d.title()}
	if d.server != "" {
		parts = append(parts, d.server)
	}
	parts = append(parts, d.windowLabel())
	parts = append(parts, fmt.Sprintf("%d panels", len(d.panels)))
	if n := d.loadingCount(); n > 0 {
		parts = append(parts, fmt.Sprintf("%d loading", n))
	}
	if d.zoom {
		parts = append(parts, "zoom")
	}
	if d.err != nil {
		parts = append(parts, errText(d.err))
	}
	return st.Title.Render(render.Truncate(strings.Join(parts, sep), w))
}

// title is the dashboard's title, or its uid.
func (d *Dashboard) title() string {
	if t := strings.TrimSpace(d.dash.Title); t != "" {
		return t
	}
	return strings.TrimSpace(d.dash.UID)
}

// windowLabel is the range the panels cover.
func (d *Dashboard) windowLabel() string {
	if label := d.window.String(); label != "-" {
		return label
	}
	return "last " + render.Duration(dashboardDefaultWindow)
}

// footerText is the key hint line.
func (d *Dashboard) footerText() string {
	if d.zoom {
		return "enter/esc unzoom · r reload · F finding · o browser"
	}
	return "enter zoom · h/l/j/k move · r reload · F finding · o browser"
}

// grid renders the visible panel rows.
func (d *Dashboard) grid(w, bodyH int, st *theme.Theme) string {
	if len(d.panels) == 0 {
		return render.Fit(st.Dim.Render(render.Truncate("the dashboard has no panels", w)), w, bodyH)
	}
	cols := d.cols(w)
	rows := (len(d.panels) + cols - 1) / cols
	vis := rows
	if maxRows := d.visibleRows(); vis > maxRows {
		vis = maxRows
	}
	if vis < 1 {
		vis = 1
	}
	rowH := bodyH / vis
	if rowH < 1 {
		rowH = 1
	}
	top := d.cursor/cols - vis + 1
	if top < 0 {
		top = 0
	}
	if top+vis > rows {
		top = rows - vis
		if top < 0 {
			top = 0
		}
	}

	cellW := d.cellWidth(w)
	var lines []string
	for r := top; r < top+vis; r++ {
		cells := make([][]string, cols)
		for c := 0; c < cols; c++ {
			idx := r*cols + c
			cw := cellW
			if c == cols-1 {
				cw = w - (cellW+dashboardGap)*c
			}
			if cw < 1 {
				cw = 1
			}
			if idx >= len(d.panels) {
				cells[c] = blankLines(cw, rowH)
				continue
			}
			focused := idx == d.cursor
			cells[c] = strings.Split(d.renderPanel(d.panels[idx], cw, rowH, st, focused), "\n")
		}
		for i := 0; i < rowH; i++ {
			var b strings.Builder
			for c := 0; c < cols; c++ {
				if c > 0 {
					b.WriteString(strings.Repeat(" ", dashboardGap))
				}
				b.WriteString(lineAt(cells[c], i))
			}
			lines = append(lines, b.String())
		}
	}
	return render.Fit(strings.Join(lines, "\n"), w, bodyH)
}

// lineAt returns one line of a rendered cell, or "" past its end.
func lineAt(cell []string, i int) string {
	if i < 0 || i >= len(cell) {
		return ""
	}
	return cell[i]
}

// blankLines is h empty lines of exactly w cells.
func blankLines(w, h int) []string {
	out := make([]string, 0, h)
	for i := 0; i < h; i++ {
		out = append(out, strings.Repeat(" ", w))
	}
	return out
}

// renderPanel draws one panel cell of exactly w by h cells: a heading, then
// the type's body, then its own spinner or error kind.
func (d *Dashboard) renderPanel(p *dashboardPanel, w, h int, st *theme.Theme, focused bool) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	if p == nil {
		return render.Fit("", w, h)
	}
	mark := " "
	if focused {
		mark = string(d.glyphs().Arrow)
	}
	head := mark + " " + p.title()
	switch {
	case p.loading:
		head += " " + d.spinner()
	case p.err != nil:
		head += " " + string(d.glyphs().Separator) + " " + string(errs.KindOf(p.err))
	}
	head = render.Truncate(head, w)
	if focused {
		head = st.Title.Render(render.PadRight(head, w))
	} else {
		head = st.Dim.Render(render.PadRight(head, w))
	}
	body := ""
	if bodyH := h - 1; bodyH > 0 {
		body = d.panelBody(p, w, bodyH, st)
	}
	return render.Fit(strings.Join([]string{head, body}, "\n"), w, h)
}

// panelBody draws the panel's content of exactly w by h cells.
func (d *Dashboard) panelBody(p *dashboardPanel, w, h int, st *theme.Theme) string {
	if h <= 0 {
		return ""
	}
	set := d.glyphs()
	switch {
	case p.err != nil:
		lines := []string{st.Crit.Render(render.Truncate("error: "+string(errs.KindOf(p.err)), w))}
		if h > 1 {
			lines = append(lines, st.Dim.Render(render.Truncate(dashboardErrDetail(p.err), w)))
		}
		return render.Fit(strings.Join(lines, "\n"), w, h)
	case p.loading:
		return render.Fit(st.Dim.Render(render.Truncate(d.spinner()+" loading"+string(set.Ellipsis), w)), w, h)
	case !p.queryable():
		return render.Fit(st.Dim.Render(render.Truncate("open in browser (o)", w)), w, h)
	}
	switch strings.ToLower(strings.TrimSpace(p.panel.Type)) {
	case "timeseries", "graph":
		return render.Chart(d.chartSeries(p), w, h)
	case "stat":
		return d.statBody(p, w, h, st)
	case "gauge":
		return d.gaugeBody(p, w, h, st)
	}
	return render.Fit(st.Dim.Render(render.Truncate("open in browser (o)", w)), w, h)
}

// statBody draws a stat panel: the big number, its sparkline and the series
// name.
func (d *Dashboard) statBody(p *dashboardPanel, w, h int, st *theme.Theme) string {
	if len(p.series) == 0 {
		return render.Fit(st.Dim.Render(render.Truncate("no data", w)), w, h)
	}
	s := p.series[0]
	value := "—"
	if v, ok := s.Last(); ok {
		value = dashboardValue(v)
	}
	lines := []string{st.Title.Render(render.Truncate(value, w))}
	if h > 1 {
		lines = append(lines, render.SparkWith(d.glyphs(), s.Values(), w))
	}
	if h > 2 && len(p.series) > 0 {
		lines = append(lines, st.Dim.Render(render.Truncate(s.Title(), w)))
	}
	return render.Fit(strings.Join(lines, "\n"), w, h)
}

// gaugeBody draws a gauge panel: the value, then a bar filled to where the
// value sits in the panel's own range.
func (d *Dashboard) gaugeBody(p *dashboardPanel, w, h int, st *theme.Theme) string {
	if len(p.series) == 0 {
		return render.Fit(st.Dim.Render(render.Truncate("no data", w)), w, h)
	}
	min, max, any := dashboardBounds(p.series)
	value := "—"
	frac := 0.0
	if v, ok := p.series[0].Last(); ok {
		value = dashboardValue(v)
		switch {
		case any && max > min:
			frac = (v - min) / (max - min)
		case v >= 0 && v <= 1:
			frac = v
		}
	}
	lines := []string{st.Title.Render(render.Truncate(value, w))}
	if h > 1 {
		lines = append(lines, render.BarWith(d.glyphs(), frac, w))
	}
	if h > 2 {
		lines = append(lines, st.Dim.Render(render.Truncate(p.series[0].Title(), w)))
	}
	return render.Fit(strings.Join(lines, "\n"), w, h)
}

// chartSeries converts the panel's series into chart lines.
func (d *Dashboard) chartSeries(p *dashboardPanel) []render.Series {
	out := make([]render.Series, 0, len(p.series))
	for i, s := range p.series {
		pts := make([]render.Point, 0, len(s.Points))
		for _, pt := range s.Points {
			pts = append(pts, render.Point{T: pt.T, V: pt.V})
		}
		out = append(out, render.Series{Name: s.Title(), Color: i, Points: pts})
	}
	return out
}

// dashboardBounds is the value range across every series of a panel.
func dashboardBounds(series []grafana.Series) (min, max float64, ok bool) {
	for _, s := range series {
		smin, smax, sok := s.Bounds()
		if !sok {
			continue
		}
		if !ok {
			min, max, ok = smin, smax, true
			continue
		}
		if smin < min {
			min = smin
		}
		if smax > max {
			max = smax
		}
	}
	return min, max, ok
}

// dashboardValue renders one sample value compactly.
func dashboardValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', 4, 64)
}

// dashboardErrDetail is the one-line detail of a failed panel.
func dashboardErrDetail(err error) string {
	if err == nil {
		return ""
	}
	var e *errs.Error
	if errors.As(err, &e) && e.Msg != "" {
		if e.Host != "" {
			return e.Host + ": " + e.Msg
		}
		return e.Msg
	}
	return err.Error()
}

// Crumb is the dashboard this screen is about.
func (d *Dashboard) Crumb() string {
	if d.dash == nil {
		return "dashboard"
	}
	return "dashboard " + d.title()
}

// Hints are the keys the view binds.
func (d *Dashboard) Hints() []tui.KeyHint {
	if d.zoom {
		return []tui.KeyHint{
			{Key: "enter", Text: "unzoom"},
			{Key: "r", Text: "reload"},
			{Key: "F", Text: "finding"},
			{Key: "o", Text: "browser"},
		}
	}
	return []tui.KeyHint{
		{Key: "enter", Text: "zoom panel"},
		{Key: "h/l/j/k", Text: "move"},
		{Key: "r", Text: "reload"},
		{Key: "F", Text: "finding"},
		{Key: "o", Text: "browser"},
	}
}

// Subject is the dashboard the grid shows.
func (d *Dashboard) Subject() tui.Subject {
	sub := tui.Subject{Kind: "dashboard"}
	if d.dash == nil {
		return sub
	}
	sub.Ref = resource.Ref{
		Backend:   "grafana",
		Kind:      "dashboard",
		Namespace: d.server,
		Name:      d.dash.UID,
	}
	return sub
}

// Capturing is false: the grid owns no text input.
func (d *Dashboard) Capturing() bool { return false }

// CopyLines is the plain text of the grid.
func (d *Dashboard) CopyLines() []string {
	if d.dash == nil {
		return []string{"dashboard"}
	}
	out := []string{"dashboard " + d.title()}
	for _, p := range d.panels {
		line := "  " + p.title() + " (" + dashboardPanelType(p) + ")"
		switch {
		case p.loading:
			line += " loading"
		case p.err != nil:
			line += " error " + string(errs.KindOf(p.err))
		case len(p.series) > 0:
			line += " " + dashboardStats(p.series[0])
		}
		out = append(out, line)
	}
	return out
}
