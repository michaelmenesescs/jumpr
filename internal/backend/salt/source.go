package salt

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"jumper/internal/errs"
	"jumper/internal/inventory"
	"jumper/internal/resource"
)

// EmptyCacheHint is the message shown to the user when the master's grains
// cache answers with no minions.
const EmptyCacheHint = "salt grains cache is empty; run saltutil.sync_grains or enable grains cache"

// Source is the Salt inventory source: one host per minion in the master's
// grains cache. It reads only the master and never contacts a minion.
type Source struct {
	Runner *Runner
	// Extra names grain keys that are always exposed as labels, even when the
	// built-in denylist would drop them.
	Extra []string
	// Now stamps SeenAt; nil means time.Now.
	Now func() time.Time
}

// NewSource wraps a Runner as an inventory source.
func NewSource(r *Runner) *Source {
	return &Source{Runner: r}
}

// Name is both the source name and its label namespace.
func (s *Source) Name() string { return "salt" }

// Load reads the grains cache and turns each minion into a host. An empty
// cache is not an error: it yields no hosts, and the caller shows
// EmptyCacheHint.
func (s *Source) Load(ctx context.Context) ([]inventory.Host, error) {
	if s.Runner == nil {
		return nil, errs.New(errs.Config, "inventory.salt", "no salt runner")
	}
	all, err := s.Runner.CachedGrains(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	stamp := now()

	minions := make([]string, 0, len(all))
	for minion := range all {
		minions = append(minions, minion)
	}
	sort.Strings(minions)

	hosts := make([]inventory.Host, 0, len(minions))
	for _, minion := range minions {
		g := all[minion]
		id := resource.NormalizeHostID(hostID(minion, g))
		hosts = append(hosts, inventory.Host{
			ID:      id,
			Aliases: aliases(minion, g, id),
			Labels:  Labels(g, s.Extra),
			SeenAt:  stamp,
		})
	}
	return hosts, nil
}

// hostID picks the identity candidate for a minion: its fqdn grain when that
// looks like a real FQDN, else the minion id.
func hostID(minion string, g Grains) string {
	if fqdn := fqdnGrain(g); fqdn != "" {
		return fqdn
	}
	return minion
}

// fqdnGrain returns the fqdn grain when it is a dotted name that is not
// localhost. A localhost fqdn is useless as an identity and would merge every
// minion that reports it into one host.
func fqdnGrain(g Grains) string {
	fqdn := strings.TrimSpace(g.Str("fqdn"))
	if !strings.Contains(fqdn, ".") || strings.HasPrefix(strings.ToLower(fqdn), "localhost") {
		return ""
	}
	return fqdn
}

// aliases lists every other name the minion is known by: the minion id, the
// host grain, the fqdn and its IPv4 addresses except loopback.
func aliases(minion string, g Grains, id resource.HostID) []string {
	candidates := []string{minion, g.Str("host"), fqdnGrain(g)}
	for _, addr := range g.StrList("ipv4") {
		if isLoopback(addr) {
			continue
		}
		candidates = append(candidates, addr)
	}
	seen := make(map[string]bool, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		n := string(resource.NormalizeHostID(c))
		if n == "" || n == string(id) || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// isLoopback reports whether addr is an IPv4 or IPv6 loopback address.
func isLoopback(addr string) bool {
	return strings.HasPrefix(addr, "127.") || addr == "::1"
}

// RosterSourceName is the inventory source name of the master's salt-ssh
// roster. It is also the label namespace of the roster labels.
const RosterSourceName = "salt-roster"

// RosterSource is the salt-ssh inventory source: one host per roster entry,
// labelled `salt.roster=true` so a selector can tell them apart from minions.
// It reads only the master; nothing is installed on a roster host.
type RosterSource struct {
	Runner *Runner
	// Path is the roster on the master; empty means DefaultRosterPath.
	Path string
	// Now stamps SeenAt; nil means time.Now.
	Now func() time.Time
}

// NewRosterSource wraps a Runner as the roster inventory source.
func NewRosterSource(r *Runner, path string) *RosterSource {
	return &RosterSource{Runner: r, Path: path}
}

// Name is both the source name and its label namespace.
func (s *RosterSource) Name() string { return RosterSourceName }

// Load reads the master's roster and turns every entry into a host. An
// unreadable roster is an error the inventory reports under this source name;
// it never hides the other sources.
func (s *RosterSource) Load(ctx context.Context) ([]inventory.Host, error) {
	if s.Runner == nil {
		return nil, errs.New(errs.Config, "inventory.salt-roster", "no salt runner")
	}
	b := &Backend{runner: s.Runner, rosterPath: s.Path, now: s.Now}
	entries, err := b.Roster(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	stamp := now()

	hosts := make([]inventory.Host, 0, len(entries))
	for _, e := range entries {
		h := RosterHost(e)
		h.SeenAt = stamp
		hosts = append(hosts, h)
	}
	return hosts, nil
}

// RosterHost turns one roster entry into an inventory host. The identity is the
// address the entry dials; the roster key stays an alias.
func RosterHost(e RosterEntry) inventory.Host {
	id := resource.NormalizeHostID(e.Address())
	labels := map[string]string{
		"host":        string(id),
		"salt.roster": "true",
	}
	if e.User != "" {
		labels["salt.roster.user"] = e.User
	}
	if e.Port > 0 {
		labels["salt.roster.port"] = strconv.Itoa(e.Port)
	}
	var aliases []string
	if key := resource.NormalizeHostID(e.ID); key != "" && key != id {
		aliases = append(aliases, string(key))
	}
	return inventory.Host{ID: id, Addr: e.Address(), Aliases: aliases, Labels: labels, Sources: []string{RosterSourceName}}
}
