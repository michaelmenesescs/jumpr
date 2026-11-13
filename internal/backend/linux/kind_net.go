package linux

import (
	"fmt"
	"strings"

	"jumper/internal/probe"
	"jumper/internal/resource"
)

// interfaceRows joins `ip -j -s link` with `ip -j addr`: one row per interface,
// its CIDRs IPv4 first. A failed link section fails the host; a failed address
// section only drops the addrs column and marks the rows degraded.
func (b *Backend) interfaceRows(host resource.HostID, sec map[string]probe.SectionResult, bad, retried map[string]error) ([]resource.Resource, error) {
	if e := bad[secIPLink]; e != nil {
		return nil, e
	}
	links, err := ParseIPLinks(sec[secIPLink].Stdout)
	if err != nil {
		return nil, err
	}

	degraded := copyErrs(retried)
	addrs := map[string][]string{}
	addrsOK := true
	if e := bad[secIPAddr]; e != nil {
		addrsOK = false
		degraded[secIPAddr] = e
	} else if parsed, aerr := ParseIPAddrs(sec[secIPAddr].Stdout); aerr != nil {
		addrsOK = false
		degraded[secIPAddr] = aerr
	} else {
		for _, a := range parsed {
			addrs[a.Name] = a.Addrs
		}
	}

	out := make([]resource.Resource, 0, len(links))
	for _, l := range links {
		if l.Name == "" {
			continue
		}
		fields := map[string]any{
			"host":    string(host),
			"name":    l.Name,
			"state":   l.State,
			"mtu":     int64(l.MTU),
			"rx":      l.RXBytes,
			"tx":      l.TXBytes,
			"rx_err":  l.RXErrors,
			"tx_err":  l.TXErrors,
			"rx_drop": l.RXDropped,
			"tx_drop": l.TXDropped,
			"master":  l.Master,
		}
		if addrsOK {
			fields["addrs"] = strings.Join(addrs[l.Name], ",")
		}
		status := resource.StatusOK
		if l.CounterWarnings() || (l.Down() && len(addrs[l.Name]) > 0) {
			status = resource.StatusWarn
		}
		labels := b.labels(host)
		applyDegraded(labels, nil, degraded)
		out = append(out, resource.Resource{
			Ref:       resource.Ref{Backend: backendName, Kind: KindInterface, Host: host, Name: l.Name},
			Fields:    fields,
			Labels:    labels,
			Status:    status,
			Raw:       l,
			FetchedAt: b.now(),
		})
	}
	return out, nil
}

// routeRows merges the IPv4 main table and the IPv6 table into one list, default
// routes first. A failed IPv4 section fails the host; a failed IPv6 section only
// drops the v6 routes and marks the rows degraded.
func (b *Backend) routeRows(host resource.HostID, sec map[string]probe.SectionResult, bad, retried map[string]error) ([]resource.Resource, error) {
	if e := bad[secIPRoute]; e != nil {
		return nil, e
	}
	routes, err := ParseIPRoute(sec[secIPRoute].Stdout)
	if err != nil {
		return nil, err
	}

	degraded := copyErrs(retried)
	if e := bad[secIPRoute6]; e != nil {
		degraded[secIPRoute6] = e
	} else if v6, rerr := ParseIPRoute6(sec[secIPRoute6].Stdout); rerr != nil {
		degraded[secIPRoute6] = rerr
	} else {
		routes = append(routes, v6...)
	}
	SortRoutes(routes)

	out := make([]resource.Resource, 0, len(routes))
	seen := make(map[string]int, len(routes))
	for _, r := range routes {
		if r.Dst == "" {
			continue
		}
		name := r.Name()
		seen[name]++
		if n := seen[name]; n > 1 {
			// Two routes can share destination, next hop and device (different
			// tables or metrics); the Ref name must still be unique.
			name = fmt.Sprintf("%s #%d", name, n)
		}
		labels := b.labels(host)
		applyDegraded(labels, nil, degraded)
		out = append(out, resource.Resource{
			Ref: resource.Ref{Backend: backendName, Kind: KindRoute, Host: host, Name: name},
			Fields: map[string]any{
				"host":    string(host),
				"dst":     r.Dst,
				"gateway": r.Gateway,
				"dev":     r.Dev,
				"proto":   r.Proto,
				"metric":  int64(r.Metric),
				"src":     r.Src,
			},
			Labels:    labels,
			Status:    resource.StatusUnknown,
			Raw:       r,
			FetchedAt: b.now(),
		})
	}
	return out, nil
}

// neighRows builds one row per neighbour table entry. FAILED and INCOMPLETE
// entries are the ones that break traffic, so they warn.
func (b *Backend) neighRows(host resource.HostID, sec map[string]probe.SectionResult, bad, retried map[string]error) ([]resource.Resource, error) {
	if e := bad[secIPNeigh]; e != nil {
		return nil, e
	}
	neighs, err := ParseIPNeigh(sec[secIPNeigh].Stdout)
	if err != nil {
		return nil, err
	}

	out := make([]resource.Resource, 0, len(neighs))
	for _, n := range neighs {
		if n.IP == "" {
			continue
		}
		name := n.IP
		if n.Dev != "" {
			name += " dev " + n.Dev
		}
		status := resource.StatusOK
		if n.Unresolved() {
			status = resource.StatusWarn
		}
		labels := b.labels(host)
		applyDegraded(labels, nil, retried)
		out = append(out, resource.Resource{
			Ref: resource.Ref{Backend: backendName, Kind: KindNeigh, Host: host, Name: name},
			Fields: map[string]any{
				"host":  string(host),
				"ip":    n.IP,
				"mac":   n.MAC,
				"dev":   n.Dev,
				"state": n.State,
			},
			Labels:    labels,
			Status:    status,
			Raw:       n,
			FetchedAt: b.now(),
		})
	}
	return out, nil
}
