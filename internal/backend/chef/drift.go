// Drift signals (SPEC §8.7): how long ago each node checked in, the last run
// status when the deployment publishes one as a node attribute, and the run
// lists of a group. Everything here is a read of the Chef server through knife
// on the workstation; nothing is written and nothing reaches a node.

package chef

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"jumper/internal/errs"
	"jumper/internal/resource"
)

const (
	// driftOp is the error Op of Drift and its parser.
	driftOp = "chef.Drift"
	// driftTimeout bounds the two knife reads one drift check makes.
	driftTimeout = 90 * time.Second
)

// DriftRow is one node's drift signal, in the order the caller asked for it. A
// host Chef does not know comes back with Known false and Status NA, which is
// itself a drift signal worth showing.
type DriftRow struct {
	Host       resource.HostID
	Node       string // the Chef node name; empty when the host is not a node
	Known      bool
	RunList    []string
	Checkin    time.Duration
	HasCheckin bool            // false when the node reported no ohai_time
	Status     resource.Status // from the check-in age; NA when unknown
	RunStatus  string          // the chef.run_status_attr value; empty when hidden or unknown
}

// RunListGroup is a set of nodes whose run lists are identical.
type RunListGroup struct {
	Key     string // the run list joined, also the grouping key
	RunList []string
	Nodes   []string // sorted
}

// DriftReport is the answer of `:chef drift`: one row per requested host and
// the run-list groups behind them.
type DriftReport struct {
	// Attr is the run-status attribute path the caller asked for. Empty means
	// the column is hidden (SPEC §20 open question 3).
	Attr   string
	Rows   []DriftRow
	Groups []RunListGroup // largest first; ties lexical by Key
}

// Identical is the size of the largest run-list group: the run list the
// fleet agrees on.
func (r DriftReport) Identical() int {
	if len(r.Groups) == 0 {
		return 0
	}
	return len(r.Groups[0].Nodes)
}

// Differ is the number of nodes outside the largest run-list group.
func (r DriftReport) Differ() int {
	n := 0
	for _, g := range r.Groups {
		n += len(g.Nodes)
	}
	return n - r.Identical()
}

// Headline renders "20 identical · 2 differ". A zero part is omitted except
// for the identical count, and an empty report renders "0 identical".
func (r DriftReport) Headline() string {
	parts := []string{fmt.Sprintf("%d identical", r.Identical())}
	if d := r.Differ(); d > 0 {
		parts = append(parts, fmt.Sprintf("%d differ", d))
	}
	return strings.Join(parts, " · ")
}

// Unknown is the number of requested hosts Chef does not know.
func (r DriftReport) Unknown() int {
	n := 0
	for _, row := range r.Rows {
		if !row.Known {
			n++
		}
	}
	return n
}

// Drift reads the drift signals of hosts. statusAttr is the node attribute path
// holding the last run status ("run_status.success", …); empty hides the
// column and skips the second read. Both reads are `knife` calls on the
// workstation.
func (b *Backend) Drift(ctx context.Context, hosts []resource.HostID, statusAttr string) (DriftReport, error) {
	const op = driftOp
	statusAttr = strings.TrimSpace(statusAttr)
	if statusAttr != "" && !validAttr(statusAttr) {
		return DriftReport{}, errs.New(errs.Config, op, fmt.Sprintf("invalid run-status attribute %q", statusAttr))
	}
	if !b.configured() {
		return DriftReport{}, errs.New(errs.Config, op, "chef backend not configured: no workstation or no runner")
	}
	want := dedupeHosts(hosts)
	if len(want) == 0 {
		return DriftReport{}, errs.New(errs.Config, op, "no hosts to check")
	}

	cctx, cancel := context.WithTimeout(ctx, driftTimeout)
	defer cancel()

	nodes, err := b.status(cctx)
	if err != nil {
		return DriftReport{}, err
	}
	byNode := make(map[string]StatusNode, len(nodes))
	for _, n := range nodes {
		byNode[strings.ToLower(n.Name)] = n
	}

	var statuses map[string]string
	if statusAttr != "" {
		statuses, err = b.runStatuses(cctx, statusAttr)
		if err != nil {
			return DriftReport{}, err
		}
	}

	now := b.now()
	rep := DriftReport{Attr: statusAttr, Rows: make([]DriftRow, 0, len(want))}
	for _, host := range want {
		row := DriftRow{Host: host, Status: resource.StatusNA}
		if node, ok := matchNode(byNode, host); ok {
			row.Node, row.Known = node.Name, true
			row.RunList = append([]string(nil), node.RunList...)
			if !node.OhaiTime.IsZero() {
				age := now.Sub(node.OhaiTime)
				if age < 0 {
					age = 0
				}
				row.Checkin, row.HasCheckin = age, true
				row.Status = b.checkinStatus(age)
			}
			if statusAttr != "" {
				row.RunStatus = statuses[strings.ToLower(node.Name)]
			}
		}
		rep.Rows = append(rep.Rows, row)
	}
	rep.Groups = runListGroups(rep.Rows)
	return rep, nil
}

