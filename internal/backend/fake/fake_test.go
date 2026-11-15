package fake

import (
	"context"
	"reflect"
	"testing"
	"time"

	"jumpr/internal/backend"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
)

// fixedNow keeps the world deterministic in tests.
func fixedNow() time.Time {
	return time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
}

// drain collects every result of a list until the channel closes.
func drain(t *testing.T, ch <-chan backend.Result) []backend.Result {
	t.Helper()
	var out []backend.Result
	for r := range ch {
		out = append(out, r)
	}
	return out
}

// items flattens the resources of a stream.
func items(results []backend.Result) []resource.Resource {
	var out []resource.Resource
	for _, r := range results {
		out = append(out, r.Items...)
	}
	return out
}

func list(t *testing.T, b *Backend, q resource.Query) []backend.Result {
	t.Helper()
	ch, err := b.List(context.Background(), q)
	if err != nil {
		t.Fatalf("List(%s): %v", q.Kind, err)
	}
	return drain(t, ch)
}

func mustSelector(t *testing.T, text string) resource.Selector {
	t.Helper()
	sel, err := resource.ParseSelector(text)
	if err != nil {
		t.Fatalf("ParseSelector(%q): %v", text, err)
	}
	return sel
}

func TestNameAndHealth(t *testing.T) {
	b := New(Options{Now: fixedNow})
	if got := b.Name(); got != "fake" {
		t.Errorf("Name() = %q, want fake", got)
	}
	h := b.Health(context.Background())
	if h.State != backend.HealthOK || h.Detail != "fake data" {
		t.Errorf("Health() = %+v, want ok/fake data", h)
	}
	if !h.Checked.Equal(fixedNow()) {
		t.Errorf("Health().Checked = %v, want %v", h.Checked, fixedNow())
	}
}

func TestKinds(t *testing.T) {
	b := New(Options{})
	kinds := b.Kinds()
	if len(kinds) != 3 {
		t.Fatalf("Kinds() = %d kinds, want 3", len(kinds))
	}
	want := map[resource.KindName][]string{
		"host":  {"hosts", "h"},
		"unit":  {"units", "u"},
		"alert": {"alerts", "al"},
	}
	for _, spec := range kinds {
		if !reflect.DeepEqual(spec.Aliases, want[spec.Name]) {
			t.Errorf("%s aliases = %v, want %v", spec.Name, spec.Aliases, want[spec.Name])
		}
	}
	host, ok := b.Kinds()[0].Column("mem")
	if !ok || host.Kind != resource.ColPercent {
		t.Errorf("host mem column = %+v, want percent", host)
	}
}

func TestFleetSize(t *testing.T) {
	b := New(Options{Now: fixedNow})
	results := list(t, b, resource.Query{Kind: "host"})
	rows := items(results)
	if len(rows) != 1512 {
		t.Fatalf("fleet size = %d, want 1512", len(rows))
	}
	perDC := map[string]int{}
	for _, row := range rows {
		perDC[row.Labels["dc"]]++
		if row.Labels["role"] != "web" || row.Labels["env"] != "prod" {
			t.Fatalf("%s labels = %v", row.Labels["host"], row.Labels)
		}
	}
	for _, dc := range datacenters {
		if perDC[dc] != 504 {
			t.Errorf("dc %s has %d hosts, want 504", dc, perDC[dc])
		}
	}
	last := results[len(results)-1]
	if last.Done != 1512 || last.Total != 1512 {
		t.Errorf("final Done/Total = %d/%d, want 1512/1512", last.Done, last.Total)
	}
}

func TestBatchSize(t *testing.T) {
	b := New(Options{Now: fixedNow})
	for _, r := range list(t, b, resource.Query{Kind: "host", Selector: mustSelector(t, "dc=ord")}) {
		if len(r.Items) > batchSize {
			t.Fatalf("batch has %d items, want <= %d", len(r.Items), batchSize)
		}
	}
}

func TestSelectorFilters(t *testing.T) {
	b := New(Options{Now: fixedNow})
	if got := len(items(list(t, b, resource.Query{Kind: "host", Selector: mustSelector(t, "dc=ewr")}))); got != 504 {
		t.Errorf("dc=ewr rows = %d, want 504", got)
	}
	if got := len(items(list(t, b, resource.Query{Kind: "host", Selector: mustSelector(t, "dc!=ewr role=web")}))); got != 1008 {
		t.Errorf("dc!=ewr rows = %d, want 1008", got)
	}
}

