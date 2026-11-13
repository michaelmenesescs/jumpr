package linux

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"jumper/internal/errs"
)

// Parser names. They double as the Op of a parser error and the ID of the
// probe section that produced the input.
const (
	ipProbeLink   = "ip-link"
	ipProbeAddr   = "ip-addr"
	ipProbeRoute  = "ip-route"
	ipProbeRoute6 = "ip-route6"
	ipProbeNeigh  = "ip-neigh"
)

// IPLink is one row of `ip -j -s link`: the interface name, its operational
// state, MTU, master bridge and byte/error/drop counters.
type IPLink struct {
	Name      string
	State     string
	MTU       int
	Master    string
	RXBytes   int64
	TXBytes   int64
	RXErrors  int64
	TXErrors  int64
	RXDropped int64
	TXDropped int64
}

// Down reports whether the interface is operationally down.
func (l IPLink) Down() bool { return strings.EqualFold(l.State, "DOWN") }

// CounterWarnings reports a non-zero error or drop counter in either direction.
func (l IPLink) CounterWarnings() bool {
	return l.RXErrors+l.TXErrors+l.RXDropped+l.TXDropped > 0
}

// ipCounters is one direction's statistics block. Only the three counters the
// interface kind shows are kept; everything else is ignored.
type ipCounters struct {
	Bytes   int64 `json:"bytes"`
	Errors  int64 `json:"errors"`
	Dropped int64 `json:"dropped"`
}

// ipLinkStats is the `stats64` (or older `stats`) object of one link.
type ipLinkStats struct {
	RX ipCounters `json:"rx"`
	TX ipCounters `json:"tx"`
}

// ipLinkDoc is one element of `ip -j -s link`. Unknown fields are ignored.
type ipLinkDoc struct {
	Name      string       `json:"ifname"`
	MTU       int          `json:"mtu"`
	OperState string       `json:"operstate"`
	Master    string       `json:"master"`
	Flags     []string     `json:"flags"`
	Stats64   *ipLinkStats `json:"stats64"`
	Stats     *ipLinkStats `json:"stats"`
}

// stateOf falls back to the flags when iproute2 omits operstate.
func (d ipLinkDoc) stateOf() string {
	if d.OperState != "" {
		return strings.ToUpper(d.OperState)
	}
	for _, f := range d.Flags {
		if strings.EqualFold(f, "UP") {
			return "UP"
		}
	}
	if len(d.Flags) > 0 {
		return "DOWN"
	}
	return ""
}

// ParseIPLinks parses `ip -j -s link`. Empty output or `[]` yields no rows;
// malformed JSON is an errs.Parse error.
func ParseIPLinks(out string) ([]IPLink, error) {
	var docs []ipLinkDoc
	if err := decodeIPArray(out, ipProbeLink, &docs); err != nil {
		return nil, err
	}
	links := make([]IPLink, 0, len(docs))
	for _, d := range docs {
		link := IPLink{
			Name:   d.Name,
			State:  d.stateOf(),
			MTU:    d.MTU,
			Master: d.Master,
		}
		stats := d.Stats64
		if stats == nil {
			stats = d.Stats
		}
		if stats != nil {
			link.RXBytes = stats.RX.Bytes
			link.TXBytes = stats.TX.Bytes
			link.RXErrors = stats.RX.Errors
			link.TXErrors = stats.TX.Errors
			link.RXDropped = stats.RX.Dropped
			link.TXDropped = stats.TX.Dropped
		}
		links = append(links, link)
	}
	return links, nil
}

// IPAddr is one interface's addresses as CIDR strings, IPv4 first. Master is
// the bridge the interface belongs to, when iproute2 reports one.
type IPAddr struct {
	Name   string
	Master string
	Addrs  []string
}

// ipAddrInfo is one element of a link's `addr_info` array. Only inet and inet6
// entries are kept.
type ipAddrInfo struct {
	Family    string `json:"family"`
	Local     string `json:"local"`
	PrefixLen int    `json:"prefixlen"`
}

// ipAddrDoc is one element of `ip -j addr`.
type ipAddrDoc struct {
	Name     string       `json:"ifname"`
	Master   string       `json:"master"`
	AddrInfo []ipAddrInfo `json:"addr_info"`
}

// ParseIPAddrs parses `ip -j addr` into the CIDRs of every interface, IPv4
// first. Link-layer entries and entries without a local address are skipped.
func ParseIPAddrs(out string) ([]IPAddr, error) {
	var docs []ipAddrDoc
	if err := decodeIPArray(out, ipProbeAddr, &docs); err != nil {
		return nil, err
	}
	addrs := make([]IPAddr, 0, len(docs))
	for _, d := range docs {
		v4 := make([]string, 0, len(d.AddrInfo))
		v6 := make([]string, 0, len(d.AddrInfo))
		for _, ai := range d.AddrInfo {
			if ai.Local == "" {
				continue
			}
			cidr := ai.Local + "/" + strconv.Itoa(ai.PrefixLen)
			switch ai.Family {
			case "inet":
				v4 = append(v4, cidr)
			case "inet6":
				v6 = append(v6, cidr)
			}
		}
		addrs = append(addrs, IPAddr{Name: d.Name, Master: d.Master, Addrs: append(v4, v6...)})
	}
	return addrs, nil
}

