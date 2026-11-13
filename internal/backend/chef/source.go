package chef

import (
	"context"
	"sort"
	"strings"
	"time"

	"jumper/internal/errs"
	"jumper/internal/inventory"
	"jumper/internal/resource"
)

// Source is the Chef inventory source: one host per registered node, read from
// `knife status -F json` on the workstation. It never contacts a node.
type Source struct {
	Runner *Runner
	// Now stamps SeenAt; nil means time.Now.
	Now func() time.Time
}

var _ inventory.Source = (*Source)(nil)

// NewSource wraps a Runner as an inventory source.
func NewSource(r *Runner) *Source { return &Source{Runner: r} }

// Name is both the source name and its label namespace.
func (s *Source) Name() string { return "chef" }

// Load reads every registered node and turns it into a host. The labels are the
// Chef facts the rest of jumper selects on: chef.environment, chef.roles
// (comma-joined) and chef.platform.
func (s *Source) Load(ctx context.Context) ([]inventory.Host, error) {
	if s == nil || s.Runner == nil {
		return nil, errs.New(errs.Config, "inventory.chef", "no chef runner")
	}
	out, err := s.Runner.Run(ctx, "status")
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	nodes, err := ParseStatus(out)
	if err != nil {
		return nil, err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	stamp := now()

	hosts := make([]inventory.Host, 0, len(nodes))
	for _, n := range nodes {
		id := resource.NormalizeHostID(n.Name)
		if id == "" {
			continue
		}
		hosts = append(hosts, inventory.Host{
			ID:      id,
			Aliases: aliasesOf(n, id),
			Labels:  labelsOf(n),
			SeenAt:  stamp,
		})
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].ID < hosts[j].ID })
	return hosts, nil
}

// labelsOf builds the chef.* labels of one node.
func labelsOf(n StatusNode) map[string]string {
	roles := rolesFromRunList(n.RunList)
	return map[string]string{
		"chef.environment": n.Environment,
		"chef.roles":       strings.Join(roles, ","),
		"chef.platform":    n.Platform,
	}
}

// aliasesOf lists the other names a node is known by: its fqdn and its address,
// when the status output carries them.
func aliasesOf(n StatusNode, id resource.HostID) []string {
	seen := map[string]bool{string(id): true}
	out := make([]string, 0, 2)
	for _, name := range []string{n.FQDN, n.IP} {
		a := string(resource.NormalizeHostID(name))
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}
