package views

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend"
	"jumper/internal/backend/prom"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/tui"
)

// fakePromQuerier is a scripted Prometheus client.
type fakePromQuerier struct {
	mu       sync.Mutex
	vector   prom.Vector
	matrix   prom.Matrix
	err      error
	queries  []string
	instants []time.Time
	ranges   [][2]time.Time
	steps    []time.Duration
}

var _ PromQuerier = (*fakePromQuerier)(nil)

func (f *fakePromQuerier) Query(ctx context.Context, expr string, ts time.Time) (prom.Vector, error) {
	f.mu.Lock()
	f.queries = append(f.queries, expr)
	f.instants = append(f.instants, ts)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	v := f.vector
	return v, nil
}

func (f *fakePromQuerier) QueryRange(ctx context.Context, expr string, from, to time.Time, step time.Duration) (prom.Matrix, error) {
	f.mu.Lock()
	f.ranges = append(f.ranges, [2]time.Time{from, to})
	f.steps = append(f.steps, step)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.matrix, nil
}

// promQLFixture is one instant and range result: two instances of `up`.
func promQLFixture(t *testing.T) (prom.Vector, prom.Matrix) {
	t.Helper()
	base := time.Unix(1763100000, 0)
	web := map[string]string{"__name__": "up", "job": "node", "instance": "web-01.example.internal:9100"}
	db := map[string]string{"__name__": "up", "job": "node", "instance": "db-01.example.internal:9100"}
	vec := prom.Vector{
		{Metric: web, Values: []prom.Sample{{T: base, V: 1}}},
		{Metric: db, Values: []prom.Sample{{T: base, V: 0}}},
	}
	mat := prom.Matrix{
		{Metric: web, Values: []prom.Sample{
			{T: base.Add(-5 * time.Minute), V: 0.5},
			{T: base.Add(-4 * time.Minute), V: 0.75},
			{T: base, V: 1},
		}},
		{Metric: db, Values: []prom.Sample{
			{T: base.Add(-5 * time.Minute), V: 1},
			{T: base, V: 0},
		}},
	}
	return vec, mat
}

// newTestPromQL builds a view over a fake client and a fixed clock.
func newTestPromQL(t *testing.T) (*PromQL, *fakePromQuerier) {
	t.Helper()
	vec, mat := promQLFixture(t)
	fake := &fakePromQuerier{vector: vec, matrix: mat}
	env := &tui.Env{
		Now:   func() time.Time { return time.Unix(1763100000, 0) },
		Theme: nil,
	}
	v := NewPromQL(env, fake, "eu", `up{job="node"}`, resource.TimeWindow{From: time.Unix(1763100000, 0).Add(-time.Hour), To: time.Unix(1763100000, 0), Label: "-1h"})
	return v, fake
}

// loadOne runs Init's command and feeds the result back in.
func loadOne(t *testing.T, v *PromQL, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command to run")
	}
	msg := cmd()
	view, next := v.Update(msg)
	if view != nil {
		*v = *view.(*PromQL)
	}
	if next != nil {
		t.Fatalf("load produced a follow-up command of type %T", next())
	}
}

// frame splits a rendered view and checks it is exactly w by h.
func frame(t *testing.T, out string, w, h int) []string {
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

func TestPromQLInstantTable(t *testing.T) {
	v, fake := newTestPromQL(t)
	loadOne(t, v, v.Init())

	if len(fake.queries) != 1 || len(fake.ranges) != 1 {
		t.Fatalf("queries = %d, ranges = %d, want one each", len(fake.queries), len(fake.ranges))
	}
	if got, want := fake.queries[0], `up{job="node"}`; got != want {
		t.Errorf("expression = %q, want %q", got, want)
	}
	if got := fake.instants[0]; !got.Equal(time.Unix(1763100000, 0)) {
		t.Errorf("instant query at %s, want the window end", got)
	}
	if got, want := len(v.rows), 2; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}

	out := v.Render(100, 10)
	lines := frame(t, out, 100, 10)
	body := strings.Join(lines, "\n")
	if !strings.Contains(body, "web-01.example.internal:9100") {
		t.Errorf("the labels are missing:\n%s", out)
	}
	if !strings.Contains(body, "LABELS") || !strings.Contains(body, "VALUE") || !strings.Contains(body, "WINDOW") {
		t.Errorf("the column headers are missing:\n%s", out)
	}
	if !strings.Contains(body, "2 series") {
		t.Errorf("the footer does not count the rows:\n%s", out)
	}
	// A sparkline per row: each row's window column carries a block rune.
	if strings.Count(body, "▁")+strings.Count(body, "█")+strings.Count(body, "▅") == 0 {
		t.Errorf("no sparkline was drawn:\n%s", out)
	}
}

