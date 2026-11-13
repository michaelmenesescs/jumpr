package plugin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"jumper/internal/errs"
	"jumper/internal/inventory"
	"jumper/internal/resource"
	"jumper/internal/shq"
	"jumper/internal/transport"
	"jumper/internal/transport/transporttest"
)

// inventoryHTTPYAML is a target: inventory plugin fed by an HTTP request.
func inventoryHTTPYAML(url string) string {
	return fmt.Sprintf(`
apiVersion: jumper/v1
kind: Plugin
name: db-pool
description: database pool members
target: inventory
connection: corp-sso
request:
  method: GET
  url: %s/v1/dbs
parse:
  type: json
view: { columns: [name, role, env] }
`, url)
}

func inventoryServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestInventorySourceHTTP runs an inventory plugin over HTTP and checks that
// its rows become hosts.
func TestInventorySourceHTTP(t *testing.T) {
	srv := inventoryServer(t, `[
		{"name":"db-ewr-001.example.internal","role":"db","env":"prod"},
		{"name":"db-ewr-002.example.internal","role":"db","env":"prod"}
	]`)
	def := loadTestDef(t, inventoryHTTPYAML(srv.URL))
	stamp := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	src, err := NewInventory(def, InventoryOptions{
		Client: testClient(t, "corp-sso"),
		Now:    func() time.Time { return stamp },
	})
	if err != nil {
		t.Fatalf("NewInventory: %v", err)
	}
	if got := src.Name(); got != "plugin.db-pool" {
		t.Fatalf("Name = %q, want plugin.db-pool", got)
	}

	hosts, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts = %d, want 2 (%+v)", len(hosts), hosts)
	}
	h := hosts[0]
	if h.ID != "db-ewr-001.example.internal" {
		t.Fatalf("ID = %q", h.ID)
	}
	if h.Addr != "db-ewr-001.example.internal" {
		t.Fatalf("Addr = %q", h.Addr)
	}
	if h.Labels["role"] != "db" || h.Labels["env"] != "prod" {
		t.Fatalf("labels = %v", h.Labels)
	}
	if h.Labels["host"] != "db-ewr-001.example.internal" {
		t.Fatalf("host label = %q", h.Labels["host"])
	}
	if !h.SeenAt.Equal(stamp) {
		t.Fatalf("SeenAt = %v, want %v", h.SeenAt, stamp)
	}
	if hosts[1].ID != "db-ewr-002.example.internal" {
		t.Fatalf("hosts are not sorted: %v", hosts)
	}
}

// TestInventorySourceCommand runs a privileged host command through the fake
// transport and parses its output.
func TestInventorySourceCommand(t *testing.T) {
	def := Definition{
		Name:       "db-list",
		Target:     TargetInventory,
		Privileged: true,
		Command:    `dbctl list {{ .Param "pool" }}`,
		Params:     []ParamDef{{Name: "pool"}},
		Parse:      ParseDef{Type: ParseColumns, Names: []string{"name", "env"}},
	}
	fake := transporttest.New().OnFunc(func(host resource.HostID, cmd transport.Command) (transporttest.Reply, bool) {
		return transporttest.Reply{Stdout: "db-ewr-001.example.internal prod\ndb-ewr-002.example.internal prod\n"}, true
	})
	src, err := NewInventory(def, InventoryOptions{
		Exec:    fake,
		Host:    "ops-workstation.example.internal",
		Params:  map[string]string{"pool": "a b; rm -rf /"},
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewInventory: %v", err)
	}
	hosts, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(hosts) != 2 || hosts[0].Labels["env"] != "prod" {
		t.Fatalf("hosts = %+v", hosts)
	}

	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	call := calls[0]
	if call.Host != "ops-workstation.example.internal" {
		t.Fatalf("host = %q", call.Host)
	}
	wantScript := "dbctl list " + shq.Quote("a b; rm -rf /")
	if call.Command.Script != wantScript {
		t.Fatalf("script = %q, want %q", call.Command.Script, wantScript)
	}
	if !call.Command.Sudo {
		t.Fatal("privileged plugin must run with sudo")
	}
	if call.Command.Timeout != 2*time.Second {
		t.Fatalf("timeout = %v", call.Command.Timeout)
	}
}

// TestInventorySourceCommandFailure checks that a failed command is an error
// of the right kind and that a transport failure passes through.
func TestInventorySourceCommandFailure(t *testing.T) {
	base := Definition{
		Name:    "db-list",
		Target:  TargetInventory,
		Command: "dbctl list",
		Parse:   ParseDef{Type: ParseLines},
	}
	cases := []struct {
		name  string
		reply transporttest.Reply
		want  errs.Kind
		match string
	}{
		{
			name:  "non-zero rc",
			reply: transporttest.Reply{RC: 3, Stderr: "permission denied\n"},
			want:  errs.Remote,
			match: "exited 3",
		},
		{
			name:  "transport error",
			reply: transporttest.Reply{Err: errs.New(errs.Unreachable, "fake.exec", "dial failed")},
			want:  errs.Unreachable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply := tc.reply
			fake := transporttest.New().OnFunc(func(resource.HostID, transport.Command) (transporttest.Reply, bool) {
				return reply, true
			})
			src, err := NewInventory(base, InventoryOptions{Exec: fake, Host: "ops.example.internal"})
			if err != nil {
				t.Fatalf("NewInventory: %v", err)
			}
			_, err = src.Load(context.Background())
			if kind := errs.KindOf(err); kind != tc.want {
				t.Fatalf("kind = %q, want %q (%v)", kind, tc.want, err)
			}
			if tc.match != "" && !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("error = %q, want %q in it", err, tc.match)
			}
		})
	}
}

