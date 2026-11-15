package prom

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/transport/httpx"
)

// fixtureServer answers each API path from a recorded JSON body. Query
// parameters are recorded so tests can assert what the client sent.
type fixtureServer struct {
	*httptest.Server

	mu    sync.Mutex
	query map[string]url.Values
	hits  map[string]int
}

// newFixtureServer serves routes (API path → file in testdata). /-/healthy
// answers 200 with Prometheus' text.
func newFixtureServer(t *testing.T, routes map[string]string) *fixtureServer {
	t.Helper()
	fs := &fixtureServer{query: map[string]url.Values{}, hits: map[string]int{}}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		fs.query[r.URL.Path] = r.URL.Query()
		fs.hits[r.URL.Path]++
		fs.mu.Unlock()
		if r.URL.Path == pathHealthy {
			_, _ = w.Write([]byte("Prometheus is Healthy.\n"))
			return
		}
		name, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":"error","errorType":"not_found","error":"no fixture"}`))
			return
		}
		body, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Errorf("read fixture %s: %v", name, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
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

// hits counts the requests that reached path.
func (fs *fixtureServer) hitCount(path string) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.hits[path]
}

// newTestClient builds a client over fs through a real httpx client, the way
// the backend builds one.
func newTestClient(t *testing.T, fs *fixtureServer, name string) *Client {
	t.Helper()
	prof := &httpx.Profile{Name: "prom-test"}
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

func TestClientQueryVector(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{pathQuery: "vector.json"})
	c := newTestClient(t, fs, "eu")

	vec, err := c.Query(context.Background(), `up{job="node"}`, time.Unix(1763100000, 0))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(vec) != 3 {
		t.Fatalf("got %d series, want 3", len(vec))
	}
	if got, want := vec[0].Metric["instance"], "web-01.example.internal:9100"; got != want {
		t.Errorf("instance = %q, want %q", got, want)
	}
	if len(vec[0].Values) != 1 || vec[0].Values[0].V != 1 {
		t.Errorf("web-01 value = %+v, want a single 1", vec[0].Values)
	}
	if got := vec[2].Values[0].V; !math.IsNaN(got) {
		t.Errorf("db-01 value = %v, want NaN", got)
	}
	if got := vec[0].Values[0].T; !got.Equal(time.Unix(1763100000, 123000000)) {
		t.Errorf("sample time = %s, want the sub-second timestamp", got)
	}
	if got, want := fs.params(pathQuery).Get("time"), "2025-11-14T06:00:00Z"; got != want {
		t.Errorf("time parameter = %q, want %q", got, want)
	}
}

func TestClientQueryRangeMatrix(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{pathQueryRange: "matrix.json"})
	c := newTestClient(t, fs, "eu")

	from := time.Unix(1763099700, 0)
	to := from.Add(5 * time.Minute)
	mat, err := c.QueryRange(context.Background(), "node_load1", from, to, 13*time.Second)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(mat) != 2 {
		t.Fatalf("got %d series, want 2", len(mat))
	}
	if len(mat[0].Values) != 6 {
		t.Fatalf("first series has %d samples, want 6", len(mat[0].Values))
	}
	if !math.IsNaN(mat[0].Values[2].V) {
		t.Errorf("sample 2 = %v, want NaN", mat[0].Values[2].V)
	}
	if !math.IsInf(mat[0].Values[4].V, 1) {
		t.Errorf("sample 4 = %v, want +Inf", mat[0].Values[4].V)
	}
	if got, want := mat[0].Values[5].V, 0.71; got != want {
		t.Errorf("last sample = %v, want %v", got, want)
	}
	q := fs.params(pathQueryRange)
	if got, want := q.Get("step"), "13"; got != want {
		t.Errorf("step parameter = %q, want %q", got, want)
	}
	if got, want := q.Get("query"), "node_load1"; got != want {
		t.Errorf("query parameter = %q, want %q", got, want)
	}
	if q.Get("start") == "" || q.Get("end") == "" {
		t.Errorf("start/end missing from %v", q)
	}
}

