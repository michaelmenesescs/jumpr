package chef

import (
	"context"
	"strings"
	"testing"
	"time"

	"jumpr/internal/backend"
	"jumpr/internal/errs"
	"jumpr/internal/redact"
	"jumpr/internal/resource"
	"jumpr/internal/transport/transporttest"
)

// newTestBackend returns a backend over the lab fixtures with a fixed clock.
func newTestBackend(t *testing.T, fake *transporttest.Fake, opts ...func(*Options)) *Backend {
	t.Helper()
	o := Options{
		Runner:      NewRunner(fake, ws, RunnerOptions{}),
		Workstation: ws,
		Now:         func() time.Time { return testTime },
	}
	for _, f := range opts {
		f(&o)
	}
	return New(o)
}

// fixtureFake answers every knife read the backend makes.
func fixtureFake(t *testing.T) *transporttest.Fake {
	t.Helper()
	return transporttest.New().
		OnHost(ws, "knife status", transporttest.Reply{Stdout: string(readFixture(t, "status.json"))}).
		OnHost(ws, "knife node list", transporttest.Reply{Stdout: string(readFixture(t, "node-list.json"))}).
		OnHost(ws, "knife node show web-01", transporttest.Reply{Stdout: string(readFixture(t, "node-show.json"))}).
		OnHost(ws, "knife role list", transporttest.Reply{Stdout: string(readFixture(t, "role-list.json"))}).
		OnHost(ws, "knife environment list", transporttest.Reply{Stdout: string(readFixture(t, "env-list.json"))}).
		OnHost(ws, "knife search", transporttest.Reply{Stdout: string(readFixture(t, "search.json"))})
}

// drain collects every result of a List.
func drain(t *testing.T, ch <-chan backend.Result) []backend.Result {
	t.Helper()
	var out []backend.Result
	for res := range ch {
		out = append(out, res)
	}
	return out
}

// TestNodesList checks the chef-node rows: one per registered node, sorted,
// with the environment, platform, roles, run list and check-in age, and the
// status of the check-in.
func TestNodesList(t *testing.T) {
	b := newTestBackend(t, fixtureFake(t))
	ch, err := b.List(context.Background(), resource.Query{Kind: KindNode})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	batches := drain(t, ch)
	if len(batches) != 1 {
		t.Fatalf("batches = %d, want 1", len(batches))
	}
	if batches[0].Err != nil {
		t.Fatalf("batch error: %v", batches[0].Err)
	}
	items := batches[0].Items
	if len(items) != 4 {
		t.Fatalf("rows = %d, want 4", len(items))
	}
	names := []string{"db-01", "web-01", "web-02", "web-03"}
	for i, want := range names {
		if items[i].Ref.Name != want {
			t.Errorf("row %d = %q, want %q", i, items[i].Ref.Name, want)
		}
		if items[i].Ref.Backend != "chef" || items[i].Ref.Kind != KindNode {
			t.Errorf("row %d ref = %v", i, items[i].Ref)
		}
	}

	web := byName(items, "web-01")
	if web.Fields["environment"] != "production" {
		t.Errorf("environment = %v", web.Fields["environment"])
	}
	if web.Fields["platform"] != "ubuntu 22.04" {
		t.Errorf("platform = %v, want the version appended", web.Fields["platform"])
	}
	if web.Fields["roles"] != "base,web" {
		t.Errorf("roles = %v", web.Fields["roles"])
	}
	if web.Fields["run_list"] != "role[base], role[web]" {
		t.Errorf("run_list = %v", web.Fields["run_list"])
	}
	if age, ok := web.Fields["checkin"].(time.Duration); !ok || age != 5*time.Minute {
		t.Errorf("checkin = %v, want 5m", web.Fields["checkin"])
	}
	if web.Status != resource.StatusOK {
		t.Errorf("web-01 status = %v, want ok", web.Status)
	}
	if web.Labels["chef.environment"] != "production" || web.Labels["chef.roles"] != "base,web" ||
		web.Labels["chef.platform"] != "ubuntu" {
		t.Errorf("labels = %v", web.Labels)
	}
}

