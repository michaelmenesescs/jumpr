package linux

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"jumper/internal/backend"
	"jumper/internal/errs"
	"jumper/internal/inventory"
	"jumper/internal/probe"
	"jumper/internal/resource"
	"jumper/internal/transport"
	"jumper/internal/transport/transporttest"
)

// testInventory is a fixed host list implementing inventory.Inventory.
type testInventory struct{ hosts []inventory.Host }

func (i testInventory) Hosts() []inventory.Host { return i.hosts }

func (i testInventory) Select(sel resource.Selector) []inventory.Host {
	var out []inventory.Host
	for _, h := range i.hosts {
		if sel.IsZero() || sel.Matches(h.Labels) {
			out = append(out, h)
		}
	}
	return out
}

func (i testInventory) Resolve(name string) (inventory.Host, bool) {
	id := resource.NormalizeHostID(name)
	for _, h := range i.hosts {
		if h.ID == id {
			return h, true
		}
	}
	return inventory.Host{}, false
}

func (i testInventory) Refresh(ctx context.Context) map[string]error { return nil }
func (i testInventory) Age() time.Duration                           { return 0 }

// inv builds an inventory of bare host names carrying a role label.
func inv(names ...string) testInventory {
	hosts := make([]inventory.Host, 0, len(names))
	for _, n := range names {
		id := resource.NormalizeHostID(n)
		hosts = append(hosts, inventory.Host{
			ID:     id,
			Addr:   string(id),
			Labels: map[string]string{"host": string(id), "role": "web"},
		})
	}
	return testInventory{hosts: hosts}
}

// mark writes one marker pair for id onto sb.
func mark(sb *strings.Builder, nonce, id string, rc int, body string) {
	fmt.Fprintf(sb, "\n@@SITREP:%s:%s:B@@\n%s\n@@SITREP:%s:%s:E:%d@@\n", nonce, id, body, nonce, id, rc)
}

// scriptReply builds the marked-up reply of a batch from per-section stdout,
// exit code and stderr. It mirrors what a real node prints: markers on both
// streams, the exit code read right after each command.
func scriptReply(batch probe.Batch, bodies map[string]string, rcs map[string]int, stderrs map[string]string) transporttest.Reply {
	var out, errb strings.Builder
	for _, s := range batch.Sections {
		rc := 0
		if v, ok := rcs[s.ID]; ok {
			rc = v
		}
		mark(&out, batch.Nonce, s.ID, rc, bodies[s.ID])
		mark(&errb, batch.Nonce, s.ID, 0, stderrs[s.ID])
	}
	return transporttest.Reply{Stdout: out.String(), Stderr: errb.String()}
}

// overviewBodies is a healthy Rocky 9 host's overview sections.
func overviewBodies() map[string]string {
	return map[string]string{
		secLoadavg:  "0.52 0.41 0.30 2/413 99123",
		secMeminfo:  "MemTotal:       1000 kB\nMemAvailable:    500 kB\nMemFree: 100 kB\nBuffers: 100 kB\nCached: 300 kB",
		secPSI:      "== cpu\nsome avg10=1.23 avg60=0.80 avg300=0.40 total=123456\n== memory\nsome avg10=0.00 avg60=0.00 avg300=0.00 total=0\n== io\nsome avg10=3.20 avg60=1.10 avg300=0.50 total=987654\nfull avg10=2.90 avg60=0.90 avg300=0.40 total=876543",
		secOS:       "ID=\"rocky\"\nVERSION_ID=\"9.4\"\nPRETTY_NAME=\"Rocky Linux 9.4 (Blue Onyx)\"",
		secKernel:   "5.14.0-427.el9.x86_64",
		secUptime:   "3561234.56 28012345.11",
		secNproc:    "8",
		secSysState: "running",
		secFailed:   "● nginx.service loaded failed failed A high performance web server",
	}
}

