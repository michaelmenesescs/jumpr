package grafana

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"jumper/internal/backend"
	"jumper/internal/errs"
	"jumper/internal/resource"
)

// fixedNow is the clock every backend test uses.
func fixedNow() time.Time { return time.Date(2026, 11, 12, 22, 30, 0, 0, time.UTC) }

// fakeClip remembers what was copied to it.
type fakeClip struct{ copied []string }

func (c *fakeClip) Copy(text string) error {
	c.copied = append(c.copied, text)
	return nil
}

// fixtureRoutes is the fixture table the backend tests share.
func fixtureRoutes() map[string]string {
	return map[string]string{
		pathSearch:              "search.json",
		dashboardPath("a1b2c3"): "dashboard.json",
		pathAlerts:              "alerts.json",
		pathHealth:              "health.json",
		pathDSQuery:             "ds_query.json",
	}
}

// newTestBackend builds a backend over one fixture server named "eu".
func newTestBackend(t *testing.T, fs *fixtureServer, o Options) *Backend {
	t.Helper()
	c := newTestClient(t, fs, "eu")
	o.Servers = []Server{{Name: "eu", Client: c}}
	o.Now = fixedNow
	return New(o)
}

// collect drains a List channel.
func collect(t *testing.T, ch <-chan backend.Result) []backend.Result {
	t.Helper()
	var out []backend.Result
	for res := range ch {
		out = append(out, res)
	}
	return out
}

