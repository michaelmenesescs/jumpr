package views

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend"
	"jumpr/internal/backend/grafana"
	"jumpr/internal/errs"
	"jumpr/internal/investigation"
	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
)

// dashCall is one panel query the fake client saw.
type dashCall struct {
	title  string
	from   time.Time
	to     time.Time
	maxDP  int64
	series []grafana.Series
}

// fakeDashboardQuerier is a scripted Grafana client: it answers per panel
// title, counts its in-flight calls and can be slowed down so the concurrency
// cap is observable.
type fakeDashboardQuerier struct {
	mu          sync.Mutex
	calls       []dashCall
	inFlight    int
	maxInFlight int
	delay       time.Duration
	fail        map[string]error
	answer      map[string][]grafana.Series
}

var _ DashboardQuerier = (*fakeDashboardQuerier)(nil)

func (f *fakeDashboardQuerier) QueryPanel(ctx context.Context, p grafana.Panel, iv grafana.Interpolation, from, to time.Time, maxDataPoints int64) ([]grafana.Series, error) {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	f.calls = append(f.calls, dashCall{title: p.Title, from: from, to: to, maxDP: maxDataPoints})
	err := f.fail[p.Title]
	series := f.answer[p.Title]
	f.mu.Unlock()

	if f.delay > 0 {
		time.Sleep(f.delay)
	}

	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if series == nil {
		series = dashSeriesFor(p)
	}
	return series, nil
}

// snapshot returns the recorded calls.
func (f *fakeDashboardQuerier) snapshot() []dashCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dashCall(nil), f.calls...)
}

// testWindow is the fixed session window every dashboard test uses.
func testWindow() resource.TimeWindow {
	to := time.UnixMilli(1763100000000)
	return resource.TimeWindow{From: to.Add(-time.Hour), To: to, Label: "-1h"}
}

// testNow is the clock the dashboard tests run on.
func testNow() time.Time { return time.UnixMilli(1763100000000) }

// dashSeriesFor is the data the fake answers for a panel: three samples, so a
// chart, a sparkline and a bar all have something to draw.
func dashSeriesFor(p grafana.Panel) []grafana.Series {
	base := time.UnixMilli(1763096400000)
	points := []grafana.Point{
		{T: base, V: 0.5},
		{T: base.Add(30 * time.Minute), V: 1.5},
		{T: base.Add(time.Hour), V: 2.5},
	}
	switch p.Title {
	case "node load":
		return []grafana.Series{
			{Name: "node_load1", Labels: map[string]string{"instance": "web-01.example.internal"}, Points: points},
			{Name: "node_load1", Labels: map[string]string{"instance": "db-01.example.internal"},
				Points: []grafana.Point{{T: base, V: 1}, {T: base.Add(time.Hour), V: 3}}},
		}
	}
	if p.Title == "" {
		return []grafana.Series{{Name: "value", Points: points}}
	}
	return []grafana.Series{{Name: p.Title, Points: points}}
}

// dashboardFixture is a dashboard with every panel shape task 120 renders: a
// timeseries, a stat, a gauge, a second timeseries, an unsupported panel and a
// row holding one more timeseries.
func dashboardFixture() *grafana.Dashboard {
	ds := grafana.Datasource{UID: "prom-eu", Type: "prometheus"}
	return &grafana.Dashboard{
		UID:   "abc123",
		Title: "Web fleet",
		Slug:  "web-fleet",
		URL:   "https://grafana.example.internal/d/abc123/web-fleet",
		Variables: []grafana.Variable{
			{Name: "host", Current: []string{"web-01.example.internal"}},
		},
		Panels: []grafana.Panel{
			{ID: 1, Title: "node load", Type: "timeseries", Datasource: ds,
				Targets: []grafana.Target{{RefID: "A", Expr: `node_load1{instance=~"$host"}`}}},
			{ID: 2, Title: "error rate", Type: "stat", Datasource: ds,
				Targets: []grafana.Target{{RefID: "A", Expr: `rate(errors_total[$__rate_interval])`}}},
			{ID: 3, Title: "disk used", Type: "gauge", Datasource: ds,
				Targets: []grafana.Target{{RefID: "A", Expr: `disk_used_percent{instance=~"$host"}`}}},
			{ID: 4, Title: "cpu", Type: "timeseries", Datasource: ds,
				Targets: []grafana.Target{{RefID: "A", Expr: "cpu_a"}, {RefID: "B", Expr: "cpu_b"}}},
			{ID: 5, Title: "notes", Type: "text"},
			{Type: "row", Title: "detail", Panels: []grafana.Panel{
				{ID: 6, Title: "memory", Type: "timeseries", Datasource: ds,
					Targets: []grafana.Target{{RefID: "A", Expr: `mem_free{instance=~"$host"}`}}},
			}},
		},
	}
}

