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

// shortName is the part of a host name before the first dot.
func shortName(name string) string {
	for i := 0; i < len(name); i++ {
		if name[i] == '.' {
			return name[:i]
		}
	}
	return name
}
