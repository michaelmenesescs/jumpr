package inventory

import (
	"sort"
	"strings"
	"time"

	"jumper/internal/resource"
)

// sourceHosts is one source's last good host list, in source order.
type sourceHosts struct {
	name  string
	hosts []Host
}

// candidate is one source host prepared for merging.
type candidate struct {
	source string
	host   Host
	id     string
	names  []string // canonical aliases, ID excluded
}

// merge folds the per-source lists into one host per machine. Hosts are the
// same machine when their canonical IDs or aliases match, or when the short
// name of one is the full ID of another. The relation is transitive.
func merge(inputs []sourceHosts, rewrites []Rewrite, now time.Time) []Host {
	var entries []candidate
	for _, in := range inputs {
		for _, h := range in.hosts {
			id := string(Canonical(string(h.ID), rewrites))
			if id == "" {
				continue
			}
			names := make([]string, 0, len(h.Aliases))
			for _, a := range h.Aliases {
				c := string(Canonical(a, rewrites))
				if c != "" && c != id {
					names = append(names, c)
				}
			}
			entries = append(entries, candidate{source: in.name, host: h, id: id, names: names})
		}
	}
	if len(entries) == 0 {
		return nil
	}

	// Union-find over candidate indices, joined through shared identity keys.
	parent := make([]int, len(entries))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[rb] = ra
		}
	}
	owner := make(map[string]int, len(entries)*2)
	for i, e := range entries {
		keys := make([]string, 0, len(e.names)+1)
		keys = append(keys, "name:"+e.id)
		for _, n := range e.names {
			keys = append(keys, "name:"+n)
		}
		for _, k := range keys {
			if j, ok := owner[k]; ok {
				union(j, i)
			} else {
				owner[k] = i
			}
		}
	}
	// The short name of one host equals the full ID of another.
	ids := make(map[string]int, len(entries))
	for i, e := range entries {
		if _, ok := ids[e.id]; !ok {
			ids[e.id] = i
		}
	}
	for i, e := range entries {
		short := shortName(e.id)
		if short == "" || short == e.id {
			continue
		}
		if j, ok := ids[short]; ok {
			union(j, i)
		}
	}

	groups := make(map[int][]int, len(entries))
	var roots []int
	for i := range entries {
		r := find(i)
		if _, ok := groups[r]; !ok {
			roots = append(roots, r)
		}
		groups[r] = append(groups[r], i)
	}

	out := make([]Host, 0, len(roots))
	for _, r := range roots {
		cands := make([]candidate, 0, len(groups[r]))
		for _, i := range groups[r] {
			cands = append(cands, entries[i])
		}
		out = append(out, buildMergedHost(cands, now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// buildMergedHost combines a group of candidates into one Host. Candidates are
// in source precedence order (ssh first), so the first Addr wins.
func buildMergedHost(cands []candidate, now time.Time) Host {
	names := make([]string, 0, len(cands)*2)
	for _, c := range cands {
		names = append(names, c.id)
		names = append(names, c.names...)
	}
	id := pickID(names)

	var addr string
	for _, c := range cands {
		if c.host.Addr != "" {
			addr = c.host.Addr
			break
		}
	}

	var aliases, sources []string
	labels := make(map[string]string)
	perKey := make(map[string]map[string]bool)
	var seen time.Time
	for _, c := range cands {
		aliases = append(aliases, c.names...)
		sources = append(sources, c.source)
		if c.host.SeenAt.After(seen) {
			seen = c.host.SeenAt
		}
		for k, v := range c.host.Labels {
			if k == "host" {
				continue
			}
			ns := c.source + "." + k
			if _, ok := labels[ns]; !ok {
				labels[ns] = v
			}
			if perKey[k] == nil {
				perKey[k] = make(map[string]bool)
			}
			perKey[k][v] = true
		}
	}
	for k, vals := range perKey {
		if len(vals) == 1 {
			for v := range vals {
				labels[k] = v
			}
		}
	}
	labels["host"] = string(id)
	if seen.IsZero() {
		seen = now
	}
	return Host{
		ID:      resource.HostID(id),
		Addr:    addr,
		Aliases: sortedUnique(aliases),
		Labels:  labels,
		Sources: sortedUnique(sources),
		SeenAt:  seen,
	}
}

// pickID chooses the longest FQDN-looking candidate; ties go to the
// lexically smallest. Without a dotted candidate the longest name wins.
func pickID(names []string) string {
	var best string
	dotted := false
	for _, n := range names {
		if n == "" {
			continue
		}
		d := strings.Contains(n, ".")
		switch {
		case best == "":
			best, dotted = n, d
		case d != dotted:
			if d {
				best, dotted = n, d
			}
		case len(n) > len(best), len(n) == len(best) && n < best:
			best = n
		}
	}
	return best
}
