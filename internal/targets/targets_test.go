package targets

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/inventory"
	"jumpr/internal/resource"
)

// testHost is one host of the stub inventory.
type testHost struct {
	id     resource.HostID
	labels map[string]string
}

// stubInventory is a fixed inventory for the resolver tests.
type stubInventory struct {
	hosts []inventory.Host
}

func newStubInventory(specs ...testHost) *stubInventory {
	out := make([]inventory.Host, 0, len(specs))
	for _, s := range specs {
		labels := map[string]string{"host": string(s.id)}
		for k, v := range s.labels {
			labels[k] = v
		}
		out = append(out, inventory.Host{ID: s.id, Addr: string(s.id), Labels: labels})
	}
	return &stubInventory{hosts: out}
}

func (s *stubInventory) Hosts() []inventory.Host { return s.hosts }

func (s *stubInventory) Select(sel resource.Selector) []inventory.Host {
	out := make([]inventory.Host, 0, len(s.hosts))
	for _, h := range s.hosts {
		if sel.Matches(h.Labels) {
			out = append(out, h)
		}
	}
	return out
}

func (s *stubInventory) Resolve(name string) (inventory.Host, bool) {
	for _, h := range s.hosts {
		if string(h.ID) == name {
			return h, true
		}
	}
	return inventory.Host{}, false
}

func (s *stubInventory) Refresh(context.Context) map[string]error { return nil }

func (s *stubInventory) Age() time.Duration { return 0 }

// fleet is the fixed inventory every resolver test runs against.
func fleet() *stubInventory {
	return newStubInventory(
		testHost{id: "web-ewr-001", labels: map[string]string{"role": "web", "dc": "ewr"}},
		testHost{id: "web-ewr-002", labels: map[string]string{"role": "web", "dc": "ewr"}},
		testHost{id: "db-ewr-001", labels: map[string]string{"role": "db", "dc": "ewr"}},
		testHost{id: "web-lax-001", labels: map[string]string{"role": "web", "dc": "lax"}},
		testHost{id: "cache-lax-001", labels: map[string]string{"role": "cache", "dc": "lax"}},
	)
}

// mustSelector parses a selector or fails the test.
func mustSelector(t *testing.T, s string) resource.Selector {
	t.Helper()
	sel, err := resource.ParseSelector(s)
	if err != nil {
		t.Fatalf("parse selector %q: %v", s, err)
	}
	return sel
}

// hostIDs renders a host list as comma-separated text for comparison.
func hostIDs(hosts []resource.HostID) string {
	out := make([]string, len(hosts))
	for i, h := range hosts {
		out[i] = string(h)
	}
	return strings.Join(out, ",")
}

func TestResolveSelector(t *testing.T) {
	cases := []struct {
		selector string
		want     string
	}{
		{"role=web", "web-ewr-001,web-ewr-002,web-lax-001"},
		{"dc=ewr", "db-ewr-001,web-ewr-001,web-ewr-002"},
		{"role=web dc=lax", "web-lax-001"},
		{"", "cache-lax-001,db-ewr-001,web-ewr-001,web-ewr-002,web-lax-001"},
	}
	for _, tc := range cases {
		t.Run(tc.selector, func(t *testing.T) {
			set, err := Resolve(fleet(), nil, mustSelector(t, tc.selector), nil)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tc.selector, err)
			}
			if got := hostIDs(set.Hosts); got != tc.want {
				t.Fatalf("Resolve(%q) hosts = %s, want %s", tc.selector, got, tc.want)
			}
			if tc.selector != "" && set.Label != tc.selector {
				t.Fatalf("Label = %q, want %q", set.Label, tc.selector)
			}
		})
	}
}

