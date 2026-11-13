package views

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend/elastic"
	"jumper/internal/errs"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/tui"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
)

const (
	// esLogsMaxRows caps the rows the screen keeps in memory.
	esLogsMaxRows = 5000
	// esLogsPageSize is the page each selected source is asked for, matching
	// the backend's `_search` size.
	esLogsPageSize = elastic.SearchSize
	// esLogsTimeWidth, esLogsLevelWidth and esLogsHostWidth are the fixed
	// column widths; the message takes the rest.
	esLogsTimeWidth  = 19
	esLogsLevelWidth = 7
	esLogsHostWidth  = 24
	// esLogsColumnWidth is the width of a column the user added.
	esLogsColumnWidth = 16
)

// esLogsSource is what the `:logs` screen needs from the Elasticsearch backend.
// *elastic.Backend implements it.
type esLogsSource interface {
	// LogSources lists the configured log sources the screen searches.
	LogSources() []elastic.SourceRef
	// Logs reads one page of one configured source.
	Logs(ctx context.Context, q elastic.LogQuery) (elastic.LogPage, error)
	// FieldCaps lists the fields a column can be added for.
	FieldCaps(ctx context.Context, cluster, source string) ([]elastic.Field, error)
}

// esLogsDataMsg is one finished load: the merged first page of every selected
// source.
type esLogsDataMsg struct {
	seq  int
	rows []elastic.LogRow
	full bool
	err  error
}

// esLogsFieldsMsg is the field-completion answer of one source.
type esLogsFieldsMsg struct {
	seq    int
	fields []elastic.Field
	err    error
}

// esLogsMode is what the keyboard is doing.
type esLogsMode int

const (
	esLogsIdle esLogsMode = iota
	esLogsFilter
	esLogsQuery
	esLogsColumn
)

// ESLogs is the `:logs` screen (SPEC §8.5): central logs from the configured
// Elasticsearch sources, filtered by the investigation's hosts and window. The
// screen is read-only. `/` is a local filter, `q` edits the `query_string`, `c`
// adds a column, `enter` shows the full document and `F` records a finding.
type ESLogs struct {
	env *tui.Env
	src esLogsSource

	sources []elastic.SourceRef
	cluster string
	source  string

	query   string
	window  resource.TimeWindow
	hosts   []resource.HostID
	columns []string

	rows   []elastic.LogRow
	view   []elastic.LogRow
	cursor int
	full   bool
	loaded time.Time

	loading bool
	err     error
	seq     int
	cancel  context.CancelFunc

	mode  esLogsMode
	input textinput.Model
	// filter is the local filter text: a substring, or a regex after "~".
	filter    string
	filterRe  *regexp.Regexp
	filterBad bool

	// fields is the completion list, filled by `c` through _field_caps.
	fields []string

	detail    bool
	detailTop int

	theme *theme.Theme
	w, h  int
}

var _ tui.View = (*ESLogs)(nil)

// NewESLogs builds the `:logs` screen over src. q.Args["cluster"],
// q.Args["source"], q.Args["query"] and q.Args["columns"] narrow the search;
// q.Hosts and q.Window come from the session.
func NewESLogs(env *tui.Env, src esLogsSource, q resource.Query) *ESLogs {
	e := &ESLogs{
		env:     env,
		src:     src,
		cluster: strings.TrimSpace(q.Args[elastic.ArgCluster]),
		source:  strings.TrimSpace(q.Args[elastic.ArgSource]),
		query:   strings.TrimSpace(q.Args[elastic.ArgQuery]),
		window:  q.Window,
		hosts:   append([]resource.HostID{}, q.Hosts...),
		columns: splitList(q.Args[elastic.ArgColumns]),
	}
	if src != nil {
		for _, ref := range src.LogSources() {
			if e.cluster != "" && ref.Cluster != e.cluster {
				continue
			}
			if e.source != "" && ref.Source.Name != e.source {
				continue
			}
			e.sources = append(e.sources, ref)
		}
	}
	if env != nil && env.Theme != nil {
		e.theme = env.Theme
	} else {
		e.theme = theme.Dark()
	}
	e.input = esLogsInput()
	return e
}

