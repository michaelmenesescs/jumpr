// Alert-to-host resolution (SPEC W1): the labels of a firing alert become the
// target set of an investigation, and a configured metric column becomes one
// value plus a window sparkline per host.
//
// The resolution rules are deliberately conservative: a direct host label wins,
// a selector built from labels the inventory also knows comes second, and
// anything ambiguous (no host at all, or too many) resolves to nothing with a
// reason the caller flashes. Opening an investigation on 900 hosts is never a
// helpful default.

package prom

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"jumpr/internal/inventory"
	"jumpr/internal/resource"
)

// DefaultHostLabelKeys are the alert label keys tried in order when the caller
// configures none (config alerts.host_labels). exported_instance comes last: it
// is the scrape target's own name, not the host it runs for.
var DefaultHostLabelKeys = []string{
	"instance",
	"host",
	"hostname",
	"node",
	"nodename",
	"exported_instance",
}

// MaxAlertTargets is the most hosts an alert may expand to. A selector that
// matches more is treated as "no target set": the investigation still opens,
// but with nothing in it.
const MaxAlertTargets = 500

// HostsFromLabels resolves an alert's labels to inventory hosts. It tries keys
// in order, strips a :port, applies the inventory's identity rewrites and
// resolves each name. When no host label resolves it builds a selector from the
// remaining labels that are also inventory label keys (dc=ewr service=api
// becomes dc=ewr) and selects on it. It returns nothing when that selector
// matches no host or more than MaxAlertTargets hosts.
func HostsFromLabels(labels map[string]string, inv inventory.Inventory, keys []string) []resource.HostID {
	hosts, _ := ResolveAlertHosts(labels, inv, keys)
	return hosts
}

// ResolveAlertHosts is HostsFromLabels plus the reason the set is empty, for the
// flash line. reason is empty when hosts is non-empty.
func ResolveAlertHosts(labels map[string]string, inv inventory.Inventory, keys []string) (hosts []resource.HostID, reason string) {
	if inv == nil {
		return nil, "no inventory configured"
	}
	if len(keys) == 0 {
		keys = DefaultHostLabelKeys
	}
	if h, ok := hostsFromHostLabels(labels, inv, keys); ok {
		return h, ""
	}

	sel, ok := alertSelector(labels, inv, keys)
	if !ok {
		return nil, "the alert names no host and no label the inventory knows"
	}
	matched := inv.Select(sel)
	switch {
	case len(matched) == 0:
		return nil, "no inventory host matches " + sel.String()
	case len(matched) > MaxAlertTargets:
		return nil, sel.String() + " matches " + pluralHosts(len(matched)) + " (limit " +
			pluralHosts(MaxAlertTargets) + ")"
	}
	out := make([]resource.HostID, 0, len(matched))
	seen := make(map[resource.HostID]bool, len(matched))
	for _, h := range matched {
		if h.ID == "" || seen[h.ID] {
			continue
		}
		seen[h.ID] = true
		out = append(out, h.ID)
	}
	if len(out) == 0 {
		return nil, "no inventory host matches " + sel.String()
	}
	sortHosts(out)
	return out, ""
}

// hostsFromHostLabels tries the configured keys in order and returns the first
// key that resolves to exactly one host.
func hostsFromHostLabels(labels map[string]string, inv inventory.Inventory, keys []string) ([]resource.HostID, bool) {
	for _, key := range keys {
		raw := strings.TrimSpace(labels[key])
		if raw == "" {
			continue
		}
		name := instanceHost(raw)
		if name == "" {
			continue
		}
		canonical := inventory.CanonicalHost(inv, string(name))
		if canonical == "" {
			continue
		}
		if host, ok := inv.Resolve(string(canonical)); ok && host.ID != "" {
			return []resource.HostID{host.ID}, true
		}
	}
	return nil, false
}

// alertSelector builds a selector from the alert labels that are also
// inventory label keys, skipping the host label keys themselves (a host label
// that did not resolve must not turn into a selector that matches nothing).
func alertSelector(labels map[string]string, inv inventory.Inventory, keys []string) (resource.Selector, bool) {
	known := inventoryLabelKeys(inv)
	skip := make(map[string]bool, len(keys))
	for _, k := range keys {
		skip[k] = true
	}

	names := make([]string, 0, len(labels))
	for k := range labels {
		if skip[k] {
			continue
		}
		if _, ok := known[k]; !ok {
			continue
		}
		if strings.TrimSpace(labels[k]) == "" {
			continue
		}
		names = append(names, k)
	}
	if len(names) == 0 {
		return resource.Selector{}, false
	}
	sort.Strings(names)
	terms := make([]resource.Term, 0, len(names))
	for _, k := range names {
		terms = append(terms, resource.Term{Key: k, Op: resource.OpEq, Value: labels[k]})
	}
	return resource.Selector{Terms: terms}, true
}

// inventoryLabelKeys is every label key the inventory's hosts carry.
func inventoryLabelKeys(inv inventory.Inventory) map[string]bool {
	out := make(map[string]bool)
	for _, h := range inv.Hosts() {
		for k := range h.Labels {
			out[k] = true
		}
	}
	return out
}

// sortHosts sorts a host list in place.
func sortHosts(hosts []resource.HostID) {
	sort.Slice(hosts, func(i, j int) bool { return hosts[i] < hosts[j] })
}

