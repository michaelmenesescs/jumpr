package inventory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sync"
	"testing"
	"time"

	sshcfg "jumper/internal/transport/ssh"

	"jumper/internal/resource"
)

// fakeSource is a Source whose result a test can change between refreshes.
type fakeSource struct {
	name string

	mu    sync.Mutex
	hosts []Host
	err   error
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) Load(ctx context.Context) ([]Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	return cloneHosts(f.hosts), nil
}

func (f *fakeSource) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func testRewrites() []Rewrite {
	return []Rewrite{
		{From: regexp.MustCompile(`:9100$`), To: ""},
		{From: regexp.MustCompile(`^ip-(\d+)-(\d+)-(\d+)-(\d+)$`), To: "$1.$2.$3.$4"},
	}
}

func TestCanonicalRewrites(t *testing.T) {
	rw := testRewrites()
	cases := map[string]string{
		"web-01:9100":                     "web-01",
		"ip-10-1-2-3":                     "10.1.2.3",
		"IP-10-1-2-3:9100":                "10.1.2.3",
		"  Web-01.Lab.Example.Internal. ": "web-01.lab.example.internal",
		"":                                "",
	}
	for in, want := range cases {
		if got := string(Canonical(in, rw)); got != want {
			t.Errorf("Canonical(%q) = %q, want %q", in, got, want)
		}
	}
	// Without rewrites the name is only normalised.
	if got := string(Canonical("WEB-01:9100", nil)); got != "web-01:9100" {
		t.Errorf("Canonical without rewrites = %q", got)
	}
}

func TestMergeByAliasAndShortName(t *testing.T) {
	inputs := []sourceHosts{
		{name: "ssh", hosts: []Host{{
			ID:      "web-01",
			Addr:    "10.0.0.1",
			Aliases: []string{"app-01"},
			Labels:  map[string]string{"user": "sre"},
		}}},
		{name: "k8s", hosts: []Host{{
			ID:     "app-01",
			Addr:   "10.0.0.2",
			Labels: map[string]string{"node": "app-01"},
		}}},
		{name: "salt", hosts: []Host{{
			ID:     "web-01.lab.example.internal",
			Labels: map[string]string{"role": "web"},
		}}},
	}
	got := merge(inputs, nil, time.Now())
	if len(got) != 1 {
		t.Fatalf("merged into %d hosts, want 1: %+v", len(got), got)
	}
	h := got[0]
	if want := resource.HostID("web-01.lab.example.internal"); h.ID != want {
		t.Errorf("ID = %q, want %q", h.ID, want)
	}
	if h.Addr != "10.0.0.1" {
		t.Errorf("Addr = %q, want the first source's 10.0.0.1", h.Addr)
	}
	if want := []string{"app-01"}; !reflect.DeepEqual(h.Aliases, want) {
		t.Errorf("Aliases = %v, want %v", h.Aliases, want)
	}
	if want := []string{"k8s", "salt", "ssh"}; !reflect.DeepEqual(h.Sources, want) {
		t.Errorf("Sources = %v, want %v", h.Sources, want)
	}
}

func TestLabelsNamespacedAndShortcuts(t *testing.T) {
	inputs := []sourceHosts{
		{name: "ssh", hosts: []Host{{
			ID:     "db-01.lab.example.internal",
			Labels: map[string]string{"role": "db", "user": "sre"},
		}}},
		{name: "static", hosts: []Host{{
			ID:     "db-01.lab.example.internal",
			Labels: map[string]string{"role": "db", "dc": "lab1"},
		}}},
		{name: "web", hosts: []Host{{
			ID:     "web-01.lab.example.internal",
			Labels: map[string]string{"role": "web"},
		}}},
		{name: "prom", hosts: []Host{{
			ID:     "web-01.lab.example.internal",
			Labels: map[string]string{"role": "web-cache"},
		}}},
	}
	got := merge(inputs, nil, time.Now())
	byID := map[resource.HostID]Host{}
	for _, h := range got {
		byID[h.ID] = h
	}
	db, ok := byID["db-01.lab.example.internal"]
	if !ok {
		t.Fatalf("missing db host: %+v", got)
	}
	if db.Labels["ssh.role"] != "db" || db.Labels["static.role"] != "db" {
		t.Errorf("namespaced labels wrong: %v", db.Labels)
	}
	if db.Labels["role"] != "db" {
		t.Errorf("unanimous role shortcut missing: %v", db.Labels)
	}
	if db.Labels["dc"] != "lab1" || db.Labels["user"] != "sre" {
		t.Errorf("single-source shortcuts missing: %v", db.Labels)
	}
	if db.Labels["host"] != string(db.ID) {
		t.Errorf("host label = %q, want %q", db.Labels["host"], db.ID)
	}

	web, ok := byID["web-01.lab.example.internal"]
	if !ok {
		t.Fatalf("missing web host: %+v", got)
	}
	if _, found := web.Labels["role"]; found {
		t.Errorf("conflicting role values must not set an unprefixed key: %v", web.Labels)
	}
	if web.Labels["web.role"] != "web" || web.Labels["prom.role"] != "web-cache" {
		t.Errorf("namespaced conflicting labels wrong: %v", web.Labels)
	}
}

