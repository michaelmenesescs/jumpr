package views

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend"
	"jumper/internal/backend/elastic"
	"jumper/internal/errs"
	"jumper/internal/redact"
	"jumper/internal/resource"
	"jumper/internal/tui"
)

// esRow builds one log row.
func esRow(at time.Time, level, host, message string) elastic.LogRow {
	return elastic.LogRow{
		Cluster: "eu",
		Source:  "app",
		Index:   "logs-app-2026.11.12",
		ID:      host + "-" + at.Format("150405"),
		Time:    at,
		Level:   level,
		Host:    host,
		Message: message,
		Sort:    []any{float64(at.UnixMilli()), float64(1)},
		Doc:     "{\n  \"message\": \"" + message + "\",\n  \"log.level\": \"" + level + "\"\n}",
		Fields: map[string]string{
			"status": "500",
		},
	}
}

// fakeESLogs is a scripted esLogsSource.
type fakeESLogs struct {
	sources  []elastic.SourceRef
	pages    map[string]elastic.LogPage
	fields   []elastic.Field
	err      error
	fieldErr error
	// tailResults is the whole scripted tail: every result is buffered on the
	// channel the fake returns, and the channel is then closed.
	tailResults []elastic.TailResult
	tailErr     error

	mu      sync.Mutex
	queries []elastic.LogQuery
	fieldQs []string
	tailQs  []elastic.TailOptions
}

func (f *fakeESLogs) LogSources() []elastic.SourceRef {
	if f.sources == nil {
		return []elastic.SourceRef{
			{Cluster: "eu", Source: elastic.LogSource{Name: "app", Index: "logs-app-*"}},
		}
	}
	return f.sources
}

func (f *fakeESLogs) Logs(ctx context.Context, q elastic.LogQuery) (elastic.LogPage, error) {
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()
	if f.err != nil {
		return elastic.LogPage{}, f.err
	}
	if p, ok := f.pages[q.Cluster+"/"+q.Source]; ok {
		return p, nil
	}
	return elastic.LogPage{}, nil
}

func (f *fakeESLogs) FieldCaps(ctx context.Context, cluster, source string) ([]elastic.Field, error) {
	f.mu.Lock()
	f.fieldQs = append(f.fieldQs, cluster+"/"+source)
	f.mu.Unlock()
	return f.fields, f.fieldErr
}

// Tail returns the scripted tail and closes it, so one Update drains it all.
func (f *fakeESLogs) Tail(ctx context.Context, o elastic.TailOptions) (<-chan elastic.TailResult, error) {
	f.mu.Lock()
	f.tailQs = append(f.tailQs, o)
	results := append([]elastic.TailResult{}, f.tailResults...)
	err := f.tailErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	ch := make(chan elastic.TailResult, len(results))
	for _, r := range results {
		ch <- r
	}
	close(ch)
	return ch, nil
}

// lastTail is the options of the last Tail call.
func (f *fakeESLogs) lastTail(t *testing.T) elastic.TailOptions {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tailQs) == 0 {
		t.Fatal("no Tail call was made")
	}
	return f.tailQs[len(f.tailQs)-1]
}

func (f *fakeESLogs) lastQuery(t *testing.T) elastic.LogQuery {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) == 0 {
		t.Fatal("no Logs call was made")
	}
	return f.queries[len(f.queries)-1]
}

// esLogsFixture builds a loaded `:logs` screen over two fully scripted sources.
func esLogsFixture(t *testing.T) (*ESLogs, *fakeESLogs) {
	t.Helper()
	base := fixedNow.Add(-time.Hour)
	f := &fakeESLogs{
		sources: []elastic.SourceRef{
			{Cluster: "eu", Source: elastic.LogSource{Name: "app", Index: "logs-app-*"}},
			{Cluster: "eu", Source: elastic.LogSource{Name: "system", Index: "logs-sys-*"}},
		},
		pages: map[string]elastic.LogPage{
			"eu/app": {Rows: []elastic.LogRow{
				esRow(base.Add(10*time.Minute), "error", "web-01", "upstream timed out"),
				esRow(base.Add(30*time.Minute), "info", "web-01", "worker started"),
			}},
			"eu/system": {Rows: []elastic.LogRow{
				esRow(base.Add(20*time.Minute), "warning", "db-01", "checkpoint behind"),
			}},
		},
		fields: []elastic.Field{
			{Name: "http.response.status_code", Type: "long"},
			{Name: "status", Type: "long"},
		},
	}
	e := NewESLogs(testEnv(), f, resource.Query{Kind: elastic.KindLog, Window: resource.TimeWindow{From: base, ToNow: true, Label: "-1h"}})
	return applyESLoads(t, e), f
}

