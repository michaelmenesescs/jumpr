package backend

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"jumper/internal/errs"
	"jumper/internal/resource"
)

// kindEntry is what a kind name or alias resolves to.
type kindEntry struct {
	backend Backend
	owner   string // backend name, kept so collisions can be reported under r.mu
	spec    resource.KindSpec
}

// claimedKey is one lower-cased key a backend owns, with the spec it resolves to.
type claimedKey struct {
	key  string
	spec resource.KindSpec
}

// registryEntry is one registered backend plus the keys it owns, kept in
// registration order.
type registryEntry struct {
	backend Backend
	name    string // as returned by Backend.Name()
	lower   string // lower-cased name, for case-insensitive matching
	specs   []resource.KindSpec
	keys    []claimedKey // lower-cased kind names and aliases this backend owns
}

// Registry maps kind names and aliases to backends.
type Registry struct {
	mu      sync.RWMutex
	entries []registryEntry
	kinds   map[string]kindEntry
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{kinds: make(map[string]kindEntry)}
}

// Register adds a backend and its kinds. It fails with an errs.Config error
// when the backend has no name, when a backend of that name is already
// registered, or when a kind name or alias is already taken.
func (r *Registry) Register(b Backend) error {
	if b == nil {
		return errs.New(errs.Config, "backend.Register", "nil backend")
	}
	name := b.Name()
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return errs.New(errs.Config, "backend.Register", "backend has an empty name")
	}
	specs := b.Kinds()

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, e := range r.entries {
		if e.lower == lower {
			return errs.New(errs.Config, "backend.Register",
				fmt.Sprintf("backend %q is already registered", name))
		}
	}

	keys, err := r.claimKeys("backend.Register", lower, specs)
	if err != nil {
		return err
	}

	r.entries = append(r.entries, registryEntry{
		backend: b,
		name:    name,
		lower:   lower,
		specs:   specs,
		keys:    keys,
	})
	for _, ck := range keys {
		r.kinds[ck.key] = kindEntry{backend: b, owner: name, spec: ck.spec}
	}
	return nil
}

// Replace swaps a registered backend for another one of the same name (plugin
// reload, task 117). The old backend's kinds are released first, so the new
// backend may reuse them.
func (r *Registry) Replace(b Backend) error {
	if b == nil {
		return errs.New(errs.Config, "backend.Replace", "nil backend")
	}
	name := b.Name()
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return errs.New(errs.Config, "backend.Replace", "backend has an empty name")
	}
	specs := b.Kinds()

	r.mu.Lock()
	defer r.mu.Unlock()

	idx := -1
	for i, e := range r.entries {
		if e.lower == lower {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errs.New(errs.Config, "backend.Replace",
			fmt.Sprintf("backend %q is not registered", name))
	}

	// The old backend's own keys are free; every other key is not.
	for _, ck := range r.entries[idx].keys {
		delete(r.kinds, ck.key)
	}
	keys, err := r.claimKeys("backend.Replace", lower, specs)
	if err != nil {
		// Put the old keys back: a failed Replace leaves the registry unchanged.
		for _, ck := range r.entries[idx].keys {
			r.kinds[ck.key] = kindEntry{backend: r.entries[idx].backend, owner: r.entries[idx].name, spec: ck.spec}
		}
		return err
	}

	r.entries[idx] = registryEntry{
		backend: b,
		name:    name,
		lower:   lower,
		specs:   specs,
		keys:    keys,
	}
	for _, ck := range keys {
		r.kinds[ck.key] = kindEntry{backend: b, owner: name, spec: ck.spec}
	}
	return nil
}

// claimKeys validates the kind names and aliases of a backend and returns every
// key it would own. It requires r.mu to be held for writing.
func (r *Registry) claimKeys(op, backendName string, specs []resource.KindSpec) ([]claimedKey, error) {
	taken := make(map[string]struct{}, len(specs))
	out := make([]claimedKey, 0, len(specs))

	for _, spec := range specs {
		key := strings.ToLower(strings.TrimSpace(string(spec.Name)))
		if key == "" {
			return nil, errs.New(errs.Config, op,
				fmt.Sprintf("backend %q has a kind with an empty name", backendName))
		}
		out = append(out, claimedKey{key: key, spec: spec})
	}

	for _, spec := range specs {
		for _, alias := range spec.Aliases {
			out = append(out, claimedKey{
				key:  strings.ToLower(strings.TrimSpace(alias)),
				spec: spec,
			})
		}
	}

	for _, ck := range out {
		if ck.key == "" {
			return nil, errs.New(errs.Config, op,
				fmt.Sprintf("backend %q has an empty alias for kind %q", backendName, ck.spec.Name))
		}
		if _, dup := taken[ck.key]; dup {
			return nil, errs.New(errs.Config, op,
				fmt.Sprintf("kind %q is claimed twice by backend %q", ck.key, backendName))
		}
		if other, used := r.kinds[ck.key]; used {
			return nil, errs.New(errs.Config, op,
				fmt.Sprintf("kind %q is already registered by backend %q", ck.key, other.owner))
		}
		taken[ck.key] = struct{}{}
	}
	return out, nil
}

// Get returns the backend registered under name. Matching is case-insensitive.
func (r *Registry) Get(name string) (Backend, bool) {
	lower := strings.ToLower(strings.TrimSpace(name))
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.entries {
		if e.lower == lower {
			return e.backend, true
		}
	}
	return nil, false
}

// Backends returns the registered backends in registration order.
func (r *Registry) Backends() []Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Backend, len(r.entries))
	for i, e := range r.entries {
		out[i] = e.backend
	}
	return out
}

// Lookup resolves a kind name or alias to its backend and kind spec. Matching
// is case-insensitive.
func (r *Registry) Lookup(kindOrAlias string) (Backend, resource.KindSpec, bool) {
	key := strings.ToLower(strings.TrimSpace(kindOrAlias))
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.kinds[key]
	if !ok {
		return nil, resource.KindSpec{}, false
	}
	return e.backend, e.spec, true
}

// Kinds returns every registered kind, sorted by name.
func (r *Registry) Kinds() []resource.KindSpec {
	r.mu.RLock()
	out := make([]resource.KindSpec, 0, len(r.kinds))
	for _, e := range r.entries {
		out = append(out, e.specs...)
	}
	r.mu.RUnlock()

	sort.SliceStable(out, func(i, j int) bool {
		return string(out[i].Name) < string(out[j].Name)
	})
	return out
}