// TestInventorySourceValidatesOptions covers the wiring mistakes a plugin
// author or the app can make.
func TestInventorySourceValidatesOptions(t *testing.T) {
	client := testClient(t, "corp-sso")
	req := &RequestDef{Method: "GET", URL: "https://lb-api.example.internal/v1/dbs"}
	cases := []struct {
		name string
		def  Definition
		opts InventoryOptions
	}{
		{
			name: "wrong target",
			def:  Definition{Name: "x", Target: TargetHTTP, Command: "id"},
			opts: InventoryOptions{Exec: transporttest.New(), Host: "h"},
		},
		{
			name: "command and request",
			def:  Definition{Name: "x", Target: TargetInventory, Command: "id", Request: req},
			opts: InventoryOptions{Exec: transporttest.New(), Host: "h", Client: client},
		},
		{
			name: "neither",
			def:  Definition{Name: "x", Target: TargetInventory},
			opts: InventoryOptions{},
		},
		{
			name: "no executor",
			def:  Definition{Name: "x", Target: TargetInventory, Command: "id"},
			opts: InventoryOptions{Host: "h"},
		},
		{
			name: "no host",
			def:  Definition{Name: "x", Target: TargetInventory, Command: "id"},
			opts: InventoryOptions{Exec: transporttest.New()},
		},
		{
			name: "request without client",
			def:  Definition{Name: "x", Target: TargetInventory, Request: req, Connection: "corp-sso"},
			opts: InventoryOptions{},
		},
		{
			name: "request without connection",
			def:  Definition{Name: "x", Target: TargetInventory, Request: req},
			opts: InventoryOptions{Client: client},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewInventory(tc.def, tc.opts); err == nil {
				t.Fatal("NewInventory accepted a broken definition")
			} else if kind := errs.KindOf(err); kind != errs.Config {
				t.Fatalf("kind = %q, want config (%v)", kind, err)
			}
		})
	}
}

// TestInventoryHostsFromRows covers the row-to-host rules.
func TestInventoryHostsFromRows(t *testing.T) {
	stamp := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	rows := []map[string]any{
		{
			"name":  "Web-EWR-001.example.internal",
			"role":  "web",
			"conns": int64(12),
			"fresh": true,
			"tags":  []any{"a", "b"},
			"attrs": map[string]any{"ignored": "yes"},
			"empty": "",
		},
		{"name": "web-ewr-001.example.internal", "role": "other", "zone": "ewr"},
		{"role": "nameless"},
		{"host": "db-ewr-002.example.internal", "role": "db"},
	}
	hosts := hostsFromRows(rows, stamp)
	if len(hosts) != 2 {
		t.Fatalf("hosts = %+v, want 2", hosts)
	}
	var web, db inventory.Host
	for _, h := range hosts {
		switch h.ID {
		case "web-ewr-001.example.internal":
			web = h
		case "db-ewr-002.example.internal":
			db = h
		}
	}
	if web.ID != "web-ewr-001.example.internal" || web.Addr != "Web-EWR-001.example.internal" {
		t.Fatalf("web = %+v", web)
	}
	if web.Labels["role"] != "web" {
		t.Fatalf("the first row wins for a duplicate: %v", web.Labels)
	}
	if web.Labels["zone"] != "ewr" {
		t.Fatalf("a later duplicate adds a missing label: %v", web.Labels)
	}
	if web.Labels["conns"] != "12" || web.Labels["fresh"] != "true" || web.Labels["tags"] != "a,b" {
		t.Fatalf("labels = %v", web.Labels)
	}
	if _, ok := web.Labels["attrs"]; ok {
		t.Fatalf("an object is not a label: %v", web.Labels)
	}
	if _, ok := web.Labels["empty"]; ok {
		t.Fatalf("an empty value is not a label: %v", web.Labels)
	}
	if web.Labels["host"] != "web-ewr-001.example.internal" {
		t.Fatalf("host label = %q", web.Labels["host"])
	}
	if !web.SeenAt.Equal(stamp) {
		t.Fatalf("SeenAt = %v", web.SeenAt)
	}
	if db.ID != "db-ewr-002.example.internal" || db.Addr != "db-ewr-002.example.internal" {
		t.Fatalf("a host column names the host too: %+v", db)
	}
}

// TestInventorySourceInStore checks the source through the inventory merge: it
// is labelled under its own namespace and appears as a source.
func TestInventorySourceInStore(t *testing.T) {
	srv := inventoryServer(t, `[{"name":"db-ewr-001.example.internal","role":"db","env":"prod"}]`)
	def := loadTestDef(t, inventoryHTTPYAML(srv.URL))
	stamp := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	src, err := NewInventory(def, InventoryOptions{
		Client: testClient(t, "corp-sso"),
		Now:    func() time.Time { return stamp },
	})
	if err != nil {
		t.Fatalf("NewInventory: %v", err)
	}

	store := inventory.New([]inventory.Source{src}, nil, "", func() time.Time { return stamp })
	if failed := store.Refresh(context.Background()); len(failed) != 0 {
		t.Fatalf("refresh failed: %v", failed)
	}
	hosts := store.Hosts()
	if len(hosts) != 1 {
		t.Fatalf("hosts = %+v", hosts)
	}
	h := hosts[0]
	if h.ID != "db-ewr-001.example.internal" {
		t.Fatalf("ID = %q", h.ID)
	}
	if h.Labels["plugin.db-pool.role"] != "db" {
		t.Fatalf("namespaced label = %v", h.Labels)
	}
	if h.Labels["role"] != "db" || h.Labels["host"] != "db-ewr-001.example.internal" {
		t.Fatalf("labels = %v", h.Labels)
	}
	if len(h.Sources) != 1 || h.Sources[0] != "plugin.db-pool" {
		t.Fatalf("sources = %v", h.Sources)
	}
}
