package plugin

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"jumper/internal/backend"
	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/transport"
	"jumper/internal/transport/transporttest"
)

// testDefinition is the nginx-status plugin of SPEC §12, as a Definition the
// loader would produce.
func testDefinition(name string) Definition {
	return Definition{
		APIVersion:  APIVersion,
		Kind:        KindPlugin,
		Name:        name,
		Description: "nginx worker and connection summary",
		Target:      TargetHost,
		Command:     "nginx -T 2>/dev/null | grep -E '^\\s*(worker_processes|listen)'",
		Parse: ParseDef{
			Type:    ParseRegex,
			Pattern: `^\s*(?P<directive>\w+)\s+(?P<value>[^;]+);`,
		},
		View: ViewDef{Columns: []string{"directive", "value"}, GroupByOutput: true},
		Links: []LinkDef{
			{Label: "journal", To: "journal", Args: map[string]string{"unit": "nginx.service"}},
		},
		Actions: []ActionDef{
			{Name: "reload", Command: "systemctl reload nginx", Privileged: true, Confirm: true},
		},
		File: "/tmp/plugins/" + name + ".yaml",
		Line: 3,
	}
}

// newTestBackend builds a backend over ex with a fixed clock.
func newTestBackend(t *testing.T, def Definition, ex transport.Executor) *Backend {
	t.Helper()
	b, err := NewBackend(BackendOptions{
		Definition: def,
		Exec:       ex,
		Timeout:    time.Second,
		Now:        func() time.Time { return time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	return b
}

// collect drains a List into results, failing on a stream error.
func collect(t *testing.T, ch <-chan backend.Result) []backend.Result {
	t.Helper()
	var out []backend.Result
	for r := range ch {
		out = append(out, r)
	}
	return out
}

// TestBackendKindsIsHostScoped checks requirement 1: the plugin is a kind named
// after itself, host-scoped, with a host column and the declared columns.
func TestBackendKindsIsHostScoped(t *testing.T) {
	b := newTestBackend(t, testDefinition("nginx-status"), transporttest.New())
	if b.Name() != "nginx-status" {
		t.Fatalf("Name() = %q, want nginx-status", b.Name())
	}
	kinds := b.Kinds()
	if len(kinds) != 1 {
		t.Fatalf("Kinds() returned %d kinds, want 1", len(kinds))
	}
	spec := kinds[0]
	if spec.Name != "nginx-status" {
		t.Fatalf("kind = %q, want nginx-status", spec.Name)
	}
	if spec.Scope != resource.ScopeHost {
		t.Fatalf("scope = %v, want ScopeHost", spec.Scope)
	}
	want := []string{"host", "directive", "value"}
	if len(spec.Columns) != len(want) {
		t.Fatalf("columns = %v, want %v", spec.Columns, want)
	}
	for i, id := range want {
		if spec.Columns[i].ID != id {
			t.Fatalf("column %d = %q, want %q", i, spec.Columns[i].ID, id)
		}
	}
	if !b.GroupByOutput() {
		t.Fatal("GroupByOutput() = false, want true from view.group_by_output")
	}
	if !b.UniformCommand() {
		t.Fatal("a command without {{ .Host }} must be uniform")
	}
}

// TestBackendListParsesRowsPerHost checks that every host runs the command
// through the fan-out and its output becomes rows of the kind.
func TestBackendListParsesRowsPerHost(t *testing.T) {
	ex := transporttest.New()
	ex.OnFunc(func(host resource.HostID, _ transport.Command) (transporttest.Reply, bool) {
		switch host {
		case "web-01":
			return transporttest.Reply{Stdout: "worker_processes 4;\nlisten 443;\n"}, true
		case "web-02":
			return transporttest.Reply{Stdout: "worker_processes 8;\n"}, true
		}
		return transporttest.Reply{}, false
	})
	b := newTestBackend(t, testDefinition("nginx-status"), ex)
	hosts := []resource.HostID{"web-01", "web-02"}
	ch, err := b.List(context.Background(), resource.Query{Kind: "nginx-status", Hosts: hosts})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := collect(t, ch)
	if len(results) != 2 {
		t.Fatalf("List yielded %d results, want one per host", len(results))
	}
	rows := map[resource.HostID]int{}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("host %s: %v", r.Host, r.Err)
		}
		if r.Total != 2 || r.Done < 1 {
			t.Fatalf("host %s: Done/Total = %d/%d", r.Host, r.Done, r.Total)
		}
		rows[r.Host] = len(r.Items)
		for _, item := range r.Items {
			if got := item.Fields["host"]; got != string(r.Host) {
				t.Fatalf("host field = %v, want %s", got, r.Host)
			}
			if item.Ref.Kind != "nginx-status" {
				t.Fatalf("ref kind = %q", item.Ref.Kind)
			}
		}
	}
	if rows["web-01"] != 2 || rows["web-02"] != 1 {
		t.Fatalf("row counts = %v, want web-01:2 web-02:1", rows)
	}
	if len(ex.Calls()) != 2 {
		t.Fatalf("executor saw %d calls, want 2", len(ex.Calls()))
	}
}

// TestBackendListReportsFailuresPerHost checks that one host failing does not
// hide the others.
func TestBackendListReportsFailuresPerHost(t *testing.T) {
	ex := transporttest.New()
	ex.OnHost("web-02", "", transporttest.Reply{
		Err: errs.New(errs.Timeout, "fake.exec", "timed out"),
	})
	ex.On("", transporttest.Reply{Stdout: "worker_processes 4;\n"})
	b := newTestBackend(t, testDefinition("nginx-status"), ex)
	ch, err := b.List(context.Background(), resource.Query{
		Hosts: []resource.HostID{"web-01", "web-02"},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var ok, failed int
	for _, r := range collect(t, ch) {
		if r.Err != nil {
			failed++
			if errs.KindOf(r.Err) != errs.Timeout {
				t.Fatalf("kind = %s, want timeout", errs.KindOf(r.Err))
			}
			continue
		}
		ok++
	}
	if ok != 1 || failed != 1 {
		t.Fatalf("ok=%d failed=%d, want 1/1", ok, failed)
	}
}

// TestBackendListUnparsableOutputIsAParseError checks the error kind a bad read
// produces.
func TestBackendListUnparsableOutputIsAParseError(t *testing.T) {
	ex := transporttest.New()
	ex.On("", transporttest.Reply{Stdout: "{not json"})
	def := testDefinition("bad-json")
	def.Parse = ParseDef{Type: ParseJSON}
	b := newTestBackend(t, def, ex)
	ch, err := b.List(context.Background(), resource.Query{Hosts: []resource.HostID{"web-01"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := collect(t, ch)
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("results = %+v, want one parse error", results)
	}
	if kind := errs.KindOf(results[0].Err); kind != errs.Parse {
		t.Fatalf("kind = %s, want parse", kind)
	}
}

// TestBackendEmitsHostsResolvesPivots checks requirement 1's emits_hosts rule:
// the row's host becomes the row's identity, the inventory resolves it, and
// `enter` opens its overview.
func TestBackendEmitsHostsResolvesPivots(t *testing.T) {
	ex := transporttest.New()
	ex.On("", transporttest.Reply{Stdout: `{"host":"WEB-03","state":"UP","conns":12}`})
	def := testDefinition("lb-pool-members")
	def.Command = "curl -s http://lb.example.internal/pool"
	def.Parse = ParseDef{Type: ParseJSON}
	def.View = ViewDef{Columns: []string{"host", "state", "conns"}}
	def.EmitsHosts = TargetHost
	def.Links = nil
	b, err := NewBackend(BackendOptions{
		Definition: def,
		Exec:       ex,
		Resolve: func(name string) (resource.HostID, bool) {
			if strings.EqualFold(name, "web-03") {
				return "web-03.example.internal", true
			}
			return "", false
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	ch, err := b.List(context.Background(), resource.Query{Hosts: []resource.HostID{"lb-01"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := collect(t, ch)
	if len(results) != 1 || len(results[0].Items) != 1 {
		t.Fatalf("results = %+v, want one row", results)
	}
	row := results[0].Items[0]
	if row.Ref.Host != "web-03.example.internal" {
		t.Fatalf("ref host = %q, want the resolved member", row.Ref.Host)
	}
	if got := row.Fields["host"]; got != "web-03.example.internal" {
		t.Fatalf("host field = %v", got)
	}
	links := b.Links(row)
	if len(links) == 0 {
		t.Fatal("emits_hosts row has no links")
	}
	if links[0].Key != "enter" || links[0].To.Kind != "host-overview" {
		t.Fatalf("first link = %+v, want enter → host-overview", links[0])
	}
	if got := links[0].To.Hosts; len(got) != 1 || got[0] != "web-03.example.internal" {
		t.Fatalf("pivot hosts = %v", got)
	}
}

// TestBackendLinksTemplateTheirArgs checks that a declared link becomes a query
// with the row's values substituted, unquoted.
func TestBackendLinksTemplateTheirArgs(t *testing.T) {
	def := testDefinition("unit-health")
	def.Links = []LinkDef{
		{Label: "journal", To: "journal", Args: map[string]string{"unit": `{{ .Param "unit" }}`, "host": "{{ .Host }}"}},
		{Label: "broken", To: "journal", Args: map[string]string{"unit": `{{ .Param "missing" }}`}},
	}
	b := newTestBackend(t, def, transporttest.New())
	row := resource.Resource{
		Ref:    resource.Ref{Kind: "unit-health", Host: "web-01"},
		Fields: map[string]any{"host": "web-01", "unit": "nginx.service"},
	}
	links := b.Links(row)
	// host overview + journal; the link whose argument cannot be filled is
	// skipped rather than failing the view.
	if len(links) != 2 {
		t.Fatalf("links = %+v, want 2", links)
	}
	if links[1].Label != "journal" {
		t.Fatalf("second link = %q", links[1].Label)
	}
	if got := links[1].To.Args["unit"]; got != "nginx.service" {
		t.Fatalf("unit arg = %q", got)
	}
	if got := links[1].To.Args["host"]; got != "web-01" {
		t.Fatalf("host arg = %q", got)
	}
	if links[1].To.Kind != "journal" {
		t.Fatalf("link kind = %q", links[1].To.Kind)
	}
}

// TestBackendGetReadsOneRow checks Get over the same command.
func TestBackendGetReadsOneRow(t *testing.T) {
	ex := transporttest.New()
	ex.On("", transporttest.Reply{Stdout: "worker_processes 4;\n"})
	b := newTestBackend(t, testDefinition("nginx-status"), ex)
	r, err := b.Get(context.Background(), resource.Ref{Kind: "nginx-status", Host: "web-01"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.Fields["directive"] != "worker_processes" || fieldString(r.Fields["value"]) != "4" {
		t.Fatalf("fields = %v", r.Fields)
	}
	if r.Ref.Host != "web-01" {
		t.Fatalf("ref host = %q", r.Ref.Host)
	}
	if _, err := b.Get(context.Background(), resource.Ref{}); err == nil {
		t.Fatal("Get without a host must fail")
	}
}

// TestBackendRejectsWriteInReadCommand is the task's pitfall: a read runs
// without confirmation, so an obvious write in it is refused. The usual output
// redirections are not writes.
func TestBackendRejectsWriteInReadCommand(t *testing.T) {
	writes := []string{
		"echo hi > /etc/motd",
		"ps aux | tee /tmp/out",
		"rm -f /var/log/app.log",
		"systemctl restart nginx",
		"journalctl -u nginx >> /tmp/log",
	}
	for _, cmd := range writes {
		def := testDefinition("writes")
		def.Command = cmd
		if _, err := NewBackend(BackendOptions{Definition: def}); err == nil {
			t.Errorf("read command %q was accepted", cmd)
		}
	}
	reads := []string{
		"nginx -T 2>/dev/null | grep listen",
		"ss -Htanp state established '( sport = :443 )' 2>&1 | wc -l",
		"date >/dev/null && uptime",
		"systemctl status nginx",
	}
	for _, cmd := range reads {
		def := testDefinition("reads")
		def.Command = cmd
		if _, err := NewBackend(BackendOptions{Definition: def}); err != nil {
			t.Errorf("read command %q was refused: %v", cmd, err)
		}
	}
}

// TestBackendRefusesNonHostTarget keeps the HTTP and inventory tracks out of
// this backend.
func TestBackendRefusesNonHostTarget(t *testing.T) {
	def := testDefinition("lb")
	def.Target = TargetHTTP
	if _, err := NewBackend(BackendOptions{Definition: def}); err == nil {
		t.Fatal("a http plugin must not register a host kind")
	}
}

// TestBackendHealthNamesItsFile checks the health line `:health` shows.
func TestBackendHealthNamesItsFile(t *testing.T) {
	b := newTestBackend(t, testDefinition("nginx-status"), transporttest.New())
	h := b.Health(context.Background())
	if h.State != backend.HealthOK {
		t.Fatalf("state = %v", h.State)
	}
	if !strings.Contains(h.Detail, "nginx-status") {
		t.Fatalf("detail = %q", h.Detail)
	}
}

// TestBackendCommandQuotesParameters checks that a parameter value cannot become
// a second shell word.
func TestBackendCommandQuotesParameters(t *testing.T) {
	def := testDefinition("param")
	def.Command = `curl -s "http://api.example.internal/v1/{{ .Param "pool" }}"`
	b := newTestBackend(t, def, transporttest.New())
	cmd, err := b.Command("web-01", map[string]string{"pool": "a; rm -rf /"})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if !strings.Contains(cmd.Script, `'a; rm -rf /'`) {
		t.Fatalf("script = %s", cmd.Script)
	}
	if _, err := b.Command("web-01", nil); err == nil {
		t.Fatal("a missing parameter must fail")
	}
}

// TestBackendHostsResolution checks the host resolution order of the query.
func TestBackendHostsResolution(t *testing.T) {
	selected := 0
	b, err := NewBackend(BackendOptions{
		Definition: testDefinition("hosts"),
		Select: func(resource.Selector) []resource.HostID {
			selected++
			return []resource.HostID{"web-09", "web-09", "web-10"}
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	hosts, err := b.Hosts(resource.Query{Hosts: []resource.HostID{"WEB-01", "web-01", "web-02"}})
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	if fmt.Sprint(hosts) != "[web-01 web-02]" {
		t.Fatalf("explicit hosts = %v", hosts)
	}
	if selected != 0 {
		t.Fatal("explicit hosts must win over the selector")
	}
	hosts, err = b.Hosts(resource.Query{Selector: resource.Selector{}})
	if err != nil {
		t.Fatalf("Hosts: %v", err)
	}
	if fmt.Sprint(hosts) != "[web-09 web-10]" {
		t.Fatalf("selector hosts = %v", hosts)
	}
	if _, err := (&Backend{def: testDefinition("none")}).Hosts(resource.Query{}); err == nil {
		t.Fatal("a query with no targets must fail")
	}
}
