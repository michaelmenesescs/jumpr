package salt

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jumpr/internal/backend"
	"jumpr/internal/errs"
	"jumpr/internal/inventory"
	"jumpr/internal/resource"
	"jumpr/internal/transport/transporttest"
)

// ---- fixtures ----

// testClock is a settable clock for cache tests.
type testClock struct{ sec int64 }

func (c *testClock) now() time.Time { return time.Unix(atomic.LoadInt64(&c.sec), 0).UTC() }
func (c *testClock) advance(d time.Duration) {
	atomic.AddInt64(&c.sec, int64(d/time.Second))
}

var baseTime = time.Date(2026, 11, 12, 13, 55, 0, 0, time.UTC)

func newClock() *testClock {
	return &testClock{sec: baseTime.Unix()}
}

// stubInv is a tiny inventory.Inventory for tests.
type stubInv struct {
	mu    sync.Mutex
	hosts []inventory.Host
}

func (s *stubInv) Hosts() []inventory.Host {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]inventory.Host(nil), s.hosts...)
}

func (s *stubInv) Select(sel resource.Selector) []inventory.Host {
	var out []inventory.Host
	for _, h := range s.Hosts() {
		if sel.Matches(h.Labels) {
			out = append(out, h)
		}
	}
	return out
}

func (s *stubInv) Resolve(nameOrAlias string) (inventory.Host, bool) {
	want := strings.ToLower(strings.TrimSpace(nameOrAlias))
	for _, h := range s.Hosts() {
		if strings.ToLower(string(h.ID)) == want {
			return h, true
		}
		for _, a := range h.Aliases {
			if strings.ToLower(a) == want {
				return h, true
			}
		}
	}
	return inventory.Host{}, false
}

func (s *stubInv) Refresh(context.Context) map[string]error { return nil }
func (s *stubInv) Age() time.Duration                       { return 0 }

// testInventory has two hosts reachable from minion IDs: one via the salt.id
// label, one via an alias that is also an accepted key, one by short name only.
func testInventory() *stubInv {
	return &stubInv{hosts: []inventory.Host{
		{
			ID:      "web-01.example.internal",
			Aliases: []string{"web-01", "10.0.0.11"},
			Labels:  map[string]string{"host": "web-01.example.internal", "salt.id": "web-01"},
		},
		{
			ID:      "db-01.example.internal",
			Aliases: []string{"db-01"},
			Labels:  map[string]string{"host": "db-01.example.internal"},
		},
		{
			ID:     "app-02.example.internal",
			Labels: map[string]string{"host": "app-02.example.internal"},
		},
	}}
}

const (
	keysJSON = `{"minions": ["web-01", "db-01", "app-02"], "minions_pre": ["new-01"],
		"minions_rejected": [], "minions_denied": []}`
	fingerprintJSON = `{"minions": {"web-01": "aa:bb:cc", "db-01": "dd:ee:ff", "app-02": "11:22:33"},
		"minions_pre": {"new-01": "44:55:66"}, "local": {"master.pub": "99:88:77"}}`
	statusJSON  = `{"up": ["web-01", "app-02"], "down": ["db-01"]}`
	versionJSON = `{"Master": "3007.1", "Up to date": ["web-01"],
		"Minion requires update": {"db-01": "3006.0"}}`
)

// newTestBackend scripts a fake transport with the fixture answers.
func newTestBackend(t *testing.T, clk *testClock) (*Backend, *transporttest.Fake) {
	t.Helper()
	f := transporttest.New().
		On("-F", transporttest.Reply{Stdout: fingerprintJSON}).
		On("-L", transporttest.Reply{Stdout: keysJSON}).
		On("manage.status", transporttest.Reply{Stdout: statusJSON}).
		On("manage.versions", transporttest.Reply{Stdout: versionJSON})
	b := New(Options{
		Runner:    NewRunner(f, master, RunnerOptions{}),
		Inventory: testInventory(),
		Master:    master,
		Now:       clk.now,
	})
	return b, f
}

