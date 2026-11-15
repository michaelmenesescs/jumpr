// Package targets owns the current target set of a session and the saved sets
// that can rebuild it. A set is always resolved: sorted, de-duplicated host
// identities.
package targets

import (
	"fmt"
	"sort"
	"strings"

	"jumpr/internal/errs"
	"jumpr/internal/inventory"
	"jumpr/internal/resource"
)

// investigationSet is the selector set that resolves through the hosts of the
// open investigation instead of the store. Its name carries no leading "@".
const investigationSet = "investigation"

// Set is the current target set of a session: what `:run`, probes and actions apply to.
type Set struct {
	Label    string            // "role=web dc=ewr", "@investigation", "3 selected"
	Selector resource.Selector // zero when built from an explicit list
	Hosts    []resource.HostID // resolved, sorted, de-duplicated
}

// Resolve expands sel against inv, including @sets. "@investigation" resolves
// through invHosts (may be nil).
//
// With no sets in the selector the result is the hosts matching the terms
// (every inventory host when the selector has no terms). With sets and no terms
// it is the union of those sets. With sets and terms it is the intersection:
// the hosts matching the terms that are also in the sets. An unknown set is an
// errs.Config error. The result is sorted and de-duplicated; Label is the
// selector text.
func Resolve(inv inventory.Inventory, store *Store, sel resource.Selector, invHosts []resource.HostID) (Set, error) {
	set := Set{Label: sel.String(), Selector: sel}

	var hosts []resource.HostID
	if len(sel.Sets) == 0 {
		hosts = selectHosts(inv, sel)
	} else {
		setHosts, err := unionSets(store, sel.Sets, invHosts)
		if err != nil {
			return Set{}, err
		}
		if len(sel.Terms) == 0 {
			hosts = setHosts
		} else {
			hosts = intersect(selectHosts(inv, sel), setHosts)
		}
	}
	set.Hosts = normalizeSorted(hosts)
	return set, nil
}

// FromHosts builds a resolved set from an explicit host list, under the given
// label ("3 selected", "@canary", "").
func FromHosts(label string, hosts []resource.HostID) Set {
	return Set{Label: label, Hosts: normalizeSorted(hosts)}
}

// selectHosts returns the IDs of the inventory hosts matching sel. A nil
// inventory matches nothing.
func selectHosts(inv inventory.Inventory, sel resource.Selector) []resource.HostID {
	if inv == nil {
		return nil
	}
	hosts := inv.Select(sel)
	out := make([]resource.HostID, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, h.ID)
	}
	return out
}

// unionSets resolves every named set and returns their union. Unknown names are
// an errs.Config error; a nil store only resolves @investigation.
func unionSets(store *Store, names []string, invHosts []resource.HostID) ([]resource.HostID, error) {
	var out []resource.HostID
	for _, name := range names {
		if name == investigationSet {
			out = append(out, normalizeSorted(invHosts)...)
			continue
		}
		if store == nil {
			return nil, unknownSet(name)
		}
		hosts, err := store.Load(name)
		if err != nil {
			return nil, err
		}
		out = append(out, hosts...)
	}
	return normalizeSorted(out), nil
}

// intersect keeps the hosts of a that also appear in b.
func intersect(a, b []resource.HostID) []resource.HostID {
	in := make(map[resource.HostID]bool, len(b))
	for _, h := range b {
		in[h] = true
	}
	out := make([]resource.HostID, 0, len(a))
	for _, h := range a {
		if in[h] {
			out = append(out, h)
		}
	}
	return out
}

// normalizeSorted trims, normalizes, de-duplicates and sorts a host list.
func normalizeSorted(in []resource.HostID) []resource.HostID {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[resource.HostID]bool, len(in))
	out := make([]resource.HostID, 0, len(in))
	for _, h := range in {
		id := resource.NormalizeHostID(string(h))
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	if len(out) == 0 {
		return nil
	}
	return out
}

// unknownSet builds the error for a set the store does not have.
func unknownSet(name string) *errs.Error {
	return errs.New(errs.Config, "targets.Resolve", fmt.Sprintf("unknown set @%s", strings.TrimPrefix(name, "@")))
}