// newTestDashboard builds the grid over the fixture and the fake client, with
// a bare environment (no theme, no keys, a fixed clock).
func newTestDashboard(t *testing.T) (*Dashboard, *fakeDashboardQuerier) {
	t.Helper()
	fake := &fakeDashboardQuerier{}
	env := &tui.Env{Now: testNow}
	v := NewDashboard(env, fake, "eu", dashboardFixture(), testWindow())
	return v, fake
}

// settleDashboard runs a command and every follow-up it produces, feeding the
// messages back into the view: what the Bubble Tea runtime does for it.
func settleDashboard(t *testing.T, v *Dashboard, cmd tea.Cmd) *Dashboard {
	t.Helper()
	for cmd != nil {
		msg := cmd()
		view, next := v.Update(msg)
		if view != nil {
			nextView, ok := view.(*Dashboard)
			if !ok {
				t.Fatalf("Update returned %T, want *Dashboard", view)
			}
			v = nextView
		}
		cmd = next
	}
	return v
}

// dashFrame splits a rendered frame and checks it is exactly w by h cells.
func dashFrame(t *testing.T, out string, w, h int) []string {
	t.Helper()
	lines := strings.Split(out, "\n")
	if len(lines) != h {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), h, out)
	}
	for i, line := range lines {
		if got := render.Width(line); got != w {
			t.Fatalf("line %d is %d cells, want %d: %q", i, got, w, line)
		}
	}
	return lines
}

func TestDashboardGridLayout(t *testing.T) {
	v, _ := newTestDashboard(t)
	if got, want := len(v.panels), 6; got != want {
		t.Fatalf("flattened %d panels, want %d (rows become sections)", got, want)
	}
	if got, want := v.panels[5].section, "detail"; got != want {
		t.Errorf("the row's panel section = %q, want %q", got, want)
	}
	// 1–3 columns depending on the pane width.
	for _, c := range []struct{ width, cols int }{{60, 1}, {100, 2}, {160, 3}} {
		if got := v.cols(c.width); got != c.cols {
			t.Errorf("cols(%d) = %d, want %d", c.width, got, c.cols)
		}
	}

	v = settleDashboard(t, v, v.Init())
	for _, c := range []struct {
		w    int
		want []string
	}{
		// One column of five visible rows: the sixth panel is below the fold.
		{60, []string{"Web fleet", "node load", "error rate", "disk used", "cpu"}},
		{100, []string{"Web fleet", "node load", "error rate", "disk used", "cpu", "notes", "memory"}},
		{160, []string{"Web fleet", "node load", "error rate", "disk used", "cpu", "notes", "memory"}},
	} {
		out := v.Render(c.w, 24)
		body := strings.Join(dashFrame(t, out, c.w, 24), "\n")
		for _, want := range c.want {
			if !strings.Contains(body, want) {
				t.Errorf("%dx24 frame lacks %q:\n%s", c.w, want, out)
			}
		}
	}
}

func TestDashboardPanels(t *testing.T) {
	v, _ := newTestDashboard(t)
	v = settleDashboard(t, v, v.Init())

	// Every queryable panel loaded; the text panel and the row did not query.
	if n := len(v.snapshotPanels()); n != 5 {
		t.Errorf("%d panels are queryable, want 5", n)
	}
	if got := v.panels[0].series; len(got) != 2 {
		t.Errorf("the timeseries panel holds %d series, want 2", len(got))
	}
	if got := v.panels[1].series; len(got) != 1 {
		t.Errorf("the stat panel holds %d series, want 1", len(got))
	}
	if !strings.Contains(v.panels[1].queries[0], "rate(errors_total[") {
		t.Errorf("the stat panel's query was not interpolated: %v", v.panels[1].queries)
	}

	out := v.Render(120, 30)
	body := strings.Join(dashFrame(t, out, 120, 30), "\n")
	if strings.Count(body, "⣿")+strings.Count(body, "⡀")+strings.Count(body, "⠉") == 0 {
		// The braille chart draws braille runes; the exact dots depend on data.
		if !strings.ContainsAny(body, "⡀⠁⠂⠃⠄⠅⠆⠇⡀⢀⣀⣤⣶⣿⣷⣯⣟⡿") {
			t.Errorf("no braille chart was drawn:\n%s", out)
		}
	}
	if !strings.Contains(body, "open in browser (o)") {
		t.Errorf("the unsupported panel does not offer the browser:\n%s", out)
	}
	if !strings.ContainsAny(body, "▁▂▃▄▅▆▇█") {
		t.Errorf("the stat panel drew no sparkline:\n%s", out)
	}
	if !strings.Contains(body, "█") {
		t.Errorf("the gauge panel drew no bar:\n%s", out)
	}
}