// runStatuses reads one attribute for every node with a single search: the
// run-status attribute is on the node objects, so one call answers for all.
func (b *Backend) runStatuses(ctx context.Context, attr string) (map[string]string, error) {
	out, err := b.runner.Run(ctx, "search", "node", "*:*", "-a", attr)
	if err != nil {
		return nil, err
	}
	return ParseRunStatus(out, attr)
}

// ParseRunStatus decodes `knife search node <query> -a <attr> -F json` into a
// node name → value map, keyed lower-cased. The value is the attribute itself
// or the object knife returns for it; the rest of the path is followed into it.
// Rows and values the parser cannot understand are skipped, never fatal.
func ParseRunStatus(b []byte, attr string) (map[string]string, error) {
	data, err := FirstJSON(b)
	if err != nil {
		return nil, err
	}
	var res struct {
		Rows []map[string]json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, errs.New(errs.Parse, driftOp, "unexpected knife search output: "+snippet(data))
	}
	path := strings.Split(strings.TrimSpace(attr), ".")
	out := make(map[string]string, len(res.Rows))
	for _, row := range res.Rows {
		for name, raw := range row {
			if v, ok := digAttr(raw, path); ok && v != "" {
				out[strings.ToLower(strings.TrimSpace(name))] = v
			}
		}
	}
	return out, nil
}

// digAttr follows path into raw and returns the leaf as a string. knife hands
// back the leaf itself when the whole path matched, so a scalar at any depth
// is the answer. An object leaf yields its first key in sorted order, which
// keeps the answer deterministic.
func digAttr(raw json.RawMessage, path []string) (string, bool) {
	cur := raw
	for _, seg := range path {
		if s, ok := rawString(cur); ok {
			return s, true
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(cur, &obj) != nil {
			return "", false
		}
		next, ok := obj[seg]
		if !ok {
			return "", false
		}
		cur = next
	}
	if s, ok := rawString(cur); ok {
		return s, true
	}
	return firstScalar(cur)
}

// firstScalar returns the first scalar field of an object, sorted by key.
func firstScalar(raw json.RawMessage) (string, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return "", false
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s, ok := rawString(obj[k]); ok {
			return s, true
		}
	}
	return "", false
}

// matchNode finds the Chef node a host id names. A host is matched by its full
// id or by its first label, because Chef node names are short ("web-01") while
// an inventory host may be an FQDN.
func matchNode(byNode map[string]StatusNode, host resource.HostID) (StatusNode, bool) {
	full := strings.ToLower(string(host))
	if n, ok := byNode[full]; ok {
		return n, true
	}
	short := full
	if i := strings.IndexByte(short, '.'); i > 0 {
		short = short[:i]
	}
	n, ok := byNode[short]
	return n, ok
}

// runListGroups groups the known rows by their run list: largest group first,
// then lexical by key, with the node names sorted inside each group.
func runListGroups(rows []DriftRow) []RunListGroup {
	byKey := make(map[string]*RunListGroup, len(rows))
	for _, row := range rows {
		if !row.Known {
			continue
		}
		key := strings.Join(row.RunList, ",")
		g := byKey[key]
		if g == nil {
			g = &RunListGroup{Key: key, RunList: append([]string(nil), row.RunList...)}
			byKey[key] = g
		}
		g.Nodes = append(g.Nodes, row.Node)
	}
	out := make([]RunListGroup, 0, len(byKey))
	for _, g := range byKey {
		sort.Strings(g.Nodes)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Nodes) != len(out[j].Nodes) {
			return len(out[i].Nodes) > len(out[j].Nodes)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// dedupeHosts normalises, drops empty and repeated host ids, keeping order.
func dedupeHosts(hosts []resource.HostID) []resource.HostID {
	out := make([]resource.HostID, 0, len(hosts))
	seen := make(map[resource.HostID]bool, len(hosts))
	for _, h := range hosts {
		n := resource.NormalizeHostID(string(h))
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}
