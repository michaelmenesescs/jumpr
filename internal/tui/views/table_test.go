package views

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend"
	"jumper/internal/backend/fake"
	"jumper/internal/errs"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/tui"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
)

// fixedNow is the clock every table test runs against.
var fixedNow = time.Date(2026, 11, 12, 15, 30, 0, 0, time.UTC)

// testEnv builds a theme/keymap/clock environment with colour turned off so
// assertions can look at plain text.
func testEnv() *tui.Env {
	theme.SetProfile("none")
	return &tui.Env{
		Theme: theme.Dark(),
		Keys:  keymap.Default(),
		Now:   func() time.Time { return fixedNow },
	}
}

// hostKind returns the fake backend's host KindSpec.
func hostKind(t *testing.T, b backend.Backend) resource.KindSpec {
	t.Helper()
	for _, spec := range b.Kinds() {
		if spec.Name == "host" {
			return spec
		}
	}
	t.Fatal("fake backend has no host kind")
	return resource.KindSpec{}
}

// newHostTable builds a table over the fake host list.
func newHostTable(t *testing.T, opts fake.Options) *Table {
	t.Helper()
	b := fake.New(opts)
	return NewTable(testEnv(), b, hostKind(t, b), resource.Query{Kind: "host"})
}

// drain drives Init and every follow-up ListBatch command by hand until the
// stream closes, exactly as the root model would.
func drain(t *testing.T, tbl *Table) *Table {
	t.Helper()
	cmd := tbl.Init()
	for i := 0; cmd != nil; i++ {
		if i > 5000 {
			t.Fatal("stream did not close")
		}
		view, next := tbl.Update(cmd())
		var ok bool
		if tbl, ok = view.(*Table); !ok {
			t.Fatalf("Update returned %T, want *Table", view)
		}
		cmd = next
	}
	return tbl
}

// press sends one key message to the table.
func press(t *testing.T, tbl *Table, msg tea.KeyMsg) *Table {
	t.Helper()
	view, _ := tbl.Update(msg)
	next, ok := view.(*Table)
	if !ok {
		t.Fatalf("Update returned %T, want *Table", view)
	}
	return next
}

// runes builds a KeyMsg carrying typed text.
func runes(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// instantiate builds a minimal fake row for message-focused tests.
func instantiate(name string) resource.Resource {
	return resource.Resource{
		Ref:    resource.Ref{Backend: "fake", Kind: "host", Name: name},
		Fields: map[string]any{"host": name},
	}
}

func TestTableImplementsView(t *testing.T) {
	var _ tui.View = newHostTable(t, fake.Options{})
}

func TestTableStreamsAllRows(t *testing.T) {
	tbl := drain(t, newHostTable(t, fake.Options{}))
	if got, want := len(tbl.rows), 1512; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}
	if !tbl.done {
		t.Fatal("table not marked done after the stream closed")
	}
	if len(tbl.view) != len(tbl.rows) {
		t.Fatalf("view = %d rows, want %d", len(tbl.view), len(tbl.rows))
	}
	if tbl.doneN != 1512 || tbl.total != 1512 {
		t.Fatalf("progress = %d/%d, want 1512/1512", tbl.doneN, tbl.total)
	}
}

func TestTableRenderSize(t *testing.T) {
	tbl := drain(t, newHostTable(t, fake.Options{}))
	for _, size := range [][2]int{{100, 20}, {40, 5}, {80, 3}, {120, 40}} {
		out := tbl.Render(size[0], size[1])
		lines := strings.Split(out, "\n")
		if len(lines) != size[1] {
			t.Fatalf("Render(%d,%d) returned %d lines", size[0], size[1], len(lines))
		}
		for i, line := range lines {
			if got := render.Width(line); got != size[0] {
				t.Fatalf("Render(%d,%d) line %d is %d cells", size[0], size[1], i, got)
			}
		}
	}
}