func TestSelect(t *testing.T) {
	src := &fakeSource{name: "static", hosts: []Host{
		{ID: "web-01.lab.example.internal", Labels: map[string]string{"role": "web"}},
		{ID: "web-02.lab.example.internal", Labels: map[string]string{"role": "web"}},
		{ID: "db-01.lab.example.internal", Labels: map[string]string{"role": "db"}},
	}}
	store := New([]Source{src}, nil, "", nil)
	if failed := store.Refresh(context.Background()); len(failed) != 0 {
		t.Fatalf("Refresh: %v", failed)
	}
	sel, err := resource.ParseSelector("role=web")
	if err != nil {
		t.Fatalf("ParseSelector: %v", err)
	}
	got := store.Select(sel)
	if len(got) != 2 {
		t.Fatalf("Select(role=web) = %d hosts, want 2: %+v", len(got), got)
	}
	for _, h := range got {
		if h.Labels["role"] != "web" {
			t.Errorf("selected %q with role %q", h.ID, h.Labels["role"])
		}
	}
	// Hosts returns copies: mutating one must not touch the store.
	got[0].Labels["role"] = "mutated"
	want := resource.HostID("web-01.lab.example.internal")
	if h, ok := store.Resolve(string(want)); !ok || h.Labels["role"] != "web" {
		t.Errorf("store mutated through a returned Host: %+v", h)
	}
}

func TestResolveAmbiguousShortName(t *testing.T) {
	src := &fakeSource{name: "static", hosts: []Host{
		{ID: "db-01.ewr.example.internal"},
		{ID: "db-01.lax.example.internal"},
		{ID: "web-01.lab.example.internal", Aliases: []string{"web-01-alias"}},
	}}
	store := New([]Source{src}, nil, "", nil)
	if failed := store.Refresh(context.Background()); len(failed) != 0 {
		t.Fatalf("Refresh: %v", failed)
	}
	if _, ok := store.Resolve("db-01"); ok {
		t.Error("ambiguous short name resolved; want not found")
	}
	if h, ok := store.Resolve("DB-01.EWR.EXAMPLE.INTERNAL"); !ok || h.ID != "db-01.ewr.example.internal" {
		t.Errorf("Resolve by ID = %+v, %v", h, ok)
	}
	if h, ok := store.Resolve("web-01-alias"); !ok || h.ID != "web-01.lab.example.internal" {
		t.Errorf("Resolve by alias = %+v, %v", h, ok)
	}
	if h, ok := store.Resolve("web-01"); !ok || h.ID != "web-01.lab.example.internal" {
		t.Errorf("Resolve by short name = %+v, %v", h, ok)
	}
	if _, ok := store.Resolve("nope"); ok {
		t.Error("unknown name resolved")
	}
}

func TestRefreshKeepsFailedSource(t *testing.T) {
	a := &fakeSource{name: "ssh", hosts: []Host{{ID: "web-01.lab.example.internal"}}}
	b := &fakeSource{name: "salt", hosts: []Host{{ID: "db-01.lab.example.internal"}}}
	store := New([]Source{a, b}, nil, "", nil)

	if failed := store.Refresh(context.Background()); len(failed) != 0 {
		t.Fatalf("first Refresh: %v", failed)
	}
	if n := len(store.Hosts()); n != 2 {
		t.Fatalf("hosts after first refresh = %d, want 2", n)
	}

	b.setErr(errors.New("salt api down"))
	failed := store.Refresh(context.Background())
	if err, ok := failed["salt"]; !ok || err == nil {
		t.Fatalf("Refresh did not report the salt failure: %v", failed)
	}
	if _, ok := failed["ssh"]; ok {
		t.Errorf("Refresh reported a healthy source as failed: %v", failed)
	}
	ids := map[resource.HostID]bool{}
	for _, h := range store.Hosts() {
		ids[h.ID] = true
	}
	if !ids["db-01.lab.example.internal"] {
		t.Error("previous hosts of the failed source were dropped")
	}
	if !ids["web-01.lab.example.internal"] {
		t.Error("healthy source's hosts are missing")
	}
}