// snapshotPanels counts the panels that ran a query.
func (d *Dashboard) snapshotPanels() []int {
	var out []int
	for i, p := range d.panels {
		if p.queryable() {
			out = append(out, i)
		}
	}
	return out
}

func TestDashboardMaxDataPoints(t *testing.T) {
	v, fake := newTestDashboard(t)
	v.Render(100, 24) // fixes the pane width the load uses
	v = settleDashboard(t, v, v.Init())

	calls := fake.snapshot()
	if len(calls) == 0 {
		t.Fatal("no panel queried")
	}
	// 100 cells, two columns, one gap: (100-1)/2 = 49 cells per panel, and a
	// braille chart needs two samples per cell.
	want := int64(49 * 2)
	for _, c := range calls {
		if c.maxDP != want {
			t.Errorf("panel %q asked for %d data points, want %d", c.title, c.maxDP, want)
		}
	}
	if !calls[0].from.Equal(testWindow().From) || !calls[0].to.Equal(testWindow().To) {
		t.Errorf("the panel queried %s..%s, want the session window", calls[0].from, calls[0].to)
	}
}

func TestDashboardConcurrencyCap(t *testing.T) {
	fake := &fakeDashboardQuerier{delay: 15 * time.Millisecond}
	dash := &grafana.Dashboard{UID: "many", Title: "Many panels"}
	for i := 1; i <= 12; i++ {
		dash.Panels = append(dash.Panels, grafana.Panel{
			ID: i, Title: "panel " + string(rune('a'+i-1)), Type: "timeseries",
			Targets: []grafana.Target{{RefID: "A", Expr: "up"}},
		})
	}
	v := NewDashboard(&tui.Env{Now: testNow}, fake, "eu", dash, testWindow())
	v = settleDashboard(t, v, v.Init())

	if got := len(fake.snapshot()); got != 12 {
		t.Fatalf("%d panels queried, want 12", got)
	}
	if fake.maxInFlight > dashboardMaxInFlight {
		t.Errorf("%d panels were in flight, want at most %d", fake.maxInFlight, dashboardMaxInFlight)
	}
	if fake.maxInFlight < 2 {
		t.Errorf("only %d panels were ever in flight, want concurrent loading", fake.maxInFlight)
	}
	for i, p := range v.panels {
		if p.loading {
			t.Errorf("panel %d is still loading after the stream closed", i)
		}
	}
	if v.loading {
		t.Error("the view still reports a load in progress")
	}
}

func TestDashboardPanelErrorKind(t *testing.T) {
	v, fake := newTestDashboard(t)
	fake.fail = map[string]error{"error rate": errs.New(errs.Timeout, "grafana.ds.query", "took too long")}
	v = settleDashboard(t, v, v.Init())

	p := v.panels[1]
	if p.err == nil {
		t.Fatal("the failing panel has no error")
	}
	if got, want := errs.KindOf(p.err), errs.Timeout; got != want {
		t.Fatalf("kind = %q, want %q", got, want)
	}
	out := v.Render(120, 30)
	body := strings.Join(dashFrame(t, out, 120, 30), "\n")
	if !strings.Contains(body, "timeout") {
		t.Errorf("the panel does not show its error kind:\n%s", out)
	}
	// One panel failing must not stop the others.
	if v.panels[0].err != nil || len(v.panels[0].series) == 0 {
		t.Error("a panel error took the other panels down with it")
	}
}