// NewESLogsFromEnv builds the screen for a query, finding the Elasticsearch
// backend in the registry. app wires `:logs` through it.
func NewESLogsFromEnv(env *tui.Env, q resource.Query) (*ESLogs, error) {
	if env == nil || env.Registry == nil {
		return nil, errs.New(errs.Config, "views.logs", "no backends configured")
	}
	for _, b := range env.Registry.Backends() {
		src, ok := b.(esLogsSource)
		if !ok {
			continue
		}
		if len(src.LogSources()) == 0 {
			continue
		}
		return NewESLogs(env, src, q), nil
	}
	return nil, errs.New(errs.Config, "views.logs", "no elasticsearch log source configured")
}

// esLogsInput builds the shared text input.
func esLogsInput() textinput.Model {
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 0
	return in
}

// splitList splits a comma-separated argument.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Init starts the first load.
func (e *ESLogs) Init() tea.Cmd { return e.load() }

// now is the clock handed in by the environment.
func (e *ESLogs) now() time.Time {
	if e.env != nil && e.env.Now != nil {
		return e.env.Now()
	}
	return time.Now()
}

// load cancels the previous load and starts a new one. Every source is asked
// for its first page inside the returned tea.Cmd, never in Update.
func (e *ESLogs) load() tea.Cmd {
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}
	e.seq++
	seq := e.seq
	e.loading = true
	e.err = nil
	e.detail = false

	if e.src == nil {
		e.loading = false
		e.err = errs.New(errs.Config, "views.logs", "no elasticsearch client")
		return nil
	}
	if len(e.sources) == 0 {
		e.loading = false
		e.err = errs.New(errs.Config, "views.logs", "no log source matches this query")
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	sources := append([]elastic.SourceRef{}, e.sources...)
	src := e.src
	q := elastic.LogQuery{
		Hosts:   append([]resource.HostID{}, e.hosts...),
		Query:   e.query,
		Window:  e.window,
		Columns: append([]string{}, e.columns...),
		Size:    esLogsPageSize,
		Now:     e.now(),
	}

	return func() tea.Msg {
		msg := esLogsDataMsg{seq: seq}
		var rows []elastic.LogRow
		for _, ref := range sources {
			lq := q
			lq.Cluster, lq.Source = ref.Cluster, ref.Source.Name
			page, err := src.Logs(ctx, lq)
			if err != nil {
				if ctx.Err() != nil {
					return esLogsDataMsg{seq: -1}
				}
				msg.err = err
				return msg
			}
			if page.Full {
				msg.full = true
			}
			rows = append(rows, page.Rows...)
		}
		sort.SliceStable(rows, func(i, j int) bool {
			if !rows[i].Time.Equal(rows[j].Time) {
				return rows[i].Time.Before(rows[j].Time)
			}
			if rows[i].Index != rows[j].Index {
				return rows[i].Index < rows[j].Index
			}
			return rows[i].ID < rows[j].ID
		})
		msg.rows = rows
		return msg
	}
}

// Update handles loads, the session window and keys. A load for a canceled Seq
// is dropped.
func (e *ESLogs) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	switch m := msg.(type) {
	case esLogsDataMsg:
		if m.seq != e.seq {
			return e, nil
		}
		e.apply(m)
		return e, nil
	case esLogsFieldsMsg:
		if m.seq != e.seq {
			return e, nil
		}
		if m.err == nil {
			e.fields = make([]string, 0, len(m.fields))
			for _, f := range m.fields {
				e.fields = append(e.fields, f.Name)
			}
		}
		return e, nil
	case tui.WindowChangedMsg:
		e.window = m.Window
		return e, e.load()
	case tea.KeyMsg:
		return e, e.handleKey(m)
	}
	return e, nil
}

// apply stores one finished load.
func (e *ESLogs) apply(m esLogsDataMsg) {
	e.loading = false
	e.loaded = e.now()
	if m.err != nil {
		e.err = m.err
		e.rows, e.view = nil, nil
		return
	}
	e.err = nil
	e.rows = m.rows
	if len(e.rows) > esLogsMaxRows {
		e.rows = e.rows[len(e.rows)-esLogsMaxRows:]
	}
	e.full = m.full
	e.cursor = 0
	e.detailTop = 0
	e.refilter()
}

