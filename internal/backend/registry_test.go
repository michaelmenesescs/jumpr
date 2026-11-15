package backend

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
)

// stubBackend is a minimal Backend for registry tests.
type stubBackend struct {
	name  string
	kinds []resource.KindSpec
}

func (s *stubBackend) Name() string                                                { return s.name }
func (s *stubBackend) Kinds() []resource.KindSpec                                  { return s.kinds }
func (s *stubBackend) List(context.Context, resource.Query) (<-chan Result, error) { return nil, nil }
func (s *stubBackend) Get(context.Context, resource.Ref) (resource.Resource, error) {
	return resource.Resource{}, nil
}
func (s *stubBackend) Links(resource.Resource) []resource.Link { return nil }
func (s *stubBackend) Health(context.Context) Health           { return Health{State: HealthOK} }

var _ Backend = (*stubBackend)(nil)

func spec(name string, aliases ...string) resource.KindSpec {
	return resource.KindSpec{Name: resource.KindName(name), Aliases: aliases, Title: name}
}

func newStub(name string, kinds ...resource.KindSpec) *stubBackend {
	return &stubBackend{name: name, kinds: kinds}
}

func TestHealthStateString(t *testing.T) {
	cases := []struct {
		state HealthState
		want  string
	}{
		{HealthUnconfigured, "unconfigured"},
		{HealthOK, "ok"},
		{HealthDegraded, "degraded"},
		{HealthDown, "down"},
		{HealthState(42), "unconfigured"},
	}
	for _, tc := range cases {
		if got := tc.state.String(); got != tc.want {
			t.Errorf("HealthState(%d).String() = %q, want %q", int(tc.state), got, tc.want)
		}
	}
}

func TestRegistryEmpty(t *testing.T) {
	r := NewRegistry()
	if got := r.Backends(); len(got) != 0 {
		t.Errorf("Backends() = %d entries, want 0", len(got))
	}
	if got := r.Kinds(); len(got) != 0 {
		t.Errorf("Kinds() = %d entries, want 0", len(got))
	}
	if b, ok := r.Get("linux"); ok || b != nil {
		t.Errorf("Get(linux) = %v, %v; want nil, false", b, ok)
	}
	if b, k, ok := r.Lookup("host"); ok || b != nil {
		t.Errorf("Lookup(host) = %v, %v, %v; want nil, zero, false", b, k, ok)
	}
}

func TestRegistryRegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	linux := newStub("linux", spec("host", "h"), spec("unit"))
	salt := newStub("salt", spec("minion"))
	if err := r.Register(linux); err != nil {
		t.Fatalf("Register(linux): %v", err)
	}
	if err := r.Register(salt); err != nil {
		t.Fatalf("Register(salt): %v", err)
	}

	// Get by backend name, case-insensitively.
	if b, ok := r.Get("LINUX"); !ok || b != linux {
		t.Errorf("Get(LINUX) = %v, %v; want linux, true", b, ok)
	}
	if b, ok := r.Get("nope"); ok || b != nil {
		t.Errorf("Get(nope) = %v, %v; want nil, false", b, ok)
	}

	// Lookup by kind name and alias, case-insensitively.
	cases := []struct {
		key   string
		owner *stubBackend
		kind  resource.KindName
	}{
		{"host", linux, "host"},
		{"HOST", linux, "host"},
		{"h", linux, "host"},
		{"Unit", linux, "unit"},
		{"minion", salt, "minion"},
	}
	for _, tc := range cases {
		b, got, ok := r.Lookup(tc.key)
		if !ok {
			t.Errorf("Lookup(%q): not found", tc.key)
			continue
		}
		if b != tc.owner {
			t.Errorf("Lookup(%q) backend = %v, want %v", tc.key, b, tc.owner)
		}
		if got.Name != tc.kind {
			t.Errorf("Lookup(%q) kind = %q, want %q", tc.key, got.Name, tc.kind)
		}
	}
	if b, k, ok := r.Lookup("missing"); ok || b != nil || k.Name != "" {
		t.Errorf("Lookup(missing) = %v, %v, %v; want nil, zero, false", b, k, ok)
	}

	// Backends keeps registration order.
	got := r.Backends()
	if len(got) != 2 || got[0] != linux || got[1] != salt {
		t.Errorf("Backends() = %v, want [linux salt]", got)
	}

	// Kinds is sorted by name regardless of registration order.
	kinds := r.Kinds()
	want := []resource.KindName{"host", "minion", "unit"}
	if len(kinds) != len(want) {
		t.Fatalf("Kinds() = %v, want %v", kinds, want)
	}
	for i, k := range kinds {
		if k.Name != want[i] {
			t.Errorf("Kinds()[%d] = %q, want %q", i, k.Name, want[i])
		}
	}
}