// collect drains one List into a single slice of rows.
func collectRows(t *testing.T, b *Backend, q resource.Query) []resource.Resource {
	t.Helper()
	ch, err := b.List(context.Background(), q)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var rows []resource.Resource
	for res := range ch {
		if res.Err != nil {
			t.Fatalf("batch error: %v", res.Err)
		}
		rows = append(rows, res.Items...)
	}
	return rows
}

func rowByName(t *testing.T, rows []resource.Resource, name string) resource.Resource {
	t.Helper()
	for _, r := range rows {
		if r.Ref.Name == name {
			return r
		}
	}
	t.Fatalf("no row %q in %v", name, names(rows))
	return resource.Resource{}
}

func names(rows []resource.Resource) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Ref.Name)
	}
	return out
}

// ---- parsers ----

func TestParseKeys(t *testing.T) {
	got, err := ParseKeys([]byte(keysJSON))
	if err != nil {
		t.Fatalf("ParseKeys: %v", err)
	}
	want := []Key{
		{ID: "app-02", State: KeyAccepted},
		{ID: "db-01", State: KeyAccepted},
		{ID: "web-01", State: KeyAccepted},
		{ID: "new-01", State: KeyPending},
	}
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseKeysToleratesNulls(t *testing.T) {
	got, err := ParseKeys([]byte(`{"minions": null, "minions_rejected": ["bad-01"]}`))
	if err != nil {
		t.Fatalf("ParseKeys: %v", err)
	}
	if len(got) != 1 || got[0].ID != "bad-01" || got[0].State != KeyRejected {
		t.Errorf("keys = %+v, want one rejected bad-01", got)
	}
}

func TestParseKeysGarbage(t *testing.T) {
	for _, in := range []string{"", "no json here", `{"minions": "web-01"}`} {
		if _, err := ParseKeys([]byte(in)); !errs.Is(err, errs.Parse) {
			t.Errorf("ParseKeys(%q) kind = %q, want parse (%v)", in, errs.KindOf(err), err)
		}
	}
}

func TestParseFingerprints(t *testing.T) {
	got, err := ParseFingerprints([]byte(fingerprintJSON))
	if err != nil {
		t.Fatalf("ParseFingerprints: %v", err)
	}
	want := map[string]string{
		"web-01": "aa:bb:cc",
		"db-01":  "dd:ee:ff",
		"app-02": "11:22:33",
		"new-01": "44:55:66",
	}
	if len(got) != len(want) {
		t.Fatalf("fingerprints = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("fingerprint[%q] = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["master.pub"]; ok {
		t.Error("local master key leaked into the minion fingerprints")
	}
}

func TestManageStatus(t *testing.T) {
	got, err := ParseManageStatus([]byte(statusJSON))
	if err != nil {
		t.Fatalf("ParseManageStatus: %v", err)
	}
	if len(got.Up) != 2 || len(got.Down) != 1 || got.Down[0] != "db-01" {
		t.Errorf("status = %+v, want up 2 down [db-01]", got)
	}
	if _, err := ParseManageStatus([]byte("not json")); !errs.Is(err, errs.Parse) {
		t.Errorf("garbage kind = %q, want parse", errs.KindOf(err))
	}
}

func TestManageVersions(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		master     string
		upToDate   map[string]string
		needUpdate map[string]string
	}{
		{
			name:       "list and map shapes",
			in:         versionJSON,
			master:     "3007.1",
			upToDate:   map[string]string{"web-01": "3007.1"},
			needUpdate: map[string]string{"db-01": "3006.0"},
		},
		{
			name:       "map shape on both sides",
			in:         `{"Master": "3007.1", "Up to date": {"web-01": "3007.1"}, "Minion requires update": {"db-01": "3006.0"}}`,
			master:     "3007.1",
			upToDate:   map[string]string{"web-01": "3007.1"},
			needUpdate: map[string]string{"db-01": "3006.0"},
		},
		{
			name:       "unknown sections are empty",
			in:         `{"Master": "3007.1"}`,
			master:     "3007.1",
			upToDate:   map[string]string{},
			needUpdate: map[string]string{},
		},
		{
			name:       "list entries of objects",
			in:         `{"Master": "3007.1", "Up to date": [{"id": "web-01", "version": "3007.1"}]}`,
			master:     "3007.1",
			upToDate:   map[string]string{"web-01": "3007.1"},
			needUpdate: map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseManageVersions([]byte(tt.in))
			if err != nil {
				t.Fatalf("ParseManageVersions: %v", err)
			}
			if got.Master != tt.master {
				t.Errorf("master = %q, want %q", got.Master, tt.master)
			}
			if len(got.UpToDate) != len(tt.upToDate) || len(got.RequiresUpdate) != len(tt.needUpdate) {
				t.Fatalf("versions = %+v (up %d, update %d), want up %d, update %d",
					got, len(got.UpToDate), len(got.RequiresUpdate), len(tt.upToDate), len(tt.needUpdate))
			}
			for id, v := range tt.upToDate {
				if got.UpToDate[id] != v {
					t.Errorf("up to date[%q] = %q, want %q", id, got.UpToDate[id], v)
				}
			}
			for id, v := range tt.needUpdate {
				if got.RequiresUpdate[id] != v {
					t.Errorf("requires update[%q] = %q, want %q", id, got.RequiresUpdate[id], v)
				}
			}
		})
	}
}

// ---- minions ----

func TestListMinionsMerged(t *testing.T) {
	clk := newClock()
	b, f := newTestBackend(t, clk)

	rows := collectRows(t, b, resource.Query{Kind: KindMinion})
	if len(rows) != 4 {
		t.Fatalf("rows = %v, want 4", names(rows))
	}
	// Sorted by minion ID.
	if got := names(rows); got[0] != "app-02" || got[1] != "db-01" || got[2] != "new-01" || got[3] != "web-01" {
		t.Errorf("order = %v, want app-02 db-01 new-01 web-01", got)
	}

	up := rowByName(t, rows, "web-01")
	if up.Status != resource.StatusOK {
		t.Errorf("web-01 status = %v, want OK", up.Status)
	}
	if got := up.Str("status"); got != "up" {
		t.Errorf("web-01 status cell = %q, want up", got)
	}
	if got := up.Str("key"); got != KeyAccepted {
		t.Errorf("web-01 key = %q, want accepted", got)
	}
	if got := up.Str("version"); got != "3007.1" {
		t.Errorf("web-01 version = %q, want 3007.1", got)
	}
	if got := up.Str("host"); got != "web-01.example.internal" {
		t.Errorf("web-01 host = %q, want web-01.example.internal", got)
	}
	for _, id := range []string{"os", "role", "dc"} {
		if up.Fields[id] != nil {
			t.Errorf("web-01 %s = %v, want nil until task 069 lands grains", id, up.Fields[id])
		}
	}

	down := rowByName(t, rows, "db-01")
	if down.Status != resource.StatusCrit {
		t.Errorf("db-01 status = %v, want Crit", down.Status)
	}
	if got := down.Str("status"); got != "down" {
		t.Errorf("db-01 status cell = %q, want down", got)
	}
	if got := down.Str("version"); got != "3006.0" {
		t.Errorf("db-01 version = %q, want 3006.0", got)
	}

	pending := rowByName(t, rows, "new-01")
	if pending.Status != resource.StatusWarn {
		t.Errorf("new-01 status = %v, want Warn", pending.Status)
	}
	if got := pending.Str("status"); got != "unaccepted" {
		t.Errorf("new-01 status cell = %q, want unaccepted", got)
	}
	if got := pending.Str("key"); got != KeyPending {
		t.Errorf("new-01 key = %q, want pending", got)
	}
	if pending.Fields["host"] != nil {
		t.Errorf("new-01 host = %v, want nil (not in the inventory)", pending.Fields["host"])
	}

	if calls := f.Calls(); len(calls) != 3 {
		t.Errorf("calls = %d, want 3 (keys, status, versions)", len(calls))
	}
}

func TestListMinionsStatusFailure(t *testing.T) {
	clk := newClock()
	f := transporttest.New().
		On("-L", transporttest.Reply{Stdout: keysJSON}).
		On("manage.status", transporttest.Reply{RC: 1, Stderr: "Salt request timed out.\n"}).
		On("manage.versions", transporttest.Reply{Stdout: versionJSON})
	b := New(Options{Runner: NewRunner(f, master, RunnerOptions{}), Inventory: testInventory(), Master: master, Now: clk.now})

	ch, err := b.List(context.Background(), resource.Query{Kind: KindMinion})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var kind errs.Kind
	for res := range ch {
		if res.Err != nil {
			kind = errs.KindOf(res.Err)
		}
	}
	if kind != errs.Unreachable {
		t.Errorf("batch error kind = %q, want unreachable", kind)
	}
}

func TestListUnknownKind(t *testing.T) {
	clk := newClock()
	b, _ := newTestBackend(t, clk)
	if _, err := b.List(context.Background(), resource.Query{Kind: "salt-beacon"}); !errs.Is(err, errs.Config) {
		t.Errorf("kind = %q, want config (%v)", errs.KindOf(err), err)
	}
}

func TestListUnconfigured(t *testing.T) {
	clk := newClock()
	b := New(Options{Master: master, Now: clk.now})
	if _, err := b.List(context.Background(), resource.Query{Kind: KindKey}); !errs.Is(err, errs.Config) {
		t.Errorf("kind = %q, want config (%v)", errs.KindOf(err), err)
	}
}

// ---- keys ----

func TestKeysPendingFirst(t *testing.T) {
	clk := newClock()
	b, _ := newTestBackend(t, clk)

	rows := collectRows(t, b, resource.Query{Kind: KindKey})
	if len(rows) != 4 {
		t.Fatalf("rows = %v, want 4", names(rows))
	}
	if got := rows[0].Ref.Name; got != "new-01" {
		t.Errorf("first row = %q, want the pending key new-01 (%v)", got, names(rows))
	}
	if got := rows[0].Str("state"); got != KeyPending {
		t.Errorf("first state = %q, want pending", got)
	}
	if got := rows[0].Str("fingerprint"); got != "44:55:66" {
		t.Errorf("new-01 fingerprint = %q, want 44:55:66", got)
	}
	if got := rows[0].Status; got != resource.StatusWarn {
		t.Errorf("pending key status = %v, want Warn", got)
	}
	web := rowByName(t, rows, "web-01")
	if got := web.Str("state"); got != KeyAccepted {
		t.Errorf("web-01 state = %q, want accepted", got)
	}
	if got := web.Str("fingerprint"); got != "aa:bb:cc" {
		t.Errorf("web-01 fingerprint = %q, want aa:bb:cc", got)
	}
	if rows[0].Ref.Kind != KindKey || rows[0].Ref.Backend != backendName {
		t.Errorf("ref = %s, want salt/salt-key", rows[0].Ref)
	}
}

// ---- identity mapping ----

func TestMinionHostMapping(t *testing.T) {
	clk := newClock()
	b, _ := newTestBackend(t, clk)

	host, ok := b.MinionHost("web-01")
	if !ok || host != "web-01.example.internal" {
		t.Errorf("MinionHost(web-01) = %q, %v; want web-01.example.internal, true", host, ok)
	}
	if _, ok := b.MinionHost("ghost-99"); ok {
		t.Error("MinionHost(ghost-99) found a host")
	}

	// The salt.id label wins.
	if id, ok := b.HostMinion("web-01.example.internal"); !ok || id != "web-01" {
		t.Errorf("HostMinion(web-01.example.internal) = %q, %v; want web-01, true", id, ok)
	}
	// Then an alias that is an accepted key (host may be named by short name).
	if id, ok := b.HostMinion("db-01"); !ok || id != "db-01" {
		t.Errorf("HostMinion(db-01) = %q, %v; want db-01, true", id, ok)
	}
	// Then the short name of the host ID.
	if id, ok := b.HostMinion("app-02.example.internal"); !ok || id != "app-02" {
		t.Errorf("HostMinion(app-02.example.internal) = %q, %v; want app-02, true", id, ok)
	}
	if _, ok := b.HostMinion("ghost-99"); ok {
		t.Error("HostMinion(ghost-99) found a minion")
	}
}

func TestHostMinionIgnoresUnacceptedKeys(t *testing.T) {
	clk := newClock()
	f := transporttest.New().
		On("-L", transporttest.Reply{Stdout: `{"minions": ["other-01"], "minions_pre": ["db-01"]}`})
	inv := &stubInv{hosts: []inventory.Host{{
		ID:      "db-01.example.internal",
		Aliases: []string{"db-01"},
		Labels:  map[string]string{"host": "db-01.example.internal"},
	}}}
	b := New(Options{Runner: NewRunner(f, master, RunnerOptions{}), Inventory: inv, Master: master, Now: clk.now})

	if id, ok := b.HostMinion("db-01.example.internal"); ok {
		t.Errorf("HostMinion = %q, true; a pending key is not a minion", id)
	}
}

// ---- links ----

func TestLinksMinion(t *testing.T) {
	clk := newClock()
	b, _ := newTestBackend(t, clk)
	rows := collectRows(t, b, resource.Query{Kind: KindMinion})

	links := b.Links(rowByName(t, rows, "web-01"))
	var host, jobs *resource.Link
	for i := range links {
		switch links[i].Key {
		case "enter":
			host = &links[i]
		case "J":
			jobs = &links[i]
		}
	}
	if host == nil || host.To.Kind != "host" || len(host.To.Hosts) != 1 || host.To.Hosts[0] != "web-01.example.internal" {
		t.Errorf("host link = %+v, want the mapped host overview", host)
	}
	if jobs == nil || jobs.To.Kind != "salt-job" || jobs.To.Args["minion"] != "web-01" {
		t.Errorf("jobs link = %+v, want salt-job filtered to web-01", jobs)
	}

	pending := b.Links(rowByName(t, rows, "new-01"))
	for _, l := range pending {
		if l.Key == "enter" {
			t.Errorf("unmapped minion got a host link: %+v", l)
		}
	}
	if len(pending) != 1 || pending[0].Key != "J" {
		t.Errorf("unmapped minion links = %+v, want only the jobs pivot", pending)
	}
}

func TestLinksIgnoresOtherKinds(t *testing.T) {
	clk := newClock()
	b, _ := newTestBackend(t, clk)
	if got := b.Links(resource.Resource{Ref: resource.Ref{Backend: backendName, Kind: KindKey, Name: "web-01"}}); got != nil {
		t.Errorf("Links(key row) = %+v, want nil", got)
	}
}

// ---- health ----

func TestHealthStates(t *testing.T) {
	tests := []struct {
		name   string
		master resource.HostID
		reply  transporttest.Reply
		want   backend.HealthState
		detail string
	}{
		{
			name:   "all up",
			master: master,
			reply:  transporttest.Reply{Stdout: `{"up": ["web-01", "db-01"], "down": []}`},
			want:   backend.HealthOK,
			detail: "2/2 minions up",
		},
		{
			name:   "some down",
			master: master,
			reply:  transporttest.Reply{Stdout: `{"up": ["web-01"], "down": ["db-01"]}`},
			want:   backend.HealthDegraded,
			detail: "1/2 up",
		},
		{
			name:   "runner error",
			master: master,
			reply:  transporttest.Reply{RC: 1, Stderr: "Salt request timed out. The master is not responding.\n"},
			want:   backend.HealthDown,
			detail: "unreachable",
		},
		{
			name:   "no master configured",
			master: "",
			reply:  transporttest.Reply{Stdout: statusJSON},
			want:   backend.HealthUnconfigured,
			detail: "not configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := newClock()
			f := transporttest.New().On("manage.status", tt.reply)
			b := New(Options{Runner: NewRunner(f, master, RunnerOptions{}), Inventory: testInventory(), Master: tt.master, Now: clk.now})

			h := b.Health(context.Background())
			if h.State != tt.want {
				t.Fatalf("state = %v (%q), want %v", h.State, h.Detail, tt.want)
			}
			if !strings.Contains(h.Detail, tt.detail) {
				t.Errorf("detail = %q, want it to contain %q", h.Detail, tt.detail)
			}
			if !h.Checked.Equal(clk.now()) {
				t.Errorf("checked = %v, want %v", h.Checked, clk.now())
			}
			if tt.want == backend.HealthUnconfigured && len(f.Calls()) != 0 {
				t.Errorf("unconfigured backend ran %d calls, want 0", len(f.Calls()))
			}
		})
	}
}

