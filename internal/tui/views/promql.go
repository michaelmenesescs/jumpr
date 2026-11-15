package views

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend/prom"
	"jumpr/internal/errs"
	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
	"jumpr/internal/tui/theme"
)

const (
	// promQLSparkWidth is the width of the per-row sparkline column.
	promQLSparkWidth = 24
	// promQLValueWidth is the width of the value column.
	promQLValueWidth = 14
	// promQLHistoryMax is how many expressions the history keeps.
	promQLHistoryMax = 50
	// promQLMinLabels is the narrowest labels column the table will draw.
	promQLMinLabels = 8
	// promQLDefaultWindow is the range used when the session has no window.
	promQLDefaultWindow = time.Hour
)

// PromQuerier is the part of the Prometheus client the view runs queries
// through. *prom.Client implements it.
type PromQuerier interface {
	Query(ctx context.Context, expr string, ts time.Time) (prom.Vector, error)
	QueryRange(ctx context.Context, expr string, from, to time.Time, step time.Duration) (prom.Matrix, error)
}

// promQLSource is what the view needs from the Prometheus backend in the
// registry: the queryable clients under their configured names.
type promQLSource interface {
	Queriers() []prom.NamedQuerier
}

// promQLDataMsg is one finished load: an instant result and the range result
// that gives every row its sparkline and the chart its lines.
type promQLDataMsg struct {
	seq    int
	vector prom.Vector
	matrix prom.Matrix
	err    error
}

// promQLRow is one instant-query result.
type promQLRow struct {
	labels string
	value  float64
	spark  []float64
}

// PromQL is the `:promql <expr>` screen: the instant result as a table with a
// sparkline per row, `enter` for the full-pane braille chart, `e` to edit the
// expression and a history of the expressions that were run. Every query uses
// the session time window and reloads when it changes (SPEC §8.3).
type PromQL struct {
	env    *tui.Env
	src    PromQuerier
	server string
	expr   string
	window resource.TimeWindow

	input     textinput.Model
	editing   bool
	history   []string
	histIdx   int
	histDraft string

	rows    []promQLRow
	matrix  prom.Matrix
	chartOn bool
	cursor  int

	loading bool
	err     error

	seq    int
	cancel context.CancelFunc

	w, h int
}

var _ tui.View = (*PromQL)(nil)

// NewPromQL builds the view for expr over window, running its queries through
// src (one configured Prometheus server, named server).
func NewPromQL(env *tui.Env, src PromQuerier, server, expr string, window resource.TimeWindow) *PromQL {
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 0
	p := &PromQL{
		env:     env,
		src:     src,
		server:  strings.TrimSpace(server),
		expr:    strings.TrimSpace(expr),
		window:  window,
		input:   in,
		histIdx: -1,
	}
	if p.server == "" {
		p.server = "prom"
	}
	if p.expr != "" {
		p.history = []string{p.expr}
	}
	return p
}

// Init runs the first query.
func (p *PromQL) Init() tea.Cmd { return p.load() }

// now is the clock handed in by the environment.
func (p *PromQL) now() time.Time {
	if p.env != nil && p.env.Now != nil {
		return p.env.Now()
	}
	return time.Now()
}

// bounds is the range every query uses: the session window, or the last hour
// when the session has none.
func (p *PromQL) bounds() (time.Time, time.Time) {
	if p.window.IsZero() {
		t := p.now()
		return t.Add(-promQLDefaultWindow), t
	}
	return p.window.Bounds(p.now())
}

// load cancels the previous load and starts a new one. Both queries run inside
// the returned tea.Cmd, never in Update.
func (p *PromQL) load() tea.Cmd {
	if p.src == nil {
		p.err = errs.New(errs.Config, "views.promql", "no prometheus client")
		return nil
	}
	if p.cancel != nil {
		p.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	p.seq++
	seq := p.seq
	p.loading = true
	p.err = nil

	src := p.src
	expr := p.expr
	from, to := p.bounds()
	step := prom.Step(from, to, prom.MaxPoints)

	return func() tea.Msg {
		vec, err := src.Query(ctx, expr, to)
		if err != nil {
			return promQLDataMsg{seq: seq, err: err}
		}
		mat, err := src.QueryRange(ctx, expr, from, to, step)
		if err != nil {
			return promQLDataMsg{seq: seq, err: err}
		}
		return promQLDataMsg{seq: seq, vector: vec, matrix: mat}
	}
}

// Update handles loads, the session window and keys. Stale loads are dropped.
func (p *PromQL) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	switch m := msg.(type) {
	case promQLDataMsg:
		if m.seq != p.seq {
			return p, nil
		}
		p.apply(m)
		return p, nil
	case tui.WindowChangedMsg:
		p.window = m.Window
		return p, p.load()
	case tea.KeyMsg:
		return p, p.handleKey(m)
	}
	return p, nil
}