func TestClientErrorEnvelope(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{pathQuery: "error.json"})
	c := newTestClient(t, fs, "eu")

	_, err := c.Query(context.Background(), "foo(", time.Now())
	if err == nil {
		t.Fatal("Query returned no error")
	}
	if got := errs.KindOf(err); got != errs.Remote {
		t.Fatalf("kind = %q, want %q (%v)", got, errs.Remote, err)
	}
	want := `bad_data: invalid parameter "query": 1:6: parse error: unexpected identifier "foo"`
	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("error is not *errs.Error: %T", err)
	}
	if e.Msg != want {
		t.Errorf("message = %q, want %q", e.Msg, want)
	}
	if e.Host != "eu" || e.Op != "prom.query" {
		t.Errorf("op/host = %q/%q, want prom.query/eu", e.Op, e.Host)
	}
}

func TestClientWarnings(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{pathQuery: "warnings.json"})
	var got []string
	prof := &httpx.Profile{Name: "prom-test"}
	hc, err := prof.Client(httpx.Deps{})
	if err != nil {
		t.Fatalf("httpx client: %v", err)
	}
	c, err := NewClient(ClientOptions{
		URL: fs.URL, Client: hc, Name: "eu",
		Warn: func(msg string) { got = append(got, msg) },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if _, err := c.Query(context.Background(), "up", time.Now()); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d warnings, want 2: %v", len(got), got)
	}
	if want := "prometheus eu: 1:2: this query matched 128 series; consider adding a label filter"; got[0] != want {
		t.Errorf("warning 0 = %q, want %q", got[0], want)
	}
}

func TestClientTargets(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{pathTargets: "targets.json"})
	c := newTestClient(t, fs, "eu")

	ts, err := c.Targets(context.Background())
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(ts) != 2 {
		t.Fatalf("got %d targets, want 2", len(ts))
	}
	if got, want := ts[0].Labels["job"], "node"; got != want {
		t.Errorf("job = %q, want %q", got, want)
	}
	if got, want := ts[0].Health, "up"; got != want {
		t.Errorf("health = %q, want %q", got, want)
	}
	if ts[0].LastScrape.IsZero() {
		t.Error("last scrape is zero, want the recorded time")
	}
	if got, want := ts[1].LastError, "dial tcp 192.0.2.21:9100: connect: connection refused"; got != want {
		t.Errorf("last error = %q, want %q", got, want)
	}
}

func TestClientRules(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{pathRules: "rules.json"})
	c := newTestClient(t, fs, "eu")

	groups, err := c.Rules(context.Background())
	if err != nil {
		t.Fatalf("Rules: %v", err)
	}
	if len(groups) != 1 || len(groups[0].Rules) != 2 {
		t.Fatalf("got %d groups with %d rules, want 1 with 2", len(groups), len(groups[0].Rules))
	}
	alerting := groups[0].Rules[0]
	if got, want := alerting.State, "firing"; got != want {
		t.Errorf("state = %q, want %q (derived from its alerts)", got, want)
	}
	if got, want := alerting.Duration, "5m0s"; got != want {
		t.Errorf("duration = %q, want %q", got, want)
	}
	if got, want := alerting.Alerts[0].Labels["alertname"], "NodeDown"; got != want {
		t.Errorf("alert name = %q, want %q", got, want)
	}
	if got := groups[0].Rules[1]; got.State != "" || got.Type != "recording" {
		t.Errorf("recording rule = %+v, want no state", got)
	}
}

func TestClientAlerts(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{pathAlerts: "alerts.json"})
	c := newTestClient(t, fs, "eu")

	as, err := c.Alerts(context.Background())
	if err != nil {
		t.Fatalf("Alerts: %v", err)
	}
	if len(as) != 1 {
		t.Fatalf("got %d alerts, want 1", len(as))
	}
	if got, want := as[0].State, "firing"; got != want {
		t.Errorf("state = %q, want %q", got, want)
	}
	if got, want := as[0].Annotations["summary"], "node down"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestClientSeriesAndLabelValues(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{
		pathSeries:                     "series.json",
		pathLabelValues + "job/values": "labelvalues.json",
	})
	c := newTestClient(t, fs, "eu")

	from := time.Unix(1763099700, 0)
	series, err := c.Series(context.Background(), []string{`up{job="node"}`}, from, from.Add(time.Hour))
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("got %d series, want 2", len(series))
	}
	if got, want := series[0]["__name__"], "up"; got != want {
		t.Errorf("__name__ = %q, want %q", got, want)
	}
	if got := fs.params(pathSeries).Get("match[]"); got != `up{job="node"}` {
		t.Errorf("match[] = %q", got)
	}

	values, err := c.LabelValues(context.Background(), "job")
	if err != nil {
		t.Fatalf("LabelValues: %v", err)
	}
	if len(values) != 2 || values[0] != "node" {
		t.Errorf("label values = %v, want [node prometheus]", values)
	}
}