// TestStaleCheckinStatus checks requirement 3: more than 2 h is Warn, more than
// 24 h is Crit, and a node that never checked in is not-applicable.
func TestStaleCheckinStatus(t *testing.T) {
	b := newTestBackend(t, fixtureFake(t))
	ch, err := b.List(context.Background(), resource.Query{Kind: KindNode})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	items := drain(t, ch)[0].Items
	cases := map[string]resource.Status{
		"web-01": resource.StatusOK,   // 5 minutes
		"web-02": resource.StatusWarn, // 3 hours
		"web-03": resource.StatusNA,   // no ohai_time
		"db-01":  resource.StatusCrit, // 30 hours
	}
	for name, want := range cases {
		row := byName(items, name)
		if row.Status != want {
			t.Errorf("%s status = %v, want %v", name, row.Status, want)
		}
		if want == resource.StatusNA {
			if _, ok := row.Fields["checkin"]; ok {
				t.Errorf("%s has a check-in age but no ohai_time", name)
			}
		}
	}
}

// TestStaleThresholdsConfigurable checks the thresholds can be moved.
func TestStaleThresholdsConfigurable(t *testing.T) {
	b := newTestBackend(t, fixtureFake(t), func(o *Options) {
		o.StaleAfter = 10 * time.Minute
		o.CriticalAfter = 4 * time.Hour
	})
	ch, err := b.List(context.Background(), resource.Query{Kind: KindNode})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	items := drain(t, ch)[0].Items
	if got := byName(items, "web-01").Status; got != resource.StatusOK {
		t.Errorf("web-01 (5m, stale after 10m) = %v, want ok", got)
	}
	if got := byName(items, "web-02").Status; got != resource.StatusWarn {
		t.Errorf("web-02 (3h, crit after 4h) = %v, want warn", got)
	}
	if got := byName(items, "db-01").Status; got != resource.StatusCrit {
		t.Errorf("db-01 (30h) = %v, want crit", got)
	}
}

// TestRolesAndEnvironments checks the role and environment kinds and the node
// counts that come from the status call.
func TestRolesAndEnvironments(t *testing.T) {
	b := newTestBackend(t, fixtureFake(t))

	ch, err := b.List(context.Background(), resource.Query{Kind: KindRole})
	if err != nil {
		t.Fatalf("List roles: %v", err)
	}
	roles := drain(t, ch)[0].Items
	if len(roles) != 3 {
		t.Fatalf("roles = %d, want 3", len(roles))
	}
	want := map[string]int64{"base": 4, "web": 3, "db": 1}
	for _, row := range roles {
		if got, ok := row.Fields["nodes"].(int64); !ok || got != want[row.Ref.Name] {
			t.Errorf("role %s nodes = %v, want %d", row.Ref.Name, row.Fields["nodes"], want[row.Ref.Name])
		}
	}

	ch, err = b.List(context.Background(), resource.Query{Kind: KindEnv})
	if err != nil {
		t.Fatalf("List environments: %v", err)
	}
	envs := drain(t, ch)[0].Items
	wantEnv := map[string]int64{"production": 3, "staging": 1, "_default": 0}
	if len(envs) != 3 {
		t.Fatalf("environments = %d, want 3", len(envs))
	}
	for _, row := range envs {
		got, ok := row.Fields["nodes"].(int64)
		if !ok {
			got = 0
		}
		if got != wantEnv[row.Ref.Name] {
			t.Errorf("environment %s nodes = %v, want %d", row.Ref.Name, got, wantEnv[row.Ref.Name])
		}
	}
}

// TestRoleListSurvivesFailedStatus checks the counts are best effort: the names
// still list when the status call fails.
func TestRoleListSurvivesFailedStatus(t *testing.T) {
	fake := transporttest.New().
		OnHost(ws, "knife role list", transporttest.Reply{Stdout: `["web"]`}).
		OnHost(ws, "knife status", transporttest.Reply{RC: 1, Stderr: "ERROR: Server returned error 401 Unauthorized"})
	b := newTestBackend(t, fake)
	ch, err := b.List(context.Background(), resource.Query{Kind: KindRole})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	batches := drain(t, ch)
	if len(batches) != 1 || batches[0].Err != nil {
		t.Fatalf("batches = %+v, want one clean batch", batches)
	}
	if len(batches[0].Items) != 1 {
		t.Fatalf("items = %d, want 1", len(batches[0].Items))
	}
	if _, ok := batches[0].Items[0].Fields["nodes"]; ok {
		t.Error("nodes count present although status failed")
	}
}