func TestResolveSetUnion(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Save("canary", []resource.HostID{"web-ewr-002", "web-lax-001"}); err != nil {
		t.Fatalf("save canary: %v", err)
	}
	if err := store.Save("storage", []resource.HostID{"db-ewr-001"}); err != nil {
		t.Fatalf("save storage: %v", err)
	}

	// No terms: the sets are unioned with nothing else.
	set, err := Resolve(fleet(), store, mustSelector(t, "@canary @storage"), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := "db-ewr-001,web-ewr-002,web-lax-001"
	if got := hostIDs(set.Hosts); got != want {
		t.Fatalf("union hosts = %s, want %s", got, want)
	}

	// A single set keeps its label as the selector text.
	set, err = Resolve(fleet(), store, mustSelector(t, "@canary"), nil)
	if err != nil {
		t.Fatalf("Resolve single: %v", err)
	}
	if set.Label != "@canary" {
		t.Fatalf("Label = %q, want @canary", set.Label)
	}
}

func TestResolveSetIntersect(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Save("canary", []resource.HostID{"web-ewr-002", "db-ewr-001"}); err != nil {
		t.Fatalf("save canary: %v", err)
	}

	set, err := Resolve(fleet(), store, mustSelector(t, "role=web @canary"), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, want := hostIDs(set.Hosts), "web-ewr-002"; got != want {
		t.Fatalf("intersection hosts = %s, want %s", got, want)
	}
}

func TestResolveInvestigation(t *testing.T) {
	set, err := Resolve(fleet(), nil, mustSelector(t, "@investigation"),
		[]resource.HostID{"Web-EWR-002", "db-ewr-001", "web-ewr-002"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, want := hostIDs(set.Hosts), "db-ewr-001,web-ewr-002"; got != want {
		t.Fatalf("investigation hosts = %s, want %s", got, want)
	}
}

func TestUnknownSet(t *testing.T) {
	store := NewStore(t.TempDir())
	_, err := Resolve(fleet(), store, mustSelector(t, "@nope"), nil)
	if err == nil {
		t.Fatal("Resolve of an unknown set returned no error")
	}
	if got := errs.KindOf(err); got != errs.Config {
		t.Fatalf("kind = %q, want %q", got, errs.Config)
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error %q does not name the set", err)
	}

	// A nil store resolves no named set either.
	if _, err := Resolve(fleet(), nil, mustSelector(t, "@nope"), nil); err == nil {
		t.Fatal("Resolve with no store returned no error")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	store.now = func() time.Time { return time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC) }

	hosts := []resource.HostID{"web-ewr-002", "db-ewr-001", "web-ewr-002", "Web-LAX-001"}
	if err := store.Save("canary", hosts); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(root, "sets", "canary.txt"))
	if err != nil {
		t.Fatalf("read set file: %v", err)
	}
	text := string(raw)
	if !strings.HasPrefix(text, "#") {
		t.Fatalf("set file has no header comment:\n%s", text)
	}
	if !strings.Contains(text, "2026-11-12") {
		t.Fatalf("set file header has no date:\n%s", text)
	}
	if strings.Contains(text, "Web-LAX-001") {
		t.Fatalf("set file is not normalized:\n%s", text)
	}

	got, err := store.Load("canary")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := "db-ewr-001,web-ewr-002,web-lax-001"; hostIDs(got) != want {
		t.Fatalf("Load = %s, want %s", hostIDs(got), want)
	}

	if names := store.List(); len(names) != 1 || names[0] != "canary" {
		t.Fatalf("List = %v, want [canary]", names)
	}

	// No temporary files are left behind by the atomic write.
	entries, err := os.ReadDir(filepath.Join(root, "sets"))
	if err != nil {
		t.Fatalf("read sets dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".set-") {
			t.Fatalf("temporary file %q left behind", e.Name())
		}
	}

	if err := store.Delete("canary"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if names := store.List(); len(names) != 0 {
		t.Fatalf("List after delete = %v, want none", names)
	}
	if _, err := store.Load("canary"); err == nil {
		t.Fatal("Load of a deleted set returned no error")
	}
}

func TestStoreRejectsBadName(t *testing.T) {
	store := NewStore(t.TempDir())
	bad := []string{"", "Canary", "canary/sets", "../escape", "-lead", "_lead", "with space", "canary.txt"}
	for _, name := range bad {
		if err := store.Save(name, []resource.HostID{"web-ewr-001"}); err == nil {
			t.Fatalf("Save(%q) returned no error", name)
		} else if got := errs.KindOf(err); got != errs.Config {
			t.Fatalf("Save(%q) kind = %q, want %q", name, got, errs.Config)
		}
		if _, err := store.Load(name); err == nil {
			t.Fatalf("Load(%q) returned no error", name)
		}
		if err := store.Delete(name); err == nil {
			t.Fatalf("Delete(%q) returned no error", name)
		}
	}
	if names := store.List(); len(names) != 0 {
		t.Fatalf("List = %v, want none", names)
	}
}
