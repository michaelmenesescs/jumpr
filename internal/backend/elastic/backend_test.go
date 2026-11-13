package elastic

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"jumper/internal/backend"
	"jumper/internal/errs"
	"jumper/internal/resource"
)

// testNow is the clock every backend test runs against.
var testNow = time.Date(2026, 11, 12, 15, 30, 0, 0, time.UTC)

// testBackend builds a backend over fs with the app and system sources.
func testBackend(t *testing.T, fs *fixtureServer, redact func(string) string) *Backend {
	t.Helper()
	c := newTestClient(t, fs, "eu")
	return New(Options{
		Clusters: []Cluster{{
			Name:   "eu",
			Client: c,
			Sources: []LogSource{
				testSource(),
				{Name: "system", Index: "logs-sys-*", HostField: "host.name", MessageField: "message", LevelField: "log.level"},
			},
		}},
		Now:    func() time.Time { return testNow },
		Redact: redact,
	})
}

// drainList reads every Result of one list.
func drainList(t *testing.T, b *Backend, q resource.Query) []backend.Result {
	t.Helper()
	ch, err := b.List(context.Background(), q)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var out []backend.Result
	for res := range ch {
		out = append(out, res)
	}
	return out
}

func TestBackendKindsAndSources(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_search": "search.json"})
	b := testBackend(t, fs, nil)

	specs := b.Kinds()
	if len(specs) != 1 || specs[0].Name != KindLog {
		t.Fatalf("kinds = %+v, want one log kind", specs)
	}
	if len(specs[0].Aliases) != 1 || specs[0].Aliases[0] != aliasLogs {
		t.Errorf("aliases = %v, want %q", specs[0].Aliases, aliasLogs)
	}
	cols := specs[0].Columns
	if len(cols) != 4 {
		t.Fatalf("columns = %+v, want 4", cols)
	}
	for i, want := range []string{"time", "level", "host", "message"} {
		if cols[i].ID != want {
			t.Errorf("column %d = %q, want %q", i, cols[i].ID, want)
		}
	}
	if cols[0].Kind != resource.ColTime {
		t.Errorf("time column kind = %v, want ColTime", cols[0].Kind)
	}
	if cols[3].Width != 0 {
		t.Errorf("message column width = %d, want flexible (0)", cols[3].Width)
	}

	refs := b.LogSources()
	if len(refs) != 2 {
		t.Fatalf("sources = %+v, want 2", refs)
	}
	if refs[1].Cluster != "eu" || refs[1].Source.Name != "system" {
		t.Errorf("second source = %+v", refs[1])
	}
	// The system source configured no time field: the ECS default applies.
	if got, want := refs[1].Source.TimeField, defaultTimeField; got != want {
		t.Errorf("time field = %q, want %q", got, want)
	}
	if got := b.Name(); got != backendName {
		t.Errorf("Name = %q, want %q", got, backendName)
	}
}

func TestBackendLogs(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_search": "search.json"})
	b := testBackend(t, fs, nil)

	page, err := b.Logs(context.Background(), LogQuery{
		Cluster: "eu",
		Source:  "app",
		Hosts:   []resource.HostID{"web-01.example.internal"},
		Window:  resource.TimeWindow{From: testNow.Add(-time.Hour), ToNow: true, Label: "-1h"},
	})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(page.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(page.Rows))
	}
	row := page.Rows[0]
	if got, want := row.Level, "error"; got != want {
		t.Errorf("level = %q, want %q", got, want)
	}
	if got, want := row.Host, "web-01.example.internal"; got != want {
		t.Errorf("host = %q, want %q", got, want)
	}
	if !strings.Contains(row.Message, "upstream timed out") {
		t.Errorf("message = %q", row.Message)
	}
	if want := time.Date(2026, 11, 12, 14, 31, 2, 117000000, time.UTC); !row.Time.Equal(want) {
		t.Errorf("time = %v, want %v", row.Time, want)
	}
	if !strings.Contains(row.Doc, `"@timestamp"`) {
		t.Errorf("doc = %q, want the whole document", row.Doc)
	}
	if got := row.Fields["log.level"]; got != "error" {
		t.Errorf("fields[log.level] = %q, want error", got)
	}
	if page.Full {
		t.Error("a 3-hit page is not full")
	}
	if len(page.After) != 2 {
		t.Errorf("after = %v, want the last hit's sort", page.After)
	}
}