func TestPromQLChart(t *testing.T) {
	v, _ := newTestPromQL(t)
	loadOne(t, v, v.Init())

	_, cmd := v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("enter returned a command: %T", cmd())
	}
	if !v.chartOn {
		t.Fatal("enter did not switch to the chart")
	}
	out := v.Render(80, 12)
	lines := frame(t, out, 80, 12)
	body := strings.Join(lines, "\n")
	braille := 0
	for _, r := range body {
		if r > 0x2800 && r <= 0x28ff {
			braille++
		}
	}
	if braille == 0 {
		t.Fatalf("the chart drew no braille dots:\n%s", out)
	}
	// Two series, two colours.
	if !strings.Contains(body, "\x1b[38;5;75m") || !strings.Contains(body, "\x1b[38;5;214m") {
		t.Errorf("both series should be coloured:\n%q", body)
	}
	if !strings.Contains(lines[11], "enter table") {
		t.Errorf("the chart footer is wrong: %q", lines[11])
	}
}

func TestPromQLWindowChangedReloads(t *testing.T) {
	v, fake := newTestPromQL(t)
	loadOne(t, v, v.Init())

	window := resource.TimeWindow{
		From:  time.Unix(1763100000, 0).Add(-6 * time.Hour),
		To:    time.Unix(1763100000, 0),
		Label: "-6h",
	}
	_, cmd := v.Update(tui.WindowChangedMsg{Window: window})
	if cmd == nil {
		t.Fatal("a window change started no query")
	}
	loadOne(t, v, cmd)
	if got, want := len(fake.ranges), 2; got != want {
		t.Fatalf("range queries = %d, want %d", got, want)
	}
	if got, want := fake.ranges[1][0], window.From; !got.Equal(want) {
		t.Errorf("reloaded from %s, want %s", got, want)
	}
	if got := window.From; !v.bounds0().Equal(got) {
		t.Errorf("view bounds start = %s, want %s", v.bounds0(), got)
	}
}

// bounds0 is the lower bound of the view's current window.
func (p *PromQL) bounds0() time.Time {
	from, _ := p.bounds()
	return from
}

func TestPromQLDropsStaleLoad(t *testing.T) {
	v, fake := newTestPromQL(t)
	first := v.Init()
	staleMsg := first()

	// A second load (the window changed) supersedes the first.
	window := resource.TimeWindow{From: time.Unix(1763100000, 0).Add(-2 * time.Hour), To: time.Unix(1763100000, 0), Label: "-2h"}
	_, cmd := v.Update(tui.WindowChangedMsg{Window: window})
	if cmd == nil {
		t.Fatal("the window change started no query")
	}
	fake.mu.Lock()
	fake.vector = prom.Vector{{Metric: map[string]string{"__name__": "up", "job": "node", "instance": "new.example.internal:9100"}, Values: []prom.Sample{{T: time.Unix(1763100000, 0), V: 3}}}}
	fake.mu.Unlock()
	loadOne(t, v, cmd)

	// The stale message arrives late and must be ignored.
	v.Update(staleMsg)
	if got, want := len(v.rows), 1; got != want {
		t.Fatalf("rows = %d, want %d (the stale load was applied)", got, want)
	}
	if !strings.Contains(v.rows[0].labels, "new.example.internal") {
		t.Errorf("row = %q, want the newest load", v.rows[0].labels)
	}
}

func TestPromQLHistoryAndEdit(t *testing.T) {
	v, _ := newTestPromQL(t)
	loadOne(t, v, v.Init())

	if _, cmd := v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}}); cmd != nil {
		t.Fatalf("e returned a command: %T", cmd())
	}
	if !v.editing || !v.Capturing() {
		t.Fatal("e did not open the expression editor")
	}
	if got := v.input.Value(); got != `up{job="node"}` {
		t.Errorf("input = %q, want the current expression", got)
	}

	// Edit it and run it.
	v.input.SetValue("sum(up)")
	_, cmd := v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on a new expression started no query")
	}
	if got, want := v.expr, "sum(up)"; got != want {
		t.Errorf("expression = %q, want %q", got, want)
	}
	if len(v.history) != 2 || v.history[1] != "sum(up)" {
		t.Errorf("history = %v, want the run expressions", v.history)
	}
	loadOne(t, v, cmd)

	// up walks the history, down comes back to the draft.
	v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	v.input.SetValue("half typed")
	if _, cmd := v.Update(tea.KeyMsg{Type: tea.KeyUp}); cmd != nil {
		t.Fatalf("history navigation returned a command: %T", cmd())
	}
	if got, want := v.input.Value(), "sum(up)"; got != want {
		t.Errorf("after one up the input = %q, want %q", got, want)
	}
	v.Update(tea.KeyMsg{Type: tea.KeyUp})
	if got, want := v.input.Value(), `up{job="node"}`; got != want {
		t.Errorf("after two ups the input = %q, want %q", got, want)
	}
	v.Update(tea.KeyMsg{Type: tea.KeyDown})
	if got, want := v.input.Value(), "sum(up)"; got != want {
		t.Errorf("after one down the input = %q, want %q", got, want)
	}
	v.Update(tea.KeyMsg{Type: tea.KeyDown})
	if got, want := v.input.Value(), "half typed"; got != want {
		t.Errorf("past the newest entry the input = %q, want the draft %q", got, want)
	}

	// esc drops the edit.
	v.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if v.editing || v.Capturing() {
		t.Error("esc did not close the editor")
	}
}

