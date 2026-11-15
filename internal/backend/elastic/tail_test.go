package elastic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/transport/httpx"
)

// tailHit is one hit a tailFixture returns.
type tailHit struct {
	ID   string
	Time string
	Sort []any
}

// tailFixture answers `_search` with one scripted page per call and remembers
// every request, so a test can assert the search_after the tail sent.
type tailFixture struct {
	*httptest.Server

	mu     sync.Mutex
	pages  [][]tailHit
	repeat bool
	status int
	calls  int
	bodies []map[string]any
}

// newTailFixture serves pages in order. With repeat, every call after the last
// page keeps returning the last page; otherwise it returns no hits.
func newTailFixture(t *testing.T, pages [][]tailHit, repeat bool) *tailFixture {
	t.Helper()
	f := &tailFixture{pages: pages, repeat: repeat}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		f.mu.Lock()
		idx := f.calls
		f.calls++
		var doc map[string]any
		if len(body) > 0 {
			_ = json.Unmarshal(body, &doc)
		}
		f.bodies = append(f.bodies, doc)
		status := f.status
		f.mu.Unlock()

		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"root_cause":[{"type":"search_phase_execution_exception","reason":"boom"}]}}`))
			return
		}
		hits := []any{}
		if page, ok := f.page(idx); ok {
			for _, h := range page {
				hits = append(hits, map[string]any{
					"_index": "logs-app-2026.11.12",
					"_id":    h.ID,
					"_source": map[string]any{
						"@timestamp": h.Time,
						"message":    "line " + h.ID,
						"host":       map[string]any{"name": "web-01.example.internal"},
						"log":        map[string]any{"level": "error"},
					},
					"sort": h.Sort,
				})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"_shards": map[string]any{"total": 1, "successful": 1, "failed": 0},
			"hits":    map[string]any{"hits": hits},
		})
	}))
	t.Cleanup(f.Close)
	return f
}

// page returns the hits of one call.
func (f *tailFixture) page(i int) ([]tailHit, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pages) == 0 {
		return nil, false
	}
	if i < len(f.pages) {
		return f.pages[i], true
	}
	if f.repeat {
		return f.pages[len(f.pages)-1], true
	}
	return nil, false
}

// count is how many requests reached the server.
func (f *tailFixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// body returns the JSON body of one request.
func (f *tailFixture) body(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		t.Fatalf("request %d never reached the server (%d seen)", i, len(f.bodies))
	}
	return f.bodies[i]
}

// newTailClient builds a real httpx client over the fixture URL.
func newTailClient(t *testing.T, url, name string) *Client {
	t.Helper()
	prof := &httpx.Profile{Name: "elastic-tail-test"}
	hc, err := prof.Client(httpx.Deps{})
	if err != nil {
		t.Fatalf("httpx client: %v", err)
	}
	c, err := NewClient(ClientOptions{URL: url, Client: hc, Name: name})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// tailBackend builds a backend over f with the app source.
func tailBackend(t *testing.T, f *tailFixture) *Backend {
	t.Helper()
	return New(Options{
		Clusters: []Cluster{{
			Name:    "eu",
			Client:  newTailClient(t, f.URL, "eu"),
			Sources: []LogSource{testSource()},
		}},
		Now: func() time.Time { return testNow },
	})
}

// testTailQuery is the query every tail test follows.
func testTailQuery() LogQuery {
	return LogQuery{
		Cluster: "eu",
		Source:  "app",
		Window:  resource.TimeWindow{From: testNow.Add(-time.Hour), ToNow: true, Label: "-1h"},
	}
}

// drainTail reads a tail until it closes, returning every result.
func drainTail(t *testing.T, ch <-chan TailResult) []TailResult {
	t.Helper()
	var out []TailResult
	timeout := time.After(10 * time.Second)
	for {
		select {
		case res, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, res)
		case <-timeout:
			t.Fatal("the tail did not close")
		}
	}
}

func TestTailFollowsSearchAfter(t *testing.T) {
	lastTime := testNow.Add(-time.Minute)
	first := []tailHit{
		{ID: "1", Time: testNow.Add(-2 * time.Minute).Format(time.RFC3339Nano), Sort: []any{float64(1), float64(10)}},
		{ID: "2", Time: lastTime.Format(time.RFC3339Nano), Sort: []any{float64(2), float64(11)}},
	}
	f := newTailFixture(t, [][]tailHit{first}, false)
	b := tailBackend(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := b.Tail(ctx, TailOptions{
		Query: testTailQuery(),
		Poll:  5 * time.Millisecond,
		Idle:  5 * time.Millisecond,
		Max:   100,
	})
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}

	got := <-ch
	if len(got.Rows) != 2 {
		t.Fatalf("first poll rows = %d, want 2", len(got.Rows))
	}
	if got.Rows[0].ID != "1" || got.Rows[1].ID != "2" {
		t.Fatalf("first poll ids = %q, %q; want 1, 2", got.Rows[0].ID, got.Rows[1].ID)
	}
	if want := testNow.Sub(lastTime); got.Lag != want {
		t.Errorf("lag = %v, want %v (now - newest event)", got.Lag, want)
	}

	// The second poll has nothing new and keeps the lag it measured.
	empty := <-ch
	if len(empty.Rows) != 0 {
		t.Fatalf("second poll rows = %d, want none", len(empty.Rows))
	}
	if empty.Lag != wantLag(lastTime) {
		t.Errorf("second poll lag = %v, want %v", empty.Lag, wantLag(lastTime))
	}
	cancel()

	// The first search carries no cursor; the one after the first page carries
	// the sort of its last hit, which is what keeps a shared timestamp from
	// skipping or repeating a line.
	if after, ok := f.body(t, 0)["search_after"]; ok && after != nil {
		t.Errorf("first search sent search_after = %v, want none", after)
	}
	body := f.body(t, 1)
	after, ok := body["search_after"].([]any)
	if !ok || len(after) != 2 {
		t.Fatalf("second search search_after = %v, want the two sort values", body["search_after"])
	}
	if after[0] != float64(2) || after[1] != float64(11) {
		t.Errorf("second search search_after = %v, want [2 11]", after)
	}
	for range ch {
	}
}

// wantLag is now minus t, the lag every poll reports after seeing an event at t.
func wantLag(ts time.Time) time.Duration {
	d := testNow.Sub(ts)
	if d < 0 {
		return 0
	}
	return d
}

func TestTailSlowsDownAfterEmptyPolls(t *testing.T) {
	// The interval state machine itself, without a server.
	tr := &tailer{poll: TailPollInterval, idle: TailIdleInterval, idleAfter: TailIdleAfter}
	for _, c := range []struct {
		empty int
		want  time.Duration
	}{
		{0, TailPollInterval},
		{1, TailPollInterval},
		{2, TailPollInterval},
		{3, TailIdleInterval},
		{9, TailIdleInterval},
	} {
		tr.empty = c.empty
		if got := tr.interval(); got != c.want {
			t.Errorf("interval with %d empty polls = %v, want %v", c.empty, got, c.want)
		}
	}

	// And the behaviour: an empty source is polled at the fast interval until
	// the threshold, then at the idle one.
	f := newTailFixture(t, nil, false)
	b := tailBackend(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := b.Tail(ctx, TailOptions{
		Query:     testTailQuery(),
		Poll:      2 * time.Millisecond,
		Idle:      50 * time.Millisecond,
		IdleAfter: 2,
		Max:       100,
	})
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	deadline := time.After(80 * time.Millisecond)
loop:
	for {
		select {
		case <-ch:
		case <-deadline:
			break loop
		}
	}
	cancel()
	if n := f.count(); n < 3 || n > 6 {
		t.Errorf("empty source polled %d times in 80ms with poll 2ms and idle 50ms; want 3-6", n)
	}
}

func TestTailStopsAtLineCap(t *testing.T) {
	page := []tailHit{
		{ID: "1", Time: testNow.Add(-3 * time.Second).Format(time.RFC3339Nano), Sort: []any{float64(1)}},
		{ID: "2", Time: testNow.Add(-2 * time.Second).Format(time.RFC3339Nano), Sort: []any{float64(2)}},
		{ID: "3", Time: testNow.Add(-time.Second).Format(time.RFC3339Nano), Sort: []any{float64(3)}},
	}
	f := newTailFixture(t, [][]tailHit{page}, true)
	b := tailBackend(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := b.Tail(ctx, TailOptions{Query: testTailQuery(), Poll: time.Millisecond, Idle: time.Millisecond, Max: 5})
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	results := drainTail(t, ch)
	total := 0
	for _, res := range results {
		if res.Err != nil {
			t.Fatalf("tail: %v", res.Err)
		}
		total += len(res.Rows)
	}
	if total != 5 {
		t.Fatalf("tail delivered %d lines, want the cap of 5", total)
	}
}

func TestTailStopsOnCancel(t *testing.T) {
	page := []tailHit{{ID: "1", Time: testNow.Add(-time.Second).Format(time.RFC3339Nano), Sort: []any{float64(1)}}}
	f := newTailFixture(t, [][]tailHit{page}, true)
	b := tailBackend(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := b.Tail(ctx, TailOptions{Query: testTailQuery(), Poll: 5 * time.Millisecond, Idle: 5 * time.Millisecond})
	if err != nil {
		cancel()
		t.Fatalf("Tail: %v", err)
	}
	if res := <-ch; len(res.Rows) != 1 {
		t.Fatalf("first poll rows = %d, want 1", len(res.Rows))
	}
	cancel()
	closed := make(chan struct{})
	go func() {
		for range ch {
		}
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the tail did not close after its context was canceled")
	}
}

func TestTailReportsPollError(t *testing.T) {
	f := newTailFixture(t, nil, false)
	f.mu.Lock()
	f.status = http.StatusInternalServerError
	f.mu.Unlock()
	b := tailBackend(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := b.Tail(ctx, TailOptions{Query: testTailQuery(), Poll: time.Millisecond})
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	results := drainTail(t, ch)
	if len(results) != 1 {
		t.Fatalf("results = %d, want one error", len(results))
	}
	if results[0].Err == nil {
		t.Fatal("a failing poll delivered no error")
	}
	if kind := errs.KindOf(results[0].Err); kind != errs.Remote {
		t.Errorf("error kind = %q, want %q: %v", kind, errs.Remote, results[0].Err)
	}
}

func TestTailUnknownSource(t *testing.T) {
	f := newTailFixture(t, nil, false)
	b := tailBackend(t, f)
	_, err := b.Tail(context.Background(), TailOptions{Query: LogQuery{Cluster: "eu", Source: "nope"}})
	if err == nil {
		t.Fatal("Tail accepted a source that is not configured")
	}
	if kind := errs.KindOf(err); kind != errs.Config {
		t.Errorf("error kind = %q, want %q", kind, errs.Config)
	}
}

// unitSource is a source whose documents carry a systemd unit.
func unitSource() LogSource {
	s := testSource()
	s.UnitField = "systemd.unit"
	return s
}

func TestUnitFilterIsAQueryStringTerm(t *testing.T) {
	cases := []struct {
		name  string
		query string
		unit  string
		want  string
	}{
		{name: "unit only", unit: "nginx.service", want: `systemd.unit:"nginx.service"`},
		{name: "with user text", query: "upstream timeout", unit: "nginx.service",
			want: `(upstream timeout) AND systemd.unit:"nginx.service"`},
		{name: "quotes and backslashes are escaped", unit: `a"b\c`, want: `systemd.unit:"a\"b\\c"`},
		{name: "no unit leaves the query alone", query: "upstream timeout", want: "upstream timeout"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := withUnit(c.query, "systemd.unit", c.unit); got != c.want {
				t.Errorf("withUnit(%q, %q) = %q, want %q", c.query, c.unit, got, c.want)
			}
		})
	}

	// A source without a unit field ignores the filter entirely.
	if got := withUnit("q", "", "nginx.service"); got != "q" {
		t.Errorf("withUnit with no unit field = %q, want %q", got, "q")
	}
}

func TestUnitFilterReachesSearch(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_search": "search.json"})
	b := New(Options{
		Clusters: []Cluster{{
			Name:    "eu",
			Client:  newTestClient(t, fs, "eu"),
			Sources: []LogSource{unitSource()},
		}},
		Now: func() time.Time { return testNow },
	})
	if _, err := b.Logs(context.Background(), LogQuery{
		Cluster: "eu",
		Source:  "app",
		Unit:    "nginx.service",
		Query:   "error",
	}); err != nil {
		t.Fatalf("Logs: %v", err)
	}
	body := fs.last(t).Body
	var sent struct {
		Query struct {
			Bool struct {
				Must []struct {
					QueryString struct {
						Query string `json:"query"`
					} `json:"query_string"`
				} `json:"must"`
			} `json:"bool"`
		} `json:"query"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("decode the search body: %v", err)
	}
	if len(sent.Query.Bool.Must) != 1 {
		t.Fatalf("must clauses = %d, want the one query_string", len(sent.Query.Bool.Must))
	}
	if got, want := sent.Query.Bool.Must[0].QueryString.Query, `(error) AND systemd.unit:"nginx.service"`; got != want {
		t.Errorf("query_string = %q, want %q", got, want)
	}
}

func TestUnitFieldNormalized(t *testing.T) {
	src := LogSource{Index: "x", UnitField: "  systemd.unit  "}.Normalized()
	if src.UnitField != "systemd.unit" {
		t.Errorf("unit field = %q, want systemd.unit", src.UnitField)
	}
	if src.TimeField != defaultTimeField {
		t.Errorf("time field = %q, want %q", src.TimeField, defaultTimeField)
	}
	if !strings.Contains(withUnit("", "systemd.unit", "nginx.service"), "nginx.service") {
		t.Error("a normalized unit field must produce a filter")
	}
}