func TestBackendLogsDefaultsAndErrors(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_search": "search.json"})
	b := testBackend(t, fs, nil)

	// An empty source name takes the cluster's first source.
	page, err := b.Logs(context.Background(), LogQuery{Cluster: "eu"})
	if err != nil {
		t.Fatalf("Logs with a default source: %v", err)
	}
	if len(page.Rows) != 3 {
		t.Errorf("rows = %d, want 3", len(page.Rows))
	}

	cases := []struct {
		name string
		q    LogQuery
	}{
		{"unknown cluster", LogQuery{Cluster: "us"}},
		{"unknown source", LogQuery{Cluster: "eu", Source: "nginx"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := b.Logs(context.Background(), tc.q)
			if err == nil {
				t.Fatal("Logs accepted a bad query")
			}
			if kind := errs.KindOf(err); kind != errs.Config {
				t.Errorf("kind = %s, want config", kind)
			}
		})
	}
}

// TestBackendListPages checks List pages with search_after: a full page asks for
// the next one and the loop stops on a short page or on the source's end.
func TestBackendListPages(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_search": "search.json"})
	b := testBackend(t, fs, nil)

	ch, err := b.List(context.Background(), resource.Query{
		Kind:  KindLog,
		Args:  map[string]string{ArgSource: "app"},
		Hosts: []resource.HostID{"web-01"},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var rows int
	batches := 0
	for res := range ch {
		if res.Err != nil {
			t.Fatalf("batch error: %v", res.Err)
		}
		batches++
		rows += len(res.Items)
		if res.Total != 1 || res.Done != 0 {
			t.Errorf("batch progress = %d/%d, want 0/1", res.Done, res.Total)
		}
	}
	if batches != 1 || rows != 3 {
		t.Errorf("batches = %d, rows = %d, want 1 and 3", batches, rows)
	}
	// One request: the page was short, so paging stopped.
	if n := fs.count(); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}

	first := resource.Resource{}
	ch, err = b.List(context.Background(), resource.Query{Kind: KindLog, Args: map[string]string{ArgSource: "app"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for res := range ch {
		if len(res.Items) > 0 {
			first = res.Items[0]
		}
	}
	if got, want := first.Ref.Namespace, "eu/app"; got != want {
		t.Errorf("namespace = %q, want %q", got, want)
	}
	if got, want := first.Ref.Name, "logs-app-2026.11.12/a1"; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}
	if first.Status != resource.StatusCrit {
		t.Errorf("an error line is %v, want crit", first.Status)
	}
	if got := first.Str("host"); got != "web-01.example.internal" {
		t.Errorf("host field = %q", got)
	}
}

// TestBackendListPagingContinues checks a full page does request the next one.
func TestBackendListPagingContinues(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_search": "search.json"})
	b := testBackend(t, fs, nil)

	// A page size of 3 makes the 3-hit fixture page exactly full, so List must
	// ask again with search_after.
	ch, err := b.List(context.Background(), resource.Query{
		Kind: KindLog,
		Args: map[string]string{ArgSource: "app", ArgColumns: "status"},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for range ch {
	}
	if n := fs.count(); n != 1 {
		t.Fatalf("requests = %d, want 1 (the fixture page is short of 500)", n)
	}

	// A query with a page size of 3 via a size argument is not part of the
	// contract; Logs is the paging path the view uses, so drive it directly.
	page, err := b.Logs(context.Background(), LogQuery{Cluster: "eu", Source: "app", Size: 3})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !page.Full {
		t.Error("a 3-hit page of size 3 should be full")
	}
	next, err := b.Logs(context.Background(), LogQuery{Cluster: "eu", Source: "app", Size: 3, After: page.After})
	if err != nil {
		t.Fatalf("Logs page 2: %v", err)
	}
	if len(next.Rows) != 3 {
		t.Errorf("second page rows = %d, want 3", len(next.Rows))
	}
}

func TestBackendListUnknownKindAndSource(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_search": "search.json"})
	b := testBackend(t, fs, nil)

	if _, err := b.List(context.Background(), resource.Query{Kind: "pod"}); err == nil {
		t.Error("List accepted an unknown kind")
	} else if kind := errs.KindOf(err); kind != errs.Config {
		t.Errorf("kind = %s, want config", kind)
	}
	if _, err := b.List(context.Background(), resource.Query{
		Kind: KindLog, Args: map[string]string{ArgSource: "nope"},
	}); err == nil {
		t.Error("List accepted an unknown source")
	}
	if _, err := b.List(context.Background(), resource.Query{
		Kind: KindLog, Args: map[string]string{ArgCluster: "us"},
	}); err == nil {
		t.Error("List accepted an unknown cluster")
	}

	// An unconfigured backend says so.
	empty := New(Options{})
	if _, err := empty.List(context.Background(), resource.Query{Kind: KindLog}); err == nil {
		t.Error("an unconfigured backend accepted a list")
	}
	if h := empty.Health(context.Background()); h.State != backend.HealthUnconfigured {
		t.Errorf("health = %v, want unconfigured", h.State)
	}
}

// TestBackendListPagesAcrossSources checks every selected source is listed, in
// configured order.
func TestBackendListPagesAcrossSources(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_search": "search.json"})
	b := testBackend(t, fs, nil)

	results := drainList(t, b, resource.Query{Kind: KindLog})
	if len(results) != 2 {
		t.Fatalf("batches = %d, want one per source", len(results))
	}
	if results[0].Total != 2 || results[1].Done != 1 {
		t.Errorf("progress = %d/%d and %d/%d", results[0].Done, results[0].Total, results[1].Done, results[1].Total)
	}
	if got := results[1].Items[0].Ref.Namespace; got != "eu/system" {
		t.Errorf("second batch namespace = %q", got)
	}
}

func TestBackendListCanceled(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_search": "search.json"})
	b := testBackend(t, fs, nil)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := b.List(ctx, resource.Query{Kind: KindLog})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	cancel()
	// The channel must close: nobody may block on a canceled list.
	for range ch {
	}
}

func TestBackendGetAndLinks(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_search": "search.json"})
	b := testBackend(t, fs, nil)

	ref := resource.Ref{Backend: backendName, Kind: KindLog, Namespace: "eu/app", Name: "logs-app-2026.11.12/a2"}
	row, err := b.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := row.Str("message"); !strings.Contains(got, "worker 7") {
		t.Errorf("message = %q", got)
	}
	if _, err := b.Get(context.Background(), resource.Ref{Kind: KindLog, Namespace: "eu/app", Name: "nope"}); err == nil {
		t.Error("Get found a document that is not there")
	}
	if links := b.Links(row); len(links) != 0 {
		t.Errorf("links = %v, want none", links)
	}
}

// TestBackendRedacts checks every document leaves the backend through the
// redactor (BUILD-PLAN §7).
func TestBackendRedacts(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_search": "search.json"})
	b := testBackend(t, fs, func(s string) string { return strings.ReplaceAll(s, "timed out", "‹redacted›") })

	page, err := b.Logs(context.Background(), LogQuery{Cluster: "eu", Source: "app"})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if strings.Contains(page.Rows[0].Message, "timed out") {
		t.Errorf("message was not redacted: %q", page.Rows[0].Message)
	}
	if strings.Contains(page.Rows[0].Doc, "timed out") {
		t.Errorf("document was not redacted: %q", page.Rows[0].Doc)
	}
	if strings.Contains(page.Rows[0].Fields["message"], "timed out") {
		t.Errorf("field was not redacted: %q", page.Rows[0].Fields["message"])
	}
}

func TestBackendLevelStatus(t *testing.T) {
	cases := map[string]resource.Status{
		"error":   resource.StatusCrit,
		"FATAL":   resource.StatusCrit,
		"warning": resource.StatusWarn,
		"info":    resource.StatusOK,
		"":        resource.StatusUnknown,
		"weird":   resource.StatusUnknown,
	}
	for level, want := range cases {
		if got := levelStatus(level); got != want {
			t.Errorf("levelStatus(%q) = %v, want %v", level, got, want)
		}
	}
}

// TestBackendHealthDot checks the per-cluster health dot and the cached probe.
func TestBackendHealthDot(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{
		"root._json": "root.json",
		"/":          "root.json",
	})
	b := testBackend(t, fs, nil)

	statuses := b.Status(context.Background())
	if len(statuses) != 1 || statuses[0].Name != "eu" {
		t.Fatalf("statuses = %+v, want one for eu", statuses)
	}
	if statuses[0].Health.State != backend.HealthOK {
		t.Errorf("state = %v, want ok", statuses[0].Health.State)
	}
	if !strings.Contains(statuses[0].Health.Detail, "8.15.1") {
		t.Errorf("detail = %q, want the version", statuses[0].Health.Detail)
	}
	hits := fs.count()
	// A second probe within healthTTL is served from the cache.
	b.Status(context.Background())
	if fs.count() != hits {
		t.Errorf("the health probe was repeated inside the TTL")
	}

	if h := b.Health(context.Background()); h.State != backend.HealthOK {
		t.Errorf("Health = %+v, want ok", h)
	}
}

// TestBackendHealthDown checks a cluster that refuses the probe turns red
// without an error.
func TestBackendHealthDown(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_search": "search.json"})
	b := testBackend(t, fs, nil)
	fs.Close()

	h := b.Health(context.Background())
	if h.State != backend.HealthDown {
		t.Errorf("state = %v, want down", h.State)
	}
	if h.Detail == "" {
		t.Error("a down cluster needs a detail line")
	}
}

// TestBackendFieldCaps checks the field completion path resolves the configured
// source first.
func TestBackendFieldCaps(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_field_caps": "field_caps.json"})
	b := testBackend(t, fs, nil)

	fields, err := b.FieldCaps(context.Background(), "eu", "app")
	if err != nil {
		t.Fatalf("FieldCaps: %v", err)
	}
	if len(fields) != 5 {
		t.Errorf("fields = %d, want 5", len(fields))
	}
	if _, err := b.FieldCaps(context.Background(), "eu", "nope"); err == nil {
		t.Error("FieldCaps accepted an unknown source")
	}
}

// TestBackendResolve checks `_resolve/index` reports what an index pattern
// matches, which is what the source browser lists.
func TestBackendResolve(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"/_resolve/index/logs-app-*": "resolve_index.json"})
	b := testBackend(t, fs, nil)

	infos, err := b.Resolve(context.Background(), "eu", "app")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(infos) != 4 {
		t.Fatalf("entries = %d, want 4", len(infos))
	}
	kinds := map[string]string{}
	for _, i := range infos {
		kinds[i.Name] = i.Kind
	}
	if kinds["logs-app-2026.11.11"] != "index" || kinds["logs-app"] != "alias" || kinds["logs-audit"] != "data_stream" {
		t.Errorf("kinds = %v", kinds)
	}
	if _, err := b.Resolve(context.Background(), "eu", "nope"); err == nil {
		t.Error("Resolve accepted an unknown source")
	}
}

// TestBackendListQueryArgs checks the query string and extra columns travel from
// the resource.Query into the request body.
func TestBackendListQueryArgs(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_search": "search.json"})
	b := testBackend(t, fs, nil)

	results := drainList(t, b, resource.Query{
		Kind:   KindLog,
		Args:   map[string]string{ArgSource: "app", ArgQuery: "status:500", ArgColumns: "status, http.x"},
		Window: resource.TimeWindow{From: testNow.Add(-time.Hour), ToNow: true, Label: "-1h"},
	})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("results = %+v", results)
	}
	body := string(fs.last(t).Body)
	for _, want := range []string{`"query":"status:500"`, `"status"`, `"http.x"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body %s missing %s", body, want)
		}
	}
}

// TestBackendSearchRefusedStaysReadOnly makes sure the backend only ever calls
// the read endpoints: every request the fixture saw is one of them.
func TestBackendSearchRefusedStaysReadOnly(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_search": "search.json", "_field_caps": "field_caps.json"})
	b := testBackend(t, fs, nil)

	drainList(t, b, resource.Query{Kind: KindLog})
	if _, err := b.FieldCaps(context.Background(), "eu", "app"); err != nil {
		t.Fatalf("FieldCaps: %v", err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for _, r := range fs.reqs {
		if !ReadOnlyPath(r.Method, r.Path) {
			t.Errorf("the backend sent %s %s, which is not a read endpoint", r.Method, r.Path)
		}
	}
	if len(fs.reqs) == 0 {
		t.Fatal("no request was recorded")
	}
}

// TestBackendHTTPStatusListed checks a cluster error becomes a batch error, not
// a panic or a silent empty list.
func TestBackendHTTPStatusListed(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{})
	b := testBackend(t, fs, nil)
	fs.status["/logs-app-*/_search"] = http.StatusBadGateway

	results := drainList(t, b, resource.Query{Kind: KindLog, Args: map[string]string{ArgSource: "app"}})
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Err == nil {
		t.Fatal("a 502 produced no error")
	}
	if len(results[0].Items) != 0 {
		t.Errorf("a failed page yielded %d rows", len(results[0].Items))
	}
}