func TestPromQLErrorRender(t *testing.T) {
	v, fake := newTestPromQL(t)
	fake.err = errors.New(`bad_data: parse error: unexpected identifier "foo"`)
	loadOne(t, v, v.Init())

	out := v.Render(60, 6)
	frame(t, out, 60, 6)
	if !strings.Contains(out, "error:") || !strings.Contains(out, "parse error") {
		t.Errorf("the error is not shown:\n%s", out)
	}
	if len(v.CopyLines()) != 2 {
		t.Errorf("CopyLines = %v, want the expression and the error", v.CopyLines())
	}
}

func TestPromQLSubjectAndCrumb(t *testing.T) {
	v, _ := newTestPromQL(t)
	loadOne(t, v, v.Init())

	if got, want := v.Crumb(), `promql up{job="node"}`; got != want {
		t.Errorf("Crumb = %q, want %q", got, want)
	}
	sub := v.Subject()
	if sub.Kind != prom.KindSeries || sub.Ref.Backend != "prom" || sub.Ref.Namespace != "eu" {
		t.Errorf("Subject = %+v", sub)
	}
	if sub.Ref.Name != `up{job="node"}` {
		t.Errorf("Subject.Name = %q, want the expression", sub.Ref.Name)
	}
	if len(v.Hints()) == 0 {
		t.Error("Hints is empty")
	}
	if got := len(v.CopyLines()); got != 3 {
		t.Errorf("CopyLines has %d lines, want the expression and two rows", got)
	}
}

// fakePromBackend is a registry entry the :promql command can find.
type fakePromBackend struct {
	q   *fakePromQuerier
	bad bool
}

var _ backend.Backend = (*fakePromBackend)(nil)

func (f *fakePromBackend) Name() string                            { return "prom" }
func (f *fakePromBackend) Kinds() []resource.KindSpec              { return prom.New(prom.Options{}).Kinds() }
func (f *fakePromBackend) Links(resource.Resource) []resource.Link { return nil }
func (f *fakePromBackend) Health(context.Context) backend.Health {
	return backend.Health{State: backend.HealthOK}
}
func (f *fakePromBackend) Get(context.Context, resource.Ref) (resource.Resource, error) {
	return resource.Resource{}, nil
}
func (f *fakePromBackend) List(context.Context, resource.Query) (<-chan backend.Result, error) {
	ch := make(chan backend.Result)
	close(ch)
	return ch, nil
}

func (f *fakePromBackend) Queriers() []prom.NamedQuerier {
	if f.bad {
		return nil
	}
	return []prom.NamedQuerier{{Name: "eu", Querier: f.q}}
}

func TestPromQLCommand(t *testing.T) {
	reg := backend.NewRegistry()
	q := &fakePromQuerier{}
	if err := reg.Register(&fakePromBackend{q: q}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	env := &tui.Env{Registry: reg, Session: &tui.SessionState{Name: "main"}, Now: time.Now}

	cmds := tui.NewCommands()
	if err := RegisterPromQL(cmds); err != nil {
		t.Fatalf("RegisterPromQL: %v", err)
	}
	cmd, ok := cmds.Lookup("promql")
	if !ok {
		t.Fatal("the promql command is not registered")
	}

	msg := cmd.Run(env, `up{job="node"}`)()
	push, ok := msg.(tui.PushViewMsg)
	if !ok {
		t.Fatalf("Run returned %T, want PushViewMsg", msg)
	}
	view, ok := push.View.(*PromQL)
	if !ok {
		t.Fatalf("pushed %T, want *PromQL", push.View)
	}
	if view.expr != `up{job="node"}` || view.server != "eu" {
		t.Errorf("view = %q on %q, want the expression on eu", view.expr, view.server)
	}

	// An empty expression and a registry without Prometheus both flash.
	if got, ok := cmd.Run(env, "  ")().(tui.FlashMsg); !ok || got.Level != tui.FlashError {
		t.Errorf("empty expression returned %T, want an error flash", cmd.Run(env, "  ")())
	}
	empty := tui.NewCommands()
	_ = RegisterPromQL(empty)
	c2, _ := empty.Lookup("promql")
	bare := &tui.Env{Registry: backend.NewRegistry()}
	if got, ok := c2.Run(bare, "up")().(tui.FlashMsg); !ok || got.Level != tui.FlashError {
		t.Errorf("no prometheus returned %T, want an error flash", c2.Run(bare, "up")())
	}
}
