package salt

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/inventory"
	sshcfg "jumpr/internal/transport/ssh"
	"jumpr/internal/transport/transporttest"
)

// grainsFrom decodes a JSON object into Grains, failing the test on bad input.
func grainsFrom(t *testing.T, s string) Grains {
	t.Helper()
	var g Grains
	if err := json.Unmarshal([]byte(s), &g); err != nil {
		t.Fatalf("grains fixture: %v", err)
	}
	return g
}

// cacheReply builds a `salt-run cache.grains` reply from minion → grains JSON.
func cacheReply(t *testing.T, byMinion map[string]string) transporttest.Reply {
	t.Helper()
	out := make(map[string]json.RawMessage, len(byMinion))
	for minion, g := range byMinion {
		if !json.Valid([]byte(g)) {
			t.Fatalf("invalid grains fixture for %s: %s", minion, g)
		}
		out[minion] = json.RawMessage(g)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal cache reply: %v", err)
	}
	return transporttest.Reply{Stdout: string(b) + "\n"}
}

func TestLabelsBuiltinsAndCustom(t *testing.T) {
	g := grainsFrom(t, `{
		"id": "web-01",
		"os": "Debian",
		"os_family": "Debian",
		"osrelease": "12",
		"osfinger": "Debian-12",
		"kernelrelease": "6.1.0-18-amd64",
		"saltversion": "3007.1",
		"virtual": "kvm",
		"num_cpus": 4,
		"mem_total": 16777216,
		"role": "web",
		"dc": "lab1",
		"path": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"ip_interfaces": {"eth0": ["10.0.0.5"]},
		"lsb_distrib_id": "Debian",
		"cpu_flags": ["fpu", "vme", "de"],
		"nested": {"a": 1}
	}`)
	got := Labels(g, nil)

	for k, want := range map[string]string{
		"id":            "web-01",
		"os":            "Debian",
		"os_family":     "Debian",
		"osrelease":     "12",
		"osfinger":      "Debian-12",
		"kernelrelease": "6.1.0-18-amd64",
		"saltversion":   "3007.1",
		"virtual":       "kvm",
		"num_cpus":      "4",
		"mem_total":     "16777216",
		"role":          "web",
		"dc":            "lab1",
	} {
		if got[k] != want {
			t.Errorf("label %q = %q, want %q (labels: %v)", k, got[k], want, got)
		}
	}
	for _, k := range []string{"path", "ip_interfaces", "lsb_distrib_id", "cpu_flags", "nested"} {
		if _, ok := got[k]; ok {
			t.Errorf("noisy grain %q became a label: %q", k, got[k])
		}
	}
}

func TestLabelsListGrainJoined(t *testing.T) {
	g := grainsFrom(t, `{"roles": ["web", "api"], "ports": [80, 443], "flags": [true, false]}`)
	got := Labels(g, nil)
	if want := "web,api"; got["roles"] != want {
		t.Errorf("roles = %q, want %q", got["roles"], want)
	}
	if want := "80,443"; got["ports"] != want {
		t.Errorf("ports = %q, want %q", got["ports"], want)
	}
	if want := "true,false"; got["flags"] != want {
		t.Errorf("flags = %q, want %q", got["flags"], want)
	}
	// Mixed lists are not labels.
	mixed := grainsFrom(t, `{"mixed": ["a", {"b": 1}]}`)
	if _, ok := Labels(mixed, nil)["mixed"]; ok {
		t.Error("list with a nested element became a label")
	}
}

func TestLabelsExtraAndLengthCap(t *testing.T) {
	long := strings.Repeat("x", maxLabelLen+1)
	g := grainsFrom(t, `{
		"role": "web",
		"uuid": "1234-5678",
		"note": "`+long+`",
		"fits": "`+strings.Repeat("y", maxLabelLen)+`"
	}`)
	got := Labels(g, nil)
	if _, ok := got["note"]; ok {
		t.Error("a value longer than the cap became a label")
	}
	if _, ok := got["uuid"]; ok {
		t.Error("denied grain uuid became a label")
	}
	if got["fits"] != strings.Repeat("y", maxLabelLen) {
		t.Error("a value exactly at the cap was dropped")
	}

	forced := Labels(g, []string{"uuid"})
	if forced["uuid"] != "1234-5678" {
		t.Errorf("extra key uuid = %q, want it exposed despite the denylist", forced["uuid"])
	}
	if forced["role"] != "web" {
		t.Errorf("extra labels changed role: %v", forced)
	}
}