func TestBackendListDashboards(t *testing.T) {
	fs := newFixtureServer(t, fixtureRoutes())
	b := newTestBackend(t, fs, Options{})

	ch, err := b.List(context.Background(), resource.Query{Kind: "dash", Args: map[string]string{ArgQuery: "node"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := collect(t, ch)
	if len(results) != 1 {
		t.Fatalf("got %d results, want one per server", len(results))
	}
	res := results[0]
	if res.Err != nil {
		t.Fatalf("result error: %v", res.Err)
	}
	if res.Done != 1 || res.Total != 1 {
		t.Errorf("done/total = %d/%d, want 1/1", res.Done, res.Total)
	}
	if len(res.Items) != 2 {
		t.Fatalf("got %d rows, want 2", len(res.Items))
	}
	row := res.Items[0]
	if row.Ref != (resource.Ref{Backend: "grafana", Kind: KindDashboard, Namespace: "eu", Name: "a1b2c3"}) {
		t.Errorf("ref = %+v", row.Ref)
	}
	if got, want := row.Fields["title"], "Node overview"; got != want {
		t.Errorf("title = %v, want %q", got, want)
	}
	if got, want := row.Fields["folder"], "Linux"; got != want {
		t.Errorf("folder = %v, want %q", got, want)
	}
	if got, want := row.Fields["tags"], "linux, node"; got != want {
		t.Errorf("tags = %v, want %q", got, want)
	}
	if got, want := row.Fields["url"], fs.URL+"/d/a1b2c3/node-overview"; got != want {
		t.Errorf("url = %v, want %q", got, want)
	}
	if got, want := row.Labels["uid"], "a1b2c3"; got != want {
		t.Errorf("uid label = %q, want %q", got, want)
	}
	if row.Status != resource.StatusOK || !row.FetchedAt.Equal(fixedNow()) {
		t.Errorf("status/fetchedAt = %v/%s", row.Status, row.FetchedAt)
	}
	if got, want := fs.params(pathSearch).Get("type"), "dash-db"; got != want {
		t.Errorf("search type = %q, want %q", got, want)
	}
}

func TestBackendListAlerts(t *testing.T) {
	fs := newFixtureServer(t, fixtureRoutes())
	b := newTestBackend(t, fs, Options{})

	ch, err := b.List(context.Background(), resource.Query{Kind: KindAlert})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := collect(t, ch)
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("results = %+v", results)
	}
	items := results[0].Items
	if len(items) != 2 {
		t.Fatalf("got %d alerts, want 2", len(items))
	}
	node := items[0]
	if got, want := node.Fields["source"], "grafana:eu"; got != want {
		t.Errorf("source = %v, want %q", got, want)
	}
	if got, want := node.Fields["alert"], "NodeDown"; got != want {
		t.Errorf("alert = %v, want %q", got, want)
	}
	if got, want := node.Fields["sev"], "critical"; got != want {
		t.Errorf("sev = %v, want %q", got, want)
	}
	if got, want := node.Fields["state"], "firing"; got != want {
		t.Errorf("state = %v, want %q", got, want)
	}
	if node.Status != resource.StatusCrit {
		t.Errorf("status = %v, want crit", node.Status)
	}
	if got, want := node.Labels["source"], "grafana:eu"; got != want {
		t.Errorf("source label = %q, want %q", got, want)
	}
	if got := items[1].Status; got != resource.StatusWarn {
		t.Errorf("pending status = %v, want warn", got)
	}
	if got, want := node.Ref.Name, LabelString(map[string]string{
		"alertname": "NodeDown", "severity": "critical", "instance": "web-01.example.internal",
		"grafana_folder": "Linux",
	}); got != want {
		t.Errorf("ref name = %q, want %q", got, want)
	}
}

func TestBackendGetDashboard(t *testing.T) {
	fs := newFixtureServer(t, fixtureRoutes())
	b := newTestBackend(t, fs, Options{})

	row, err := b.Get(context.Background(), resource.Ref{
		Backend: "grafana", Kind: KindDashboard, Namespace: "eu", Name: "a1b2c3",
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	d, ok := row.Raw.(*Dashboard)
	if !ok {
		t.Fatalf("Raw = %T, want *Dashboard", row.Raw)
	}
	if got, want := d.Title, "Node overview"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
	if got, want := row.Fields["panels"], 6; got != want {
		t.Errorf("panels field = %v, want %d", got, want)
	}
	if len(d.Variables) != 4 {
		t.Errorf("variables = %d, want 4", len(d.Variables))
	}
	if d.URL == "" {
		t.Error("dashboard URL is empty")
	}
	if _, ok := d.Variable("host"); !ok {
		t.Error("the fetched dashboard lost its host variable")
	}
}

func TestBackendGetAlert(t *testing.T) {
	fs := newFixtureServer(t, fixtureRoutes())
	b := newTestBackend(t, fs, Options{})
	name := LabelString(map[string]string{
		"alertname": "HighLoad", "severity": "warning", "instance": "web-02.example.internal",
	})
	row, err := b.Get(context.Background(), resource.Ref{
		Backend: "grafana", Kind: KindAlert, Namespace: "eu", Name: name,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got, want := row.Fields["alert"], "HighLoad"; got != want {
		t.Errorf("alert = %v, want %q", got, want)
	}

	_, err = b.Get(context.Background(), resource.Ref{Kind: KindAlert, Namespace: "eu", Name: "missing"})
	if err == nil || errs.KindOf(err) != errs.NotAvailable {
		t.Errorf("missing alert error = %v, want NotAvailable", err)
	}
}

func TestBackendListErrors(t *testing.T) {
	fs := newFixtureServer(t, fixtureRoutes())
	ctx := context.Background()

	empty := New(Options{})
	if _, err := empty.List(ctx, resource.Query{Kind: KindDashboard}); err == nil {
		t.Error("an unconfigured backend listed")
	} else if errs.KindOf(err) != errs.Config {
		t.Errorf("unconfigured kind = %q, want config", errs.KindOf(err))
	}

	b := newTestBackend(t, fs, Options{})
	if _, err := b.List(ctx, resource.Query{Kind: "nope"}); err == nil || errs.KindOf(err) != errs.Config {
		t.Errorf("unknown kind error = %v, want config", err)
	}
	if _, err := b.List(ctx, resource.Query{Kind: KindDashboard, Args: map[string]string{ArgServer: "us"}}); err == nil || errs.KindOf(err) != errs.Config {
		t.Errorf("unknown server error = %v, want config", err)
	}

	// A server without a client fails its own batch rather than the list.
	b = New(Options{Servers: []Server{{Name: "eu"}}, Now: fixedNow})
	ch, err := b.List(ctx, resource.Query{Kind: KindDashboard})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := collect(t, ch)
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("results = %+v, want one failed batch", results)
	}
	if got := errs.KindOf(results[0].Err); got != errs.Config {
		t.Errorf("no-client kind = %q, want config", got)
	}
}

func TestBackendKinds(t *testing.T) {
	b := New(Options{})
	specs := b.Kinds()
	if got, want := len(specs), 2; got != want {
		t.Fatalf("got %d kinds, want %d", got, want)
	}
	if specs[0].Name != KindDashboard || specs[1].Name != KindAlert {
		t.Errorf("kinds = %q, %q", specs[0].Name, specs[1].Name)
	}
	if !containsString(specs[0].Aliases, "dash") {
		t.Errorf("dashboard aliases = %v, want dash", specs[0].Aliases)
	}
	if specs[0].Scope != resource.ScopeGlobal || specs[1].Scope != resource.ScopeGlobal {
		t.Error("grafana kinds must be global")
	}

	// The registry accepts the backend and resolves the alias.
	reg := backend.NewRegistry()
	if err := reg.Register(b); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, spec, ok := reg.Lookup("dash")
	if !ok || got.Name() != "grafana" || spec.Name != KindDashboard {
		t.Errorf("Lookup(dash) = %v, %q, %v", got, spec.Name, ok)
	}
}

func TestBackendHealth(t *testing.T) {
	healthy := newFixtureServer(t, fixtureRoutes())
	down := newFixtureServer(t, map[string]string{pathHealth: "health_down.json"})
	b := New(Options{
		Servers: []Server{
			{Name: "eu", Client: newTestClient(t, healthy, "eu")},
			{Name: "us", Client: newTestClient(t, down, "us")},
		},
		Now: fixedNow,
	})
	h := b.Health(context.Background())
	if h.State != backend.HealthDegraded {
		t.Fatalf("health = %+v, want degraded", h)
	}
	if got, want := h.Detail, "us: database unreachable"; got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}
	statuses := b.Status(context.Background())
	if len(statuses) != 2 || statuses[0].Name != "eu" || statuses[0].Health.State != backend.HealthOK {
		t.Errorf("statuses = %+v", statuses)
	}
	// The second probe is answered from the cache.
	if got := healthy.hitCount(pathHealth); got != 1 {
		t.Errorf("/api/health hits = %d, want 1", got)
	}

	unconfigured := New(Options{})
	if got := unconfigured.Health(context.Background()); got.State != backend.HealthUnconfigured {
		t.Errorf("unconfigured health = %+v", got)
	}
}

func TestBackendOpenResource(t *testing.T) {
	fs := newFixtureServer(t, fixtureRoutes())
	resourceRow := resource.Resource{Fields: map[string]any{"url": "https://grafana.example.internal/d/a1b2c3/x"}}

	clip := &fakeClip{}
	b := newTestBackend(t, fs, Options{Clipboard: clip})
	if err := b.OpenResource(resourceRow); err != nil {
		t.Fatalf("OpenResource: %v", err)
	}
	if want := []string{"https://grafana.example.internal/d/a1b2c3/x"}; !reflect.DeepEqual(clip.copied, want) {
		t.Errorf("copied = %v, want %v", clip.copied, want)
	}

	var opened []string
	b = newTestBackend(t, fs, Options{Open: func(url string) error {
		opened = append(opened, url)
		return nil
	}})
	if err := b.OpenResource(resourceRow); err != nil {
		t.Fatalf("OpenResource with a hook: %v", err)
	}
	if len(opened) != 1 {
		t.Errorf("opened = %v, want one URL", opened)
	}

	b = newTestBackend(t, fs, Options{Open: func(string) error { return errors.New("no browser") }})
	if err := b.OpenResource(resourceRow); err == nil || errs.KindOf(err) != errs.Internal {
		t.Errorf("hook failure = %v, want Internal", err)
	}

	b = newTestBackend(t, fs, Options{})
	if err := b.OpenURL("https://grafana.example.internal/d/x"); err == nil || errs.KindOf(err) != errs.Config {
		t.Errorf("no opener = %v, want Config", err)
	}
	if err := b.OpenResource(resource.Resource{}); err == nil || errs.KindOf(err) != errs.Config {
		t.Errorf("row without a URL = %v, want Config", err)
	}
}

func TestBackendResolveVariable(t *testing.T) {
	fs := newFixtureServer(t, fixtureRoutes())
	b := newTestBackend(t, fs, Options{})
	d := &Dashboard{
		Variables: []Variable{
			{Name: "host", Query: VariableQuery{Expr: "label_values(node_load1, instance)"},
				Datasource: Datasource{UID: "prom-1", Type: "prometheus"}},
			{Name: "env", Type: "custom", Options: []VariableOption{
				{Text: "prod", Value: "prod"}, {Text: "staging", Value: "staging"},
			}},
			{Name: "free", Query: VariableQuery{Expr: "query_result(up)"}},
		},
	}

	values, err := b.ResolveVariable(context.Background(), "eu", d, "host")
	if err != nil {
		t.Fatalf("ResolveVariable: %v", err)
	}
	if len(values) != 3 || values[2] != "db-01.example.internal" {
		t.Errorf("values = %v, want three instances", values)
	}

	values, err = b.ResolveVariable(context.Background(), "eu", d, "env")
	if err != nil {
		t.Fatalf("ResolveVariable(env): %v", err)
	}
	if want := []string{"prod", "staging"}; !reflect.DeepEqual(values, want) {
		t.Errorf("env values = %v, want %v", values, want)
	}

	if values, err = b.ResolveVariable(context.Background(), "eu", d, "free"); err != nil || values != nil {
		t.Errorf("free text = %v, %v; want no options and no error", values, err)
	}

	if _, err := b.ResolveVariable(context.Background(), "eu", d, "missing"); err == nil || errs.KindOf(err) != errs.Config {
		t.Errorf("missing variable = %v, want Config", err)
	}
}