func TestTableFilter(t *testing.T) {
	tbl := drain(t, newHostTable(t, fake.Options{}))

	tbl = press(t, tbl, runes("/"))
	if !tbl.Capturing() {
		t.Fatal("Capturing() = false after /")
	}
	// While capturing, j/k/q/: are text, not navigation.
	tbl = press(t, tbl, runes("j"))
	if tbl.filter != "j" || len(tbl.view) != 0 {
		t.Fatalf("filter j: filter=%q rows=%d", tbl.filter, len(tbl.view))
	}
	tbl = press(t, tbl, tea.KeyMsg{Type: tea.KeyCtrlU})
	if tbl.filter != "" || len(tbl.view) != 1512 {
		t.Fatalf("ctrl+u: filter=%q rows=%d", tbl.filter, len(tbl.view))
	}

	tbl = press(t, tbl, runes("web-ewr-0"))
	if got, want := len(tbl.view), 99; got != want {
		t.Fatalf("filtered rows = %d, want %d", got, want)
	}
	for _, r := range tbl.view {
		if !strings.Contains(r.Str("host"), "web-ewr-0") {
			t.Fatalf("row %q does not match the filter", r.Str("host"))
		}
	}

	// Enter keeps the filter and stops capturing.
	tbl = press(t, tbl, tea.KeyMsg{Type: tea.KeyEnter})
	if tbl.Capturing() {
		t.Fatal("Capturing() = true after enter")
	}
	if len(tbl.view) != 99 {
		t.Fatalf("filter lost after enter: %d rows", len(tbl.view))
	}

	// Esc with a filter clears it.
	tbl = press(t, tbl, tea.KeyMsg{Type: tea.KeyEsc})
	if tbl.filter != "" || len(tbl.view) != 1512 {
		t.Fatalf("esc: filter=%q rows=%d", tbl.filter, len(tbl.view))
	}

	// Regex form.
	tbl = press(t, tbl, runes("/"))
	tbl = press(t, tbl, runes("~^web-lax-0"))
	if got, want := len(tbl.view), 99; got != want {
		t.Fatalf("regex rows = %d, want %d", got, want)
	}

	// Invalid regex matches nothing.
	tbl = press(t, tbl, tea.KeyMsg{Type: tea.KeyCtrlU})
	tbl = press(t, tbl, runes("~("))
	if !tbl.filterBad {
		t.Fatal("invalid regex not flagged")
	}
	if len(tbl.view) != 0 {
		t.Fatalf("invalid regex matched %d rows, want 0", len(tbl.view))
	}
}