// applyESLoads drives the load command and every reload it returns.
func applyESLoads(t *testing.T, e *ESLogs) *ESLogs {
	t.Helper()
	runESTree(t, e, e.Init())
	return e
}

// runESTree executes cmd and applies every esLogsDataMsg it yields, ignoring
// timer commands (the text input's cursor blink) so a test never sleeps.
func runESTree(t *testing.T, e *ESLogs, cmd tea.Cmd) *ESLogs {
	t.Helper()
	for i := 0; cmd != nil && i < 100; i++ {
		msg := cmd()
		switch m := msg.(type) {
		case esLogsDataMsg, esLogsFieldsMsg:
			view, next := e.Update(m)
			nextView, ok := view.(*ESLogs)
			if !ok {
				t.Fatalf("Update returned %T, want *ESLogs", view)
			}
			e, cmd = nextView, next
		default:
			cmd = nil
		}
	}
	return e
}

// pressES sends one key.
func pressES(t *testing.T, e *ESLogs, msg tea.KeyMsg) (*ESLogs, tea.Cmd) {
	t.Helper()
	return updateES(t, e, msg)
}

// updateES sends any message and returns the screen Update produced.
func updateES(t *testing.T, e *ESLogs, msg tea.Msg) (*ESLogs, tea.Cmd) {
	t.Helper()
	view, cmd := e.Update(msg)
	next, ok := view.(*ESLogs)
	if !ok {
		t.Fatalf("Update returned %T, want *ESLogs", view)
	}
	return next, cmd
}

// typeES sends s one rune at a time, as the root would.
func typeES(t *testing.T, e *ESLogs, s string) *ESLogs {
	t.Helper()
	for _, r := range s {
		e, _ = pressES(t, e, runeKey(string(r)))
	}
	return e
}

func TestESLogsLoadsAndRenders(t *testing.T) {
	e, f := esLogsFixture(t)

	if len(e.rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(e.rows))
	}
	// The two sources are merged and sorted by time.
	if got, want := e.view[0].Message, "upstream timed out"; got != want {
		t.Errorf("first row = %q, want %q", got, want)
	}
	if got, want := e.view[1].Message, "checkpoint behind"; got != want {
		t.Errorf("second row = %q, want %q (merged by time)", got, want)
	}
	if got, want := e.view[2].Message, "worker started"; got != want {
		t.Errorf("third row = %q, want %q", got, want)
	}
	if len(f.queries) != 2 {
		t.Errorf("Logs called %d times, want once per source", len(f.queries))
	}
	if got := f.queries[0]; got.Cluster != "eu" || got.Source != "app" || got.Size != elastic.SearchSize {
		t.Errorf("first query = %+v", got)
	}

	out := e.Render(110, 10)
	for _, want := range []string{"TIME", "LEVEL", "HOST", "MESSAGE", "error", "warning", "info", "web-01", "db-01", "upstream timed out", "checkpoint behind"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	if got := strings.Count(out, "\n"); got != 9 {
		t.Errorf("render has %d newlines, want 9 (exactly 10 lines)", got)
	}
	banner := strings.SplitN(out, "\n", 2)[0]
	for _, want := range []string{"logs", "2 sources", "-1h → now", "3"} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner %q missing %q", banner, want)
		}
	}
	if got := e.Crumb(); got != "logs (3)" {
		t.Errorf("crumb = %q", got)
	}
}

// TestESLogsBannerPlus checks a full page is reported as a lower bound.
func TestESLogsBannerPlus(t *testing.T) {
	base := fixedNow.Add(-time.Hour)
	f := &fakeESLogs{pages: map[string]elastic.LogPage{
		"eu/app": {Rows: []elastic.LogRow{
			esRow(base.Add(time.Minute), "error", "web-01", "boom"),
		}, Full: true},
	}}
	e := NewESLogs(testEnv(), f, resource.Query{Kind: elastic.KindLog, Args: map[string]string{"query": "status:500"}})
	e = applyESLoads(t, e)

	banner := strings.SplitN(e.Render(110, 10), "\n", 2)[0]
	if !strings.Contains(banner, "1+") {
		t.Errorf("banner %q missing the 500+ marker", banner)
	}
	if !strings.Contains(banner, "q=status:500") {
		t.Errorf("banner %q missing the query text", banner)
	}
	if got, want := f.lastQuery(t).Query, "status:500"; got != want {
		t.Errorf("query sent = %q, want %q", got, want)
	}
}

