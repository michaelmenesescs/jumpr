package grafana

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
	"jumpr/internal/transport/httpx"
)

// window is the fixed range the query tests cover: one hour ending at an
// exact epoch second.
func window(t *testing.T) (time.Time, time.Time) {
	t.Helper()
	to := time.UnixMilli(1763100000000)
	return to.Add(-time.Hour), to
}

func TestInterpolateVariables(t *testing.T) {
	from, to := window(t)
	vars := Vars{
		"host":     {"web-01.example.internal"},
		"instance": {"a.example.internal", "b.example.internal"},
		"job":      {"node"},
		"old":      {"legacy"},
	}
	iv := NewInterpolation(vars, from, to, 200)

	cases := []struct{ in, want string }{
		{`up{instance="$host"}`, `up{instance="web-01.example.internal"}`},
		{`up{instance=~"$instance"}`, `up{instance=~"a.example.internal|b.example.internal"}`},
		{`up{job="${job}"}`, `up{job="node"}`},
		{`up{job="[[job]]"}`, `up{job="node"}`},
		{`up{job="${instance:pipe}"}`, `up{job="a.example.internal|b.example.internal"}`},
		{`up{job="${instance:csv}"}`, `up{job="a.example.internal,b.example.internal"}`},
		{`up{job="${instance:json}"}`, `up{job="["a.example.internal","b.example.internal"]"}`},
		{`up{job="$nope"}`, `up{job="$nope"}`},
		{`cost $$5`, `cost $5`},
		{`$__from`, "1763096400000"},
	}
	for _, c := range cases {
		if got := iv.Apply(c.in); got != c.want {
			t.Errorf("Apply(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// $job must not be read as the prefix of $jobs.
	if got := iv.Apply(`$job $jobs`); got != `node $jobs` {
		t.Errorf("Apply($job $jobs) = %q", got)
	}
}

func TestInterpolateAllValue(t *testing.T) {
	from, to := window(t)
	iv := NewInterpolation(Vars{"instance": {"$__all"}}, from, to, 100)
	if got := iv.Apply(`up{instance=~"$instance"}`); got != `up{instance=~".*"}` {
		t.Errorf("Apply = %q, want the all value", got)
	}
	iv.AllValue = "prod-.*"
	if got := iv.Apply(`up{instance=~"$instance"}`); got != `up{instance=~"prod-.*"}` {
		t.Errorf("Apply = %q, want the configured all value", got)
	}
}

func TestInterpolateBuiltins(t *testing.T) {
	from, to := window(t) // one hour, 200 points → 18s
	iv := NewInterpolation(nil, from, to, 200)

	if got, want := iv.Apply("$__range"), "3600s"; got != want {
		t.Errorf("$__range = %q, want %q", got, want)
	}
	if got, want := iv.Apply("$__range_s"), "3600"; got != want {
		t.Errorf("$__range_s = %q, want %q", got, want)
	}
	if got, want := iv.Apply("$__interval"), "18s"; got != want {
		t.Errorf("$__interval = %q, want %q", got, want)
	}
	if got, want := iv.Apply("$__interval_ms"), "18000"; got != want {
		t.Errorf("$__interval_ms = %q, want %q", got, want)
	}
	if got, want := iv.Apply("$__rate_interval"), "72s"; got != want {
		t.Errorf("$__rate_interval = %q, want %q", got, want)
	}
	if got, want := iv.Apply("$__to"), "1763100000000"; got != want {
		t.Errorf("$__to = %q, want %q", got, want)
	}
	// A whole-hour interval reads in minutes, a whole-minute one in minutes.
	hour := NewInterpolation(nil, from, to, 60) // 60s
	if got, want := hour.Apply("$__interval"), "1m"; got != want {
		t.Errorf("$__interval over an hour with 60 points = %q, want %q", got, want)
	}
}

func TestInterpolateFormatInterval(t *testing.T) {
	cases := map[time.Duration]string{
		0:                      "1s",
		500 * time.Millisecond: "500ms",
		90 * time.Second:       "90s",
		2 * time.Minute:        "2m",
		3 * time.Hour:          "3h",
	}
	for d, want := range cases {
		if got := formatInterval(d); got != want {
			t.Errorf("formatInterval(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestInterpolateQueryVars(t *testing.T) {
	d := &Dashboard{Variables: []Variable{
		{Name: "host", Current: []string{"web-01.example.internal"}},
		{Name: "dc", Current: []string{"ewr", "lax"}},
		{Name: "empty"},
	}}
	vars := d.QueryVars()
	if got := vars.Get("host"); len(got) != 1 || got[0] != "web-01.example.internal" {
		t.Errorf("host = %v", got)
	}
	if got := vars.Get("$dc"); len(got) != 2 {
		t.Errorf("dc = %v, want both values", got)
	}
	from, to := window(t)
	iv := NewInterpolation(vars, from, to, 100)
	if got, want := iv.Apply(`up{dc=~"$dc"}`), `up{dc=~"ewr|lax"}`; got != want {
		t.Errorf("Apply = %q, want %q", got, want)
	}
	if got := VarsOf(map[string]string{"$host": "web-02.example.internal"}).Get("host"); len(got) != 1 {
		t.Errorf("VarsOf did not strip the $: %v", got)
	}
}

func TestQueryMaxDataPoints(t *testing.T) {
	if got, want := MaxDataPointsFor(40), int64(80); got != want {
		t.Errorf("MaxDataPointsFor(40) = %d, want %d (two samples per cell)", got, want)
	}
	if got := MaxDataPointsFor(0); got < 1 {
		t.Errorf("MaxDataPointsFor(0) = %d, want at least 1", got)
	}
}

// panel is a timeseries panel with a templated Prometheus query.
func panel() Panel {
	return Panel{
		ID:         4,
		Title:      "node load",
		Type:       "timeseries",
		Datasource: Datasource{UID: "prom-eu", Type: "prometheus"},
		Targets: []Target{
			{RefID: "A", Expr: `node_load1{instance=~"$host"}`},
			{RefID: "B", Expr: `rate(node_cpu_seconds_total{instance=~"$host"}[$__rate_interval])`},
			{RefID: "C", Expr: `hidden_one{}`, Hidden: true},
			{RefID: "D", Expr: "   "},
		},
	}
}

func TestQueryBuildPanelRequest(t *testing.T) {
	from, to := window(t)
	iv := NewInterpolation(Vars{"host": {"web-01.example.internal"}}, from, to, 160)

	pq, err := BuildPanelQuery(panel(), iv, from, to, 160)
	if err != nil {
		t.Fatalf("BuildPanelQuery: %v", err)
	}
	if got, want := pq.Request.From, "1763096400000"; got != want {
		t.Errorf("from = %q, want %q (epoch ms string)", got, want)
	}
	if got, want := pq.Request.To, "1763100000000"; got != want {
		t.Errorf("to = %q, want %q", got, want)
	}
	if len(pq.Request.Queries) != 2 {
		t.Fatalf("got %d queries, want the two visible ones", len(pq.Request.Queries))
	}
	q := pq.Request.Queries[0]
	if q.RefID != "A" || q.Datasource.UID != "prom-eu" || q.Datasource.Type != "prometheus" {
		t.Errorf("query A = %+v, want the panel's datasource", q)
	}
	if got, want := q.Expr, `node_load1{instance=~"web-01.example.internal"}`; got != want {
		t.Errorf("expr = %q, want %q", got, want)
	}
	if q.MaxDataPoints != 160 || q.IntervalMs < 1 {
		t.Errorf("maxDataPoints = %d, intervalMs = %d, want 160 and a positive interval", q.MaxDataPoints, q.IntervalMs)
	}
	if got, want := q.IntervalMs, int64(22500); got != want {
		t.Errorf("intervalMs = %d, want the window divided by the budget (%d)", got, want)
	}
	if got, want := pq.Request.Queries[1].Expr,
		`rate(node_cpu_seconds_total{instance=~"web-01.example.internal"}[90s])`; got != want {
		t.Errorf("query B expr = %q, want %q", got, want)
	}
	if len(pq.Expressions) != 2 || !strings.HasPrefix(pq.Expressions[1], "B: ") {
		t.Errorf("expressions = %v", pq.Expressions)
	}

	// The body is exactly the shape task 120 requires.
	body, err := json.Marshal(pq.Request)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"queries"`, `"refId":"A"`, `"datasource":{"type":"prometheus","uid":"prom-eu"}`,
		`"intervalMs":22500`, `"maxDataPoints":160`, `"from":"1763096400000"`, `"to":"1763100000000"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("request body lacks %s: %s", want, body)
		}
	}
}

func TestQueryBuildPanelTargetDatasourceAndRefIDs(t *testing.T) {
	from, to := window(t)
	p := Panel{Datasource: Datasource{UID: "panel-ds"}, Targets: []Target{
		{Expr: "a", Datasource: Datasource{UID: "mixed-ds"}},
		{RefID: "Q", Expr: "b"},
	}}
	pq, err := BuildPanelQuery(p, NewInterpolation(nil, from, to, 50), from, to, 50)
	if err != nil {
		t.Fatalf("BuildPanelQuery: %v", err)
	}
	if got := pq.Request.Queries[0].RefID; got != "A" {
		t.Errorf("a target without a refId gets %q, want A", got)
	}
	if got := pq.Request.Queries[0].Datasource.UID; got != "mixed-ds" {
		t.Errorf("target datasource = %q, want the target's own", got)
	}
	if got := pq.Request.Queries[1].Datasource.UID; got != "panel-ds" {
		t.Errorf("inherited datasource = %q, want the panel's", got)
	}
}

func TestQueryBuildPanelNoTargets(t *testing.T) {
	from, to := window(t)
	_, err := BuildPanelQuery(Panel{Type: "timeseries"}, Interpolation{}, from, to, 100)
	if err == nil {
		t.Fatal("a panel without queries must be an error")
	}
	if got := errs.KindOf(err); got != errs.NotAvailable {
		t.Errorf("kind = %q, want %q", got, errs.NotAvailable)
	}
}

// dsServer answers /api/ds/query with a fixed body and records what it got.
type dsServer struct {
	*httptest.Server

	mu     sync.Mutex
	body   []byte
	hits   int
	status int
	answer string
}

func newDSServer(t *testing.T, answer string) *dsServer {
	t.Helper()
	s := &dsServer{answer: answer, status: http.StatusOK}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.mu.Lock()
		s.body, s.hits = payload, s.hits+1
		s.mu.Unlock()
		if r.URL.Path != pathDSQuery {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(s.answer))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *dsServer) lastBody() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body
}

// clientFor builds a client over the test server.
func clientFor(t *testing.T, s *dsServer) *Client {
	t.Helper()
	prof := &httpx.Profile{Name: "grafana-test"}
	hc, err := prof.Client(httpx.Deps{})
	if err != nil {
		t.Fatalf("httpx client: %v", err)
	}
	c, err := NewClient(ClientOptions{URL: s.URL, Client: hc, Name: "eu"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

const dsAnswer = `{"results":{"A":{"status":200,"frames":[
 {"schema":{"refId":"A","fields":[{"name":"Time","type":"time"},
  {"name":"Value","type":"number","labels":{"instance":"web-01.example.internal"}}]},
  "data":{"values":[[1763096400000,1763098200000,1763100000000],[1,2,3]]}}]}}}`

func TestQueryPanelDecodesFrames(t *testing.T) {
	s := newDSServer(t, dsAnswer)
	c := clientFor(t, s)
	from, to := window(t)

	series, err := c.QueryPanel(context.Background(), panel(), NewInterpolation(nil, from, to, 160), from, to, 160)
	if err != nil {
		t.Fatalf("QueryPanel: %v", err)
	}
	if len(series) != 1 || len(series[0].Points) != 3 {
		t.Fatalf("got %d series with %d points, want one with three", len(series), len(series[0].Points))
	}
	if v, ok := series[0].Last(); !ok || v != 3 {
		t.Errorf("last = %v, %v, want 3", v, ok)
	}

	var sent struct {
		Queries []DSQuery `json:"queries"`
		From    string    `json:"from"`
		To      string    `json:"to"`
	}
	if err := json.Unmarshal(s.lastBody(), &sent); err != nil {
		t.Fatalf("the request body did not parse: %v (%s)", err, s.lastBody())
	}
	if len(sent.Queries) != 2 {
		t.Errorf("sent %d queries, want 2", len(sent.Queries))
	}
	if sent.From == "" || sent.To == "" {
		t.Errorf("the window was not sent: %+v", sent)
	}
	if sent.Queries[0].MaxDataPoints != 160 {
		t.Errorf("maxDataPoints = %d, want 160", sent.Queries[0].MaxDataPoints)
	}
}

func TestQueryPanelError(t *testing.T) {
	s := newDSServer(t, `{"results":{"A":{"status":500,"error":"bad query"}}}`)
	c := clientFor(t, s)
	from, to := window(t)

	_, err := c.QueryPanel(context.Background(), panel(), NewInterpolation(nil, from, to, 160), from, to, 160)
	if err == nil {
		t.Fatal("an error result must be an error")
	}
	if got := errs.KindOf(err); got != errs.Remote {
		t.Errorf("kind = %q, want %q", got, errs.Remote)
	}
	if !strings.Contains(err.Error(), "bad query") {
		t.Errorf("error %q does not carry Grafana's message", err)
	}
}

func TestQueryPanelHTTPStatus(t *testing.T) {
	s := newDSServer(t, `{"message":"forbidden"}`)
	s.status = http.StatusForbidden
	c := clientFor(t, s)
	from, to := window(t)

	_, err := c.QueryPanel(context.Background(), panel(), NewInterpolation(nil, from, to, 160), from, to, 160)
	if got := errs.KindOf(err); got != errs.Auth {
		t.Errorf("kind = %q, want %q for a 403", got, errs.Auth)
	}
}

func TestQueryPanelContextCanceled(t *testing.T) {
	s := newDSServer(t, dsAnswer)
	c := clientFor(t, s)
	from, to := window(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.QueryPanel(ctx, panel(), NewInterpolation(nil, from, to, 160), from, to, 160); err == nil {
		t.Fatal("a canceled context must not return series")
	}
}