// IPRoute is one row of `ip -j route`: destination, next hop, device, protocol,
// metric and preferred source.
type IPRoute struct {
	Dst     string
	Gateway string
	Dev     string
	Proto   string
	Metric  int
	Src     string
}

// Name is the row's Ref name: destination, then the next hop and device when
// they are set.
func (r IPRoute) Name() string {
	var sb strings.Builder
	sb.WriteString(r.Dst)
	if r.Gateway != "" {
		sb.WriteString(" via " + r.Gateway)
	}
	if r.Dev != "" {
		sb.WriteString(" dev " + r.Dev)
	}
	return sb.String()
}

// Default reports whether this is a default route.
func (r IPRoute) Default() bool { return r.Dst == "default" || r.Dst == "::/0" || r.Dst == "0.0.0.0/0" }

// ipRouteDoc is one element of `ip -j route`. iproute2 calls the protocol
// "protocol" and the preferred source "prefsrc".
type ipRouteDoc struct {
	Dst      string `json:"dst"`
	Gateway  string `json:"gateway"`
	Dev      string `json:"dev"`
	Protocol string `json:"protocol"`
	Metric   int    `json:"metric"`
	PrefSrc  string `json:"prefsrc"`
}

// ParseIPRoute parses one `ip -j route` document.
func ParseIPRoute(out string) ([]IPRoute, error) { return parseIPRoutes(out, ipProbeRoute) }

// ParseIPRoute6 parses one `ip -j -6 route` document. It has the same shape as
// the IPv4 table.
func ParseIPRoute6(out string) ([]IPRoute, error) { return parseIPRoutes(out, ipProbeRoute6) }

func parseIPRoutes(out, op string) ([]IPRoute, error) {
	var docs []ipRouteDoc
	if err := decodeIPArray(out, op, &docs); err != nil {
		return nil, err
	}
	routes := make([]IPRoute, 0, len(docs))
	for _, d := range docs {
		routes = append(routes, IPRoute{
			Dst:     d.Dst,
			Gateway: d.Gateway,
			Dev:     d.Dev,
			Proto:   d.Protocol,
			Metric:  d.Metric,
			Src:     d.PrefSrc,
		})
	}
	return routes, nil
}

// SortRoutes puts the default routes first and keeps the kernel's order within
// each group, so the table reads top-down the way a routing decision is made.
func SortRoutes(routes []IPRoute) {
	sort.SliceStable(routes, func(i, j int) bool {
		return routes[i].Default() && !routes[j].Default()
	})
}

// IPNeigh is one row of `ip -j neigh`: a neighbour table entry.
type IPNeigh struct {
	IP    string
	MAC   string
	Dev   string
	State string
}

// ipNeighState decodes a state that iproute2 prints as an array and some
// versions print as a bare string.
type ipNeighState []string

func (s *ipNeighState) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		*s = nil
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var list []string
		if err := json.Unmarshal(b, &list); err != nil {
			*s = nil
			return nil
		}
		*s = list
		return nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err != nil {
		*s = nil
		return nil
	}
	*s = []string{one}
	return nil
}

// ipNeighDoc is one element of `ip -j neigh`.
type ipNeighDoc struct {
	Dst    string       `json:"dst"`
	LLAddr string       `json:"lladdr"`
	Dev    string       `json:"dev"`
	State  ipNeighState `json:"state"`
}

// ParseIPNeigh parses `ip -j neigh`. Entries dropped from the table carry no
// link-layer address and no state; they are kept, because "FAILED" is the
// interesting case.
func ParseIPNeigh(out string) ([]IPNeigh, error) {
	var docs []ipNeighDoc
	if err := decodeIPArray(out, ipProbeNeigh, &docs); err != nil {
		return nil, err
	}
	neighs := make([]IPNeigh, 0, len(docs))
	for _, d := range docs {
		neighs = append(neighs, IPNeigh{
			IP:    d.Dst,
			MAC:   d.LLAddr,
			Dev:   d.Dev,
			State: strings.Join(d.State, ","),
		})
	}
	return neighs, nil
}

// Unresolved reports a neighbour entry that has no usable link-layer address.
func (n IPNeigh) Unresolved() bool {
	for _, s := range strings.Split(n.State, ",") {
		switch strings.ToUpper(strings.TrimSpace(s)) {
		case "FAILED", "INCOMPLETE":
			return true
		}
	}
	return false
}

// decodeIPArray decodes an `ip -j` document into docs. Empty output and `[]`
// are an empty list, not an error; malformed JSON is errs.Parse naming op.
func decodeIPArray(out, op string, docs any) error {
	s := strings.TrimSpace(out)
	if s == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(s), docs); err != nil {
		return errs.New(errs.Parse, op, "invalid json: "+err.Error())
	}
	return nil
}