func TestClientStep(t *testing.T) {
	base := time.Unix(1763100000, 0)
	cases := []struct {
		name     string
		from, to time.Time
		max      int
		want     time.Duration
	}{
		{"one hour over 300 points", base, base.Add(time.Hour), MaxPoints, 13 * time.Second},
		{"five minutes", base, base.Add(5 * time.Minute), MaxPoints, 2 * time.Second},
		{"thirty seconds floors at one second", base, base.Add(30 * time.Second), MaxPoints, time.Second},
		{"a day", base, base.Add(24 * time.Hour), MaxPoints, 289 * time.Second},
		{"ten points", base, base.Add(time.Hour), 10, 400 * time.Second},
		{"zero length range", base, base, MaxPoints, time.Second},
		{"inverted range", base, base.Add(-time.Hour), MaxPoints, time.Second},
		{"max points below two means the default", base, base.Add(time.Hour), 1, 13 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Step(tc.from, tc.to, tc.max); got != tc.want {
				t.Errorf("Step(%s, %s, %d) = %s, want %s", tc.from, tc.to, tc.max, got, tc.want)
			}
		})
	}
}

// TestStepKeepsPointCount checks the property the step exists for: the number
// of endpoints a range query returns never exceeds the cap.
func TestStepKeepsPointCount(t *testing.T) {
	from := time.Unix(1763100000, 0)
	for _, span := range []time.Duration{
		30 * time.Second, 5 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour, 7 * 24 * time.Hour,
	} {
		to := from.Add(span)
		step := Step(from, to, MaxPoints)
		points := int(to.Sub(from)/step) + 1
		if points > MaxPoints {
			t.Errorf("span %s: %d points with step %s, want at most %d", span, points, step, MaxPoints)
		}
	}
}

func TestClientRejectsBadInput(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{})
	c := newTestClient(t, fs, "eu")
	ctx := context.Background()

	cases := []struct {
		name string
		err  error
	}{
		{"empty expression", second(c.Query(ctx, "  ", time.Now()))},
		{"zero step", second(c.QueryRange(ctx, "up", time.Now(), time.Now().Add(time.Hour), 0))},
		{"inverted range", second(c.QueryRange(ctx, "up", time.Now(), time.Now().Add(-time.Hour), time.Minute))},
		{"no series selector", second(c.Series(ctx, nil, time.Time{}, time.Time{}))},
		{"empty label", second(c.LabelValues(ctx, " "))},
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

	if _, err := NewClient(ClientOptions{URL: "prometheus.example.internal"}); err == nil {
		t.Error("NewClient accepted a URL without a scheme")
	}
}

func TestClientHealthy(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{})
	c := newTestClient(t, fs, "eu")
	h := c.Healthy(context.Background())
	if !h.Health || h.Name != "eu" {
		t.Fatalf("Healthy = %+v, want a healthy eu", h)
	}
}

func TestLabelString(t *testing.T) {
	cases := []struct {
		labels map[string]string
		want   string
	}{
		{nil, "{}"},
		{map[string]string{"job": "node"}, `{job="node"}`},
		{
			map[string]string{"instance": "a:9100", "__name__": "up", "job": "node"},
			`{__name__="up", instance="a:9100", job="node"}`,
		},
	}
	for _, tc := range cases {
		if got := LabelString(tc.labels); got != tc.want {
			t.Errorf("LabelString(%v) = %q, want %q", tc.labels, got, tc.want)
		}
	}
}

// second returns the error of a two-value call.
func second[T any](v T, err error) error { return err }