func TestESLogsFilterAndClear(t *testing.T) {
	e, _ := esLogsFixture(t)

	e, cmd := pressES(t, e, runeKey("/"))
	if !e.Capturing() {
		t.Fatal("Capturing is false after /")
	}
	_ = cmd
	e = typeES(t, e, "timed")
	if len(e.view) != 1 || !strings.Contains(e.view[0].Message, "timed out") {
		t.Fatalf("filter left %d rows: %+v", len(e.view), e.view)
	}
	e, _ = pressES(t, e, tea.KeyMsg{Type: tea.KeyEnter})
	if e.Capturing() {
		t.Error("Capturing is still true after enter")
	}
	if got, want := e.filter, "timed"; got != want {
		t.Errorf("filter = %q, want %q", got, want)
	}
	if !strings.Contains(e.Render(110, 10), "/filter: timed") {
		t.Error("banner does not show the filter")
	}

	// A regex filter works too.
	e, _ = pressES(t, e, runeKey("/"))
	e = typeES(t, e, "~^warn")
	e, _ = pressES(t, e, tea.KeyMsg{Type: tea.KeyEnter})
	if len(e.view) != 1 || e.view[0].Level != "warning" {
		t.Errorf("regex filter left %d rows", len(e.view))
	}

	// esc clears the filter, then pops.
	e, cmd = pressES(t, e, tea.KeyMsg{Type: tea.KeyEsc})
	if e.filter != "" || len(e.view) != 3 {
		t.Errorf("esc did not clear the filter: %q, %d rows", e.filter, len(e.view))
	}
	if cmd != nil {
		t.Error("the first esc should clear the filter, not pop")
	}
	_, cmd = pressES(t, e, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Error("the second esc should pop the view")
	} else if _, ok := cmd().(tui.PopViewMsg); !ok {
		t.Errorf("esc emitted %T, want PopViewMsg", cmd())
	}
}

func TestESLogsQueryEdit(t *testing.T) {
	e, f := esLogsFixture(t)

	e, _ = pressES(t, e, runeKey("q"))
	if !e.Capturing() {
		t.Fatal("Capturing is false after q")
	}
	if !strings.Contains(e.Render(110, 10), "query> ") {
		t.Error("the query prompt is not rendered")
	}
	e = typeES(t, e, "level:error")
	e, cmd := pressES(t, e, tea.KeyMsg{Type: tea.KeyEnter})
	if e.Capturing() {
		t.Error("Capturing is still true after enter")
	}
	e = runESTree(t, e, cmd)

	if got, want := e.query, "level:error"; got != want {
		t.Errorf("query = %q, want %q", got, want)
	}
	if got, want := f.lastQuery(t).Query, "level:error"; got != want {
		t.Errorf("query sent = %q, want %q", got, want)
	}

	// esc drops an edit without searching.
	e, _ = pressES(t, e, runeKey("q"))
	e = typeES(t, e, "junk")
	e, cmd = pressES(t, e, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		t.Error("esc should not reload")
	}
	if e.query != "level:error" {
		t.Errorf("esc changed the query to %q", e.query)
	}
}

func TestESLogsAddColumn(t *testing.T) {
	e, f := esLogsFixture(t)

	e, cmd := pressES(t, e, runeKey("c"))
	if cmd == nil {
		t.Fatal("c should ask for the field list (_field_caps)")
	}
	e = runESTree(t, e, cmd)
	if len(e.fields) != 2 {
		t.Fatalf("fields = %v, want the completion list", e.fields)
	}
	if len(f.fieldQs) != 1 || f.fieldQs[0] != "eu/app" {
		t.Errorf("field caps asked for %v", f.fieldQs)
	}

	e = typeES(t, e, "st")
	e, _ = pressES(t, e, tea.KeyMsg{Type: tea.KeyTab})
	if got := e.input.Value(); got != "status" {
		t.Errorf("tab completed to %q", got)
	}

	e, cmd = pressES(t, e, tea.KeyMsg{Type: tea.KeyEnter})
	e = runESTree(t, e, cmd)
	if len(e.columns) != 1 || e.columns[0] != "status" {
		t.Fatalf("columns = %v", e.columns)
	}
	if got := f.lastQuery(t).Columns; len(got) != 1 || got[0] != "status" {
		t.Errorf("the reload asked for columns %v", got)
	}
	out := e.Render(140, 10)
	if !strings.Contains(out, "STATUS") {
		t.Errorf("render missing the added column header:\n%s", out)
	}
	if !strings.Contains(out, "500") {
		t.Errorf("render missing the column value:\n%s", out)
	}
}