// newTestBackend builds a backend with the fixed test nonce.
func newTestBackend(t *testing.T, ex transport.Executor, in inventory.Inventory) *Backend {
	t.Helper()
	return New(Options{
		Exec:      ex,
		Inventory: in,
		Timeout:   time.Second,
		Nonce:     func() string { return "deadbeef" },
		Now:       func() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) },
	})
}

func batchOf(t *testing.T, b *Backend, kind resource.KindName) probe.Batch {
	t.Helper()
	batch, err := b.batch(kind)
	if err != nil {
		t.Fatalf("batch(%s): %v", kind, err)
	}
	return batch
}

func drainResults(t *testing.T, ch <-chan backend.Result) []backend.Result {
	t.Helper()
	var out []backend.Result
	for r := range ch {
		out = append(out, r)
	}
	return out
}

func TestBackendListHosts(t *testing.T) {
	ex := transporttest.New()
	b := newTestBackend(t, ex, inv("web-01", "web-02", "web-03"))
	batch := batchOf(t, b, KindHost)

	reply := scriptReply(batch, overviewBodies(), nil, nil)
	ex.OnHost("web-01", "", reply)
	ex.OnHost("web-02", "", reply)
	ex.OnHost("web-03", "", transporttest.Reply{Err: errs.New(errs.Timeout, "test", "dial timed out")})

	ch, err := b.List(context.Background(), resource.Query{Kind: KindHost})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := drainResults(t, ch)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}

	items, failed := 0, 0
	for _, r := range results {
		if r.Total != 3 {
			t.Errorf("Total = %d, want 3", r.Total)
		}
		if r.Done < 1 || r.Done > 3 {
			t.Errorf("Done = %d, want 1..3", r.Done)
		}
		switch {
		case r.Err != nil:
			failed++
			if k := errs.KindOf(r.Err); k != errs.Timeout {
				t.Errorf("error kind = %s, want timeout", k)
			}
			if r.Host != "web-03" {
				t.Errorf("failing host = %s, want web-03", r.Host)
			}
		default:
			items++
			if len(r.Items) != 1 {
				t.Fatalf("host %s: %d items, want 1", r.Host, len(r.Items))
			}
			row := r.Items[0]
			if row.Ref.Backend != "linux" || row.Ref.Kind != KindHost || row.Ref.Name != string(r.Host) {
				t.Errorf("bad ref %+v", row.Ref)
			}
			if got := row.Fields["load"]; got != 0.52 {
				t.Errorf("load = %v, want 0.52", got)
			}
			if got := row.Fields["mem"]; got != 50.0 {
				t.Errorf("mem = %v, want 50", got)
			}
			if got := row.Fields["psi_io"]; got != 3.2 {
				t.Errorf("psi_io = %v, want 3.2", got)
			}
			if got := row.Fields["failed"]; got != int64(1) {
				t.Errorf("failed = %v, want 1", got)
			}
			if got := row.Fields["units"]; got != "nginx.service" {
				t.Errorf("units = %v, want nginx.service", got)
			}
			if got := row.Fields["state"]; got != "running" {
				t.Errorf("state = %v, want running", got)
			}
			if row.Status != resource.StatusCrit {
				t.Errorf("status = %v, want crit (a failed unit)", row.Status)
			}
			if row.Labels["role"] != "web" {
				t.Errorf("labels not copied from inventory: %v", row.Labels)
			}
			if row.FetchedAt.IsZero() {
				t.Error("FetchedAt is zero")
			}
			if _, ok := row.Raw.(*hostInfo); !ok {
				t.Errorf("Raw = %T, want *hostInfo", row.Raw)
			}
		}
	}
	if items != 2 || failed != 1 {
		t.Errorf("items=%d failed=%d, want 2 and 1", items, failed)
	}
}