// handleKey routes one key press. While an input owns the keyboard every key is
// text.
func (e *ESLogs) handleKey(k tea.KeyMsg) tea.Cmd {
	if e.mode != esLogsIdle {
		return e.inputKey(k)
	}
	keys := e.keys()

	if e.detail {
		switch {
		case keys.Back.Matches(k.String()) || k.String() == "esc":
			e.detail = false
			return nil
		case keys.Finding.Matches(k.String()):
			return e.finding()
		case keys.Up.Matches(k.String()):
			e.scrollDetail(-1)
			return nil
		case keys.Down.Matches(k.String()):
			e.scrollDetail(1)
			return nil
		case keys.PageUp.Matches(k.String()):
			e.scrollDetail(-e.pageSize())
			return nil
		case keys.PageDown.Matches(k.String()):
			e.scrollDetail(e.pageSize())
			return nil
		}
		return nil
	}

	switch {
	case keys.Filter.Matches(k.String()):
		return e.beginInput(esLogsFilter, "")
	case k.String() == "q":
		return e.beginInput(esLogsQuery, e.query)
	case k.String() == "c":
		return e.beginInput(esLogsColumn, "")
	case keys.Open.Matches(k.String()):
		return e.openDetail()
	case keys.Back.Matches(k.String()):
		if e.filter != "" {
			e.setFilter("")
			return nil
		}
		return tui.Pop()
	case keys.Finding.Matches(k.String()):
		return e.finding()
	case keys.Refresh.Matches(k.String()):
		return e.load()
	case keys.Up.Matches(k.String()):
		e.move(-1)
	case keys.Down.Matches(k.String()):
		e.move(1)
	case keys.PageUp.Matches(k.String()):
		e.move(-e.pageSize())
	case keys.PageDown.Matches(k.String()):
		e.move(e.pageSize())
	case keys.Top.Matches(k.String()):
		e.cursor = 0
	case keys.Bottom.Matches(k.String()):
		if len(e.view) > 0 {
			e.cursor = len(e.view) - 1
		}
	}
	return nil
}

// keys is the session key map, or the default one.
func (e *ESLogs) keys() *keymap.Map {
	if e.env != nil && e.env.Keys != nil {
		return e.env.Keys
	}
	return keymap.Default()
}

// beginInput opens one of the three prompts, pre-filled with value.
func (e *ESLogs) beginInput(mode esLogsMode, value string) tea.Cmd {
	e.mode = mode
	e.input.SetValue(value)
	e.input.CursorEnd()
	e.input.Focus()
	if mode == esLogsColumn && len(e.fields) == 0 {
		return e.loadFields()
	}
	return nil
}

// loadFields fetches the completion list of the first selected source.
func (e *ESLogs) loadFields() tea.Cmd {
	if e.src == nil || len(e.sources) == 0 {
		return nil
	}
	src := e.src
	ref := e.sources[0]
	seq := e.seq
	return func() tea.Msg {
		fields, err := src.FieldCaps(context.Background(), ref.Cluster, ref.Source.Name)
		return esLogsFieldsMsg{seq: seq, fields: fields, err: err}
	}
}

// inputKey edits the active prompt. enter applies it, esc drops it and tab
// completes a field name.
func (e *ESLogs) inputKey(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "enter":
		value := strings.TrimSpace(e.input.Value())
		mode := e.mode
		e.mode = esLogsIdle
		e.input.Blur()
		switch mode {
		case esLogsFilter:
			e.setFilter(value)
			return nil
		case esLogsQuery:
			e.query = value
			return e.load()
		case esLogsColumn:
			if value == "" {
				return tui.Flash(tui.FlashInfo, "a column needs a field name")
			}
			for _, c := range e.columns {
				if c == value {
					return tui.Flash(tui.FlashInfo, "column already shown")
				}
			}
			e.columns = append(e.columns, value)
			return e.load()
		}
		return nil
	case "esc":
		mode := e.mode
		e.mode = esLogsIdle
		e.input.Blur()
		if mode == esLogsFilter {
			e.setFilter("")
		}
		return nil
	case "tab":
		if e.mode == esLogsColumn {
			e.completeField()
		}
		return nil
	}
	var cmd tea.Cmd
	e.input, cmd = e.input.Update(k)
	if e.mode == esLogsFilter {
		e.setFilter(e.input.Value())
	}
	return cmd
}

// completeField fills the input with the first field that has the typed text as
// a prefix.
func (e *ESLogs) completeField() {
	prefix := strings.TrimSpace(e.input.Value())
	if prefix == "" {
		return
	}
	for _, f := range e.fields {
		if strings.HasPrefix(f, prefix) {
			e.input.SetValue(f)
			e.input.CursorEnd()
			return
		}
	}
}

