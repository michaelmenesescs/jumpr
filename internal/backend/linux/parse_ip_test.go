package linux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jumper/internal/errs"
	"jumper/internal/probe"
	"jumper/internal/resource"
	"jumper/internal/transport/transporttest"
)

// readIPTestdata reads one fixture of the network capture.
func readIPTestdata(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join("..", "..", "..", "testdata", "linux", "ip", "rocky9", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func TestParseIPLinkGolden(t *testing.T) {
	links, err := ParseIPLinks(readIPTestdata(t, "link.json"))
	if err != nil {
		t.Fatalf("ParseIPLinks: %v", err)
	}
	if got, want := len(links), 5; got != want {
		t.Fatalf("links = %d, want %d: %+v", got, want, links)
	}
	if got, want := links[0].Name, "lo"; got != want {
		t.Errorf("links[0].Name = %q, want %q", got, want)
	}
	if got, want := links[0].State, "UNKNOWN"; got != want {
		t.Errorf("lo state = %q, want %q", got, want)
	}
	if got, want := links[0].MTU, 65536; got != want {
		t.Errorf("lo mtu = %d, want %d", got, want)
	}
	if got, want := links[1].Name, "eth0"; got != want {
		t.Fatalf("links[1].Name = %q, want %q", got, want)
	}
	eth0 := links[1]
	if eth0.State != "UP" || eth0.MTU != 1500 || eth0.Master != "br0" {
		t.Errorf("eth0 = %+v, want UP/1500/br0", eth0)
	}
	if eth0.RXBytes != 123456789 || eth0.TXBytes != 987654321 {
		t.Errorf("eth0 bytes = %d/%d", eth0.RXBytes, eth0.TXBytes)
	}
	if eth0.CounterWarnings() {
		t.Errorf("eth0 has counter warnings: %+v", eth0)
	}
	eth1 := links[2]
	if !eth1.Down() {
		t.Errorf("eth1 state = %q, want DOWN", eth1.State)
	}
	if eth1.RXErrors != 3 || eth1.TXErrors != 1 || eth1.RXDropped != 12 || eth1.TXDropped != 4 {
		t.Errorf("eth1 counters = %+v", eth1)
	}
	if !eth1.CounterWarnings() {
		t.Errorf("eth1 counter warnings not detected")
	}
	// br0 has no stats block at all: the counters stay zero rather than failing.
	if br0 := links[4]; br0.Name != "br0" || br0.RXBytes != 0 || br0.CounterWarnings() {
		t.Errorf("br0 = %+v", br0)
	}
}

func TestParseIPLinkLegacyStats(t *testing.T) {
	out := `[{"ifname":"eth2","operstate":"up","mtu":1500,"stats":{"rx":{"bytes":10,"errors":1,"dropped":2},"tx":{"bytes":20,"errors":3,"dropped":4}}}]`
	links, err := ParseIPLinks(out)
	if err != nil {
		t.Fatalf("ParseIPLinks: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("links = %d, want 1", len(links))
	}
	want := IPLink{Name: "eth2", State: "UP", MTU: 1500, RXBytes: 10, TXBytes: 20, RXErrors: 1, TXErrors: 3, RXDropped: 2, TXDropped: 4}
	if links[0] != want {
		t.Errorf("link = %+v, want %+v", links[0], want)
	}
}

func TestParseIPLinkWithoutStatsOrOperstate(t *testing.T) {
	out := `[{"ifname":"tun9","flags":["POINTOPOINT","NOARP"]}]`
	links, err := ParseIPLinks(out)
	if err != nil {
		t.Fatalf("ParseIPLinks: %v", err)
	}
	if len(links) != 1 || links[0].State != "DOWN" {
		t.Fatalf("links = %+v, want one DOWN interface", links)
	}
	if links[0].MTU != 0 || links[0].CounterWarnings() {
		t.Errorf("link = %+v, want zero mtu and counters", links[0])
	}
}

func TestParseIPAddrGolden(t *testing.T) {
	addrs, err := ParseIPAddrs(readIPTestdata(t, "addr.json"))
	if err != nil {
		t.Fatalf("ParseIPAddrs: %v", err)
	}
	if got, want := len(addrs), 5; got != want {
		t.Fatalf("addrs = %d, want %d: %+v", got, want, addrs)
	}
	byName := make(map[string]IPAddr, len(addrs))
	for _, a := range addrs {
		byName[a.Name] = a
	}
	if got, want := strings.Join(byName["eth0"].Addrs, ","), "192.0.2.10/24,2001:db8::10/64"; got != want {
		t.Errorf("eth0 addrs = %q, want %q (IPv4 first)", got, want)
	}
	if got, want := byName["eth0"].Master, "br0"; got != want {
		t.Errorf("eth0 master = %q, want %q", got, want)
	}
	if got, want := strings.Join(byName["lo"].Addrs, ","), "127.0.0.1/8,::1/128"; got != want {
		t.Errorf("lo addrs = %q, want %q", got, want)
	}
	if got := byName["tun0"].Addrs; len(got) != 1 || got[0] != "198.51.100.7/32" {
		t.Errorf("tun0 addrs = %v", got)
	}
	if got := byName["eth1"].Addrs; len(got) != 0 {
		t.Errorf("eth1 addrs = %v, want none", got)
	}
}

func TestParseIPRouteGolden(t *testing.T) {
	routes, err := ParseIPRoute(readIPTestdata(t, "route.json"))
	if err != nil {
		t.Fatalf("ParseIPRoute: %v", err)
	}
	if got, want := len(routes), 4; got != want {
		t.Fatalf("routes = %d, want %d: %+v", got, want, routes)
	}
	byDst := make(map[string]IPRoute, len(routes))
	for _, r := range routes {
		byDst[r.Dst] = r
	}
	def := byDst["default"]
	if def.Gateway != "192.0.2.1" || def.Dev != "eth0" || def.Proto != "static" || def.Metric != 100 || def.Src != "192.0.2.10" {
		t.Errorf("default route = %+v", def)
	}
	if !def.Default() {
		t.Errorf("default route not recognized")
	}
	if lan := byDst["192.0.2.0/24"]; lan.Gateway != "" || lan.Proto != "kernel" || lan.Metric != 0 {
		t.Errorf("kernel route = %+v", lan)
	}
}

func TestParseIPRouteV6Golden(t *testing.T) {
	routes, err := ParseIPRoute6(readIPTestdata(t, "route6.json"))
	if err != nil {
		t.Fatalf("ParseIPRoute6: %v", err)
	}
	if got, want := len(routes), 2; got != want {
		t.Fatalf("routes = %d, want %d: %+v", got, want, routes)
	}
	if r := routes[1]; r.Dst != "default" || r.Gateway != "fe80::1" || r.Proto != "ra" || r.Metric != 1024 {
		t.Errorf("v6 default = %+v", r)
	}
}

func TestParseIPRouteDefaultsFirst(t *testing.T) {
	routes, err := ParseIPRoute(readIPTestdata(t, "route.json"))
	if err != nil {
		t.Fatalf("ParseIPRoute: %v", err)
	}
	if routes[0].Default() {
		t.Fatal("fixture should not start with the default route")
	}
	SortRoutes(routes)
	if !routes[0].Default() {
		t.Errorf("routes[0] = %+v, want the default route", routes[0])
	}
	// Stable: the rest keeps the kernel order.
	want := []string{"default", "192.0.2.0/24", "198.51.100.0/24", "10.0.0.0/8"}
	for i, r := range routes {
		if r.Dst != want[i] {
			t.Errorf("routes[%d].Dst = %q, want %q", i, r.Dst, want[i])
		}
	}
}

func TestParseIPNeighGolden(t *testing.T) {
	neighs, err := ParseIPNeigh(readIPTestdata(t, "neigh.json"))
	if err != nil {
		t.Fatalf("ParseIPNeigh: %v", err)
	}
	if got, want := len(neighs), 4; got != want {
		t.Fatalf("neighs = %d, want %d: %+v", got, want, neighs)
	}
	first := neighs[0]
	if first.IP != "192.0.2.1" || first.MAC != "52:54:00:aa:bb:cc" || first.Dev != "eth0" || first.State != "REACHABLE" {
		t.Errorf("neighs[0] = %+v", first)
	}
	if first.Unresolved() {
		t.Errorf("REACHABLE marked unresolved")
	}
	if n := neighs[2]; n.State != "FAILED" || !n.Unresolved() || n.MAC != "" {
		t.Errorf("failed entry = %+v", n)
	}
	if n := neighs[3]; n.State != "INCOMPLETE" || !n.Unresolved() {
		t.Errorf("incomplete entry = %+v", n)
	}
}

func TestParseIPNeighStringState(t *testing.T) {
	out := `[{"dst":"203.0.113.9","dev":"eth0","lladdr":"52:54:00:aa:bb:ee","state":"STALE"}]`
	neighs, err := ParseIPNeigh(out)
	if err != nil {
		t.Fatalf("ParseIPNeigh: %v", err)
	}
	if len(neighs) != 1 || neighs[0].State != "STALE" || neighs[0].Unresolved() {
		t.Errorf("neighs = %+v", neighs)
	}
}

func TestIPParsersEmpty(t *testing.T) {
	empty := []string{"", "   \n", "[]", "null"}
	for _, out := range empty {
		links, err := ParseIPLinks(out)
		if err != nil || len(links) != 0 {
			t.Errorf("ParseIPLinks(%q) = %+v, %v; want none", out, links, err)
		}
		addrs, err := ParseIPAddrs(out)
		if err != nil || len(addrs) != 0 {
			t.Errorf("ParseIPAddrs(%q) = %+v, %v; want none", out, addrs, err)
		}
		routes, err := ParseIPRoute(out)
		if err != nil || len(routes) != 0 {
			t.Errorf("ParseIPRoute(%q) = %+v, %v; want none", out, routes, err)
		}
		neighs, err := ParseIPNeigh(out)
		if err != nil || len(neighs) != 0 {
			t.Errorf("ParseIPNeigh(%q) = %+v, %v; want none", out, neighs, err)
		}
	}
}

func TestIPParsersGarbageDoesNotPanic(t *testing.T) {
	garbage := []string{"not json", "{", `{"ifname":"eth0"}`, `[{"ifname":`}
	for _, out := range garbage {
		if _, err := ParseIPLinks(out); errs.KindOf(err) != errs.Parse {
			t.Errorf("ParseIPLinks(%q) kind = %s, want parse", out, errs.KindOf(err))
		}
		if _, err := ParseIPAddrs(out); errs.KindOf(err) != errs.Parse {
			t.Errorf("ParseIPAddrs(%q) kind = %s, want parse", out, errs.KindOf(err))
		}
		if _, err := ParseIPRoute(out); errs.KindOf(err) != errs.Parse {
			t.Errorf("ParseIPRoute(%q) kind = %s, want parse", out, errs.KindOf(err))
		}
		if _, err := ParseIPNeigh(out); errs.KindOf(err) != errs.Parse {
			t.Errorf("ParseIPNeigh(%q) kind = %s, want parse", out, errs.KindOf(err))
		}
	}
}

// netSections assembles a successful section map from the fixtures.
func netSections(t *testing.T, ids map[string]string) map[string]probe.SectionResult {
	t.Helper()
	out := make(map[string]probe.SectionResult, len(ids))
	for id, name := range ids {
		out[id] = probe.SectionResult{ID: id, Stdout: readIPTestdata(t, name)}
	}
	return out
}

func TestInterfaceRows(t *testing.T) {
	b := newTestBackend(t, transporttest.New(), inv("web-01"))
	sec := netSections(t, map[string]string{
		secIPLink: "link.json",
		secIPAddr: "addr.json",
	})
	rows, err := b.rows(KindInterface, "web-01", resource.Query{Kind: KindInterface}, sec, nil, nil)
	if err != nil {
		t.Fatalf("interfaceRows: %v", err)
	}
	if got, want := len(rows), 5; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}
	byName := make(map[string]resource.Resource, len(rows))
	for _, r := range rows {
		byName[r.Ref.Name] = r
	}
	eth0 := byName["eth0"]
	if eth0.Ref.Kind != KindInterface || eth0.Ref.Host != "web-01" || eth0.Ref.Backend != "linux" {
		t.Errorf("eth0 ref = %+v", eth0.Ref)
	}
	if got, want := eth0.Fields["addrs"], "192.0.2.10/24,2001:db8::10/64"; got != want {
		t.Errorf("eth0 addrs = %v, want %q", got, want)
	}
	if got, want := eth0.Fields["state"], "UP"; got != want {
		t.Errorf("eth0 state = %v, want %q", got, want)
	}
	if got, want := eth0.Fields["master"], "br0"; got != want {
		t.Errorf("eth0 master = %v, want %q", got, want)
	}
	if got, want := eth0.Fields["rx"], int64(123456789); got != want {
		t.Errorf("eth0 rx = %v, want %v", got, want)
	}
	if eth0.Status != resource.StatusOK {
		t.Errorf("eth0 status = %v, want ok", eth0.Status)
	}
	if got := byName["eth1"].Status; got != resource.StatusWarn {
		t.Errorf("eth1 status = %v, want warn (non-zero error counters)", got)
	}
	if got := byName["tun0"].Status; got != resource.StatusWarn {
		t.Errorf("tun0 status = %v, want warn (DOWN with an address)", got)
	}
	if got := byName["eth1"].Fields["addrs"]; got != "" {
		t.Errorf("eth1 addrs = %v, want empty", got)
	}
}

func TestInterfaceRowsAddrSectionFailed(t *testing.T) {
	b := newTestBackend(t, transporttest.New(), inv("web-01"))
	sec := netSections(t, map[string]string{secIPLink: "link.json"})
	bad := map[string]error{secIPAddr: errs.New(errs.Remote, secIPAddr, "exit 1")}
	rows, err := b.rows(KindInterface, "web-01", resource.Query{Kind: KindInterface}, sec, bad, nil)
	if err != nil {
		t.Fatalf("interfaceRows: %v", err)
	}
	if got, want := len(rows), 5; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}
	if _, ok := rows[0].Fields["addrs"]; ok {
		t.Errorf("addrs present although the address section failed")
	}
	if got := rows[0].Labels["_degraded"]; got != "ip-addr: remote" {
		t.Errorf("degraded label = %q, want %q", got, "ip-addr: remote")
	}
}

func TestInterfaceRowsLinkSectionFailed(t *testing.T) {
	b := newTestBackend(t, transporttest.New(), inv("web-01"))
	bad := map[string]error{secIPLink: errs.New(errs.Remote, secIPLink, "exit 1")}
	if _, err := b.rows(KindInterface, "web-01", resource.Query{Kind: KindInterface}, nil, bad, nil); err == nil {
		t.Fatal("expected the host to fail when the link section fails")
	}
}

func TestRouteRows(t *testing.T) {
	b := newTestBackend(t, transporttest.New(), inv("web-01"))
	sec := netSections(t, map[string]string{
		secIPRoute:  "route.json",
		secIPRoute6: "route6.json",
	})
	rows, err := b.rows(KindRoute, "web-01", resource.Query{Kind: KindRoute}, sec, nil, nil)
	if err != nil {
		t.Fatalf("routeRows: %v", err)
	}
	if got, want := len(rows), 6; got != want {
		t.Fatalf("rows = %d, want %d: %+v", got, want, rows)
	}
	if got, want := rows[0].Fields["dst"], "default"; got != want {
		t.Errorf("rows[0].dst = %v, want %q", got, want)
	}
	if got := rows[0].Ref.Name; got != "default via 192.0.2.1 dev eth0" {
		t.Errorf("default ref name = %q", got)
	}
	if got, want := rows[0].Fields["metric"], int64(100); got != want {
		t.Errorf("default metric = %v, want %v", got, want)
	}
	if got, want := rows[0].Fields["src"], "192.0.2.10"; got != want {
		t.Errorf("default src = %v, want %q", got, want)
	}
	if got, want := rows[0].Fields["proto"], "static"; got != want {
		t.Errorf("default proto = %v, want %q", got, want)
	}
	// The v6 default sorts next to the v4 default, both before the rest.
	if got, want := rows[1].Fields["dst"], "default"; got != want {
		t.Errorf("rows[1].dst = %v, want the v6 default", got)
	}
	if got, want := rows[1].Fields["gateway"], "fe80::1"; got != want {
		t.Errorf("v6 gateway = %v, want %q", got, want)
	}
	seen := make(map[string]bool, len(rows))
	for _, r := range rows {
		if seen[r.Ref.Name] {
			t.Errorf("duplicate ref name %q", r.Ref.Name)
		}
		seen[r.Ref.Name] = true
	}
}

func TestRouteRowsV6SectionFailed(t *testing.T) {
	b := newTestBackend(t, transporttest.New(), inv("web-01"))
	sec := netSections(t, map[string]string{secIPRoute: "route.json"})
	bad := map[string]error{secIPRoute6: errs.New(errs.Remote, secIPRoute6, "exit 1")}
	rows, err := b.rows(KindRoute, "web-01", resource.Query{Kind: KindRoute}, sec, bad, nil)
	if err != nil {
		t.Fatalf("routeRows: %v", err)
	}
	if got, want := len(rows), 4; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}
	if got := rows[0].Labels["_degraded"]; got != "ip-route6: remote" {
		t.Errorf("degraded label = %q", got)
	}
}