func TestRegistryKindsSorted(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(newStub("b", spec("zebra"), spec("apple"), spec("mango"))); err != nil {
		t.Fatalf("Register: %v", err)
	}
	kinds := r.Kinds()
	for i := 1; i < len(kinds); i++ {
		if kinds[i-1].Name > kinds[i].Name {
			t.Fatalf("Kinds() not sorted: %q before %q", kinds[i-1].Name, kinds[i].Name)
		}
	}
}

func TestRegistryRegisterErrors(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*Registry)
		add   Backend
	}{
		{
			name: "nil backend",
			add:  nil,
		},
		{
			name: "empty name",
			add:  newStub("", spec("host")),
		},
		{
			name: "whitespace name",
			add:  newStub("   ", spec("host")),
		},
		{
			name:  "duplicate backend name",
			setup: func(r *Registry) { _ = r.Register(newStub("linux", spec("host"))) },
			add:   newStub("linux", spec("unit")),
		},
		{
			name:  "duplicate backend name different case",
			setup: func(r *Registry) { _ = r.Register(newStub("linux", spec("host"))) },
			add:   newStub("Linux", spec("unit")),
		},
		{
			name:  "kind name taken",
			setup: func(r *Registry) { _ = r.Register(newStub("linux", spec("host"))) },
			add:   newStub("salt", spec("host")),
		},
		{
			name:  "kind name taken case-insensitively",
			setup: func(r *Registry) { _ = r.Register(newStub("linux", spec("Host"))) },
			add:   newStub("salt", spec("host")),
		},
		{
			name:  "alias collides with kind name",
			setup: func(r *Registry) { _ = r.Register(newStub("linux", spec("host"))) },
			add:   newStub("salt", spec("minion", "host")),
		},
		{
			name:  "alias collides with alias",
			setup: func(r *Registry) { _ = r.Register(newStub("linux", spec("host", "h"))) },
			add:   newStub("salt", spec("minion", "H")),
		},
		{
			name:  "duplicate alias within one backend",
			setup: func(r *Registry) {},
			add:   newStub("salt", spec("minion", "m"), spec("node", "m")),
		},
		{
			name:  "empty kind name",
			setup: func(r *Registry) {},
			add:   newStub("salt", spec("")),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry()
			if tc.setup != nil {
				tc.setup(r)
			}
			before := len(r.Backends())
			err := r.Register(tc.add)
			if err == nil {
				t.Fatalf("Register() = nil, want error")
			}
			if got := errs.KindOf(err); got != errs.Config {
				t.Errorf("KindOf(err) = %q, want %q", got, errs.Config)
			}
			if after := len(r.Backends()); after != before {
				t.Errorf("Backends() = %d after failed Register, want %d", after, before)
			}
		})
	}
}

func TestRegistryFailedRegisterLeavesKinds(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(newStub("linux", spec("host"))); err != nil {
		t.Fatalf("Register(linux): %v", err)
	}
	// salt claims "minion" (free) and then "host" (taken): nothing may be added.
	if err := r.Register(newStub("salt", spec("minion"), spec("host"))); err == nil {
		t.Fatal("Register(salt) = nil, want error")
	}
	if _, _, ok := r.Lookup("minion"); ok {
		t.Error("Lookup(minion) succeeded after a failed Register")
	}
	if b, _, ok := r.Lookup("host"); !ok || b.Name() != "linux" {
		t.Errorf("Lookup(host) backend = %v, want linux", b)
	}
}