// apply stores one finished load.
func (p *PromQL) apply(m promQLDataMsg) {
	p.loading = false
	if m.err != nil {
		p.err = m.err
		p.rows = nil
		p.matrix = nil
		return
	}
	sparks := make(map[string][]float64, len(m.matrix))
	for _, s := range m.matrix {
		sparks[prom.LabelString(s.Metric)] = promQLValues(s.Values)
	}
	rows := make([]promQLRow, 0, len(m.vector))
	for _, s := range m.vector {
		labels := prom.LabelString(s.Metric)
		v := math.NaN()
		if len(s.Values) > 0 {
			v = s.Values[len(s.Values)-1].V
		}
		rows = append(rows, promQLRow{labels: labels, value: v, spark: sparks[labels]})
	}
	p.rows = rows
	p.matrix = m.matrix
	p.chartOn = false
	p.cursor = 0
}

// handleKey routes one key to the expression input or the result list.
func (p *PromQL) handleKey(k tea.KeyMsg) tea.Cmd {
	if p.editing {
		switch k.String() {
		case "enter":
			expr := strings.TrimSpace(p.input.Value())
			if expr == "" {
				return tui.Flash(tui.FlashError, "promql needs an expression")
			}
			p.editing = false
			p.input.Blur()
			p.setExpr(expr)
			return p.load()
		case "esc":
			p.editing = false
			p.input.Blur()
			p.input.SetValue(p.expr)
			p.histIdx = -1
			return nil
		case "up":
			p.historyPrev()
			return nil
		case "down":
			p.historyNext()
			return nil
		}
		var cmd tea.Cmd
		p.input, cmd = p.input.Update(k)
		return cmd
	}

	switch k.String() {
	case "enter":
		if len(p.rows) == 0 && len(p.matrix) == 0 {
			return tui.Flash(tui.FlashInfo, "nothing to chart yet")
		}
		p.chartOn = !p.chartOn
	case "e":
		p.beginEdit()
	case "up", "k":
		if p.cursor > 0 {
			p.cursor--
		}
	case "down", "j":
		if p.cursor+1 < len(p.rows) {
			p.cursor++
		}
	case "r":
		return p.load()
	case "esc":
		if p.chartOn {
			p.chartOn = false
		}
	}
	return nil
}

// beginEdit puts the expression in the input for editing.
func (p *PromQL) beginEdit() {
	p.editing = true
	p.histIdx = -1
	p.histDraft = p.expr
	p.input.SetValue(p.expr)
	p.input.CursorEnd()
	p.input.Focus()
}

// setExpr records a new expression and its history entry.
func (p *PromQL) setExpr(expr string) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return
	}
	p.expr = expr
	if len(p.history) == 0 || p.history[len(p.history)-1] != expr {
		p.history = append(p.history, expr)
		if len(p.history) > promQLHistoryMax {
			p.history = p.history[len(p.history)-promQLHistoryMax:]
		}
	}
	p.histIdx = -1
}

// historyPrev steps back through the expressions already run, remembering the
// text that was being typed.
func (p *PromQL) historyPrev() {
	if len(p.history) == 0 {
		return
	}
	switch {
	case p.histIdx == -1:
		p.histDraft = p.input.Value()
		p.histIdx = len(p.history) - 1
	case p.histIdx > 0:
		p.histIdx--
	default:
		return
	}
	p.input.SetValue(p.history[p.histIdx])
	p.input.CursorEnd()
}

// historyNext steps forward again, and back to the draft past the newest entry.
func (p *PromQL) historyNext() {
	if p.histIdx == -1 {
		return
	}
	if p.histIdx+1 < len(p.history) {
		p.histIdx++
		p.input.SetValue(p.history[p.histIdx])
		p.input.CursorEnd()
		return
	}
	p.histIdx = -1
	p.input.SetValue(p.histDraft)
	p.input.CursorEnd()
}