func TestTableFuzzy(t *testing.T) {
	tbl := drain(t, newHostTable(t, fake.Options{}))

	// ctrl+f opens the fuzzy prompt; typing a scattered subsequence finds the
	// one host whose name lines up.
	tbl = press(t, tbl, tea.KeyMsg{Type: tea.KeyCtrlF})
	if !tbl.Capturing() {
		t.Fatal("Capturing() = false after ctrl+f")
	}
	if !tbl.fuzzy {
		t.Fatal("ctrl+f did not switch the filter to fuzzy mode")
	}
	tbl = press(t, tbl, runes("webewr012"))
	if got := len(tbl.view); got != 1 {
		t.Fatalf("fuzzy rows = %d, want 1", got)
	}
	if got := tbl.view[0].Str("host"); got != "web-ewr-012" {
		t.Fatalf("fuzzy match = %q, want web-ewr-012", got)
	}

	// Enter applies and stops capturing; the rows stay filtered.
	tbl = press(t, tbl, tea.KeyMsg{Type: tea.KeyEnter})
	if tbl.Capturing() {
		t.Fatal("Capturing() = true after enter")
	}
	if len(tbl.view) != 1 {
		t.Fatalf("fuzzy filter lost after enter: %d rows", len(tbl.view))
	}

	// Esc clears it.
	tbl = press(t, tbl, tea.KeyMsg{Type: tea.KeyEsc})
	if tbl.filter != "" || tbl.fuzzy || len(tbl.view) != 1512 {
		t.Fatalf("esc: filter=%q fuzzy=%v rows=%d", tbl.filter, tbl.fuzzy, len(tbl.view))
	}

	// The existing "/" substring filter keeps its behaviour.
	tbl = press(t, tbl, runes("/"))
	tbl = press(t, tbl, runes("web-ewr"))
	if got, want := len(tbl.view), 504; got != want {
		t.Fatalf("substring rows = %d, want %d", got, want)
	}
	if tbl.fuzzy {
		t.Fatal("/ switched the filter to fuzzy mode")
	}
	tbl = press(t, tbl, tea.KeyMsg{Type: tea.KeyEsc})

	// And so does the ~ regex form.
	tbl = press(t, tbl, runes("/"))
	tbl = press(t, tbl, runes("~^web-lax-0"))
	if got, want := len(tbl.view), 99; got != want {
		t.Fatalf("regex rows = %d, want %d", got, want)
	}
	if len(tbl.CopyLines()) != 100 {
		t.Fatalf("CopyLines = %d lines, want 100", len(tbl.CopyLines()))
	}
	tbl = press(t, tbl, tea.KeyMsg{Type: tea.KeyEsc})

	// Fuzzy matches the name column and label values, not other cells.
	row := resource.Resource{
		Ref:    resource.Ref{Backend: "fake", Kind: "host", Name: "web-ord-001"},
		Fields: map[string]any{"load": 9.99},
		Labels: map[string]string{"dc": "zzlabel", "role": "web"},
	}
	tbl.filter, tbl.fuzzy = "webo", true
	if !tbl.matches(row) {
		t.Error("fuzzy did not match the row name")
	}
	tbl.filter = "zlb"
	if !tbl.matches(row) {
		t.Error("fuzzy did not match a label value")
	}
	tbl.filter = "999"
	if tbl.matches(row) {
		t.Error("fuzzy matched a field that is not the name or a label")
	}
}

func TestTableSortKeepsCursor(t *testing.T) {
	tbl := drain(t, newHostTable(t, fake.Options{}))

	for i := 0; i < 5; i++ {
		tbl = press(t, tbl, runes("j"))
	}
	ref := tbl.view[tbl.cursor].Ref

	tbl = press(t, tbl, runes("s"))
	if tbl.sortID != "mem" {
		t.Fatalf("sort column = %q, want mem", tbl.sortID)
	}
	if tbl.sortDesc {
		t.Fatal("s should sort ascending")
	}
	if got := tbl.view[tbl.cursor].Ref; got != ref {
		t.Fatalf("cursor moved to %v, want %v", got, ref)
	}
	for i := 1; i < len(tbl.view); i++ {
		if resource.Compare(tbl.view[i-1].Fields["mem"], tbl.view[i].Fields["mem"]) > 0 {
			t.Fatalf("rows not sorted ascending at %d", i)
		}
	}

	// S reverses, still on the same ref.
	tbl = press(t, tbl, runes("S"))
	if !tbl.sortDesc {
		t.Fatal("S did not reverse")
	}
	if got := tbl.view[tbl.cursor].Ref; got != ref {
		t.Fatalf("cursor moved after S to %v, want %v", got, ref)
	}
}

func TestTableErrorsShown(t *testing.T) {
	opts := fake.Options{FailHosts: map[resource.HostID]errs.Kind{
		"web-ewr-007": errs.Unreachable,
		"web-ewr-012": errs.Timeout,
	}}
	tbl := drain(t, newHostTable(t, opts))

	if got, want := len(tbl.rows), 1510; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}
	if got, want := len(tbl.errs), 2; got != want {
		t.Fatalf("errors = %d, want %d", got, want)
	}

	out := tbl.Render(100, 20)
	if !strings.Contains(out, "2 errors") {
		t.Fatalf("banner does not count errors:\n%s", out)
	}
	if !strings.Contains(out, "unreachable") || !strings.Contains(out, "timeout") {
		t.Fatalf("error rows missing their kinds:\n%s", out)
	}
	if !strings.Contains(out, "web-ewr-007") || !strings.Contains(out, "web-ewr-012") {
		t.Fatalf("error rows missing their hosts:\n%s", out)
	}

	// The kind sits in the first non-name column: HOST is index 0, LOAD 1.
	lines := strings.Split(out, "\n")
	found := false
	for _, line := range lines {
		if strings.Contains(line, "web-ewr-007") && strings.Contains(line, "unreachable") {
			found = true
		}
	}
	if !found {
		t.Fatalf("error kind not on the error row:\n%s", out)
	}
}