func TestSourceBuildsHosts(t *testing.T) {
	f := transporttest.New().On("cache.grains", cacheReply(t, map[string]string{
		"web-01": `{
			"id": "web-01",
			"fqdn": "web-01.lab.example.internal",
			"host": "web-01",
			"ipv4": ["127.0.0.1", "10.0.0.5"],
			"role": "web",
			"dc": "lab1"
		}`,
		"db-02": `{
			"id": "db-02",
			"fqdn": "localhost.localdomain",
			"ipv4": ["10.0.0.9", "::1"],
			"role": "db"
		}`,
	}))
	src := NewSource(NewRunner(f, master, RunnerOptions{}))
	src.Now = func() time.Time { return time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC) }

	if src.Name() != "salt" {
		t.Errorf("Name = %q, want salt", src.Name())
	}
	hosts, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("got %d hosts, want 2: %+v", len(hosts), hosts)
	}
	byID := map[string]inventory.Host{}
	for _, h := range hosts {
		byID[string(h.ID)] = h
	}

	web, ok := byID["web-01.lab.example.internal"]
	if !ok {
		t.Fatalf("missing web host (fqdn should be the ID): %+v", hosts)
	}
	if web.Addr != "" {
		t.Errorf("Addr = %q, want empty (ssh_config or the FQDN decides)", web.Addr)
	}
	if want := []string{"10.0.0.5", "web-01"}; !reflect.DeepEqual(web.Aliases, want) {
		t.Errorf("Aliases = %v, want %v", web.Aliases, want)
	}
	if web.Labels["role"] != "web" || web.Labels["dc"] != "lab1" {
		t.Errorf("Labels = %v", web.Labels)
	}
	if web.SeenAt.IsZero() {
		t.Error("SeenAt is zero")
	}

	db, ok := byID["db-02"]
	if !ok {
		t.Fatalf("missing db host (localhost fqdn must fall back to the minion id): %+v", hosts)
	}
	if want := []string{"10.0.0.9"}; !reflect.DeepEqual(db.Aliases, want) {
		t.Errorf("db Aliases = %v, want %v", db.Aliases, want)
	}
	for _, a := range append(append([]string{}, web.Aliases...), db.Aliases...) {
		if strings.HasPrefix(a, "127.") || a == "::1" {
			t.Errorf("loopback %q is an alias", a)
		}
	}
}

func TestSourceEmptyCache(t *testing.T) {
	f := transporttest.New().On("cache.grains", transporttest.Reply{Stdout: "{}\n"})
	src := NewSource(NewRunner(f, master, RunnerOptions{}))

	hosts, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load on an empty cache returned an error: %v", err)
	}
	if len(hosts) != 0 {
		t.Errorf("got %d hosts, want 0: %+v", len(hosts), hosts)
	}
	if !strings.Contains(EmptyCacheHint, "grains cache is empty") {
		t.Errorf("EmptyCacheHint = %q", EmptyCacheHint)
	}
}

func TestCachedGrainsCommand(t *testing.T) {
	f := transporttest.New().On("cache.grains", transporttest.Reply{Stdout: `{"web-01": {"role": "web"}}` + "\n"})
	r := NewRunner(f, master, RunnerOptions{})

	all, err := r.CachedGrains(context.Background())
	if err != nil {
		t.Fatalf("CachedGrains: %v", err)
	}
	if len(all) != 1 || all["web-01"].Str("role") != "web" {
		t.Fatalf("grains = %+v", all)
	}
	calls := f.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	script := calls[0].Command.Script
	for _, want := range []string{"salt-run", "cache.grains", "tgt="} {
		if !strings.Contains(script, want) {
			t.Errorf("script %q does not contain %q", script, want)
		}
	}
	if !strings.Contains(script, "*") {
		t.Errorf("script %q does not pass the wildcard target", script)
	}
	if calls[0].Host != master {
		t.Errorf("host = %q, want %q", calls[0].Host, master)
	}
}