func TestDeterministic(t *testing.T) {
	b := New(Options{Now: fixedNow})
	q := resource.Query{Kind: "host", Selector: mustSelector(t, "dc=ewr")}
	first := items(list(t, b, q))
	second := items(list(t, b, q))
	if !reflect.DeepEqual(first, second) {
		t.Fatal("two identical Lists returned different rows")
	}
	if first[0].Ref.Name != "web-ewr-001" {
		t.Errorf("first row = %q, want web-ewr-001", first[0].Ref.Name)
	}
	if got := first[0].Fields["load"]; got != hostRow(fixedNow(), "web-ewr-001").Fields["load"] {
		t.Errorf("load is not a pure function of the name: %v", got)
	}
}

func TestFailedUnits(t *testing.T) {
	b := New(Options{Now: fixedNow})
	results := list(t, b, resource.Query{Kind: "unit", Hosts: []resource.HostID{"web-ewr-007"}})
	if len(results) != 1 {
		t.Fatalf("unit results = %d, want 1", len(results))
	}
	if results[0].Host != "web-ewr-007" || results[0].Done != 1 || results[0].Total != 1 {
		t.Errorf("result = %+v, want host web-ewr-007 1/1", results[0])
	}
	rows := results[0].Items
	if len(rows) == 0 {
		t.Fatal("no units for web-ewr-007")
	}
	byName := map[string]resource.Resource{}
	for _, row := range rows {
		byName[row.Ref.Name] = row
	}
	nginx, ok := byName["nginx.service"]
	if !ok {
		t.Fatal("nginx.service missing")
	}
	if nginx.Fields["active"] != "failed" || nginx.Fields["sub"] != "failed" || nginx.Status != resource.StatusCrit {
		t.Errorf("nginx = %+v, want failed/crit", nginx.Fields)
	}
	if haproxy := byName["haproxy.service"]; haproxy.Fields["active"] != "active" || haproxy.Fields["sub"] != "running" {
		t.Errorf("haproxy = %+v, want active/running", haproxy.Fields)
	}

	healthy := items(list(t, b, resource.Query{Kind: "unit", Hosts: []resource.HostID{"web-ewr-001"}}))
	for _, row := range healthy {
		if row.Fields["active"] != "active" {
			t.Errorf("web-ewr-001 %s active = %v, want active", row.Ref.Name, row.Fields["active"])
		}
	}
}

func TestUnitRequiresHost(t *testing.T) {
	b := New(Options{Now: fixedNow})
	if _, err := b.List(context.Background(), resource.Query{Kind: "unit"}); err == nil {
		t.Fatal("unit list without hosts returned no error")
	}
	if _, err := b.List(context.Background(), resource.Query{Kind: "unit", Args: map[string]string{"host": "web-ewr-007"}}); err != nil {
		t.Fatalf("unit list with Args[host] failed: %v", err)
	}
}

func TestAlerts(t *testing.T) {
	b := New(Options{Now: fixedNow})
	rows := items(list(t, b, resource.Query{Kind: "alert"}))
	if len(rows) != 6 {
		t.Fatalf("alerts = %d, want 6", len(rows))
	}
	names := map[string]bool{}
	for _, row := range rows {
		names[row.Fields["alert"].(string)] = true
		if row.Fields["source"] != "alertmanager" {
			t.Errorf("source = %v", row.Fields["source"])
		}
	}
	if !names["HighErrorRate"] {
		t.Error("HighErrorRate missing")
	}
	if got := len(items(list(t, b, resource.Query{Kind: "al", Selector: mustSelector(t, "dc=ewr")}))); got != 2 {
		t.Errorf("dc=ewr alerts = %d, want 2", got)
	}
}

