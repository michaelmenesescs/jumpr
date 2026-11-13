// Inventory source "prom" (BUILD-PLAN §4.7): hosts come from Prometheus scrape
// targets, so an alert's instance label resolves to a real inventory host even
// when the fleet is only known to Prometheus.

package prom

import (
	"context"
	"sort"
	"strings"
	"time"

	"jumper/internal/errs"
	"jumper/internal/inventory"
	"jumper/internal/resource"
)

// Source is the inventory source fed by one Prometheus server's /api/v1/targets.
// Only targets that are up become hosts: a down target is a scrape failure, not
// a machine jumper should offer to open a shell on.
type Source struct {
	// Server is the configured Prometheus name, kept as a label so a fleet
	// scraped by several servers stays distinguishable.
	Server string
	// Client is the typed client for it. Nil makes Load fail with errs.Config.
	Client *Client
	// Now stamps SeenAt; nil means time.Now.
	Now func() time.Time
}

var _ inventory.Source = (*Source)(nil)

// NewSource returns the inventory source of one configured Prometheus.
func NewSource(server string, c *Client) *Source {
	return &Source{Server: server, Client: c}
}

// Name is both the source name and its label namespace: every label lands under
// "prom." and, when unambiguous, also unprefixed.
func (s *Source) Name() string { return "prom" }

// Load reads every active target and keeps the up ones. The instance label
// becomes the host ID with its port and any user@ removed; job and every other
// target label are kept as labels.
func (s *Source) Load(ctx context.Context) ([]inventory.Host, error) {
	if s == nil || s.Client == nil {
		return nil, errs.New(errs.Config, "inventory.prom", "no prometheus client configured")
	}
	targets, err := s.Client.Targets(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	stamp := now()

	out := make([]inventory.Host, 0, len(targets))
	seen := make(map[resource.HostID]int, len(targets))
	for _, t := range targets {
		if !strings.EqualFold(strings.TrimSpace(t.Health), "up") {
			continue
		}
		instance := strings.TrimSpace(t.Labels["instance"])
		id := instanceHost(instance)
		if id == "" {
			continue
		}
		labels := make(map[string]string, len(t.Labels)+1)
		for k, v := range t.Labels {
			if k == "instance" || strings.TrimSpace(v) == "" {
				continue
			}
			labels[k] = v
		}
		if s.Server != "" {
			labels["server"] = s.Server
		}
		if i, ok := seen[id]; ok {
			for k, v := range labels {
				if _, exists := out[i].Labels[k]; !exists {
					out[i].Labels[k] = v
				}
			}
			continue
		}
		seen[id] = len(out)
		out = append(out, inventory.Host{
			ID:      id,
			Aliases: instanceAliases(instance, id),
			Labels:  labels,
			SeenAt:  stamp,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// instanceAliases keeps the raw instance as an alias when it differs from the
// host ID (the port is the usual difference), so a selector can use either.
func instanceAliases(instance string, id resource.HostID) []string {
	instance = strings.TrimSpace(instance)
	if instance == "" || strings.EqualFold(instance, string(id)) {
		return nil
	}
	return []string{strings.ToLower(instance)}
}
