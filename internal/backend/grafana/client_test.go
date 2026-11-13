package grafana

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"jumper/internal/errs"
	"jumper/internal/transport/httpx"
)

// fixtureServer answers each API path from a recorded JSON body. Query
// parameters and request bodies are recorded so tests can assert what the
// client sent.
type fixtureServer struct {
	*httptest.Server

	mu     sync.Mutex
	query  map[string]url.Values
	hits   map[string]int
	bodies map[string][]byte
	method map[string]string
}

// newFixtureServer serves routes (API path → file in testdata).
func newFixtureServer(t *testing.T, routes map[string]string) *fixtureServer {
	t.Helper()
	fs := &fixtureServer{
		query:  map[string]url.Values{},
		hits:   map[string]int{},
		bodies: map[string][]byte{},
		method: map[string]string{},
	}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		fs.mu.Lock()
		fs.query[r.URL.Path] = r.URL.Query()
		fs.hits[r.URL.Path]++
		fs.bodies[r.URL.Path] = body
		fs.method[r.URL.Path] = r.Method
		fs.mu.Unlock()

		name, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"no fixture"}`))
			return
		}
		payload, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Errorf("read fixture %s: %v", name, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	t.Cleanup(fs.Close)
	return fs
}

// params returns the query parameters the last request to path carried.
func (fs *fixtureServer) params(path string) url.Values {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.query[path]
}

// body returns the last request body sent to path.
func (fs *fixtureServer) body(path string) []byte {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.bodies[path]
}

// hitCount counts the requests that reached path.
func (fs *fixtureServer) hitCount(path string) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.hits[path]
}

// newTestClient builds a client over fs through a real httpx client, the way
// the backend builds one.
func newTestClient(t *testing.T, fs *fixtureServer, name string) *Client {
	t.Helper()
	prof := &httpx.Profile{Name: "grafana-test"}
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

// dashboardPath is the API path of one dashboard fixture.
func dashboardPath(uid string) string { return pathDashboard + uid }

func TestClientSearch(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{pathSearch: "search.json"})
	c := newTestClient(t, fs, "eu")

	hits, err := c.Search(context.Background(), "node")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	if got, want := hits[0].Title, "Node overview"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
	if got, want := hits[0].Folder, "Linux"; got != want {
		t.Errorf("folderTitle = %q, want %q", got, want)
	}
	if got, want := hits[0].URL, "/d/a1b2c3/node-overview"; got != want {
		t.Errorf("url = %q, want %q", got, want)
	}
	if got, want := c.Absolute(hits[0].URL), fs.URL+"/d/a1b2c3/node-overview"; got != want {
		t.Errorf("absolute url = %q, want %q", got, want)
	}
	if len(hits[0].Tags) != 2 || hits[0].Tags[0] != "linux" {
		t.Errorf("tags = %v, want [linux node]", hits[0].Tags)
	}
	// The second hit carries the newer folder object shape.
	if got, want := hits[1].Folder, "Network"; got != want {
		t.Errorf("object folder = %q, want %q", got, want)
	}
	q := fs.params(pathSearch)
	if got, want := q.Get("type"), "dash-db"; got != want {
		t.Errorf("type parameter = %q, want %q", got, want)
	}
	if got, want := q.Get("query"), "node"; got != want {
		t.Errorf("query parameter = %q, want %q", got, want)
	}
}

func TestClientDashboard(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{dashboardPath("a1b2c3"): "dashboard.json"})
	c := newTestClient(t, fs, "eu")

	d, err := c.Dashboard(context.Background(), "a1b2c3")
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}
	if got, want := d.Title, "Node overview"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
	if got, want := d.Folder, "Linux"; got != want {
		t.Errorf("folder = %q, want %q", got, want)
	}
	if got, want := d.URL, fs.URL+"/d/a1b2c3/node-overview"; got != want {
		t.Errorf("url = %q, want %q", got, want)
	}
	if got, want := d.Time.From, "now-6h"; got != want {
		t.Errorf("time.from = %q, want %q", got, want)
	}
	if got, want := d.Time.Refresh, "1m"; got != want {
		t.Errorf("time.refresh = %q, want %q", got, want)
	}
	if len(d.Variables) != 4 {
		t.Fatalf("got %d variables, want 4", len(d.Variables))
	}
	host, ok := d.Variable("host")
	if !ok || host.Value() != "web-01.example.internal" {
		t.Errorf("host variable = %+v, want web-01.example.internal", host)
	}
	instance, _ := d.Variable("instance")
	if got := instance.Datasource; got.Name != "Prometheus" || got.Type != "prometheus" {
		t.Errorf("legacy datasource = %+v, want the name and its type", got)
	}
	if len(instance.Current) != 1 || instance.Query.RefID != "StandardVariableQuery" {
		t.Errorf("instance = %+v, want the object query form", instance)
	}
	env, _ := d.Variable("env")
	if len(env.Options) != 2 || env.Options[0].Value != "prod" || !env.Options[0].Selected {
		t.Errorf("env options = %+v, want prod selected and staging", env.Options)
	}

	rows := d.PanelRows()
	if len(rows) != 6 {
		t.Fatalf("got %d panel rows, want 6: %+v", len(rows), rows)
	}
	if got, want := rows[0].Section, "CPU"; got != want {
		t.Errorf("first section = %q, want %q", got, want)
	}
	if !rows[0].Supported || rows[0].Type != "timeseries" {
		t.Errorf("CPU busy = %+v, want a supported timeseries", rows[0])
	}
	if rows[2].Supported || rows[2].Type != "table" {
		t.Errorf("table = %+v, want unsupported", rows[2])
	}
	if got, want := rows[4].Type, "text"; got != want || rows[4].Supported {
		t.Errorf("text panel = %+v, want an unsupported text", rows[4])
	}
	if got, want := rows[5].Section, "Nested"; got != want {
		t.Errorf("nested section = %q, want %q", got, want)
	}
	if got, want := rows[5].Type, "graph"; got != want || !rows[5].Supported {
		t.Errorf("legacy graph = %+v, want a supported graph", rows[5])
	}
}

func TestClientDashboardLegacyRows(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{dashboardPath("legacy01"): "dashboard_legacy.json"})
	c := newTestClient(t, fs, "eu")

	d, err := c.Dashboard(context.Background(), "legacy01")
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}
	rows := d.PanelRows()
	if len(rows) != 1 {
		t.Fatalf("got %d panel rows, want 1", len(rows))
	}
	if got, want := rows[0].Section, "Row A"; got != want {
		t.Errorf("section = %q, want %q", got, want)
	}
	if got, want := rows[0].Title, "Old graph"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
}

func TestClientAlerts(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{pathAlerts: "alerts.json"})
	c := newTestClient(t, fs, "eu")

	alerts, err := c.Alerts(context.Background())
	if err != nil {
		t.Fatalf("Alerts: %v", err)
	}
	if len(alerts) != 2 {
		t.Fatalf("got %d alerts, want 2", len(alerts))
	}
	if got, want := alerts[0].Labels["alertname"], "NodeDown"; got != want {
		t.Errorf("alertname = %q, want %q", got, want)
	}
	if got, want := alerts[0].State, "firing"; got != want {
		t.Errorf("state = %q, want %q", got, want)
	}
	if !alerts[0].ActiveAt.Equal(time.Date(2026, 11, 12, 21, 0, 0, 0, time.UTC)) {
		t.Errorf("activeAt = %s, want 2026-11-12T21:00:00Z", alerts[0].ActiveAt)
	}
	if got, want := alerts[0].Annotations["summary"], "node down"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestClientResolveLabelValues(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{pathDSQuery: "ds_query.json"})
	c := newTestClient(t, fs, "eu")

	ds := Datasource{UID: "prom-1", Type: "prometheus"}
	values, err := c.ResolveLabelValues(context.Background(), ds, "label_values(node_load1, instance)")
	if err != nil {
		t.Fatalf("ResolveLabelValues: %v", err)
	}
	if len(values) != 3 || values[0] != "web-01.example.internal" {
		t.Errorf("values = %v, want three instances", values)
	}
	if got, want := fs.method[pathDSQuery], http.MethodPost; got != want {
		t.Errorf("method = %q, want %q", got, want)
	}
	var req DSRequest
	if err := json.Unmarshal(fs.body(pathDSQuery), &req); err != nil {
		t.Fatalf("request body: %v", err)
	}
	if len(req.Queries) != 1 {
		t.Fatalf("got %d queries, want 1", len(req.Queries))
	}
	q := req.Queries[0]
	if got, want := q.Expr, "label_values(node_load1, instance)"; got != want {
		t.Errorf("expr = %q, want %q", got, want)
	}
	if !q.Instant || q.Range {
		t.Errorf("query = %+v, want an instant query", q)
	}
	if got, want := q.Datasource.UID, "prom-1"; got != want {
		t.Errorf("datasource uid = %q, want %q", got, want)
	}
	if got, want := q.Datasource.Type, "prometheus"; got != want {
		t.Errorf("datasource type = %q, want %q", got, want)
	}
}

func TestClientHealthy(t *testing.T) {
	ok := newFixtureServer(t, map[string]string{pathHealth: "health.json"})
	c := newTestClient(t, ok, "eu")
	h := c.Healthy(context.Background())
	if !h.Health || h.Name != "eu" {
		t.Fatalf("Healthy = %+v, want a healthy eu", h)
	}
	if got, want := h.Detail, "11.2.0 healthy"; got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}

	down := newFixtureServer(t, map[string]string{pathHealth: "health_down.json"})
	c = newTestClient(t, down, "eu")
	h = c.Healthy(context.Background())
	if h.Health {
		t.Fatalf("Healthy = %+v, want unhealthy", h)
	}
	if got, want := h.Detail, "database unreachable"; got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}

	// No fixture means 404: any answer is a Health, never an error.
	missing := newFixtureServer(t, map[string]string{})
	c = newTestClient(t, missing, "eu")
	if h = c.Healthy(context.Background()); h.Health || h.Detail == "" {
		t.Errorf("Healthy = %+v, want a down status with a detail", h)
	}
}

func TestClientErrorKinds(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{})
	c := newTestClient(t, fs, "eu")

	_, err := c.Dashboard(context.Background(), "missing")
	if err == nil {
		t.Fatal("Dashboard returned no error")
	}
	if got := errs.KindOf(err); got != errs.NotAvailable {
		t.Errorf("404 kind = %q, want %q (%v)", got, errs.NotAvailable, err)
	}

	locked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Unauthorized"}`))
	}))
	t.Cleanup(locked.Close)
	prof := &httpx.Profile{Name: "grafana-test"}
	hc, err := prof.Client(httpx.Deps{})
	if err != nil {
		t.Fatalf("httpx client: %v", err)
	}
	authClient, err := NewClient(ClientOptions{URL: locked.URL, Client: hc, Name: "eu"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := authClient.Search(context.Background(), ""); err == nil {
		t.Fatal("Search returned no error")
	} else if got := errs.KindOf(err); got != errs.Auth {
		t.Errorf("401 kind = %q, want %q (%v)", got, errs.Auth, err)
	}
}

func TestClientRejectsBadInput(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{pathDSQuery: "ds_query.json"})
	c := newTestClient(t, fs, "eu")
	ctx := context.Background()

	cases := []struct {
		name string
		err  error
	}{
		{"empty uid", second(c.Dashboard(ctx, "  "))},
		{"empty query request", second(c.QueryData(ctx, DSRequest{}))},
		{"empty variable expression", second(c.ResolveLabelValues(ctx, Datasource{UID: "p"}, " "))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err == nil {
				t.Fatal("got no error")
			}
			if got := errs.KindOf(tc.err); got != errs.Config {
				t.Errorf("kind = %q, want %q (%v)", got, errs.Config, tc.err)
			}
		})
	}

	if _, err := NewClient(ClientOptions{URL: "grafana.example.internal"}); err == nil {
		t.Error("NewClient accepted a URL without a scheme")
	}
}

// second returns the error of a two-value call.
func second[T any](v T, err error) error { return err }
