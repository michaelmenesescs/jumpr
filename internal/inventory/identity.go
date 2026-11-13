package inventory

import (
	"regexp"

	"jumper/internal/resource"
)

// Rewrite replaces every match of From with To. Both are applied to the
// lower-cased, trimmed name in order.
type Rewrite struct {
	From *regexp.Regexp
	To   string
}

// Canonical returns the stable identity of a name: trimmed, lower-cased, with
// one trailing dot removed, then every rewrite applied with ReplaceAllString.
func Canonical(name string, rewrites []Rewrite) resource.HostID {
	n := string(resource.NormalizeHostID(name))
	for _, rw := range rewrites {
		if rw.From == nil {
			continue
		}
		n = rw.From.ReplaceAllString(n, rw.To)
	}
	return resource.HostID(n)
}

// Rewriter is an Inventory that can hand back its identity rewrites, so a
// caller holding a name from somewhere else can canonicalize it the same way
// Resolve does. Store implements it.
type Rewriter interface {
	Rewrites() []Rewrite
}

// Rewrites returns the store's identity rewrites, as a copy.
func (s *Store) Rewrites() []Rewrite {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Rewrite(nil), s.rewrites...)
}

// CanonicalHost canonicalizes name with inv's identity rewrites when the
// inventory exposes them. An inventory without rewrites only gets the
// normalized form, which is what Resolve would compute for it anyway.
func CanonicalHost(inv Inventory, name string) resource.HostID {
	if inv == nil {
		return ""
	}
	var rewrites []Rewrite
	if r, ok := inv.(Rewriter); ok {
		rewrites = r.Rewrites()
	}
	return Canonical(name, rewrites)
}

// shortName is the part of a host name before the first dot.
func shortName(name string) string {
	for i := 0; i < len(name); i++ {
		if name[i] == '.' {
			return name[:i]
		}
	}
	return name
}