func TestDashboardZoomAndReload(t *testing.T) {
	v, fake := newTestDashboard(t)
	v = settleDashboard(t, v, v.Init())
	v.Render(120, 30)

	// enter zooms the selected panel to the whole pane.
	view, cmd := v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	v = view.(*Dashboard)
	if cmd != nil {
		t.Fatalf("enter produced a command of type %T", cmd())
	}
	if !v.zoom {
		t.Fatal("enter did not zoom the panel")
	}
	out := v.Render(120, 30)
	body := strings.Join(dashFrame(t, out, 120, 30), "\n")
	if !strings.Contains(body, "node load") {
		t.Errorf("the zoomed frame lost the panel title:\n%s", out)
	}
	for _, hidden := range []string{"error rate", "disk used"} {
		if strings.Contains(body, hidden) {
			t.Errorf("the zoomed frame still shows %q:\n%s", hidden, out)
		}
	}

	// esc unzooms instead of popping the view.
	view, cmd = v.Update(tea.KeyMsg{Type: tea.KeyEsc})
	v = view.(*Dashboard)
	if cmd != nil {
		t.Fatalf("esc unzooming produced a command of type %T", cmd())
	}
	if v.zoom {
		t.Fatal("esc did not unzoom")
	}

	// r reloads every panel.
	before := len(fake.snapshot())
	view, cmd = v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	v = view.(*Dashboard)
	v = settleDashboard(t, v, cmd)
	if got := len(fake.snapshot()); got <= before {
		t.Errorf("r did not reload: %d calls, was %d", got, before)
	}

	// A window change reloads over the new range.
	from := testNow().Add(-6 * time.Hour)
	view, cmd = v.Update(tui.WindowChangedMsg{Window: resource.TimeWindow{From: from, ToNow: true, Label: "-6h"}})
	v = view.(*Dashboard)
	v = settleDashboard(t, v, cmd)
	calls := fake.snapshot()
	last := calls[len(calls)-1]
	if !last.from.Equal(from) {
		t.Errorf("after the window change a panel queried from %s, want %s", last.from, from)
	}
	if !last.to.Equal(testNow()) {
		t.Errorf("after the window change a panel queried to %s, want now", last.to)
	}
}

func TestDashboardFindings(t *testing.T) {
	rec := &fakeDashRecorder{}
	fake := &fakeDashboardQuerier{}
	env := &tui.Env{Now: testNow, Recorder: rec}
	v := NewDashboard(env, fake, "eu", dashboardFixture(), testWindow())
	v = settleDashboard(t, v, v.Init())

	view, cmd := v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("F")})
	v = view.(*Dashboard)
	if cmd == nil {
		t.Fatal("F produced no command")
	}
	// The command batches the append with the flash; running both is enough.
	if batch, ok := cmd().(tea.BatchMsg); ok {
		for _, c := range batch {
			if c != nil {
				_ = c()
			}
		}
	}

	entries := rec.entries()
	if len(entries) != 1 {
		t.Fatalf("%d entries recorded, want 1", len(entries))
	}
	if entries[0].Kind != investigation.EntryFinding {
		t.Errorf("entry kind = %q, want %q", entries[0].Kind, investigation.EntryFinding)
	}
	text := entries[0].Text
	for _, want := range []string{"node load", "timeseries"} {
		if !strings.Contains(text, want) {
			t.Errorf("the finding lacks %q:\n%s", want, text)
		}
	}
	output := entries[0].Output
	for _, want := range []string{`node_load1{instance=~"web-01.example.internal"}`, "min 0.5", "max 2.5", "last 2.5"} {
		if !strings.Contains(output, want) {
			t.Errorf("the finding output lacks %q:\n%s", want, output)
		}
	}
}

// fakeDashRecorder is an open investigation that keeps what it is given.
type fakeDashRecorder struct {
	mu   sync.Mutex
	seen []investigation.Entry
}

func (r *fakeDashRecorder) Append(e investigation.Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, e)
	return nil
}

func (r *fakeDashRecorder) Current() (investigation.Meta, bool) {
	return investigation.Meta{ID: "inc-1", Status: "open"}, true
}

func (r *fakeDashRecorder) entries() []investigation.Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]investigation.Entry(nil), r.seen...)
}

// fakeDashBackend is a Grafana backend that only knows how to open URLs.
type fakeDashBackend struct {
	mu     sync.Mutex
	opened []string
}

func (f *fakeDashBackend) Name() string                            { return "grafana" }
func (f *fakeDashBackend) Kinds() []resource.KindSpec              { return nil }
func (f *fakeDashBackend) Links(resource.Resource) []resource.Link { return nil }
func (f *fakeDashBackend) Health(context.Context) backend.Health   { return backend.Health{} }
func (f *fakeDashBackend) List(context.Context, resource.Query) (<-chan backend.Result, error) {
	return nil, errs.New(errs.Config, "test", "not implemented")
}
func (f *fakeDashBackend) Get(context.Context, resource.Ref) (resource.Resource, error) {
	return resource.Resource{}, errs.New(errs.Config, "test", "not implemented")
}

func (f *fakeDashBackend) OpenURL(url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, url)
	return nil
}

func (f *fakeDashBackend) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.opened) == 0 {
		return ""
	}
	return f.opened[len(f.opened)-1]
}