// style is the theme, or a fresh dark one in a bare test.
func (p *PromQL) style() *theme.Theme {
	if p.env != nil && p.env.Theme != nil {
		return p.env.Theme
	}
	return theme.Dark()
}

// glyphs is the active glyph set, defaulting to the unicode one.
func (p *PromQL) glyphs() render.GlyphSet {
	if p.env != nil && p.env.Glyphs.Name != "" {
		return p.env.Glyphs
	}
	set, err := render.Glyphs("unicode")
	if err != nil {
		return render.GlyphSet{}
	}
	return set
}

// Render returns exactly h lines of exactly w cells: the expression, the
// result table or the chart, and a footer.
func (p *PromQL) Render(w, h int) string {
	p.w, p.h = w, h
	if w <= 0 || h <= 0 {
		return ""
	}
	st := p.style()
	p.input.Width = w - 1

	head := p.header(w, st)
	foot := st.Dim.Render(render.Truncate(p.footerText(), w))
	body := ""
	if bodyH := h - 2; bodyH > 0 {
		if p.chartOn {
			body = render.Chart(p.chartSeries(), w, bodyH)
		} else {
			body = p.tableBody(w, bodyH, st)
		}
	}
	return render.Fit(strings.Join([]string{head, body, foot}, "\n"), w, h)
}

// header is the expression line, or the input while it is being edited.
func (p *PromQL) header(w int, st *theme.Theme) string {
	if p.editing {
		p.input.Width = w - len("promql> ")
		return st.Accent.Render(render.Truncate("promql> "+p.input.View(), w))
	}
	line := fmt.Sprintf("promql · %s · %s", p.expr, p.boundsLabel())
	if p.loading {
		line += " · loading"
	}
	return st.Title.Render(render.Truncate(line, w))
}

// boundsLabel is the window the queries cover.
func (p *PromQL) boundsLabel() string {
	from, to := p.bounds()
	span := to.Sub(from)
	if label := p.window.String(); label != "-" {
		return label
	}
	return render.Duration(span) + " to now"
}

// footerText is the key hint line.
func (p *PromQL) footerText() string {
	switch {
	case p.editing:
		return "enter run · esc cancel · up/down history"
	case p.chartOn:
		return "enter table · e edit · esc back"
	}
	return fmt.Sprintf("enter chart · e edit · up/down select · r reload · %d series", len(p.rows))
}

// tableBody renders the column header and the visible rows.
func (p *PromQL) tableBody(w, bodyH int, st *theme.Theme) string {
	if bodyH <= 0 {
		return ""
	}
	if p.err != nil {
		return render.Fit(st.Crit.Render(errText(p.err)), w, bodyH)
	}
	if len(p.rows) == 0 {
		if p.loading {
			return render.Fit(st.Dim.Render("querying…"), w, bodyH)
		}
		return render.Fit(st.Dim.Render("no series"), w, bodyH)
	}

	sparkW, valueW := promQLSparkWidth, promQLValueWidth
	if w < promQLMinLabels+valueW+sparkW+2 {
		sparkW = w / 3
		valueW = w / 4
	}
	labelW := w - valueW - sparkW - 2
	if labelW < 1 {
		labelW = 1
	}

	lines := make([]string, 0, bodyH)
	lines = append(lines, st.Header.Render(
		render.PadRight("LABELS", labelW)+" "+
			render.PadLeft("VALUE", valueW)+" "+
			render.PadRight("WINDOW", sparkW)))

	visible := bodyH - 1
	top := 0
	if visible > 0 && p.cursor >= visible {
		top = p.cursor - visible + 1
	}
	for i := top; i < len(p.rows) && len(lines) < bodyH; i++ {
		row := p.rows[i]
		line := render.PadRight(render.Truncate(row.labels, labelW), labelW) + " " +
			render.PadLeft(render.Truncate(promQLValue(row.value), valueW), valueW) + " " +
			render.PadRight(render.SparkWith(p.glyphs(), row.spark, sparkW), sparkW)
		if i == p.cursor {
			line = st.Selected.Render(render.PadRight(line, w))
		}
		lines = append(lines, line)
	}
	return render.Fit(strings.Join(lines, "\n"), w, bodyH)
}