func TestCachedGrainsSkipsNonObjectsAndReportsParseErrors(t *testing.T) {
	f := transporttest.New().On("cache.grains", transporttest.Reply{
		Stdout: `{"web-01": {"role": "web"}, "web-02": false}` + "\n",
	})
	r := NewRunner(f, master, RunnerOptions{})
	all, err := r.CachedGrains(context.Background())
	if err != nil {
		t.Fatalf("CachedGrains: %v", err)
	}
	if len(all) != 1 || all["web-01"].Str("role") != "web" {
		t.Errorf("grains = %+v, want only web-01", all)
	}

	bad := transporttest.New().On("cache.grains", transporttest.Reply{Stdout: "not json\n"})
	if _, err := NewRunner(bad, master, RunnerOptions{}).CachedGrains(context.Background()); !errs.Is(err, errs.Parse) {
		t.Errorf("kind = %q, want parse (%v)", errs.KindOf(err), err)
	}
}

func TestKnownGrainKeys(t *testing.T) {
	all := map[string]Grains{
		"web-01": grainsFrom(t, `{"role": "web", "roles": ["web"], "nested": {"a": 1}, "mixed": ["a", {"b": 2}]}`),
		"web-02": grainsFrom(t, `{"dc": "lab1", "role": "web", "num_cpus": 8}`),
	}
	got := KnownGrainKeys(all)
	for _, k := range []string{"role", "roles", "dc", "num_cpus"} {
		if !got[k] {
			t.Errorf("known key %q missing: %v", k, got)
		}
	}
	for _, k := range []string{"nested", "mixed"} {
		if got[k] {
			t.Errorf("%q must not count as a known grain key", k)
		}
	}
	if n := len(KnownGrainKeys(nil)); n != 0 {
		t.Errorf("KnownGrainKeys(nil) has %d keys, want 0", n)
	}
}

// TestSourceMergesWithSSHConfig is the integration case: an ssh_config alias
// and a minion naming the same machine become one host with both label sets.
func TestSourceMergesWithSSHConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config")
	if err := os.WriteFile(cfgPath, []byte(`
Host web-01
    HostName web-01.lab.example.internal
    User sre
`), 0o600); err != nil {
		t.Fatalf("write ssh config: %v", err)
	}
	resolver, err := sshcfg.NewResolver([]string{cfgPath}, dir, "default")
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	f := transporttest.New().On("cache.grains", cacheReply(t, map[string]string{
		"web-01": `{
			"id": "web-01",
			"fqdn": "web-01.lab.example.internal",
			"role": "web",
			"dc": "lab1"
		}`,
	}))
	saltSrc := NewSource(NewRunner(f, master, RunnerOptions{}))

	store := inventory.New([]inventory.Source{
		inventory.NewSSHConfigSource(resolver),
		saltSrc,
	}, nil, "", nil)
	if failed := store.Refresh(context.Background()); len(failed) != 0 {
		t.Fatalf("Refresh: %v", failed)
	}
	hosts := store.Hosts()
	if len(hosts) != 1 {
		t.Fatalf("got %d hosts, want the two sources merged into 1: %+v", len(hosts), hosts)
	}
	h := hosts[0]
	if h.ID != "web-01.lab.example.internal" {
		t.Errorf("ID = %q, want web-01.lab.example.internal", h.ID)
	}
	if h.Labels["ssh.user"] != "sre" {
		t.Errorf("ssh.user = %q, want sre (labels: %v)", h.Labels["ssh.user"], h.Labels)
	}
	if h.Labels["salt.role"] != "web" {
		t.Errorf("salt.role = %q, want web (labels: %v)", h.Labels["salt.role"], h.Labels)
	}
	if h.Labels["role"] != "web" {
		t.Errorf("unprefixed role = %q, want web (labels: %v)", h.Labels["role"], h.Labels)
	}
}