// pluralHosts renders a host count: "1 host", "504 hosts".
func pluralHosts(n int) string {
	if n == 1 {
		return "1 host"
	}
	return strconv.Itoa(n) + " hosts"
}

// HostColumn is one metrics.host_columns entry: a metric shown per host. Expr
// is PromQL; $host and $instance are replaced by the host's identity.
type HostColumn struct {
	ID    string
	Title string
	Expr  string
}

// HostValue is one host's answer for one column over the session window.
type HostValue struct {
	Value float64
	Spark []float64
}

// HostColumnChunk is the most hosts one batched metric query names. Every
// column costs one instant and one range query per chunk, never one per host.
const HostColumnChunk = 200

// SubstituteHost replaces $host and $instance in expr with host.
func SubstituteHost(expr string, host resource.HostID) string {
	r := strings.NewReplacer("$host", string(host), "$instance", string(host))
	return r.Replace(expr)
}

// SubstituteHosts replaces $host and $instance in expr with a regex alternation
// of hosts, for a matcher written as instance=~"$instance". Hosts are escaped,
// so a name with a dot stays a literal dot.
func SubstituteHosts(expr string, hosts []resource.HostID) string {
	parts := make([]string, 0, len(hosts))
	for _, h := range hosts {
		parts = append(parts, regexp.QuoteMeta(string(h)))
	}
	alt := strings.Join(parts, "|")
	r := strings.NewReplacer("$host", alt, "$instance", alt)
	return r.Replace(expr)
}

// QueryHostColumns evaluates every column for hosts over [from, to] and
// returns one value and sparkline per host and column. Queries are batched:
// one instant and one range query per column per HostColumnChunk hosts. A host
// a column has no series for is simply absent from its column.
func QueryHostColumns(ctx context.Context, q Querier, cols []HostColumn, hosts []resource.HostID, from, to time.Time) (map[resource.HostID]map[string]HostValue, error) {
	out := make(map[resource.HostID]map[string]HostValue, len(hosts))
	if q == nil || len(cols) == 0 || len(hosts) == 0 {
		return out, nil
	}
	step := Step(from, to, MaxPoints)
	chunks := chunkHosts(hosts, HostColumnChunk)

	for _, col := range cols {
		expr := strings.TrimSpace(col.Expr)
		if expr == "" || strings.TrimSpace(col.ID) == "" {
			continue
		}
		batches := chunks
		if !strings.Contains(expr, "$host") && !strings.Contains(expr, "$instance") {
			// Nothing to substitute: one query answers for every host.
			batches = [][]resource.HostID{hosts}
		}
		for _, chunk := range batches {
			qexpr := SubstituteHosts(expr, chunk)
			vec, err := q.Query(ctx, qexpr, to)
			if err != nil {
				return out, err
			}
			mat, err := q.QueryRange(ctx, qexpr, from, to, step)
			if err != nil {
				return out, err
			}
			sparks := make(map[resource.HostID][]float64, len(mat))
			for _, m := range mat {
				if h := seriesHost(m.Metric, chunk); h != "" {
					sparks[h] = values(m.Values)
				}
			}
			for _, s := range vec {
				h := seriesHost(s.Metric, chunk)
				if h == "" {
					continue
				}
				val := 0.0
				if n := len(s.Values); n > 0 {
					val = s.Values[n-1].V
				}
				if out[h] == nil {
					out[h] = make(map[string]HostValue, len(cols))
				}
				out[h][col.ID] = HostValue{Value: val, Spark: sparks[h]}
			}
		}
	}
	return out, nil
}

// seriesHost derives the host a series belongs to, falling back to the single
// host of the batch when the expression carries no host label.
func seriesHost(labels map[string]string, chunk []resource.HostID) resource.HostID {
	for _, key := range []string{"instance", "host", "node", "nodename"} {
		if raw := strings.TrimSpace(labels[key]); raw != "" {
			h := instanceHost(raw)
			if h != "" {
				return h
			}
		}
	}
	if len(chunk) == 1 {
		return chunk[0]
	}
	return ""
}

// chunkHosts splits hosts into chunks of at most n.
func chunkHosts(hosts []resource.HostID, n int) [][]resource.HostID {
	if n <= 0 {
		n = len(hosts)
	}
	out := make([][]resource.HostID, 0, (len(hosts)+n-1)/n)
	for len(hosts) > 0 {
		size := n
		if size > len(hosts) {
			size = len(hosts)
		}
		out = append(out, hosts[:size:size])
		hosts = hosts[size:]
	}
	return out
}

// MetricsBackend is a Prometheus backend decorated with the host metric
// columns from metrics.host_columns. Register it instead of the plain backend
// when the config names columns; hosts views find it through HostColumns.
type MetricsBackend struct {
	*Backend
	Columns []HostColumn
}

// NewMetricsBackend wraps b with the configured host columns.
func NewMetricsBackend(b *Backend, cols []HostColumn) *MetricsBackend {
	return &MetricsBackend{Backend: b, Columns: append([]HostColumn(nil), cols...)}
}

// HostColumns returns the configured columns.
func (m *MetricsBackend) HostColumns() []HostColumn { return m.Columns }

// DefaultHostColumns is the default value of metrics.host_columns: none.
func DefaultHostColumns() []HostColumn { return nil }
