package prom

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"jumpr/internal/inventory"
	"jumpr/internal/resource"
)

// testInventory is a fixed inventory with optional identity rewrites.
type testInventory struct {
	hosts    []inventory.Host
	rewrites []inventory.Rewrite
}

func (t *testInventory) Hosts() []inventory.Host { return t.hosts }

func (t *testInventory) Select(sel resource.Selector) []inventory.Host {
	out := make([]inventory.Host, 0, len(t.hosts))
	for _, h := range t.hosts {
		if sel.Matches(h.Labels) {
			out = append(out, h)
		}
	}
	return out
}

func (t *testInventory) Resolve(name string) (inventory.Host, bool) {
	want := inventory.CanonicalHost(t, name)
	for _, h := range t.hosts {
		if h.ID == want {
			return h, true
		}
	}
	return inventory.Host{}, false
}

func (t *testInventory) Refresh(context.Context) map[string]error { return nil }

func (t *testInventory) Age() time.Duration { return 0 }

func (t *testInventory) Rewrites() []inventory.Rewrite { return t.rewrites }

// hostsFixture builds n hosts of dc with the labels the fake fleet carries.
func hostsFixture(dc string, n int, extra map[string]string) []inventory.Host {
	out := make([]inventory.Host, 0, n)
	for i := 1; i <= n; i++ {
		id := resource.HostID(fmt.Sprintf("web-%s-%03d", dc, i))
		labels := map[string]string{"host": string(id), "role": "web", "dc": dc}
		for k, v := range extra {
			labels[k] = v
		}
		out = append(out, inventory.Host{ID: id, Addr: string(id), Labels: labels})
	}
	return out
}

func hostIDs(hosts []inventory.Host) []resource.HostID {
	out := make([]resource.HostID, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, h.ID)
	}
	return out
}