func TestESLogsDetailAndFinding(t *testing.T) {
	e, _ := esLogsFixture(t)
	e.env.Redact = nil

	e, _ = pressES(t, e, tea.KeyMsg{Type: tea.KeyEnter})
	if !e.detail {
		t.Fatal("enter did not open the document")
	}
	out := e.Render(110, 12)
	if !strings.Contains(out, "full document") || !strings.Contains(out, `"message"`) {
		t.Errorf("document pane missing the document:\n%s", out)
	}
	if strings.Count(out, "\n") != 11 {
		t.Errorf("document pane is not exactly 12 lines: %q", out)
	}

	e, cmd := pressES(t, e, runeKey("F"))
	if cmd == nil {
		t.Fatal("F produced no finding command")
	}
	if _, ok := cmd().(tui.FlashMsg); !ok {
		t.Errorf("finding without an investigation emitted %T, want a flash", cmd())
	}

	e, _ = pressES(t, e, tea.KeyMsg{Type: tea.KeyEsc})
	if e.detail {
		t.Error("esc did not close the document")
	}
}

// TestESLogsDetailRedacted checks the document pane passes text through the
// session redactor.
func TestESLogsDetailRedacted(t *testing.T) {
	red, err := redact.New(nil)
	if err != nil {
		t.Fatalf("redact.New: %v", err)
	}

	base := fixedNow.Add(-time.Hour)
	row := esRow(base.Add(time.Minute), "error", "web-01", "login failed")
	row.Doc = "{\n  \"dsn\": \"postgres://app:password=hunter2@db-01\",\n  \"message\": \"login failed\"\n}"
	f := &fakeESLogs{pages: map[string]elastic.LogPage{"eu/app": {Rows: []elastic.LogRow{row}}}}

	e := NewESLogs(testEnv(), f, resource.Query{Kind: elastic.KindLog})
	e.env.Redact = red
	e = applyESLoads(t, e)
	e.detail = true

	out := e.Render(120, 14)
	if !strings.Contains(out, "full document") {
		t.Fatalf("document pane not rendered:\n%s", out)
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("the document pane leaked a secret:\n%s", out)
	}
	if !strings.Contains(out, "redacted") {
		t.Errorf("the document pane did not show the redaction marker:\n%s", out)
	}
}

func TestESLogsError(t *testing.T) {
	f := &fakeESLogs{err: errs.New(errs.Remote, "elastic.search", "status 400: failed to create query")}
	e := NewESLogs(testEnv(), f, resource.Query{Kind: elastic.KindLog})
	e = applyESLoads(t, e)

	out := e.Render(110, 10)
	if !strings.Contains(out, "failed to create query") {
		t.Errorf("render missing the error:\n%s", out)
	}
	if got := e.CopyLines(); len(got) < 2 || !strings.Contains(got[1], "failed to create query") {
		t.Errorf("copy lines = %v", got)
	}
}

func TestESLogsWindowChange(t *testing.T) {
	e, f := esLogsFixture(t)
	window := resource.TimeWindow{From: fixedNow.Add(-6 * time.Hour), ToNow: true, Label: "-6h"}

	view, cmd := e.Update(tui.WindowChangedMsg{Window: window})
	next, ok := view.(*ESLogs)
	if !ok {
		t.Fatalf("Update returned %T", view)
	}
	if got := next.window.Label; got != "-6h" {
		t.Errorf("window = %q, want -6h", got)
	}
	next = runESTree(t, next, cmd)
	if got := f.lastQuery(t).Window.Label; got != "-6h" {
		t.Errorf("the reload used window %q", got)
	}
}

func TestESLogsStaleLoadDropped(t *testing.T) {
	e, f := esLogsFixture(t)
	before := len(e.rows)

	// A load for an older Seq is ignored.
	view, _ := e.Update(esLogsDataMsg{seq: e.seq - 1, rows: []elastic.LogRow{esRow(fixedNow, "error", "x", "stale")}})
	next := view.(*ESLogs)
	if len(next.rows) != before {
		t.Errorf("a stale load changed the rows: %d", len(next.rows))
	}
	_ = f
}