func TestNeighRows(t *testing.T) {
	b := newTestBackend(t, transporttest.New(), inv("web-01"))
	sec := netSections(t, map[string]string{secIPNeigh: "neigh.json"})
	rows, err := b.rows(KindNeigh, "web-01", resource.Query{Kind: KindNeigh}, sec, nil, nil)
	if err != nil {
		t.Fatalf("neighRows: %v", err)
	}
	if got, want := len(rows), 4; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}
	if got, want := rows[0].Ref.Name, "192.0.2.1 dev eth0"; got != want {
		t.Errorf("ref name = %q, want %q", got, want)
	}
	if rows[0].Status != resource.StatusOK {
		t.Errorf("reachable status = %v, want ok", rows[0].Status)
	}
	if got, want := rows[0].Fields["mac"], "52:54:00:aa:bb:cc"; got != want {
		t.Errorf("mac = %v, want %q", got, want)
	}
	for _, i := range []int{2, 3} {
		if got := rows[i].Status; got != resource.StatusWarn {
			t.Errorf("rows[%d] status = %v, want warn", i, got)
		}
	}
}

func TestNetworkKindSpecs(t *testing.T) {
	b := New(Options{})
	specs := make(map[resource.KindName]resource.KindSpec)
	for _, s := range b.Kinds() {
		specs[s.Name] = s
	}
	cases := []struct {
		kind    resource.KindName
		columns []string
	}{
		{KindInterface, []string{"name", "state", "mtu", "addrs", "rx", "tx", "rx_err", "tx_err", "rx_drop", "tx_drop", "master"}},
		{KindRoute, []string{"dst", "gateway", "dev", "proto", "metric", "src"}},
		{KindNeigh, []string{"ip", "mac", "dev", "state"}},
	}
	for _, tc := range cases {
		spec, ok := specs[tc.kind]
		if !ok {
			t.Fatalf("%s kind not registered", tc.kind)
		}
		if spec.Scope != resource.ScopeHost {
			t.Errorf("%s scope = %v, want host", tc.kind, spec.Scope)
		}
		ids := make([]string, len(spec.Columns))
		for i, c := range spec.Columns {
			ids[i] = c.ID
		}
		if strings.Join(ids, ",") != strings.Join(tc.columns, ",") {
			t.Errorf("%s columns = %v, want %v", tc.kind, ids, tc.columns)
		}
	}

	aliases := map[string]resource.KindName{
		"interface": KindInterface, "if": KindInterface, "ifaces": KindInterface, "ip": KindInterface,
		"route": KindRoute, "routes": KindRoute,
		"neigh": KindNeigh, "arp": KindNeigh,
	}
	for alias, want := range aliases {
		got, ok := normalizeKind(resource.KindName(alias))
		if !ok || got != want {
			t.Errorf("normalizeKind(%q) = %q, %v; want %q", alias, got, ok, want)
		}
	}
}

func TestBackendListInterfacesOneRoundTrip(t *testing.T) {
	ex := transporttest.New()
	b := newTestBackend(t, ex, inv("web-01"))
	batch := batchOf(t, b, KindInterface)
	ex.On("", scriptReply(batch, map[string]string{
		secIPLink: readIPTestdata(t, "link.json"),
		secIPAddr: readIPTestdata(t, "addr.json"),
	}, nil, nil))

	ch, err := b.List(context.Background(), resource.Query{Kind: KindInterface})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var rows []resource.Resource
	for r := range ch {
		if r.Err != nil {
			t.Fatalf("host %s: %v", r.Host, r.Err)
		}
		rows = append(rows, r.Items...)
	}
	if got, want := len(rows), 5; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}
	if got, want := len(ex.Calls()), 1; got != want {
		t.Errorf("Exec calls = %d, want %d (one batch per host)", got, want)
	}
	if !strings.Contains(ex.Calls()[0].Command.Script, "ip -j -s link") {
		t.Errorf("not the interface batch: %q", ex.Calls()[0].Command.Script)
	}
}
