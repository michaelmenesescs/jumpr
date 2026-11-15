// Package inventory merges hosts from several sources into one list with
// stable identities and labels.
package inventory

import (
	"context"
	"sort"
	"sync"
	"time"

	"jumpr/internal/resource"
)

type Host struct {
	ID      resource.HostID
	Addr    string            // what to dial over SSH (ssh_config alias or FQDN)
	Aliases []string          // short name, FQDN, IPs, k8s node name, minion id, prom instance
	Labels  map[string]string // namespaced ("salt.role") plus unprefixed shortcuts when unambiguous; always has "host"
	Sources []string
	SeenAt  time.Time
}

// Source produces hosts from one place. Load must respect ctx and return quickly or fail.
type Source interface {
	Name() string // also the label namespace: "ssh", "static", "salt", "k8s", "prom", "chef"
	Load(ctx context.Context) ([]Host, error)
}

type Inventory interface {
	Hosts() []Host                                // sorted by ID; a copy
	Select(sel resource.Selector) []Host          // Sets in sel are ignored here
	Resolve(nameOrAlias string) (Host, bool)      // after identity rewrites
	Refresh(ctx context.Context) map[string]error // per source name; successful sources still apply
	Age() time.Duration                           // since the last successful refresh of any source
}

// loadTimeout bounds one source load inside Refresh.
const loadTimeout = 30 * time.Second

// sourceState is the last good result of one source.
type sourceState struct {
	hosts   []Host
	updated time.Time
}

// Store is the merged inventory. It implements Inventory.
type Store struct {
	sources   []Source
	rewrites  []Rewrite
	cachePath string
	now       func() time.Time

	mu          sync.RWMutex
	bySource    map[string]sourceState
	hosts       []Host
	lastRefresh time.Time
}

// New builds an empty store. now may be nil (time.Now).
func New(sources []Source, rewrites []Rewrite, cachePath string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		sources:   append([]Source(nil), sources...),
		rewrites:  append([]Rewrite(nil), rewrites...),
		cachePath: cachePath,
		now:       now,
		bySource:  make(map[string]sourceState),
	}
}

// Refresh loads every source concurrently, keeps the previous hosts of any
// source that fails, merges and swaps the result atomically, then writes the
// cache. It returns one entry per source that failed.
func (s *Store) Refresh(ctx context.Context) map[string]error {
	type result struct {
		name  string
		hosts []Host
		err   error
	}
	srcs := s.sources
	results := make([]result, len(srcs))
	var wg sync.WaitGroup
	for i, src := range srcs {
		wg.Add(1)
		go func(i int, src Source) {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, loadTimeout)
			defer cancel()
			hosts, err := src.Load(c)
			results[i] = result{name: src.Name(), hosts: hosts, err: err}
		}(i, src)
	}
	wg.Wait()

	failed := make(map[string]error)
	s.mu.Lock()
	now := s.now()
	configured := make(map[string]bool, len(s.sources))
	for _, src := range s.sources {
		configured[src.Name()] = true
	}
	for name := range s.bySource {
		if !configured[name] {
			delete(s.bySource, name)
		}
	}
	for _, r := range results {
		if r.err != nil {
			failed[r.name] = r.err
			continue
		}
		s.bySource[r.name] = sourceState{hosts: cloneHosts(r.hosts), updated: now}
	}
	if len(failed) < len(results) {
		s.lastRefresh = now
	}
	s.rebuildLocked()
	snap := s.cacheSnapshotLocked()
	s.mu.Unlock()

	// A cache write failure must not fail a refresh; the merged view is live.
	_ = s.writeCache(snap)
	return failed
}

// Hosts returns the merged hosts sorted by ID. The result is a deep copy.
func (s *Store) Hosts() []Host {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneHosts(s.hosts)
}

// Select returns the hosts whose labels match sel. Sets are ignored.
func (s *Store) Select(sel resource.Selector) []Host {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Host, 0, len(s.hosts))
	for _, h := range s.hosts {
		if sel.Matches(h.Labels) {
			out = append(out, cloneHost(h))
		}
	}
	return out
}

// Resolve finds a host by ID, alias or short name (after rewrites). An
// ambiguous short name is not found.
func (s *Store) Resolve(nameOrAlias string) (Host, bool) {
	n := string(Canonical(nameOrAlias, s.rewrites))
	if n == "" {
		return Host{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.hosts {
		if string(h.ID) == n {
			return cloneHost(h), true
		}
	}
	var byAlias []Host
	for _, h := range s.hosts {
		for _, a := range h.Aliases {
			if string(Canonical(a, s.rewrites)) == n {
				byAlias = append(byAlias, cloneHost(h))
				break
			}
		}
	}
	if len(byAlias) == 1 {
		return byAlias[0], true
	}
	if len(byAlias) > 1 {
		return Host{}, false
	}
	var byShort []Host
	for _, h := range s.hosts {
		if shortName(string(h.ID)) == n {
			byShort = append(byShort, cloneHost(h))
		}
	}
	if len(byShort) == 1 {
		return byShort[0], true
	}
	return Host{}, false
}

// Age reports how long ago the last successful refresh (or cache load) was.
func (s *Store) Age() time.Duration {
	s.mu.RLock()
	last := s.lastRefresh
	s.mu.RUnlock()
	if last.IsZero() {
		return 0
	}
	d := s.now().Sub(last)
	if d < 0 {
		return 0
	}
	return d
}

// rebuildLocked recomputes the merged host list from the per-source states.
// ssh_config hosts come first so their Addr wins.
func (s *Store) rebuildLocked() {
	ordered := s.orderedSourcesLocked()
	inputs := make([]sourceHosts, 0, len(ordered))
	seen := make(map[string]bool, len(ordered))
	for _, src := range ordered {
		name := src.Name()
		if seen[name] {
			continue
		}
		seen[name] = true
		st, ok := s.bySource[name]
		if !ok {
			continue
		}
		inputs = append(inputs, sourceHosts{name: name, hosts: st.hosts})
	}
	// Cached sources that are not (yet) configured still make the first frame.
	extra := make([]string, 0, len(s.bySource))
	for name := range s.bySource {
		if !seen[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		inputs = append(inputs, sourceHosts{name: name, hosts: s.bySource[name].hosts})
	}
	s.hosts = merge(inputs, s.rewrites, s.now())
}

// orderedSourcesLocked returns the configured sources with "ssh" first.
func (s *Store) orderedSourcesLocked() []Source {
	out := make([]Source, 0, len(s.sources))
	for _, src := range s.sources {
		if src.Name() == "ssh" {
			out = append(out, src)
		}
	}
	for _, src := range s.sources {
		if src.Name() != "ssh" {
			out = append(out, src)
		}
	}
	return out
}

// cloneHost deep-copies one host.
func cloneHost(h Host) Host {
	out := h
	out.Aliases = append([]string(nil), h.Aliases...)
	out.Sources = append([]string(nil), h.Sources...)
	if h.Labels != nil {
		out.Labels = make(map[string]string, len(h.Labels))
		for k, v := range h.Labels {
			out.Labels[k] = v
		}
	}
	return out
}

func cloneHosts(in []Host) []Host {
	if in == nil {
		return nil
	}
	out := make([]Host, len(in))
	for i, h := range in {
		out[i] = cloneHost(h)
	}
	return out
}

// sortedUnique returns the de-duplicated, sorted copy of in.
func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