func TestTableRefreshIgnoresStaleSeq(t *testing.T) {
	tbl := drain(t, newHostTable(t, fake.Options{}))
	before := len(tbl.rows)

	stale := tui.ListBatchMsg{
		Seq:     tbl.seq + 7,
		Results: []backend.Result{{Items: []resource.Resource{instantiate("bogus")}}},
	}
	view, cmd := tbl.Update(stale)
	if cmd != nil {
		t.Fatal("stale batch returned a command")
	}
	tbl = view.(*Table)
	if len(tbl.rows) != before {
		t.Fatalf("stale batch changed rows: %d, want %d", len(tbl.rows), before)
	}
	if tbl.rows[0].Ref.Name == "bogus" {
		t.Fatal("stale batch was appended")
	}

	seq := tbl.seq
	view, cmd = tbl.Update(runes("r"))
	tbl = view.(*Table)
	if tbl.seq != seq+1 {
		t.Fatalf("seq = %d, want %d", tbl.seq, seq+1)
	}
	if len(tbl.rows) != 0 || len(tbl.view) != 0 || tbl.done {
		t.Fatalf("refresh did not clear state: rows=%d view=%d done=%v", len(tbl.rows), len(tbl.view), tbl.done)
	}
	if cmd == nil {
		t.Fatal("refresh returned no ListBatch command")
	}
	if tbl.cancel != nil {
		tbl.cancel()
	}
}

func TestTableRendersOnlyVisibleRows(t *testing.T) {
	tbl := drain(t, newHostTable(t, fake.Options{}))
	tbl.w, tbl.h = 100, 20

	out := tbl.Render(100, 20)
	if len(strings.Split(out, "\n")) != 20 {
		t.Fatalf("got %d lines, want 20", len(strings.Split(out, "\n")))
	}
	// The first row is visible; a row far down the list is not in the frame.
	if !strings.Contains(out, tbl.view[0].Str("host")) {
		t.Fatalf("first row %q missing from the frame", tbl.view[0].Str("host"))
	}
	if strings.Contains(out, tbl.view[200].Str("host")) {
		t.Fatalf("row 200 %q rendered outside the window", tbl.view[200].Str("host"))
	}

	skipIfBusy(t, "5 ms render")
	start := time.Now()
	const runs = 100
	for i := 0; i < runs; i++ {
		_ = tbl.Render(100, 20)
	}
	avg := time.Since(start) / runs
	if avg > 5*time.Millisecond {
		t.Fatalf("average Render time %v for %d rows, want < 5ms", avg, len(tbl.rows))
	}
	t.Logf("average Render: %v over %d rows (frame 100x20)", avg, len(tbl.rows))
}