func TestESLogsFromEnv(t *testing.T) {
	env := testEnv()
	env.Registry = backend.NewRegistry()
	if _, err := NewESLogsFromEnv(env, resource.Query{Kind: elastic.KindLog}); err == nil {
		t.Error("NewESLogsFromEnv accepted a registry without elasticsearch")
	}

	b := elastic.New(elastic.Options{Clusters: []elastic.Cluster{{
		Name:    "eu",
		Sources: []elastic.LogSource{{Name: "app", Index: "logs-app-*"}},
	}}})
	if err := env.Registry.Register(b); err != nil {
		t.Fatalf("register: %v", err)
	}
	e, err := NewESLogsFromEnv(env, resource.Query{
		Kind:  elastic.KindLog,
		Hosts: []resource.HostID{"web-01.example.internal"},
	})
	if err != nil {
		t.Fatalf("NewESLogsFromEnv: %v", err)
	}
	if len(e.sources) != 1 || e.sources[0].Source.Name != "app" {
		t.Errorf("sources = %+v", e.sources)
	}
	if got := e.Subject(); len(got.Hosts) != 1 || got.Kind != elastic.KindLog {
		t.Errorf("subject = %+v", got)
	}
	if hints := e.Hints(); len(hints) == 0 {
		t.Error("no hints")
	}
}

// TestESLogsNoSources checks a query that matches no source says so instead of
// searching nothing.
func TestESLogsNoSources(t *testing.T) {
	e := NewESLogs(testEnv(), &fakeESLogs{}, resource.Query{
		Kind: elastic.KindLog,
		Args: map[string]string{"source": "nope"},
	})
	e = applyESLoads(t, e)
	if e.err == nil || !strings.Contains(e.err.Error(), "no log source") {
		t.Fatalf("err = %v, want a missing-source error", e.err)
	}
	if !strings.Contains(e.Render(100, 8), "no log source") {
		t.Error("render missing the error line")
	}
}

// TestESLogsCopyLines checks copy mode carries the plain text.
func TestESLogsCopyLines(t *testing.T) {
	e, _ := esLogsFixture(t)
	lines := e.CopyLines()
	if len(lines) != 5 {
		t.Fatalf("copy lines = %d, want header + 3 rows + title", len(lines))
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"web-01", "upstream timed out", "db-01"} {
		if !strings.Contains(joined, want) {
			t.Errorf("copy lines missing %q", want)
		}
	}
	if strings.Contains(joined, "\x1b[") {
		t.Error("copy lines carry ANSI escapes")
	}
}

// TestESLogsTailFollows checks `w` follows the source: the poll's rows are
// appended, the lag is shown and a closed tail stops following.
func TestESLogsTailFollows(t *testing.T) {
	base := fixedNow.Add(-time.Minute)
	f := &fakeESLogs{sources: []elastic.SourceRef{
		{Cluster: "eu", Source: elastic.LogSource{Name: "app", Index: "logs-app-*"}},
	}}
	f.tailResults = []elastic.TailResult{
		{Rows: []elastic.LogRow{esRow(base, "error", "web-01", "tail line one")}, Lag: 12 * time.Second},
		{Rows: []elastic.LogRow{esRow(base.Add(time.Second), "info", "web-01", "tail line two")}, Lag: 11 * time.Second},
	}
	e := applyESLoads(t, NewESLogs(testEnv(), f, resource.Query{
		Kind:   elastic.KindLog,
		Hosts:  []resource.HostID{"web-01.example.internal"},
		Window: resource.TimeWindow{From: fixedNow.Add(-time.Hour), ToNow: true, Label: "-1h"},
	}))

	e, cmd := pressES(t, e, runeKey("w"))
	if cmd == nil {
		t.Fatal("w returned no command")
	}
	start := cmd()
	startMsg, ok := start.(esLogsTailStartMsg)
	if !ok {
		t.Fatalf("w produced %T, want esLogsTailStartMsg", start)
	}
	e, cmd = updateES(t, e, startMsg)
	if !e.tailing {
		t.Fatal("the screen is not tailing after w")
	}
	if cmd == nil {
		t.Fatal("the started tail did not ask for its first poll")
	}
	batch := cmd()
	batchMsg, ok := batch.(esLogsTailMsg)
	if !ok {
		t.Fatalf("tail produced %T, want esLogsTailMsg", batch)
	}
	e, cmd = updateES(t, e, batchMsg)

	if len(e.rows) != 2 {
		t.Fatalf("rows = %d, want the 2 tailed lines", len(e.rows))
	}
	if got, want := e.view[len(e.view)-1].Message, "tail line two"; got != want {
		t.Errorf("last row = %q, want %q (the tail follows the bottom)", got, want)
	}
	if e.lag != 11*time.Second {
		t.Errorf("lag = %v, want the newest poll's 11s", e.lag)
	}
	if !strings.Contains(e.Render(120, 10), "tail") {
		t.Error("the banner does not say the screen is tailing")
	}
	// The scripted channel was closed, so following stops by itself.
	if e.tailing {
		t.Error("a closed tail is still reported as tailing")
	}
	if cmd != nil {
		t.Error("a closed tail asked for another poll")
	}
	if o := f.lastTail(t); o.Query.Window.ToNow != true || len(o.Query.Hosts) != len(e.hosts) {
		t.Errorf("tail options = %+v, want the screen's window and hosts", o.Query)
	}
}