// setFilter installs the local filter: a substring, or a regex after "~".
func (e *ESLogs) setFilter(s string) {
	e.filter = s
	e.filterRe = nil
	e.filterBad = false
	if pat, ok := strings.CutPrefix(s, "~"); ok {
		re, err := regexp.Compile("(?i)" + pat)
		if err != nil {
			e.filterBad = true
		} else {
			e.filterRe = re
		}
	}
	e.refilter()
}

// matches reports whether a row passes the local filter. It looks at the level,
// the host and the message.
func (e *ESLogs) matches(r elastic.LogRow) bool {
	if e.filter == "" {
		return true
	}
	if e.filterBad {
		return false
	}
	text := r.Level + " " + r.Host + " " + r.Message
	if e.filterRe != nil {
		return e.filterRe.MatchString(text)
	}
	return strings.Contains(strings.ToLower(text), strings.ToLower(e.filter))
}

// refilter rebuilds the visible rows, keeping the cursor inside them.
func (e *ESLogs) refilter() {
	out := make([]elastic.LogRow, 0, len(e.rows))
	for _, r := range e.rows {
		if e.matches(r) {
			out = append(out, r)
		}
	}
	e.view = out
	if e.cursor >= len(e.view) {
		e.cursor = len(e.view) - 1
	}
	if e.cursor < 0 {
		e.cursor = 0
	}
}

// move moves the cursor by delta and clamps it.
func (e *ESLogs) move(delta int) {
	e.cursor += delta
	if e.cursor < 0 {
		e.cursor = 0
	}
	if e.cursor >= len(e.view) {
		e.cursor = len(e.view) - 1
	}
	if e.cursor < 0 {
		e.cursor = 0
	}
}

// pageSize is one screen of rows.
func (e *ESLogs) pageSize() int {
	if n := e.h - 3; n > 1 {
		return n
	}
	return 10
}

// openDetail shows the full document of the selected row.
func (e *ESLogs) openDetail() tea.Cmd {
	if len(e.view) == 0 {
		return tui.Flash(tui.FlashInfo, "no log lines yet")
	}
	e.detail = true
	e.detailTop = 0
	return nil
}

// scrollDetail scrolls the document pane.
func (e *ESLogs) scrollDetail(delta int) {
	e.detailTop += delta
	if e.detailTop < 0 {
		e.detailTop = 0
	}
}

// finding records the selected line as an investigation finding.
func (e *ESLogs) finding() tea.Cmd {
	if len(e.view) == 0 {
		return tui.Flash(tui.FlashInfo, "no log lines")
	}
	if e.env == nil {
		return nil
	}
	return e.env.RecordFinding(e.rowText(e.view[e.cursor]))
}

// rowText is the plain text of one row.
func (e *ESLogs) rowText(r elastic.LogRow) string {
	parts := []string{esLogsTimeText(r.Time), r.Level, r.Host, r.Message}
	for _, c := range e.columns {
		parts = append(parts, c+"="+r.Fields[c])
	}
	return strings.Join(parts, "  ")
}

// Close cancels a running load. The view stack calls it on pop and replace.
func (e *ESLogs) Close() error {
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}
	return nil
}

// columnsLayout is the render layout for width w: the fixed columns, the
// columns the user added and the flexible message column.
func (e *ESLogs) columnsLayout(w int) ([]render.TableColumn, []int) {
	cols := []render.TableColumn{
		{Title: "TIME", Width: esLogsTimeWidth},
		{Title: "LEVEL", Width: esLogsLevelWidth},
		{Title: "HOST", Width: esLogsHostWidth},
	}
	for _, name := range e.columns {
		title := strings.ToUpper(name)
		if len(title) > esLogsColumnWidth {
			title = title[:esLogsColumnWidth]
		}
		cols = append(cols, render.TableColumn{Title: title, Width: esLogsColumnWidth})
	}
	cols = append(cols, render.TableColumn{Title: "MESSAGE"})
	return cols, render.ColumnWidths(cols, w)
}