func TestTableCellFormatting(t *testing.T) {
	env := testEnv()
	b := fake.New(fake.Options{})
	tbl := NewTable(env, b, hostKind(t, b), resource.Query{Kind: "host"})

	col, ok := tbl.spec.Column("load")
	if !ok {
		t.Fatal("host kind has no load column")
	}
	row := resource.Resource{
		Fields: map[string]any{
			"load":   4.125,
			"failed": 3,
		},
		Status: resource.StatusCrit,
	}
	if got := cellText(col, row, fixedNow, 6); got != "4.12" {
		t.Fatalf("float number = %q, want 4.12", got)
	}

	if got := cellText(resource.Column{ID: "missing", Kind: resource.ColNumber}, row, fixedNow, 6); got != "—" {
		t.Fatalf("nil = %q, want —", got)
	}
	if got := cellText(resource.Column{ID: "load", Kind: resource.ColPercent}, row, fixedNow, 6); got != "4.1%" {
		t.Fatalf("percent = %q, want 4.1%%", got)
	}
	if got := cellText(resource.Column{ID: "load", Kind: resource.ColBytes}, resource.Resource{Fields: map[string]any{"load": int64(1536)}}, fixedNow, 6); got != "1.5 KiB" {
		t.Fatalf("bytes = %q", got)
	}
	if got := cellText(resource.Column{ID: "d", Kind: resource.ColDuration}, resource.Resource{Fields: map[string]any{"d": 38*time.Minute + 12*time.Second}}, fixedNow, 8); got != "38m 12s" {
		t.Fatalf("duration = %q", got)
	}
	if got := cellText(resource.Column{ID: "t", Kind: resource.ColTime}, resource.Resource{Fields: map[string]any{"t": fixedNow.Add(-time.Hour)}}, fixedNow, 8); got != "14:30:00" {
		t.Fatalf("time = %q", got)
	}
	if got := cellText(resource.Column{ID: "t", Kind: resource.ColTime}, resource.Resource{Fields: map[string]any{"t": fixedNow.Add(-48 * time.Hour)}}, fixedNow, 8); got != "Nov 10" {
		t.Fatalf("old time = %q", got)
	}
	wantSpark := render.Spark([]float64{1, 2}, 4)
	if got := cellText(resource.Column{ID: "s", Kind: resource.ColSpark}, resource.Resource{Fields: map[string]any{"s": []float64{1, 2}}}, fixedNow, 4); got != wantSpark {
		t.Fatalf("spark = %q, want %q", got, wantSpark)
	}
	if got := cellText(resource.Column{ID: "st", Kind: resource.ColStatus}, row, fixedNow, 6); got != "crit" {
		t.Fatalf("status = %q", got)
	}
	if got := statusText(resource.StatusNA); got != "n/a" {
		t.Fatalf("NA status = %q", got)
	}
}

// TestTableCommandLine checks the `:` line that rebuilds the table: the kind,
// its selector, then the explicit hosts and the sorted args.
func TestTableCommandLine(t *testing.T) {
	b := fake.New(fake.Options{})
	spec := hostKind(t, b)

	plain := NewTable(testEnv(), b, spec, resource.Query{Kind: "host"})
	if got := plain.CommandLine(); got != "host" {
		t.Fatalf("CommandLine() = %q, want \"host\"", got)
	}
	if got := plain.FilterState(); got != "" {
		t.Fatalf("FilterState() = %q, want empty", got)
	}

	sel, err := resource.ParseSelector("role=web and dc=ewr")
	if err != nil {
		t.Fatalf("parse selector: %v", err)
	}
	q := resource.Query{
		Kind:     "host",
		Selector: sel,
		Hosts:    []resource.HostID{"web-01", "web-02"},
		Args:     map[string]string{"unit": "nginx.service", "env": "prod"},
	}
	tbl := NewTable(testEnv(), b, spec, q)
	want := "host role=web and dc=ewr --host web-01,web-02 --arg env=prod --arg unit=nginx.service"
	if got := tbl.CommandLine(); got != want {
		t.Fatalf("CommandLine() = %q, want %q", got, want)
	}

	// The filter travels with the view, unescaped.
	tbl.filter = "web-01"
	if got := tbl.FilterState(); got != "web-01" {
		t.Fatalf("FilterState() = %q, want web-01", got)
	}
}

// TestTableSetContextStopsStream checks that a cancelled pane context stops the
// list instead of streaming to the end.
func TestTableSetContextStopsStream(t *testing.T) {
	tbl := newHostTable(t, fake.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	tbl.SetContext(ctx)

	cmd := tbl.Init()
	cancel()
	for i := 0; cmd != nil; i++ {
		if i > 1000 {
			t.Fatal("stream did not stop after the context was cancelled")
		}
		view, next := tbl.Update(cmd())
		var ok bool
		if tbl, ok = view.(*Table); !ok {
			t.Fatalf("Update returned %T, want *Table", view)
		}
		cmd = next
	}
	if !tbl.done {
		t.Fatal("table did not finish after the context was cancelled")
	}
}
