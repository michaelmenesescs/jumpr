package elastic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/transport/httpx"
)

// recorded is one request the fixture server saw.
type recorded struct {
	Method string
	Path   string
	Query  url.Values
	Body   []byte
}

// fixtureServer answers API paths from recorded JSON bodies and remembers every
// request, so a test can assert exactly what the client sent.
type fixtureServer struct {
	*httptest.Server

	mu     sync.Mutex
	routes map[string]string
	reqs   []recorded
	status map[string]int
}

// newFixtureServer serves routes (API path or path suffix → testdata file).
func newFixtureServer(t *testing.T, routes map[string]string) *fixtureServer {
	t.Helper()
	fs := &fixtureServer{routes: routes, status: map[string]int{}}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		fs.mu.Lock()
		fs.reqs = append(fs.reqs, recorded{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Body: body})
		name, ok := fs.route(r.URL.Path)
		code := fs.status[r.URL.Path]
		fs.mu.Unlock()

		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"root_cause":[{"type":"index_not_found_exception","reason":"no such index"}],"type":"index_not_found_exception","reason":"no such index"},"status":404}`))
			return
		}
		payload, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Errorf("read fixture %s: %v", name, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if code != 0 {
			w.WriteHeader(code)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	t.Cleanup(fs.Close)
	return fs
}

// route finds the fixture for a path: an exact key wins, then a suffix key.
// The caller holds fs.mu.
func (fs *fixtureServer) route(path string) (string, bool) {
	if name, ok := fs.routes[path]; ok {
		return name, true
	}
	for key, name := range fs.routes {
		if strings.HasSuffix(path, key) {
			return name, true
		}
	}
	return "", false
}

// last returns the last recorded request.
func (fs *fixtureServer) last(t *testing.T) recorded {
	t.Helper()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.reqs) == 0 {
		t.Fatal("no request reached the fixture server")
	}
	return fs.reqs[len(fs.reqs)-1]
}

// count counts the requests the server saw.
func (fs *fixtureServer) count() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.reqs)
}

// newTestClient builds a client over fs through a real httpx client, the way
// the backend builds one.
func newTestClient(t *testing.T, fs *fixtureServer, name string) *Client {
	t.Helper()
	prof := &httpx.Profile{Name: "elastic-test"}
	hc, err := prof.Client(httpx.Deps{})
	if err != nil {
		t.Fatalf("httpx client: %v", err)
	}
	c, err := NewClient(ClientOptions{URL: fs.URL, Client: hc, Name: name})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// testSource is the log source every client test searches.
func testSource() LogSource {
	return LogSource{
		Name:         "app",
		Index:        "logs-app-*",
		TimeField:    "@timestamp",
		HostField:    "host.name",
		MessageField: "message",
		LevelField:   "log.level",
	}
}

// assertJSON compares two JSON documents structurally.
func assertJSON(t *testing.T, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("expected body is not JSON: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		gp, _ := json.MarshalIndent(g, "", "  ")
		wp, _ := json.MarshalIndent(w, "", "  ")
		t.Errorf("request body mismatch\n got: %s\nwant: %s", gp, wp)
	}
}

func TestClientSearchRequest(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_search": "search.json"})
	c := newTestClient(t, fs, "eu")

	from := time.Date(2026, 11, 12, 14, 30, 0, 0, time.UTC)
	to := time.Date(2026, 11, 12, 15, 30, 0, 0, time.UTC)
	page, err := c.Search(context.Background(), SearchOptions{
		Source:      testSource(),
		Hosts:       []resource.HostID{"web-01.example.internal", "db-01"},
		QueryString: "status:500",
		From:        from,
		To:          to,
		SearchAfter: []any{float64(1762957960000), float64(1)},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	req := fs.last(t)
	if req.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.Method)
	}
	if want := "/logs-app-*/_search"; req.Path != want {
		t.Errorf("path = %q, want %q", req.Path, want)
	}
	assertJSON(t, req.Body, []byte(`{
		"size": 500,
		"track_total_hits": false,
		"_source": ["@timestamp", "host.name", "message", "log.level"],
		"query": {"bool": {"filter": [
			{"range": {"@timestamp": {"gte": "2026-11-12T14:30:00Z", "lte": "2026-11-12T15:30:00Z"}}},
			{"terms": {"host.name": ["db-01", "web-01", "web-01.example.internal"]}}
		], "must": [
			{"query_string": {"query": "status:500"}}
		]}},
		"sort": [{"@timestamp": {"order": "asc"}}, {"_doc": {"order": "asc"}}],
		"search_after": [1762957960000, 1]
	}`))

	if len(page.Hits) != 3 {
		t.Fatalf("got %d hits, want 3", len(page.Hits))
	}
	if got, want := page.Hits[0].ID, "a1"; got != want {
		t.Errorf("first hit id = %q, want %q", got, want)
	}
	if got := textValue(lookupPath(page.Hits[0].Source, "host.name")); got != "web-01.example.internal" {
		t.Errorf("nested host.name = %q", got)
	}
	if len(page.Hits[0].Sort) != 2 {
		t.Errorf("sort values = %v, want 2", page.Hits[0].Sort)
	}
}

// TestClientSearchCrossCluster checks a cross-cluster pattern passes through the
// path unchanged, colon and all.
func TestClientSearchCrossCluster(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_search": "search.json"})
	c := newTestClient(t, fs, "eu")

	src := testSource()
	src.Index = "remote-eu:logs-app-*"
	if _, err := c.Search(context.Background(), SearchOptions{Source: src}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got, want := fs.last(t).Path, "/remote-eu:logs-app-*/_search"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

// TestClientSearchPaging checks search_after paging: the second page carries the
// sort of the last hit of the first, and the PIT body asks for the _shard_doc
// tiebreaker.
func TestClientSearchPaging(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_search": "search.json"})
	c := newTestClient(t, fs, "eu")

	first, err := c.Search(context.Background(), SearchOptions{Source: testSource(), PIT: "pit-1"})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	var body struct {
		Sort        []map[string]map[string]string `json:"sort"`
		PIT         map[string]string              `json:"pit"`
		SearchAfter []any                          `json:"search_after"`
	}
	if err := json.Unmarshal(fs.last(t).Body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Sort) != 2 || body.Sort[1]["_shard_doc"]["order"] != "asc" {
		t.Errorf("sort = %v, want a _shard_doc tiebreaker", body.Sort)
	}
	if body.PIT["id"] != "pit-1" || body.PIT["keep_alive"] == "" {
		t.Errorf("pit = %v, want the id and a keep_alive", body.PIT)
	}

	after := first.Hits[len(first.Hits)-1].Sort
	if _, err := c.Search(context.Background(), SearchOptions{Source: testSource(), SearchAfter: after, PIT: "pit-1"}); err != nil {
		t.Fatalf("second page: %v", err)
	}
	if err := json.Unmarshal(fs.last(t).Body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.SearchAfter) != len(after) {
		t.Errorf("search_after = %v, want %v", body.SearchAfter, after)
	}
}

// TestClientErrorEnvelope checks `error.root_cause` becomes the message and the
// status code chooses the kind.
func TestClientErrorEnvelope(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_search": "error.json"})
	fs.status["/logs-app-*/_search"] = http.StatusBadRequest
	c := newTestClient(t, fs, "eu")

	_, err := c.Search(context.Background(), SearchOptions{Source: testSource()})
	if err == nil {
		t.Fatal("Search accepted a 400")
	}
	if kind := errs.KindOf(err); kind != errs.Remote {
		t.Errorf("kind = %s, want remote", kind)
	}
	msg := err.Error()
	for _, want := range []string{"status 400", "failed to create query", "query_shard_exception"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}

	// A 404 is not-available and still carries the root cause.
	_, err = c.Resolve(context.Background(), "missing")
	if err == nil {
		t.Fatal("Resolve accepted a 404")
	}
	if kind := errs.KindOf(err); kind != errs.NotAvailable {
		t.Errorf("kind = %s, want not-available", kind)
	}
	if !strings.Contains(err.Error(), "no such index") {
		t.Errorf("error %q missing the root cause", err)
	}
}

// TestClientRefusedPaths is the read-only guarantee: every path that is not one
// of the five read endpoints is refused, and nothing is sent.
func TestClientRefusedPaths(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_search": "search.json"})
	c := newTestClient(t, fs, "eu")

	refused := []struct{ method, path string }{
		{http.MethodPost, "/logs-app-*/_doc"},
		{http.MethodPost, "/_bulk"},
		{http.MethodPost, "/logs-app-*/_delete_by_query"},
		{http.MethodPost, "/logs-app-*/_update/1"},
		{http.MethodDelete, "/_pit"},
		{http.MethodPut, "/logs-app-*/_search"},
		{http.MethodGet, "/_cluster/settings"},
		{http.MethodGet, "/../_search"},
		{http.MethodGet, "/_search?q=secret"},
	}
	for _, tc := range refused {
		if ReadOnlyPath(tc.method, tc.path) {
			t.Errorf("ReadOnlyPath(%s %s) = true, want false", tc.method, tc.path)
		}
		if err := c.exchange(context.Background(), "test", tc.method, tc.path, nil, nil, nil); err == nil {
			t.Errorf("%s %s was not refused", tc.method, tc.path)
		} else if kind := errs.KindOf(err); kind != errs.Config {
			t.Errorf("%s %s: kind = %s, want config", tc.method, tc.path, kind)
		} else if !strings.Contains(err.Error(), "not a read-only endpoint") {
			t.Errorf("%s %s: error = %q", tc.method, tc.path, err)
		}
	}
	if n := fs.count(); n != 0 {
		t.Errorf("a refused path reached the server (%d requests)", n)
	}

	// The five read endpoints are allowed.
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/logs-app-*/_search"},
		{http.MethodGet, "/logs-app-*/_field_caps"},
		{http.MethodGet, "/_resolve/index/logs-*"},
		{http.MethodGet, "/_cat/indices/logs-*"},
		{http.MethodPost, "/logs-app-*/_pit"},
		{http.MethodGet, "/"},
	} {
		if !ReadOnlyPath(tc.method, tc.path) {
			t.Errorf("ReadOnlyPath(%s %s) = false, want true", tc.method, tc.path)
		}
	}
}

// TestClientBadIndexPattern checks a pattern that could change the endpoint is a
// config error rather than a request.
func TestClientBadIndexPattern(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_search": "search.json"})
	c := newTestClient(t, fs, "eu")

	src := testSource()
	src.Index = "logs/../_bulk"
	_, err := c.Search(context.Background(), SearchOptions{Source: src})
	if err == nil {
		t.Fatal("Search accepted a path traversal in the index pattern")
	}
	if kind := errs.KindOf(err); kind != errs.Config {
		t.Errorf("kind = %s, want config", kind)
	}
	if n := fs.count(); n != 0 {
		t.Errorf("%d requests reached the server, want 0", n)
	}
}

func TestClientFieldCaps(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{"_field_caps": "field_caps.json"})
	c := newTestClient(t, fs, "eu")

	fields, err := c.FieldCaps(context.Background(), "logs-app-*", "*")
	if err != nil {
		t.Fatalf("FieldCaps: %v", err)
	}
	names := make([]string, len(fields))
	for i, f := range fields {
		names[i] = f.Name
	}
	want := []string{"@timestamp", "host.name", "log.level", "message", "status"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("fields = %v, want %v (sorted)", names, want)
	}
	if fields[1].Type != "keyword" || fields[3].Type != "text" {
		t.Errorf("types = %+v, want keyword and text", fields)
	}
	req := fs.last(t)
	if req.Method != http.MethodGet || req.Path != "/logs-app-*/_field_caps" {
		t.Errorf("request = %s %s", req.Method, req.Path)
	}
	if got, want := req.Query.Get("fields"), "*"; got != want {
		t.Errorf("fields parameter = %q, want %q", got, want)
	}
}

func TestClientResolveAndCat(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{
		"/_resolve/index/logs-*": "resolve_index.json",
		"/_cat/indices/logs-*":   "cat_indices.json",
	})
	c := newTestClient(t, fs, "eu")

	infos, err := c.Resolve(context.Background(), "logs-*")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	kinds := map[string]string{}
	for _, i := range infos {
		kinds[i.Name] = i.Kind
	}
	if len(infos) != 4 {
		t.Fatalf("got %d entries, want 4: %+v", len(infos), infos)
	}
	if kinds["logs-app-2026.11.12"] != "index" {
		t.Errorf("index kind = %q", kinds["logs-app-2026.11.12"])
	}
	if kinds["logs-app"] != "alias" {
		t.Errorf("alias kind = %q", kinds["logs-app"])
	}
	if kinds["logs-audit"] != "data_stream" {
		t.Errorf("data stream kind = %q", kinds["logs-audit"])
	}

	cat, err := c.CatIndices(context.Background(), "logs-*")
	if err != nil {
		t.Fatalf("CatIndices: %v", err)
	}
	if len(cat) != 2 || cat[1].Docs != 9104 || cat[1].Health != "yellow" {
		t.Fatalf("cat = %+v", cat)
	}
	if got, want := fs.last(t).Query.Get("format"), "json"; got != want {
		t.Errorf("format parameter = %q, want %q", got, want)
	}
}

func TestClientOpenPITAndHealth(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{
		"_pit": "pit.json",
		"/":    "root.json",
	})
	c := newTestClient(t, fs, "eu")

	id, err := c.OpenPIT(context.Background(), "logs-app-*", "2m")
	if err != nil {
		t.Fatalf("OpenPIT: %v", err)
	}
	if id == "" {
		t.Fatal("OpenPIT returned no id")
	}
	req := fs.last(t)
	if req.Method != http.MethodPost || req.Path != "/logs-app-*/_pit" {
		t.Errorf("request = %s %s", req.Method, req.Path)
	}
	if got, want := req.Query.Get("keep_alive"), "2m"; got != want {
		t.Errorf("keep_alive = %q, want %q", got, want)
	}

	h := c.Healthy(context.Background())
	if !h.Health {
		t.Fatalf("Health = %+v, want healthy", h)
	}
	if h.Version != "8.15.1" || h.Cluster != "logs-eu" {
		t.Errorf("health = %+v", h)
	}

	// An unreachable cluster is a Health, not an error.
	fs.Close()
	h = c.Healthy(context.Background())
	if h.Health || h.Detail == "" {
		t.Errorf("health of a closed server = %+v", h)
	}
}

func TestNewClientRejectsBadURL(t *testing.T) {
	for _, u := range []string{"", "elastic.example.internal:9200", "ftp://elastic.example.internal"} {
		if _, err := NewClient(ClientOptions{URL: u, Name: "eu"}); err == nil {
			t.Errorf("NewClient(%q) accepted a non-http URL", u)
		} else if kind := errs.KindOf(err); kind != errs.Config {
			t.Errorf("NewClient(%q): kind = %s, want config", u, kind)
		}
	}
}