func TestHealthIsCached(t *testing.T) {
	clk := newClock()
	f := transporttest.New().On("manage.status", transporttest.Reply{Stdout: statusJSON})
	b := New(Options{Runner: NewRunner(f, master, RunnerOptions{}), Inventory: testInventory(), Master: master, Now: clk.now})

	first := b.Health(context.Background())
	clk.advance(29 * time.Second)
	second := b.Health(context.Background())
	if n := len(f.Calls()); n != 1 {
		t.Fatalf("manage.status calls = %d, want 1 within the 30 s window", n)
	}
	if !second.Checked.Equal(first.Checked) {
		t.Errorf("cached Checked = %v, want %v", second.Checked, first.Checked)
	}
	clk.advance(2 * time.Second)
	b.Health(context.Background())
	if n := len(f.Calls()); n != 2 {
		t.Errorf("manage.status calls after the window = %d, want 2", n)
	}
}

func TestHealthSharesInflight(t *testing.T) {
	clk := newClock()
	f := transporttest.New().On("manage.status", transporttest.Reply{Stdout: `{"up": ["web-01", "db-01"], "down": []}`, Delay: 80 * time.Millisecond})
	b := New(Options{Runner: NewRunner(f, master, RunnerOptions{}), Inventory: testInventory(), Master: master, Now: clk.now})

	var wg sync.WaitGroup
	results := make([]backend.Health, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = b.Health(context.Background())
		}(i)
	}
	wg.Wait()
	for i, h := range results {
		if h.State != backend.HealthOK {
			t.Errorf("health %d = %v (%q), want OK", i, h.State, h.Detail)
		}
	}
	if n := len(f.Calls()); n != 1 {
		t.Errorf("manage.status calls = %d, want 1 shared call", n)
	}
}