// rowLine renders one log line: time dim, level coloured by its severity, host
// dim and the message in the base style.
func (e *ESLogs) rowLine(r elastic.LogRow, cols []render.TableColumn, widths []int, selected bool) string {
	st := e.theme
	level := st.StatusStyle(esLevelStatus(r.Level))
	dim := st.Dim
	cells := make([]render.Cell, len(cols))
	for i := range cols {
		var text string
		var style func(string) string
		switch {
		case i == 0:
			text = esLogsTimeText(r.Time)
			style = func(s string) string { return dim.Render(s) }
		case i == 1:
			text = r.Level
			style = func(s string) string { return level.Render(s) }
		case i == 2:
			text = r.Host
			style = func(s string) string { return dim.Render(s) }
		case i == len(cols)-1:
			text = r.Message
			style = func(s string) string { return st.Base.Render(s) }
		default:
			text = r.Fields[e.columns[i-3]]
			style = func(s string) string { return st.Faint.Render(s) }
		}
		cells[i] = render.Cell{Text: render.Truncate(text, widths[i]), Style: style}
	}
	line := render.Row(cells, widths, cols)
	if selected {
		line = st.Selected.Render(line)
	}
	return line
}

// banner is the one-line summary: source, window, query text and the count,
// with a trailing "+" when a source came back with a full page.
func (e *ESLogs) banner() string {
	parts := []string{"logs"}
	parts = append(parts, e.sourceText())
	parts = append(parts, windowText(e.window))
	count := render.Int(int64(len(e.view)))
	if e.full {
		count += "+"
	}
	parts = append(parts, count)
	if e.query != "" {
		parts = append(parts, "q="+e.query)
	}
	if e.filter != "" {
		parts = append(parts, "/filter: "+e.filter)
	}
	if e.loading {
		parts = append(parts, "loading")
	}
	return e.theme.Dim.Render(strings.Join(parts, " · "))
}

// sourceText names what is searched: one source, N sources or the whole
// configured set.
func (e *ESLogs) sourceText() string {
	switch len(e.sources) {
	case 0:
		return "no source"
	case 1:
		s := e.sources[0]
		if s.Cluster != "" {
			return s.Cluster + "/" + s.Source.Name
		}
		return s.Source.Name
	}
	return fmt.Sprintf("%d sources", len(e.sources))
}

// Render returns exactly h lines of exactly w cells: the banner, the header,
// the visible rows (or the full document) and a footer.
func (e *ESLogs) Render(w, h int) string {
	e.w, e.h = w, h
	if w <= 0 || h <= 0 {
		return ""
	}
	st := e.theme
	e.input.Width = w - 2

	head := e.headerLine(w)
	if e.detail {
		return e.renderDetail(head, w, h)
	}

	lines := make([]string, 0, h)
	lines = append(lines, head)
	cols, widths := e.columnsLayout(w)
	lines = append(lines, st.Header.Render(render.Header(cols, widths)))

	body := h - 2
	if body < 0 {
		body = 0
	}
	switch {
	case e.err != nil:
		lines = append(lines, render.Fit(st.Crit.Render("error: "+e.err.Error()), w, body))
	case len(e.view) == 0:
		text := "no log lines in the window"
		if e.loading {
			text = "searching…"
		}
		lines = append(lines, render.Fit(st.Dim.Render(text), w, body))
	default:
		start := 0
		if e.cursor >= body && body > 0 {
			start = e.cursor - body + 1
		}
		for i := start; i < len(e.view) && len(lines) < h-1; i++ {
			lines = append(lines, e.rowLine(e.view[i], cols, widths, i == e.cursor))
		}
	}
	if len(lines) < h {
		lines = append(lines, st.Dim.Render(render.Truncate(e.footerText(), w)))
	}
	return render.Fit(strings.Join(lines, "\n"), w, h)
}

// renderDetail draws the full document of the selected row, redacted.
func (e *ESLogs) renderDetail(head string, w, h int) string {
	st := e.theme
	lines := []string{head}
	doc := e.detailDoc()
	body := h - 2
	if body < 0 {
		body = 0
	}
	if body > 0 {
		if e.detailTop > len(doc)-1 {
			e.detailTop = len(doc) - 1
		}
		if e.detailTop < 0 {
			e.detailTop = 0
		}
		shown := doc[e.detailTop:]
		if len(shown) > body {
			shown = shown[:body]
		}
		lines = append(lines, shown...)
	}
	if len(lines) < h {
		lines = append(lines, st.Dim.Render(render.Truncate("full document · up/down scroll · F finding · esc back", w)))
	}
	return render.Fit(strings.Join(lines, "\n"), w, h)
}