func TestCacheRoundTrip(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	path := filepath.Join(t.TempDir(), "inventory.json")
	src := &fakeSource{name: "static", hosts: []Host{
		{ID: "web-01.lab.example.internal", Addr: "10.0.0.1", Aliases: []string{"web-01"},
			Labels: map[string]string{"role": "web"}},
	}}
	first := New([]Source{src}, nil, path, clock)
	if failed := first.Refresh(context.Background()); len(failed) != 0 {
		t.Fatalf("Refresh: %v", failed)
	}

	cached := New(nil, nil, path, clock)
	if err := cached.LoadCache(); err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if want, got := first.Hosts(), cached.Hosts(); !reflect.DeepEqual(got, want) {
		t.Errorf("cached hosts = %+v, want %+v", got, want)
	}
	if d := cached.Age(); d != 0 {
		t.Errorf("Age after cache load = %v, want 0", d)
	}

	empty := New(nil, nil, filepath.Join(t.TempDir(), "missing.json"), clock)
	if err := empty.LoadCache(); err != nil {
		t.Errorf("missing cache file must not error: %v", err)
	}
	if n := len(empty.Hosts()); n != 0 {
		t.Errorf("hosts after missing cache = %d, want 0", n)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestSSHConfigSource(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config")
	writeFile(t, cfgPath, `
Host web-01
    HostName web-01.lab.example.internal
    User sre
    ProxyJump bastion-01

Host bastion-01
    HostName bastion-01.lab.example.internal

Host *
    User nobody
`)
	r, err := sshcfg.NewResolver([]string{cfgPath}, dir, "default")
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	src := NewSSHConfigSource(r)
	src.ExcludePatterns = []string{"bastion*"}
	hosts, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("got %d hosts, want 1: %+v", len(hosts), hosts)
	}
	h := hosts[0]
	if h.ID != "web-01" || h.Addr != "web-01" {
		t.Errorf("ID/Addr = %q/%q, want web-01/web-01", h.ID, h.Addr)
	}
	if want := []string{"web-01.lab.example.internal"}; !reflect.DeepEqual(h.Aliases, want) {
		t.Errorf("Aliases = %v, want %v", h.Aliases, want)
	}
	if h.Labels["user"] != "sre" {
		t.Errorf("user label = %q, want sre", h.Labels["user"])
	}
	if h.Labels["jump"] != "bastion-01" {
		t.Errorf("jump label = %q, want bastion-01", h.Labels["jump"])
	}
	if src.Name() != "ssh" {
		t.Errorf("Name = %q, want ssh", src.Name())
	}
}

func TestStaticSourceYAMLAndPlain(t *testing.T) {
	src := NewStaticSource(filepath.Join("testdata", "hosts.yaml"))
	hosts, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load YAML: %v", err)
	}
	if src.Name() != "static" {
		t.Errorf("Name = %q, want static", src.Name())
	}
	if len(hosts) != 3 {
		t.Fatalf("got %d hosts, want 3: %+v", len(hosts), hosts)
	}
	byID := map[resource.HostID]Host{}
	for _, h := range hosts {
		byID[h.ID] = h
	}
	web, ok := byID["web-01.lab.example.internal"]
	if !ok {
		t.Fatalf("missing web host: %+v", hosts)
	}
	if web.Addr != "web-01.lab.example.internal" {
		t.Errorf("Addr = %q", web.Addr)
	}
	if !reflect.DeepEqual(web.Aliases, []string{"web-01"}) {
		t.Errorf("Aliases = %v", web.Aliases)
	}
	if web.Labels["role"] != "web" || web.Labels["dc"] != "lab1" {
		t.Errorf("Labels = %v", web.Labels)
	}
	if _, ok := byID["bare-01"]; !ok {
		t.Errorf("missing bare host: %+v", hosts)
	}

	plainPath := filepath.Join(t.TempDir(), "hosts.txt")
	writeFile(t, plainPath, "# lab hosts\n\nweb-02.lab.example.internal\n  DB-02  # short name\n")
	plain, err := NewStaticSource(plainPath).Load(context.Background())
	if err != nil {
		t.Fatalf("Load plain: %v", err)
	}
	if len(plain) != 2 {
		t.Fatalf("plain got %d hosts, want 2: %+v", len(plain), plain)
	}
	if plain[0].ID != "web-02.lab.example.internal" || plain[1].ID != "db-02" {
		t.Errorf("plain IDs = %q, %q", plain[0].ID, plain[1].ID)
	}
}