func TestHostsFromLabels(t *testing.T) {
	stripPort := inventory.Rewrite{From: regexp.MustCompile(`^(.*):9100$`), To: "$1"}
	inv := &testInventory{
		hosts:    hostsFixture("ewr", 3, nil),
		rewrites: []inventory.Rewrite{stripPort},
	}
	empty := &testInventory{}

	cases := []struct {
		name   string
		labels map[string]string
		inv    inventory.Inventory
		keys   []string
		want   []resource.HostID
	}{
		{
			name:   "instance with a port",
			labels: map[string]string{"instance": "web-ewr-002:9100"},
			inv:    inv,
			want:   []resource.HostID{"web-ewr-002"},
		},
		{
			name:   "host label",
			labels: map[string]string{"instance": "192.0.2.9:9100", "host": "web-ewr-003"},
			inv:    inv,
			want:   []resource.HostID{"web-ewr-003"},
		},
		{
			name:   "key order wins",
			labels: map[string]string{"instance": "web-ewr-001", "host": "web-ewr-002"},
			inv:    inv,
			want:   []resource.HostID{"web-ewr-001"},
		},
		{
			name:   "configured key order",
			labels: map[string]string{"node": "web-ewr-001", "instance": "web-ewr-002"},
			inv:    inv,
			keys:   []string{"node", "instance"},
			want:   []resource.HostID{"web-ewr-001"},
		},
		{
			name:   "identity rewrite",
			labels: map[string]string{"instance": "WEB-EWR-001:9100"},
			inv:    inv,
			want:   []resource.HostID{"web-ewr-001"},
		},
		{
			name:   "user prefix is dropped",
			labels: map[string]string{"instance": "prom@web-ewr-002:9100"},
			inv:    inv,
			want:   []resource.HostID{"web-ewr-002"},
		},
		{
			name:   "selector from shared labels",
			labels: map[string]string{"dc": "ewr", "service": "api", "alertname": "HighErrorRate"},
			inv:    inv,
			want:   []resource.HostID{"web-ewr-001", "web-ewr-002", "web-ewr-003"},
		},
		{
			name:   "selector matches nothing",
			labels: map[string]string{"dc": "ord"},
			inv:    inv,
			want:   nil,
		},
		{
			name:   "no usable label",
			labels: map[string]string{"alertname": "HighErrorRate", "service": "api"},
			inv:    inv,
			want:   nil,
		},
		{
			name:   "no inventory",
			labels: map[string]string{"host": "web-ewr-001"},
			inv:    nil,
			want:   nil,
		},
		{
			name:   "empty inventory",
			labels: map[string]string{"host": "web-ewr-001"},
			inv:    empty,
			want:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HostsFromLabels(tc.labels, tc.inv, tc.keys)
			if len(got) != len(tc.want) {
				t.Fatalf("hosts = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("hosts = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestHostsFromLabelsCap(t *testing.T) {
	big := &testInventory{hosts: hostsFixture("ewr", MaxAlertTargets+1, nil)}
	labels := map[string]string{"dc": "ewr"}

	if got := HostsFromLabels(labels, big, nil); len(got) != 0 {
		t.Fatalf("hosts = %d, want 0 above the cap", len(got))
	}
	hosts, reason := ResolveAlertHosts(labels, big, nil)
	if len(hosts) != 0 {
		t.Fatalf("hosts = %v, want none", hosts)
	}
	if !strings.Contains(reason, "limit 500") {
		t.Fatalf("reason = %q, want the cap in it", reason)
	}

	// Exactly the cap is still fine.
	atCap := &testInventory{hosts: hostsFixture("ewr", MaxAlertTargets, nil)}
	if got := HostsFromLabels(labels, atCap, nil); len(got) != MaxAlertTargets {
		t.Fatalf("hosts = %d, want %d", len(got), MaxAlertTargets)
	}
}

func TestHostsFromLabelsReason(t *testing.T) {
	inv := &testInventory{hosts: hostsFixture("ewr", 2, nil)}
	if hosts, reason := ResolveAlertHosts(map[string]string{"host": "web-ewr-001"}, inv, nil); reason != "" || len(hosts) != 1 {
		t.Fatalf("resolved %v (%q), want one host and no reason", hosts, reason)
	}
	_, reason := ResolveAlertHosts(map[string]string{"dc": "ord"}, inv, nil)
	if !strings.Contains(reason, "no inventory host matches") {
		t.Fatalf("reason = %q", reason)
	}
	_, reason = ResolveAlertHosts(map[string]string{"alertname": "X"}, inv, nil)
	if !strings.Contains(reason, "names no host") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestSource(t *testing.T) {
	fs := newFixtureServer(t, map[string]string{pathTargets: "targets.json"})
	now := time.Date(2026, 11, 12, 14, 5, 0, 0, time.UTC)
	src := NewSource("eu", newTestClient(t, fs, "eu"))
	src.Now = func() time.Time { return now }
	if got := src.Name(); got != "prom" {
		t.Fatalf("Name = %q, want prom", got)
	}

	hosts, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("hosts = %+v, want only the up target", hosts)
	}
	h := hosts[0]
	if h.ID != "web-01.example.internal" {
		t.Errorf("ID = %q, want the instance without its port", h.ID)
	}
	if got, want := h.Labels["job"], "node"; got != want {
		t.Errorf("job label = %q, want %q", got, want)
	}
	if got, want := h.Labels["server"], "eu"; got != want {
		t.Errorf("server label = %q, want %q", got, want)
	}
	if _, ok := h.Labels["instance"]; ok {
		t.Errorf("instance label kept; it is the ID, not a label")
	}
	if !h.SeenAt.Equal(now) {
		t.Errorf("SeenAt = %v, want %v", h.SeenAt, now)
	}
}

func TestSourceErrors(t *testing.T) {
	if _, err := NewSource("eu", nil).Load(context.Background()); err == nil {
		t.Fatalf("Load with no client returned no error")
	}
	fs := newFixtureServer(t, map[string]string{})
	if _, err := NewSource("eu", newTestClient(t, fs, "eu")).Load(context.Background()); err == nil {
		t.Fatalf("Load of a 404 targets endpoint returned no error")
	}
}

// recordingQuerier records the expressions QueryHostColumns asks for.
type recordingQuerier struct {
	exprs   []string
	instant map[string][]Series
	rangeQ  map[string]Matrix
}

func (r *recordingQuerier) Query(_ context.Context, expr string, _ time.Time) (Vector, error) {
	r.exprs = append(r.exprs, expr)
	return append(Vector(nil), r.instant[expr]...), nil
}

func (r *recordingQuerier) QueryRange(_ context.Context, expr string, _, _ time.Time, _ time.Duration) (Matrix, error) {
	r.exprs = append(r.exprs, expr)
	return append(Matrix(nil), r.rangeQ[expr]...), nil
}

func TestQueryHostColumnsBatches(t *testing.T) {
	hosts := make([]resource.HostID, 0, 450)
	for i := 0; i < 450; i++ {
		hosts = append(hosts, resource.HostID(fmt.Sprintf("web-ewr-%03d", i)))
	}
	q := &recordingQuerier{instant: map[string][]Series{}, rangeQ: map[string]Matrix{}}
	cols := []HostColumn{{ID: "load", Title: "LOAD", Expr: `node_load1{instance=~"$instance"}`}}

	from := time.Date(2026, 11, 12, 13, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	if _, err := QueryHostColumns(context.Background(), q, cols, hosts, from, to); err != nil {
		t.Fatalf("QueryHostColumns: %v", err)
	}
	// 450 hosts at 200 per chunk: three instant and three range queries.
	if got, want := len(q.exprs), 6; got != want {
		t.Fatalf("queries = %d, want %d (one per column per chunk)", got, want)
	}
	for _, expr := range q.exprs {
		if strings.Contains(expr, "$instance") {
			t.Fatalf("expr %q was not substituted", expr)
		}
	}
	if n := strings.Count(q.exprs[0], "|"); n != 199 {
		t.Fatalf("first chunk names %d hosts, want 200", n+1)
	}
}

func TestQueryHostColumnsValues(t *testing.T) {
	from := time.Date(2026, 11, 12, 13, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	expr := `node_load1{instance=~"web-ewr-001|web-ewr-002"}`
	q := &recordingQuerier{
		instant: map[string][]Series{
			expr: {
				{Metric: map[string]string{"instance": "web-ewr-001:9100"}, Values: []Sample{{T: to, V: 1.5}}},
				{Metric: map[string]string{"instance": "web-ewr-002:9100"}, Values: []Sample{{T: to, V: 2.5}}},
			},
		},
		rangeQ: map[string]Matrix{
			expr: {
				{Metric: map[string]string{"instance": "web-ewr-001:9100"}, Values: []Sample{{T: from, V: 1}, {T: to, V: 1.5}}},
				{Metric: map[string]string{"instance": "web-ewr-002:9100"}, Values: []Sample{{T: from, V: 2}, {T: to, V: 2.5}}},
			},
		},
	}
	hosts := []resource.HostID{"web-ewr-001", "web-ewr-002"}
	cols := []HostColumn{{ID: "load", Title: "LOAD", Expr: `node_load1{instance=~"$instance"}`}}

	got, err := QueryHostColumns(context.Background(), q, cols, hosts, from, to)
	if err != nil {
		t.Fatalf("QueryHostColumns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("values = %+v, want two hosts", got)
	}
	if v := got["web-ewr-001"]["load"]; v.Value != 1.5 || len(v.Spark) != 2 {
		t.Errorf("web-ewr-001 load = %+v, want 1.5 with two points", v)
	}
	if v := got["web-ewr-002"]["load"]; v.Value != 2.5 {
		t.Errorf("web-ewr-002 load = %+v, want 2.5", v)
	}
}

func TestSubstituteHostPlaceholders(t *testing.T) {
	if got, want := SubstituteHost(`up{instance="$instance", host="$host"}`, "web-01"), `up{instance="web-01", host="web-01"}`; got != want {
		t.Errorf("SubstituteHost = %q, want %q", got, want)
	}
	got := SubstituteHosts(`x{instance=~"$instance"}`, []resource.HostID{"a.example", "b.example"})
	if want := `x{instance=~"a\.example|b\.example"}`; got != want {
		t.Errorf("SubstituteHosts = %q, want %q", got, want)
	}
}

func TestDefaultHostLabelKeys(t *testing.T) {
	want := []string{"instance", "host", "hostname", "node", "nodename", "exported_instance"}
	if strings.Join(DefaultHostLabelKeys, ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v, want %v", DefaultHostLabelKeys, want)
	}
	if DefaultHostColumns() != nil {
		t.Fatalf("default host columns are not empty")
	}
}