// TestNodeGetDetail checks Get returns the full `knife node show -l` detail in
// Raw, with the same row fields as the list.
func TestNodeGetDetail(t *testing.T) {
	red, err := redact.New(nil)
	if err != nil {
		t.Fatalf("redactor: %v", err)
	}
	b := newTestBackend(t, fixtureFake(t), func(o *Options) { o.Redactor = red })
	row, err := b.Get(context.Background(), resource.Ref{Backend: "chef", Kind: KindNode, Name: "web-01"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	d, ok := row.Raw.(NodeDetail)
	if !ok {
		t.Fatalf("Raw = %T, want NodeDetail", row.Raw)
	}
	if d.Name != "web-01" || len(d.Attributes) == 0 {
		t.Errorf("detail = %+v", d)
	}
	if !d.FetchedAt.Equal(testTime) {
		t.Errorf("FetchedAt = %s, want %s", d.FetchedAt, testTime)
	}
	if row.Fields["environment"] != "production" || row.Fields["roles"] != "base,web" {
		t.Errorf("fields = %v", row.Fields)
	}
}

// TestNodeGetRejectsBadName checks a node name that is not one shell word never
// reaches the command line.
func TestNodeGetRejectsBadName(t *testing.T) {
	fake := fixtureFake(t)
	b := newTestBackend(t, fake)
	_, err := b.Get(context.Background(), resource.Ref{Kind: KindNode, Name: "web-01; rm -rf /"})
	if errs.KindOf(err) != errs.Config {
		t.Fatalf("kind = %q, want config (%v)", errs.KindOf(err), err)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("calls = %d, want none", len(fake.Calls()))
	}
}

// TestSearchQuotesTheQuery checks a user-typed Solr query reaches knife as one
// quoted argument (task 113 pitfall).
func TestSearchQuotesTheQuery(t *testing.T) {
	fake := fixtureFake(t)
	b := newTestBackend(t, fake)
	rows, err := b.Search(context.Background(), "role:web AND chef_environment:production", "platform")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	calls := fake.Calls()
	last := calls[len(calls)-1].Command.Script
	want := "knife search node 'role:web AND chef_environment:production' -a platform -F json"
	if last != want {
		t.Errorf("script = %q, want %q", last, want)
	}
}

// TestSearchRejectsBadAttribute checks the attribute name is validated.
func TestSearchRejectsBadAttribute(t *testing.T) {
	fake := fixtureFake(t)
	b := newTestBackend(t, fake)
	if _, err := b.Search(context.Background(), "*:*", "platform; id"); errs.KindOf(err) != errs.Config {
		t.Fatalf("kind = %q, want config (%v)", errs.KindOf(err), err)
	}
	if _, err := b.Search(context.Background(), "  "); errs.KindOf(err) != errs.Config {
		t.Fatalf("empty query kind = %q, want config", errs.KindOf(err))
	}
}

// TestLinks checks a node row pivots to its host.
func TestLinks(t *testing.T) {
	b := newTestBackend(t, fixtureFake(t))
	row := resource.Resource{Ref: resource.Ref{Backend: "chef", Kind: KindNode, Name: "Web-01"}}
	links := b.Links(row)
	if len(links) != 1 {
		t.Fatalf("links = %+v, want 1", links)
	}
	if links[0].Key != "enter" || links[0].Label != "host" {
		t.Errorf("link = %+v", links[0])
	}
	if got := links[0].To.Hosts; len(got) != 1 || got[0] != "web-01" {
		t.Errorf("target hosts = %v, want the normalised node name", got)
	}
	if got := b.Links(resource.Resource{Ref: resource.Ref{Kind: KindRole, Name: "web"}}); len(got) != 0 {
		t.Errorf("role links = %+v, want none", got)
	}
}

// TestHealth checks the health dot: a `knife node list` round trip, cached for
// healthTTL, and down when the workstation cannot answer.
func TestHealth(t *testing.T) {
	fake := fixtureFake(t)
	b := newTestBackend(t, fake)
	h := b.Health(context.Background())
	if h.State != backend.HealthOK {
		t.Fatalf("state = %v (%s), want ok", h.State, h.Detail)
	}
	if h.Detail != "4 nodes" {
		t.Errorf("detail = %q, want \"4 nodes\"", h.Detail)
	}
	if !h.Checked.Equal(testTime) {
		t.Errorf("checked = %s", h.Checked)
	}
	before := len(fake.Calls())
	if again := b.Health(context.Background()); again.State != backend.HealthOK {
		t.Errorf("second state = %v", again.State)
	}
	if after := len(fake.Calls()); after != before {
		t.Errorf("cached health made a call: %d → %d", before, after)
	}
}

// TestHealthDown checks a failing knife turns the dot red and keeps the message.
func TestHealthDown(t *testing.T) {
	fake := transporttest.New().OnHost(ws, "knife node list", transporttest.Reply{
		Stdout: "", RC: 1, Stderr: "ERROR: Failed to authenticate to https://chef.lab.example.internal/ as sre",
	})
	b := newTestBackend(t, fake)
	h := b.Health(context.Background())
	if h.State != backend.HealthDown {
		t.Fatalf("state = %v, want down", h.State)
	}
	if !strings.Contains(h.Detail, "authenticate") {
		t.Errorf("detail = %q", h.Detail)
	}
}

// TestHealthUnconfigured checks a backend without a workstation is grey.
func TestHealthUnconfigured(t *testing.T) {
	b := New(Options{})
	if h := b.Health(context.Background()); h.State != backend.HealthUnconfigured {
		t.Errorf("state = %v, want unconfigured", h.State)
	}
}

// TestListErrors checks the two ways List refuses to start.
func TestListErrors(t *testing.T) {
	b := newTestBackend(t, fixtureFake(t))
	if _, err := b.List(context.Background(), resource.Query{Kind: "chef-unicorn"}); errs.KindOf(err) != errs.Config {
		t.Errorf("unknown kind = %q, want config", errs.KindOf(err))
	}
	if _, err := New(Options{}).List(context.Background(), resource.Query{Kind: KindNode}); errs.KindOf(err) != errs.Config {
		t.Errorf("unconfigured = %q, want config", errs.KindOf(err))
	}
}

// TestListFailureArrivesAsABatch checks a failed knife call arrives as a batch
// error, not as a List error.
func TestListFailureArrivesAsABatch(t *testing.T) {
	fake := transporttest.New().OnHost(ws, "knife status", transporttest.Reply{
		RC: 1, Stderr: `ERROR: Errno::ECONNREFUSED: Connection refused - connect(2)`,
	})
	b := newTestBackend(t, fake)
	ch, err := b.List(context.Background(), resource.Query{Kind: KindNode})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	batches := drain(t, ch)
	if len(batches) != 1 || batches[0].Err == nil {
		t.Fatalf("batches = %+v, want one with an error", batches)
	}
	if got := errs.KindOf(batches[0].Err); got != errs.Unreachable {
		t.Errorf("kind = %q, want unreachable", got)
	}
}

// TestKindSpecs checks the three kinds are registered with the columns the task
// names.
func TestKindSpecs(t *testing.T) {
	specs := kindSpecs()
	byName := map[resource.KindName]resource.KindSpec{}
	for _, s := range specs {
		byName[s.Name] = s
	}
	node, ok := byName[KindNode]
	if !ok {
		t.Fatalf("no chef-node spec in %+v", specs)
	}
	for _, id := range []string{"name", "environment", "platform", "roles", "run_list", "checkin", "status"} {
		if _, ok := node.Column(id); !ok {
			t.Errorf("chef-node has no %q column", id)
		}
	}
	if node.Scope != resource.ScopeGlobal {
		t.Errorf("chef-node scope = %v, want global", node.Scope)
	}
	if _, ok := byName[KindRole]; !ok {
		t.Error("no chef-role spec")
	}
	if _, ok := byName[KindEnv]; !ok {
		t.Error("no chef-env spec")
	}
}

// byName returns the row with the given name.
func byName(items []resource.Resource, name string) resource.Resource {
	for _, r := range items {
		if r.Ref.Name == name {
			return r
		}
	}
	return resource.Resource{}
}