// TestESLogsTailTogglesOff checks a second `w` stops following and that Close
// cancels it too.
func TestESLogsTailTogglesOff(t *testing.T) {
	e, f := esLogsFixture(t)
	f.tailResults = []elastic.TailResult{{Lag: 3 * time.Second}}

	e, cmd := pressES(t, e, runeKey("w"))
	e, _ = updateES(t, e, cmd())
	if !e.tailing {
		t.Fatal("the screen did not start tailing")
	}
	e, _ = pressES(t, e, runeKey("w"))
	if e.tailing {
		t.Error("a second w did not stop the tail")
	}
	if err := e.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestESLogsTailError checks a tail that cannot start reports the error and
// leaves the screen alone.
func TestESLogsTailError(t *testing.T) {
	e, f := esLogsFixture(t)
	f.tailErr = errs.New(errs.Config, "elastic.tail", "no such source")

	e, cmd := pressES(t, e, runeKey("w"))
	msg := cmd()
	startMsg, ok := msg.(esLogsTailStartMsg)
	if !ok {
		t.Fatalf("w produced %T, want esLogsTailStartMsg", msg)
	}
	if startMsg.err == nil {
		t.Fatal("the failed Tail produced no error")
	}
	e, _ = updateES(t, e, startMsg)
	if e.tailing {
		t.Error("a failed tail is still reported as tailing")
	}
}

// TestESLogsTailKeepsNewestLines checks the row cap: the tail keeps the newest
// esLogsTailMaxRows lines instead of growing without bound.
func TestESLogsTailKeepsNewestLines(t *testing.T) {
	e, _ := esLogsFixture(t)
	start := fixedNow.Add(-time.Hour)
	rows := make([]elastic.LogRow, esLogsTailMaxRows+50)
	for i := range rows {
		rows[i] = esRow(start.Add(time.Duration(i)*time.Millisecond), "info", "web-01", "line")
		rows[i].ID = time.Duration(i).String()
	}
	e.tailing = true
	e.tailSeq++
	e, _ = updateES(t, e, esLogsTailMsg{seq: e.tailSeq, result: elastic.TailResult{Rows: rows}})
	if got := len(e.rows); got != esLogsTailMaxRows {
		t.Fatalf("rows = %d, want the cap of %d", got, esLogsTailMaxRows)
	}
	if !e.capped {
		t.Error("the screen does not record that it hit the line cap")
	}
	if got, want := e.rows[len(e.rows)-1].ID, (time.Duration(len(rows) - 1)).String(); got != want {
		t.Errorf("newest kept row = %q, want %q", got, want)
	}
}

// TestESLogsUnitArg checks a unit detail's `l` query reaches the search and the
// banner.
func TestESLogsUnitArg(t *testing.T) {
	_, f := esLogsFixture(t)
	e := NewESLogs(testEnv(), f, resource.Query{
		Kind:  elastic.KindLog,
		Hosts: []resource.HostID{"web-01.example.internal"},
		Args:  map[string]string{elastic.ArgUnit: "nginx.service"},
	})
	e = applyESLoads(t, e)
	if got := f.lastQuery(t).Unit; got != "nginx.service" {
		t.Errorf("search unit = %q, want nginx.service", got)
	}
	if !strings.Contains(e.Render(120, 10), "unit=nginx.service") {
		t.Error("the banner does not name the unit")
	}
}