func TestBackendListUnitsSortedFailedFirst(t *testing.T) {
	ex := transporttest.New()
	b := newTestBackend(t, ex, inv("web-01"))
	batch := batchOf(t, b, KindUnit)
	ex.OnHost("web-01", "", scriptReply(batch, map[string]string{
		secUnits: strings.Join([]string{
			"db.service loaded active running Database",
			"nginx.service loaded failed failed A high performance web server",
			"cron.timer loaded active waiting Daily cron",
		}, "\n"),
	}, nil, nil))

	ch, err := b.List(context.Background(), resource.Query{Kind: KindUnit, Hosts: []resource.HostID{"web-01"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := drainResults(t, ch)
	if len(results) != 1 || len(results[0].Items) != 3 {
		t.Fatalf("got %d results, want one with 3 items", len(results))
	}
	rows := results[0].Items
	want := []string{"nginx.service", "db.service", "cron.timer"}
	for i, name := range want {
		if rows[i].Ref.Name != name {
			t.Errorf("row %d = %s, want %s", i, rows[i].Ref.Name, name)
		}
	}
	if rows[0].Status != resource.StatusCrit {
		t.Errorf("failed unit status = %v, want crit", rows[0].Status)
	}
	if rows[0].Fields["host"] != "web-01" {
		t.Errorf("unit row host field = %v", rows[0].Fields["host"])
	}
}

func TestBackendListProcessesDefaultSort(t *testing.T) {
	ex := transporttest.New()
	b := newTestBackend(t, ex, inv("web-01"))
	batch := batchOf(t, b, KindProcess)
	ex.OnHost("web-01", "", scriptReply(batch, map[string]string{
		secProcs: strings.Join([]string{
			"  1234     1 root      Ss    0.5  1.2  20480 3600 /usr/sbin/nginx -g daemon off;",
			"  4321  4200 app       Rl   51.2  7.5 102400  120 /opt/app/worker --threads 4",
			"     9     2 root      S     0.0  0.0      0 99999 [kworker/0:1]",
		}, "\n"),
	}, nil, nil))

	ch, err := b.List(context.Background(), resource.Query{Kind: KindProcess, Hosts: []resource.HostID{"web-01"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := drainResults(t, ch)
	if len(results) != 1 || len(results[0].Items) != 3 {
		t.Fatalf("got %d results, want one with 3 items", len(results))
	}
	rows := results[0].Items
	if rows[0].Ref.Name != "4321" {
		t.Errorf("first process = %s, want the 51.2%% CPU one (4321)", rows[0].Ref.Name)
	}
	if got := rows[0].Fields["rss"]; got != int64(102400*1024) {
		t.Errorf("rss = %v, want bytes", got)
	}
	if got := rows[0].Fields["elapsed"]; got != 120*time.Second {
		t.Errorf("elapsed = %v, want 2m", got)
	}
	if got := rows[0].Fields["command"]; got != "/opt/app/worker --threads 4" {
		t.Errorf("command = %v", got)
	}
}

func TestBackendSocketsNeedPrivilegeDegrades(t *testing.T) {
	const ssOut = "tcp   LISTEN 0      511          0.0.0.0:443        0.0.0.0:*\n" +
		"tcp   LISTEN 0      128             [::]:22            [::]:*"

	ex := transporttest.New()
	b := newTestBackend(t, ex, inv("web-01"))
	batch := batchOf(t, b, KindSocket)

	ex.OnFunc(func(host resource.HostID, cmd transport.Command) (transporttest.Reply, bool) {
		if strings.Contains(cmd.Script, "sudo -n -- sh -c") {
			return scriptReply(batch, map[string]string{secSockets: ""},
				map[string]int{secSockets: 1},
				map[string]string{secSockets: "sudo: a password is required"}), true
		}
		retry, err := socketRetryBatch(batch.Nonce)
		if err != nil {
			t.Fatalf("socketRetryBatch: %v", err)
		}
		return scriptReply(retry, map[string]string{secSockets: ssOut}, nil, nil), true
	})

	ch, err := b.List(context.Background(), resource.Query{Kind: KindSocket, Hosts: []resource.HostID{"web-01"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := drainResults(t, ch)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("host failed: %v", results[0].Err)
	}
	rows := results[0].Items
	if len(rows) != 2 {
		t.Fatalf("got %d sockets, want 2", len(rows))
	}
	for _, row := range rows {
		if got := row.Labels["_degraded"]; got != "sockets: sudo" {
			t.Errorf("_degraded = %q, want \"sockets: sudo\"", got)
		}
		if v, ok := row.Fields["process"]; ok && v != nil {
			t.Errorf("process = %v, want nil after the unprivileged retry", v)
		}
	}
	if calls := len(ex.Calls()); calls != 2 {
		t.Errorf("Exec calls = %d, want 2 (sudo batch + unprivileged retry)", calls)
	}
}

func TestBackendListNoHosts(t *testing.T) {
	b := newTestBackend(t, transporttest.New(), inv())
	_, err := b.List(context.Background(), resource.Query{Kind: KindHost})
	if err == nil {
		t.Fatal("List succeeded, want an error")
	}
	if k := errs.KindOf(err); k != errs.Config {
		t.Errorf("kind = %s, want config", k)
	}
	if !strings.Contains(err.Error(), "no hosts match") {
		t.Errorf("message = %q, want it to mention \"no hosts match\"", err)
	}
}

func TestBackendGetUnit(t *testing.T) {
	ex := transporttest.New()
	b := newTestBackend(t, ex, inv("web-01"))
	batch := batchOf(t, b, KindUnit)
	ex.OnHost("web-01", "", scriptReply(batch, map[string]string{
		secUnits: "nginx.service loaded failed failed A high performance web server\n" +
			"sshd.service loaded active running OpenSSH server",
	}, nil, nil))

	row, err := b.Get(context.Background(), resource.Ref{
		Backend: "linux", Kind: KindUnit, Host: "web-01", Name: "nginx.service",
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Ref.Name != "nginx.service" || row.Ref.Host != "web-01" {
		t.Errorf("bad ref %+v", row.Ref)
	}
	if row.Fields["active"] != "failed" {
		t.Errorf("active = %v, want failed", row.Fields["active"])
	}
	if _, err := b.Get(context.Background(), resource.Ref{
		Backend: "linux", Kind: KindUnit, Host: "web-01", Name: "missing.service",
	}); errs.KindOf(err) != errs.NotAvailable {
		t.Errorf("missing unit: kind = %s, want not-available", errs.KindOf(err))
	}
}

func TestBackendLinksFromHost(t *testing.T) {
	b := newTestBackend(t, transporttest.New(), inv("web-01"))
	links := b.Links(resource.Resource{Ref: resource.Ref{Backend: "linux", Kind: KindHost, Host: "web-01", Name: "web-01"}})
	if len(links) != 8 {
		t.Fatalf("got %d links, want 8", len(links))
	}
	if first := links[0]; first.Key != "enter" || first.To.Kind != KindHostOverview {
		t.Errorf("first link = %+v, want enter → host-overview", first)
	}
	want := map[string]resource.KindName{
		"enter": KindHostOverview,
		"u":     KindUnit,
		"p":     KindProcess,
		"s":     KindSocket,
		"j":     KindJournal,
		"m":     KindMount,
		"D":     KindDisk,
		"i":     KindInterface,
	}
	for _, l := range links {
		kind, ok := want[l.Key]
		if !ok {
			t.Errorf("unexpected link key %q", l.Key)
			continue
		}
		if l.To.Kind != kind {
			t.Errorf("link %q kind = %s, want %s", l.Key, l.To.Kind, kind)
		}
		if len(l.To.Hosts) != 1 || l.To.Hosts[0] != "web-01" {
			t.Errorf("link %q hosts = %v", l.Key, l.To.Hosts)
		}
		delete(want, l.Key)
	}
	if len(want) != 0 {
		t.Errorf("missing link keys %v", want)
	}
	if got := b.Links(resource.Resource{Ref: resource.Ref{Kind: KindUnit, Host: "web-01", Name: "nginx.service"}}); len(got) != 1 || got[0].To.Args["unit"] != "nginx.service" {
		t.Errorf("unit links = %v, want one journal link with the unit arg", got)
	}
}

func TestBackendOneRoundTripPerHost(t *testing.T) {
	ex := transporttest.New()
	b := newTestBackend(t, ex, inv("web-01", "web-02", "web-03"))
	batch := batchOf(t, b, KindHost)
	ex.On("", scriptReply(batch, overviewBodies(), nil, nil))

	ch, err := b.List(context.Background(), resource.Query{Kind: KindHost})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	drainResults(t, ch)

	if calls := len(ex.Calls()); calls != 3 {
		t.Errorf("Exec calls = %d, want 3 (one batch per host)", calls)
	}
	for _, c := range ex.Calls() {
		if !strings.Contains(c.Command.Script, "cat /proc/loadavg") {
			t.Errorf("host %s: command is not the overview batch: %q", c.Host, c.Command.Script)
		}
	}
}

func TestBackendHealthStates(t *testing.T) {
	b := newTestBackend(t, transporttest.New(), inv())
	if h := b.Health(context.Background()); h.State != backend.HealthUnconfigured {
		t.Errorf("empty inventory: state = %s, want unconfigured", h.State)
	}

	b = newTestBackend(t, transporttest.New(), inv("web-01"))
	if h := b.Health(context.Background()); h.State != backend.HealthOK {
		t.Errorf("no samples: state = %s, want ok", h.State)
	}
	for i := 0; i < 10; i++ {
		b.record(true)
	}
	if h := b.Health(context.Background()); h.State != backend.HealthDegraded {
		t.Errorf("all failed: state = %s, want degraded", h.State)
	}
	// The window keeps the last 20: 19 good samples flush the failures out.
	for i := 0; i < 19; i++ {
		b.record(false)
	}
	if h := b.Health(context.Background()); h.State != backend.HealthOK {
		t.Errorf("mostly good: state = %s, want ok (%s)", h.State, h.Detail)
	}
}

func TestBackendUnknownKind(t *testing.T) {
	b := newTestBackend(t, transporttest.New(), inv("web-01"))
	if _, err := b.List(context.Background(), resource.Query{Kind: "pod"}); errs.KindOf(err) != errs.Config {
		t.Errorf("List kind = %s, want config", errs.KindOf(err))
	}
	if got := len(b.Kinds()); got != 14 {
		t.Errorf("Kinds() = %d, want 14", got)
	}
	if b.Name() != "linux" {
		t.Errorf("Name() = %q, want linux", b.Name())
	}
}

func TestBackendHostDegradesMissingSection(t *testing.T) {
	ex := transporttest.New()
	b := newTestBackend(t, ex, inv("web-01"))
	batch := batchOf(t, b, KindHost)

	bodies := overviewBodies()
	delete(bodies, secPSI) // a kernel without PSI still answers every other section
	var out strings.Builder
	for _, s := range batch.Sections {
		if s.ID == secPSI {
			continue
		}
		mark(&out, batch.Nonce, s.ID, 0, bodies[s.ID])
	}
	ex.OnHost("web-01", "", transporttest.Reply{Stdout: out.String()})

	ch, err := b.List(context.Background(), resource.Query{Kind: KindHost})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := drainResults(t, ch)
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("host failed: %+v", results)
	}
	row := results[0].Items[0]
	if v, ok := row.Fields["psi_io"]; ok && v != nil {
		t.Errorf("psi_io = %v, want nil", v)
	}
	if got := row.Labels["_degraded"]; !strings.Contains(got, "psi: ") {
		t.Errorf("_degraded = %q, want it to name the psi section", got)
	}
	if row.Fields["load"] != 0.52 {
		t.Errorf("other sections must survive: load = %v", row.Fields["load"])
	}
}