func TestDashboardOpenInBrowser(t *testing.T) {
	reg := backend.NewRegistry()
	gb := &fakeDashBackend{}
	if err := reg.Register(gb); err != nil {
		t.Fatalf("register: %v", err)
	}
	env := &tui.Env{Now: testNow, Registry: reg}
	v := NewDashboard(env, &fakeDashboardQuerier{}, "eu", dashboardFixture(), testWindow())
	v = settleDashboard(t, v, v.Init())

	// The unsupported panel is the fifth; o opens its browser URL.
	v.cursor = 4
	if got, want := v.panels[4].panel.Type, "text"; got != want {
		t.Fatalf("panel 5 is a %q panel, want %q", got, want)
	}
	_, cmd := v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	if cmd == nil {
		t.Fatal("o produced no command")
	}
	_ = cmd()
	if got, want := gb.last(), "https://grafana.example.internal/d/abc123/web-fleet?viewPanel=5"; got != want {
		t.Errorf("opened %q, want %q", got, want)
	}
}

func TestDashboardNoBrowserConfigured(t *testing.T) {
	v, _ := newTestDashboard(t) // bare env: no registry
	v = settleDashboard(t, v, v.Init())
	_, cmd := v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	if cmd == nil {
		t.Fatal("o produced no command")
	}
	msg, ok := cmd().(tui.FlashMsg)
	if !ok {
		t.Fatalf("o produced %T, want a flash", cmd())
	}
	if msg.Level != tui.FlashWarn {
		t.Errorf("flash level = %v, want warn", msg.Level)
	}
}

func TestDashboardStaleLoadIsDropped(t *testing.T) {
	v, fake := newTestDashboard(t)
	v.Render(100, 24)
	// A sequence that is not the current one must change nothing.
	stale := dashboardBatchMsg{seq: v.seq + 1, results: []dashboardResultMsg{{
		seq: v.seq + 1, index: 0,
		series: []grafana.Series{{Name: "stale", Points: []grafana.Point{{V: 9}}}},
	}}}
	view, cmd := v.Update(stale)
	v = view.(*Dashboard)
	if cmd != nil {
		t.Fatal("a stale batch produced a command")
	}
	if len(v.panels[0].series) != 0 {
		t.Errorf("a stale batch reached the grid: %v", v.panels[0].series)
	}
	if len(fake.snapshot()) != 0 {
		t.Errorf("a stale batch ran %d queries", len(fake.snapshot()))
	}

	// A canceled load stops delivering: the view drops later messages.
	v = settleDashboard(t, v, v.Init())
	if v.panels[0].err != nil {
		t.Errorf("panel 0 failed: %v", v.panels[0].err)
	}
}

func TestDashboardErrorsDoNotPanic(t *testing.T) {
	v := NewDashboard(&tui.Env{Now: testNow}, nil, "eu", nil, testWindow())
	if cmd := v.Init(); cmd != nil {
		t.Fatalf("a dashboard-less view produced a command: %T", cmd())
	}
	out := v.Render(40, 6)
	lines := dashFrame(t, out, 40, 6)
	if !strings.Contains(strings.Join(lines, "\n"), "no dashboard") {
		t.Errorf("the frame does not explain itself:\n%s", out)
	}

	// A nil env, a nil clock and a zero size must not panic either.
	bare := NewDashboard(nil, nil, "", dashboardFixture(), resource.TimeWindow{})
	if got := bare.Render(0, 0); got != "" {
		t.Errorf("Render(0,0) = %q, want the empty string", got)
	}
	bare = settleDashboard(t, bare, bare.Init())
	if bare.err == nil && bare.panels[0].err == nil {
		t.Errorf("a view without a client reported no error: %+v", bare.panels[0])
	}
	if got := errs.KindOf(bare.panels[0].err); got != errs.Config {
		t.Errorf("kind = %q, want %q", got, errs.Config)
	}
}

func TestDashboardViewInterface(t *testing.T) {
	v, _ := newTestDashboard(t)
	v = settleDashboard(t, v, v.Init())

	if got := v.Crumb(); got != "dashboard Web fleet" {
		t.Errorf("Crumb() = %q", got)
	}
	if v.Capturing() {
		t.Error("the grid captures no keys")
	}
	sub := v.Subject()
	if sub.Ref.Name != "abc123" || sub.Ref.Namespace != "eu" {
		t.Errorf("Subject = %+v", sub.Ref)
	}
	if len(v.CopyLines()) == 0 || !strings.Contains(v.CopyLines()[0], "Web fleet") {
		t.Errorf("CopyLines = %v", v.CopyLines())
	}
	if len(v.Hints()) == 0 {
		t.Error("the view binds no keys")
	}
}