func TestFailHostsYieldErr(t *testing.T) {
	b := New(Options{
		Now:       fixedNow,
		FailHosts: map[resource.HostID]errs.Kind{"web-ewr-007": errs.Timeout},
	})
	results := list(t, b, resource.Query{Kind: "host", Hosts: []resource.HostID{"web-ewr-007", "web-ewr-008"}})
	var errsSeen, rowsSeen int
	for _, r := range results {
		if r.Err != nil {
			errsSeen++
			if !errs.Is(r.Err, errs.Timeout) {
				t.Errorf("error kind = %q, want timeout", errs.KindOf(r.Err))
			}
			if r.Host != "web-ewr-007" {
				t.Errorf("failed host = %q, want web-ewr-007", r.Host)
			}
			if len(r.Items) != 0 {
				t.Errorf("failed host returned %d items", len(r.Items))
			}
		}
		rowsSeen += len(r.Items)
	}
	if errsSeen != 1 || rowsSeen != 1 {
		t.Fatalf("errors/rows = %d/%d, want 1/1", errsSeen, rowsSeen)
	}
}

func TestFailHostUnitKind(t *testing.T) {
	b := New(Options{
		Now:       fixedNow,
		FailHosts: map[resource.HostID]errs.Kind{"web-ewr-007": errs.Unreachable},
	})
	results := list(t, b, resource.Query{Kind: "unit", Hosts: []resource.HostID{"web-ewr-007"}})
	if len(results) != 1 || results[0].Err == nil || !errs.Is(results[0].Err, errs.Unreachable) {
		t.Fatalf("unit result = %+v, want one unreachable error", results)
	}
}

func TestListStopsOnCancel(t *testing.T) {
	b := New(Options{Now: fixedNow, Latency: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := b.List(ctx, resource.Query{Kind: "host"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Read one batch, then cancel while the producer is sleeping.
	if _, ok := <-ch; !ok {
		t.Fatal("channel closed before the first batch")
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
	case <-time.After(2 * time.Second):
		t.Fatal("producer did not stop after cancel")
	}
}

func TestGet(t *testing.T) {
	b := New(Options{Now: fixedNow})
	host, err := b.Get(context.Background(), resource.Ref{Backend: "fake", Kind: "host", Name: "web-ewr-007"})
	if err != nil {
		t.Fatalf("Get host: %v", err)
	}
	if host.Fields["failed"] != int64(1) || host.Status != resource.StatusCrit {
		t.Errorf("host = %+v, want failed/crit", host.Fields)
	}

	unit, err := b.Get(context.Background(), resource.Ref{Backend: "fake", Kind: "unit", Host: "web-ewr-007", Name: "nginx.service"})
	if err != nil {
		t.Fatalf("Get unit: %v", err)
	}
	if unit.Fields["active"] != "failed" {
		t.Errorf("unit active = %v, want failed", unit.Fields["active"])
	}

	alert, err := b.Get(context.Background(), resource.Ref{Backend: "fake", Kind: "alert", Name: "HighErrorRate"})
	if err != nil {
		t.Fatalf("Get alert: %v", err)
	}
	if alert.Fields["sev"] != "critical" {
		t.Errorf("alert sev = %v", alert.Fields["sev"])
	}

	if _, err := b.Get(context.Background(), resource.Ref{Kind: "host", Name: "web-ewr-999"}); !errs.Is(err, errs.NotAvailable) {
		t.Errorf("unknown host err = %v, want not-available", err)
	}
	if _, err := b.Get(context.Background(), resource.Ref{Kind: "unit", Host: "web-ewr-001", Name: "nope.service"}); !errs.Is(err, errs.NotAvailable) {
		t.Errorf("unknown unit err = %v, want not-available", err)
	}
}

func TestLinks(t *testing.T) {
	b := New(Options{Now: fixedNow})
	row := hostRow(fixedNow(), "web-ewr-012")
	links := b.Links(row)
	if len(links) != 1 {
		t.Fatalf("Links() = %d, want 1", len(links))
	}
	if links[0].Key != "u" || links[0].To.Kind != "unit" || len(links[0].To.Hosts) != 1 || links[0].To.Hosts[0] != "web-ewr-012" {
		t.Errorf("link = %+v, want unit pivot with key u", links[0])
	}
	if got := b.Links(resource.Resource{Ref: resource.Ref{Kind: "alert", Name: "x"}}); got != nil {
		t.Errorf("Links(alert) = %v, want nil", got)
	}
}

func TestUnknownKind(t *testing.T) {
	b := New(Options{Now: fixedNow})
	if _, err := b.List(context.Background(), resource.Query{Kind: "pod"}); !errs.Is(err, errs.Config) {
		t.Fatalf("unknown kind err = %v, want config", err)
	}
}