// chartSeries converts the range result into chart lines: one per series, up to
// the chart's four colours.
func (p *PromQL) chartSeries() []render.Series {
	n := len(p.matrix)
	if n > render.MaxChartSeries {
		n = render.MaxChartSeries
	}
	out := make([]render.Series, 0, n)
	for i := 0; i < n; i++ {
		s := p.matrix[i]
		points := make([]render.Point, 0, len(s.Values))
		for _, v := range s.Values {
			points = append(points, render.Point{T: v.T, V: v.V})
		}
		out = append(out, render.Series{Name: prom.LabelString(s.Metric), Color: i, Points: points})
	}
	return out
}

// Crumb is the expression this screen is about.
func (p *PromQL) Crumb() string { return "promql " + p.expr }

// Hints are the keys the view binds.
func (p *PromQL) Hints() []tui.KeyHint {
	if p.editing {
		return []tui.KeyHint{
			{Key: "enter", Text: "run"},
			{Key: "esc", Text: "cancel"},
			{Key: "up/down", Text: "history"},
		}
	}
	return []tui.KeyHint{
		{Key: "enter", Text: "chart"},
		{Key: "e", Text: "edit expression"},
		{Key: "r", Text: "reload"},
	}
}

// Subject is the query this screen evaluated.
func (p *PromQL) Subject() tui.Subject {
	return tui.Subject{
		Kind: prom.KindSeries,
		Ref: resource.Ref{
			Backend:   "prom",
			Kind:      prom.KindSeries,
			Namespace: p.server,
			Name:      p.expr,
		},
	}
}

// Capturing is true while the expression input owns the keyboard.
func (p *PromQL) Capturing() bool { return p.editing }

// CopyLines is the plain text of the screen, without colour.
func (p *PromQL) CopyLines() []string {
	out := []string{"promql " + p.expr}
	if p.err != nil {
		return append(out, errText(p.err))
	}
	for _, row := range p.rows {
		out = append(out, row.labels+" "+promQLValue(row.value))
	}
	return out
}

// promQLValue renders one sample value. NaN and the infinities keep their
// Prometheus spelling.
func promQLValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', 6, 64)
}

// promQLValues extracts the values of a range series, keeping NaN gaps.
func promQLValues(samples []prom.Sample) []float64 {
	out := make([]float64, 0, len(samples))
	for _, s := range samples {
		out = append(out, s.V)
	}
	return out
}

// errText is the message line for a failed query.
func errText(err error) string {
	return "error: " + err.Error()
}

// newPromQLFromEnv builds the view for expr over the session window, using the
// first configured Prometheus server in the registry. It does no I/O.
func newPromQLFromEnv(env *tui.Env, expr string) (*PromQL, error) {
	if env == nil || env.Registry == nil {
		return nil, errs.New(errs.Config, "views.promql", "no backends configured")
	}
	for _, b := range env.Registry.Backends() {
		src, ok := b.(promQLSource)
		if !ok {
			continue
		}
		qs := src.Queriers()
		if len(qs) == 0 {
			continue
		}
		var window resource.TimeWindow
		if env.Session != nil {
			window = env.Session.Window
		}
		return NewPromQL(env, qs[0].Querier, qs[0].Name, expr, window), nil
	}
	return nil, errs.New(errs.Config, "views.promql", "no prometheus server configured")
}

// RegisterPromQL registers the `:promql <expr>` command. app calls it once at
// startup, after RegisterBuiltins.
func RegisterPromQL(c *tui.Commands) error {
	if c == nil {
		return errs.New(errs.Config, "views.RegisterPromQL", "no command registry")
	}
	return c.Register(tui.Command{
		Name:  "promql",
		Usage: ":promql <expression>",
		Help:  "run a PromQL query over the session window",
		Run: func(env *tui.Env, args string) tea.Cmd {
			expr := strings.TrimSpace(args)
			if expr == "" {
				return tui.Flash(tui.FlashError, "promql needs an expression")
			}
			v, err := newPromQLFromEnv(env, expr)
			if err != nil {
				return tui.Flash(tui.FlashError, "%v", err)
			}
			return func() tea.Msg { return tui.PushViewMsg{View: v} }
		},
	})
}