func TestRegistryReplace(t *testing.T) {
	r := NewRegistry()
	old := newStub("plugin", spec("first"))
	if err := r.Register(old); err != nil {
		t.Fatalf("Register(old): %v", err)
	}
	cur := newStub("plugin", spec("second"))
	if err := r.Replace(cur); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	if b, ok := r.Get("plugin"); !ok || b != cur {
		t.Errorf("Get(plugin) = %v, %v; want new backend", b, ok)
	}
	if _, _, ok := r.Lookup("first"); ok {
		t.Error("Lookup(first) still resolves after Replace")
	}
	if b, _, ok := r.Lookup("second"); !ok || b != cur {
		t.Error("Lookup(second) does not resolve to the new backend")
	}
	if got := len(r.Backends()); got != 1 {
		t.Errorf("Backends() = %d entries, want 1", got)
	}
	if got := r.Kinds(); len(got) != 1 || got[0].Name != "second" {
		t.Errorf("Kinds() = %v, want [second]", got)
	}
}

func TestRegistryReplaceReleasesKinds(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(newStub("plugin", spec("old-kind"))); err != nil {
		t.Fatalf("Register(plugin): %v", err)
	}
	// Another backend may now claim the kind the first one dropped, and the
	// replacement may claim a kind the other backend owns.
	if err := r.Register(newStub("other", spec("shared"))); err != nil {
		t.Fatalf("Register(other): %v", err)
	}
	if err := r.Replace(newStub("plugin", spec("shared"))); err == nil {
		t.Fatal("Replace() = nil when the kind is owned by another backend, want error")
	}
	// The failed Replace must have left the registry as it was.
	if _, _, ok := r.Lookup("old-kind"); !ok {
		t.Error("Lookup(old-kind) failed after a failed Replace")
	}
	if b, _, ok := r.Lookup("shared"); !ok || b.Name() != "other" {
		t.Error("Lookup(shared) no longer resolves to other after a failed Replace")
	}
}

func TestRegistryReplaceNotFound(t *testing.T) {
	r := NewRegistry()
	err := r.Replace(newStub("ghost", spec("host")))
	if err == nil {
		t.Fatal("Replace(ghost) = nil, want error")
	}
	if got := errs.KindOf(err); got != errs.Config {
		t.Errorf("KindOf(err) = %q, want %q", got, errs.Config)
	}
	if err := r.Replace(nil); err == nil {
		t.Fatal("Replace(nil) = nil, want error")
	}
}

func TestRegistryConcurrentUse(t *testing.T) {
	r := NewRegistry()
	base := newStub("linux", spec("host", "h"), spec("unit"))
	if err := r.Register(base); err != nil {
		t.Fatalf("Register(linux): %v", err)
	}

	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers * 2)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if b, ok := r.Get("linux"); !ok || b != base {
					t.Errorf("Get(linux) = %v, %v", b, ok)
					return
				}
				if b, _, ok := r.Lookup("host"); !ok || b != base {
					t.Errorf("Lookup(host) = %v, %v", b, ok)
					return
				}
				_ = r.Kinds()
				_ = r.Backends()
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("stub-%02d", i)
			if err := r.Register(newStub(name, spec(name))); err != nil {
				t.Errorf("Register(%s): %v", name, err)
				return
			}
			for j := 0; j < 100; j++ {
				if b, ok := r.Get(name); !ok || b.Name() != name {
					t.Errorf("Get(%s) = %v, %v", name, b, ok)
					return
				}
				if _, _, ok := r.Lookup(name); !ok {
					t.Errorf("Lookup(%s): not found", name)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	if got := len(r.Backends()); got != workers+1 {
		t.Errorf("Backends() = %d, want %d", got, workers+1)
	}
}