// ---- kinds ----

func TestKinds(t *testing.T) {
	clk := newClock()
	b, _ := newTestBackend(t, clk)
	if b.Name() != "salt" {
		t.Errorf("Name = %q, want salt", b.Name())
	}
	specs := b.Kinds()
	if len(specs) != 5 {
		t.Fatalf("kinds = %d, want 5", len(specs))
	}
	byName := map[resource.KindName]resource.KindSpec{}
	for _, s := range specs {
		byName[s.Name] = s
	}
	minions, ok := byName[KindMinion]
	if !ok {
		t.Fatalf("no %q kind in %v", KindMinion, specs)
	}
	for _, col := range []string{"minion", "status", "key", "os", "role", "dc", "version", "host"} {
		if _, ok := minions.Column(col); !ok {
			t.Errorf("salt-minion lacks column %q", col)
		}
	}
	if minions.Scope != resource.ScopeGlobal {
		t.Errorf("salt-minion scope = %v, want global", minions.Scope)
	}
	keys, ok := byName[KindKey]
	if !ok {
		t.Fatalf("no %q kind", KindKey)
	}
	for _, col := range []string{"id", "state", "fingerprint"} {
		if _, ok := keys.Column(col); !ok {
			t.Errorf("salt-key lacks column %q", col)
		}
	}
	if keys.Scope != resource.ScopeGlobal {
		t.Errorf("salt-key scope = %v, want global", keys.Scope)
	}
}

func TestGetMinion(t *testing.T) {
	clk := newClock()
	b, _ := newTestBackend(t, clk)
	row, err := b.Get(context.Background(), resource.Ref{Backend: backendName, Kind: KindMinion, Name: "db-01"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Ref.Name != "db-01" || row.Str("status") != "down" {
		t.Errorf("row = %s status %q, want db-01 down", row.Ref, row.Str("status"))
	}
	if _, err := b.Get(context.Background(), resource.Ref{Backend: backendName, Kind: KindMinion, Name: "ghost-99"}); !errs.Is(err, errs.NotAvailable) {
		t.Errorf("kind = %q, want not-available (%v)", errs.KindOf(err), err)
	}
}
