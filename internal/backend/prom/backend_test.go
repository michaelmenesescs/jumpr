package prom

import (
	"context"
	"strings"
	"testing"
	"time"

	"jumpr/internal/backend"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
)

// collect drains a List stream into one slice of results.
func collect(t *testing.T, ch <-chan backend.Result) []backend.Result {
	t.Helper()
	var out []backend.Result
	for r := range ch {
		out = append(out, r)
	}
	return out
}

func TestBackendKinds(t *testing.T) {
	b := New(Options{})
	names := map[resource.KindName]bool{}
	var aliases []string
	for _, spec := range b.Kinds() {
		names[spec.Name] = true
		aliases = append(aliases, spec.Aliases...)
		if spec.Scope != resource.ScopeGlobal {
			t.Errorf("%s scope = %v, want global", spec.Name, spec.Scope)
		}
		if len(spec.Columns) == 0 {
			t.Errorf("%s has no columns", spec.Name)
		}
	}
	for _, want := range []resource.KindName{KindTarget, KindRule, KindSeries} {
		if !names[want] {
			t.Errorf("kind %q missing from %v", want, names)
		}
	}
	for _, alias := range []string{"targets", "up", "rules", "promql"} {
		if !contains(aliases, alias) {
			t.Errorf("alias %q missing from %v", alias, aliases)
		}
	}
	if got := b.Name(); got != "prom" {
		t.Errorf("Name = %q, want prom", got)
	}
}

func TestBackendListTargets(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{pathTargets: "targets.json"})
	b := New(Options{Servers: []Server{{Name: "eu", Client: newTestClient(t, fs, "eu")}}})

	ch, err := b.List(context.Background(), resource.Query{Kind: KindTarget})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	res := collect(t, ch)
	if len(res) != 1 {
		t.Fatalf("got %d results, want 1", len(res))
	}
	if res[0].Err != nil {
		t.Fatalf("result error: %v", res[0].Err)
	}
	if res[0].Done != 1 || res[0].Total != 1 {
		t.Errorf("done/total = %d/%d, want 1/1", res[0].Done, res[0].Total)
	}
	if len(res[0].Items) != 2 {
		t.Fatalf("got %d rows, want 2", len(res[0].Items))
	}
	row := res[0].Items[0]
	if got, want := row.Ref.Namespace, "eu"; got != want {
		t.Errorf("namespace = %q, want %q", got, want)
	}
	if got, want := row.Ref.Name, "node/web-01.example.internal:9100"; got != want {
		t.Errorf("ref name = %q, want %q", got, want)
	}
	if got, want := row.Str("job"), "node"; got != want {
		t.Errorf("job = %q, want %q", got, want)
	}
	if row.Status != resource.StatusOK {
		t.Errorf("status = %v, want OK", row.Status)
	}
	if !res[0].Items[1].Fields["scrape"].(time.Time).Equal(time.Date(2026, 11, 12, 14, 0, 1, 0, time.UTC)) {
		t.Errorf("scrape time = %v", row.Fields["scrape"])
	}
	if got, want := res[0].Items[1].Str("error"), "dial tcp 192.0.2.21:9100: connect: connection refused"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}

func TestBackendListRules(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{pathRules: "rules.json"})
	b := New(Options{Servers: []Server{{Name: "eu", Client: newTestClient(t, fs, "eu")}}})

	res := collect(t, mustList(t, b, resource.Query{Kind: KindRule}))
	if len(res) != 1 || len(res[0].Items) != 2 {
		t.Fatalf("got %d results with %d rows, want 1 with 2", len(res), len(res[0].Items))
	}
	row := res[0].Items[0]
	if got, want := row.Str("group"), "node-rules"; got != want {
		t.Errorf("group = %q, want %q", got, want)
	}
	if got, want := row.Str("state"), "firing"; got != want {
		t.Errorf("state = %q, want %q", got, want)
	}
	if row.Status != resource.StatusOK {
		t.Errorf("status = %v, want OK (health is ok)", row.Status)
	}
}

func TestBackendSeriesRows(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{
		pathQuery:      "vector.json",
		pathQueryRange: "matrix.json",
	})
	now := time.Unix(1763100000, 0)
	b := New(Options{
		Servers: []Server{{Name: "eu", Client: newTestClient(t, fs, "eu")}},
		Now:     func() time.Time { return now },
	})
	q := resource.Query{
		Kind:   KindSeries,
		Args:   map[string]string{ArgExpr: `up{job="node"}`},
		Window: resource.TimeWindow{From: now.Add(-time.Hour), To: now},
	}

	res := collect(t, mustList(t, b, q))
	if len(res) != 1 || res[0].Err != nil {
		t.Fatalf("results = %+v", res)
	}
	rows := res[0].Items
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	if got, want := rows[0].Str("labels"), `{__name__="up", instance="web-01.example.internal:9100", job="node"}`; got != want {
		t.Errorf("labels = %q, want %q", got, want)
	}
	if v, ok := rows[0].Float("value"); !ok || v != 1 {
		t.Errorf("value = %v (%v), want 1", v, ok)
	}
	spark, ok := rows[0].Fields["spark"].([]float64)
	if !ok || len(spark) != 6 {
		t.Fatalf("sparkline = %v, want six points from the range query", rows[0].Fields["spark"])
	}
	if math := fs.params(pathQueryRange); math.Get("query") != `up{job="node"}` {
		t.Errorf("range query = %q", math.Get("query"))
	}
	if got := fs.params(pathQuery).Get("time"); got != "2025-11-14T06:00:00Z" {
		t.Errorf("instant query time = %q, want the window end", got)
	}
}