// detailDoc is the document pane's text: the redacted document, split into
// lines, or the row's columns when the document is empty.
func (e *ESLogs) detailDoc() []string {
	if len(e.view) == 0 {
		return []string{"no log line"}
	}
	r := e.view[e.cursor]
	raw := r.Doc
	if raw == "" {
		raw = e.rowText(r)
	}
	if e.env != nil && e.env.Redact != nil {
		raw = e.env.Redact.String(raw)
	}
	return strings.Split(raw, "\n")
}

// headerLine is the banner, or the active prompt while one is open.
func (e *ESLogs) headerLine(w int) string {
	switch e.mode {
	case esLogsFilter:
		return e.theme.Accent.Render(render.Truncate("filter> "+e.input.View(), w))
	case esLogsQuery:
		return e.theme.Accent.Render(render.Truncate("query> "+e.input.View(), w))
	case esLogsColumn:
		hint := "column> "
		if len(e.fields) > 0 {
			hint = fmt.Sprintf("column> (%d fields, tab completes) ", len(e.fields))
		}
		return e.theme.Accent.Render(render.Truncate(hint+e.input.View(), w))
	}
	return e.banner()
}

// footerText is the key hint line.
func (e *ESLogs) footerText() string {
	switch e.mode {
	case esLogsFilter:
		return "enter keep · esc clear"
	case esLogsQuery:
		return "enter search · esc cancel"
	case esLogsColumn:
		return "enter add column · tab complete · esc cancel"
	}
	if e.detail {
		return "full document · up/down scroll · F finding · esc back"
	}
	return fmt.Sprintf("%d lines · enter document · q query · c column · / filter · F finding · r reload",
		len(e.view))
}

// esLogsTimeText renders a log timestamp for the time column.
func esLogsTimeText(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// esLevelStatus colours a level.
func esLevelStatus(level string) resource.Status {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "error", "err", "fatal", "critical", "crit", "emergency", "alert":
		return resource.StatusCrit
	case "warn", "warning":
		return resource.StatusWarn
	case "info", "notice", "debug", "trace":
		return resource.StatusOK
	}
	return resource.StatusUnknown
}

// Crumb is "logs (n)".
func (e *ESLogs) Crumb() string {
	return fmt.Sprintf("logs (%d)", len(e.view))
}

// Hints lists the keys the screen binds.
func (e *ESLogs) Hints() []tui.KeyHint {
	if e.mode != esLogsIdle {
		return []tui.KeyHint{
			{Key: "enter", Text: "apply"},
			{Key: "esc", Text: "cancel"},
		}
	}
	if e.detail {
		return []tui.KeyHint{
			{Key: "esc", Text: "back"},
			{Key: "F", Text: "finding"},
		}
	}
	return []tui.KeyHint{
		{Key: "enter", Text: "document"},
		{Key: "q", Text: "query"},
		{Key: "c", Text: "column"},
		{Key: "/", Text: "filter"},
		{Key: "F", Text: "finding"},
		{Key: "esc", Text: "back"},
	}
}

// Subject is the hosts and kind this screen searched.
func (e *ESLogs) Subject() tui.Subject {
	return tui.Subject{
		Hosts: e.hosts,
		Kind:  elastic.KindLog,
		Ref: resource.Ref{
			Backend:   "elastic",
			Kind:      elastic.KindLog,
			Namespace: e.cluster,
			Name:      e.query,
		},
	}
}

// Capturing is true while a prompt owns the keyboard.
func (e *ESLogs) Capturing() bool { return e.mode != esLogsIdle }

// CopyLines is the plain text of the screen, without colour.
func (e *ESLogs) CopyLines() []string {
	out := []string{fmt.Sprintf("logs %s %s", e.sourceText(), windowText(e.window))}
	if e.query != "" {
		out = append(out, "q="+e.query)
	}
	if e.err != nil {
		return append(out, "error: "+e.err.Error())
	}
	if e.detail {
		return append(out, e.detailDoc()...)
	}
	out = append(out, "TIME  LEVEL  HOST  MESSAGE")
	for _, r := range e.view {
		out = append(out, e.rowText(r))
	}
	return out
}