func TestBackendSeriesNeedsExpression(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{})
	b := New(Options{Servers: []Server{{Name: "eu", Client: newTestClient(t, fs, "eu")}}})
	_, err := b.List(context.Background(), resource.Query{Kind: KindSeries})
	if err == nil || errs.KindOf(err) != errs.Config {
		t.Fatalf("List = %v, want a Config error", err)
	}
	if !strings.Contains(err.Error(), "expression") {
		t.Errorf("error = %v, want it to name the missing expression", err)
	}
}

func TestBackendServerSelection(t *testing.T) {
	eu := newFixtureServer(t, map[string]string{pathTargets: "targets.json"})
	us := newFixtureServer(t, map[string]string{pathTargets: "targets.json"})
	b := New(Options{Servers: []Server{
		{Name: "eu", Client: newTestClient(t, eu, "eu")},
		{Name: "us", Client: newTestClient(t, us, "us")},
	}})

	res := collect(t, mustList(t, b, resource.Query{Kind: KindTarget, Args: map[string]string{ArgServer: "us"}}))
	if len(res) != 1 {
		t.Fatalf("got %d results, want 1", len(res))
	}
	if got, want := res[0].Items[0].Ref.Namespace, "us"; got != want {
		t.Errorf("namespace = %q, want %q", got, want)
	}
	if got := eu.hitCount(pathTargets); got != 0 {
		t.Errorf("the eu server was asked %d times, want 0", got)
	}

	if _, err := b.List(context.Background(), resource.Query{Kind: KindTarget, Args: map[string]string{ArgServer: "mars"}}); err == nil {
		t.Error("List accepted an unknown server")
	}
	if got := b.Servers(); len(got) != 2 || got[0] != "eu" || got[1] != "us" {
		t.Errorf("Servers = %v, want [eu us]", got)
	}
}

func TestBackendGetAndLinks(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{pathTargets: "targets.json"})
	b := New(Options{Servers: []Server{{Name: "eu", Client: newTestClient(t, fs, "eu")}}})

	row, err := b.Get(context.Background(), resource.Ref{
		Backend: "prom", Kind: KindTarget, Namespace: "eu",
		Name: "node/db-01.example.internal:9100",
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := row.Str("instance"); got != "db-01.example.internal:9100" {
		t.Errorf("instance = %q", got)
	}

	links := b.Links(row)
	if len(links) != 1 || links[0].Label != "host" {
		t.Fatalf("Links = %+v, want one host link", links)
	}
	if got, want := links[0].To.Hosts[0], resource.HostID("db-01.example.internal"); got != want {
		t.Errorf("host link = %q, want %q", got, want)
	}
	if _, err := b.Get(context.Background(), resource.Ref{Kind: KindTarget, Name: "node/nope"}); err == nil {
		t.Error("Get returned no error for a missing row")
	}
}

func TestBackendHealthStatus(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{})
	broken := newFixtureServer(t, map[string]string{})
	brokenURL := broken.URL
	broken.Close()

	brokenClient, err := NewClient(ClientOptions{URL: brokenURL, Client: nil, Name: "us"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	b := New(Options{
		Servers: []Server{
			{Name: "eu", Client: newTestClient(t, fs, "eu")},
			{Name: "us", Client: brokenClient},
		},
		Now: time.Now,
	})

	status := b.Status(context.Background())
	if len(status) != 2 {
		t.Fatalf("got %d statuses, want 2", len(status))
	}
	if status[0].Name != "eu" || status[0].Health.State != backend.HealthOK {
		t.Errorf("eu status = %+v, want healthy", status[0])
	}
	if status[1].Name != "us" || status[1].Health.State != backend.HealthDown {
		t.Errorf("us status = %+v, want down", status[1])
	}
	if got := b.Health(context.Background()); got.State != backend.HealthDegraded {
		t.Errorf("aggregate = %+v, want degraded", got)
	}

	// The health answer is cached for healthTTL: a second call inside the TTL
	// must not hit the network again.
	first := fs.hitCount(pathHealthy)
	if first == 0 {
		t.Fatal("the health probe never reached the server")
	}
	b.Health(context.Background())
	if got := fs.hitCount(pathHealthy); got != first {
		t.Errorf("health probes = %d after a second Health, want the cached %d", got, first)
	}
}

func TestBackendHealthUnconfigured(t *testing.T) {
	b := New(Options{})
	if got := b.Health(context.Background()); got.State != backend.HealthUnconfigured {
		t.Errorf("Health = %+v, want unconfigured", got)
	}
	if _, err := b.List(context.Background(), resource.Query{Kind: KindTarget}); err == nil {
		t.Error("List on an unconfigured backend returned no error")
	}
	if _, err := b.List(context.Background(), resource.Query{Kind: "nope"}); err == nil {
		t.Error("List on an unknown kind returned no error")
	}
}

func TestInstanceHost(t *testing.T) {
	cases := map[string]resource.HostID{
		"web-01.example.internal:9100":   "web-01.example.internal",
		"WEB-02.Example.Internal":        "web-02.example.internal",
		"user@db-01.example.internal:22": "db-01.example.internal",
		"[2001:db8::1]:9100":             "2001:db8::1",
		"":                               "",
	}
	for in, want := range cases {
		if got := instanceHost(in); got != want {
			t.Errorf("instanceHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// mustList starts a List and fails the test when it cannot start.
func mustList(t *testing.T, b *Backend, q resource.Query) <-chan backend.Result {
	t.Helper()
	ch, err := b.List(context.Background(), q)
	if err != nil {
		t.Fatalf("List(%s): %v", q.Kind, err)
	}
	return ch
}

// contains reports whether list holds want.
func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
