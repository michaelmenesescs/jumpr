// Nodegroups, beacons and schedules (SPEC §8.6, task 097).
//
// A nodegroup is a named target expression the master's config defines; it is
// read with `salt-run config.get nodegroups` and resolved to members with
// `salt … --preview-target -N <name>`, so jumpr never guesses what a group
// contains. Beacons and schedules are per-minion read-only lists, asked for
// with `return_yaml=False` so the master answers with plain data instead of a
// YAML document.

package salt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
)

const (
	// nodegroupsOp is the error Op of the nodegroup reads.
	nodegroupsOp = "salt.nodegroups"
	// beaconsOp and schedulesOp are the error Ops of the two list reads.
	beaconsOp   = "salt.beacons"
	schedulesOp = "salt.schedule"
	// returnsFalse is the kwarg that asks for data instead of YAML.
	returnsFalse = "False"
)

// nodegroupNameRe matches a nodegroup name Salt can take after -N.
var nodegroupNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// nodegroupSetPrefix is the selector set prefix that names a Salt nodegroup:
// `@salt:webs`.
const nodegroupSetPrefix = "salt:"

// Nodegroup is one nodegroup: the name, the expression the config holds (a
// render of the list form when the config gives one) and the minions
// --preview-target resolved it to. Err records why the members are unknown;
// the group is still listed.
type Nodegroup struct {
	Name    string
	Expr    string
	Members []string
	Err     error
}

// Count is the number of members the preview returned.
func (g Nodegroup) Count() int { return len(g.Members) }

// ParseNodegroups decodes `salt-run config.get nodegroups`. The value is a map
// from name to either an expression string or a list of minion IDs; anything
// else is skipped, because remote config is data, not a contract.
func ParseNodegroups(b []byte) ([]Nodegroup, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	var raw map[string]json.RawMessage
	if err := DecodeInto(b, &raw); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]Nodegroup, 0, len(names))
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			continue
		}
		value := bytes.TrimSpace(raw[name])
		if len(value) == 0 || isNull(value) {
			continue
		}
		switch value[0] {
		case '"':
			var expr string
			if err := json.Unmarshal(value, &expr); err != nil {
				continue
			}
			if expr = strings.TrimSpace(expr); expr == "" {
				continue
			}
			out = append(out, Nodegroup{Name: name, Expr: expr})
		case '[':
			var ids []string
			if err := json.Unmarshal(value, &ids); err != nil {
				continue
			}
			members := sortedMinions(ids)
			if len(members) == 0 {
				continue
			}
			out = append(out, Nodegroup{Name: name, Expr: strings.Join(members, ", "), Members: members})
		}
	}
	if len(out) == 0 && len(raw) > 0 {
		return nil, errs.New(errs.Parse, nodegroupsOp, "config.get nodegroups answered no nodegroups")
	}
	return out, nil
}

// Nodegroups lists the master's nodegroups with their members. A group whose
// preview fails is kept with Err set: one bad group must not hide the others.
func (b *Backend) Nodegroups(ctx context.Context) ([]Nodegroup, error) {
	if b.runner == nil {
		return nil, errs.New(errs.Config, nodegroupsOp, "salt backend not configured: no runner")
	}
	out, err := b.runner.Run(ctx, Call{Tool: toolRun, Fun: "config.get", Args: []string{"nodegroups"}})
	if err != nil {
		return nil, err
	}
	groups, err := ParseNodegroups(out)
	if err != nil {
		return nil, err
	}
	for i := range groups {
		if groups[i].Members != nil {
			continue
		}
		members, perr := b.PreviewTarget(ctx, Target{Type: "nodegroup", Expr: groups[i].Name})
		if perr != nil {
			groups[i].Err = perr
			continue
		}
		groups[i].Members = members
	}
	return groups, nil
}

// NodegroupSet returns the nodegroup name a selector references when it is
// exactly one `@salt:<name>` set and nothing else. Any other shape reports
// false: a nodegroup only compiles natively as the whole selector.
func NodegroupSet(sel resource.Selector) (string, bool) {
	if len(sel.Terms) != 0 || len(sel.Sets) != 1 {
		return "", false
	}
	name, ok := strings.CutPrefix(sel.Sets[0], nodegroupSetPrefix)
	if !ok {
		return "", false
	}
	name = strings.TrimSpace(name)
	if !nodegroupNameRe.MatchString(name) {
		return "", false
	}
	return name, true
}

// NodegroupTarget is the -N target of a selector that is exactly one
// `@salt:<name>` set.
func NodegroupTarget(sel resource.Selector) (Target, bool) {
	name, ok := NodegroupSet(sel)
	if !ok {
		return Target{}, false
	}
	return Target{Type: "nodegroup", Expr: name}, true
}

// NodegroupMinions resolves one nodegroup through `--preview-target -N name`.
func (b *Backend) NodegroupMinions(ctx context.Context, name string) ([]string, error) {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "@")
	name = strings.TrimPrefix(name, nodegroupSetPrefix)
	if !nodegroupNameRe.MatchString(name) {
		return nil, errs.New(errs.Config, nodegroupsOp, fmt.Sprintf("invalid nodegroup name %q", name))
	}
	return b.PreviewTarget(ctx, Target{Type: "nodegroup", Expr: name})
}

// BeaconRow is one beacon of one minion.
type BeaconRow struct {
	Minion  string
	Name    string
	Enabled bool
	// Summary is a one-line render of the beacon's own settings.
	Summary string
}

// ScheduleRow is one schedule entry of one minion.
type ScheduleRow struct {
	Minion  string
	Name    string
	Enabled bool
	// Next is the interval or next fire time the schedule config implies.
	Next string
}

// beaconsOrSchedules reads one per-minion list with return_yaml=False and
// returns the decoded per-minion returns, keyed by minion ID.
func (b *Backend) beaconsOrSchedules(ctx context.Context, minions []string, fun, op string) (map[string]json.RawMessage, error) {
	if b.runner == nil {
		return nil, errs.New(errs.Config, op, "salt backend not configured: no runner")
	}
	ids := sortedMinions(minions)
	if len(ids) == 0 {
		return nil, errs.New(errs.Config, op, "no minions to target")
	}
	call := Call{
		Tool:   toolSalt,
		Target: &Target{Type: "list", Expr: strings.Join(ids, ",")},
		Fun:    fun,
		Kwargs: map[string]string{"return_yaml": returnsFalse},
	}
	out, err := b.runner.Run(ctx, call)
	if err != nil {
		return nil, err
	}
	return DecodeObject(out)
}

// Beacons reads beacons.list for the given minions.
func (b *Backend) Beacons(ctx context.Context, minions []string) ([]BeaconRow, error) {
	byMinion, err := b.beaconsOrSchedules(ctx, minions, "beacons.list", beaconsOp)
	if err != nil {
		return nil, err
	}
	return ParseBeacons(byMinion), nil
}

// Schedules reads schedule.list for the given minions.
func (b *Backend) Schedules(ctx context.Context, minions []string) ([]ScheduleRow, error) {
	byMinion, err := b.beaconsOrSchedules(ctx, minions, "schedule.list", schedulesOp)
	if err != nil {
		return nil, err
	}
	return ParseSchedules(byMinion), nil
}

// ParseBeacons turns per-minion beacons.list returns into rows. A minion that
// did not answer with an object contributes nothing.
func ParseBeacons(byMinion map[string]json.RawMessage) []BeaconRow {
	minions := sortedKeys(byMinion)
	var rows []BeaconRow
	for _, minion := range minions {
		configs := perNameConfigs(byMinion[minion])
		names := make([]string, 0, len(configs))
		for name := range configs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			row := BeaconRow{Minion: minion, Name: name, Enabled: enabledOf(configs[name])}
			row.Summary = configSummary(configs[name], "enabled")
			rows = append(rows, row)
		}
	}
	return rows
}

// ParseSchedules turns per-minion schedule.list returns into rows.
func ParseSchedules(byMinion map[string]json.RawMessage) []ScheduleRow {
	minions := sortedKeys(byMinion)
	var rows []ScheduleRow
	for _, minion := range minions {
		configs := perNameConfigs(byMinion[minion])
		names := make([]string, 0, len(configs))
		for name := range configs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			row := ScheduleRow{Minion: minion, Name: name, Enabled: enabledOf(configs[name])}
			row.Next = scheduleNext(configs[name])
			rows = append(rows, row)
		}
	}
	return rows
}

// perNameConfigs decodes one minion's answer into name → settings object.
// Anything that is not an object of objects is skipped.
func perNameConfigs(raw json.RawMessage) map[string]map[string]json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &top); err != nil {
		return nil
	}
	out := make(map[string]map[string]json.RawMessage, len(top))
	for name, value := range top {
		t := bytes.TrimSpace(value)
		if len(t) == 0 || t[0] != '{' {
			continue
		}
		var settings map[string]json.RawMessage
		if err := json.Unmarshal(t, &settings); err != nil {
			continue
		}
		out[name] = settings
	}
	return out
}

// enabledOf reports whether a config object is enabled. Salt omits the key for
// data that is on by default, so a missing key counts as enabled.
func enabledOf(settings map[string]json.RawMessage) bool {
	raw, ok := settings["enabled"]
	if !ok {
		return true
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return true
	}
	return b
}

// configSummary renders a config object as sorted "k=v" pairs of its scalar
// values, skipping the named keys and nested data.
func configSummary(settings map[string]json.RawMessage, skip ...string) string {
	omit := make(map[string]bool, len(skip))
	for _, k := range skip {
		omit[k] = true
	}
	keys := make([]string, 0, len(settings))
	for k := range settings {
		if omit[k] || strings.HasPrefix(k, "__") {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if v, ok := scalarValue(settings[k]); ok && v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, ", ")
}

// scheduleIntervalKeys are the interval forms a schedule config can use, in the
// order they are rendered.
var scheduleIntervalKeys = []string{"seconds", "minutes", "hours", "days", "weeks"}

// scheduleNext renders when a schedule runs: its next fire time when the master
// reports one, else the function and interval it is configured with.
func scheduleNext(settings map[string]json.RawMessage) string {
	parts := make([]string, 0, 3)
	for _, key := range []string{"function", "when", "cron"} {
		if v, ok := scalarValue(settings[key]); ok && v != "" {
			parts = append(parts, key+" "+v)
		}
	}
	for _, key := range scheduleIntervalKeys {
		v, ok := scalarValue(settings[key])
		if !ok || v == "" {
			continue
		}
		unit := strings.TrimSuffix(key, "s")
		if n, err := strconv.Atoi(v); err == nil && n == 1 {
			parts = append(parts, "every 1 "+unit)
		} else {
			parts = append(parts, "every "+v+" "+key)
		}
	}
	if v, ok := scalarValue(settings["next_fire_time"]); ok && v != "" {
		parts = append(parts, "next "+v)
	}
	return strings.Join(parts, ", ")
}

// NodegroupRows renders nodegroups as table cells: name, member count, the
// expression, and the error kind when the preview failed.
func NodegroupRows(groups []Nodegroup) [][]string {
	rows := make([][]string, 0, len(groups))
	for _, g := range groups {
		count := strconv.Itoa(g.Count())
		note := ""
		if g.Err != nil {
			count = "?"
			note = string(errs.KindOf(g.Err))
		}
		rows = append(rows, []string{g.Name, count, g.Expr, note})
	}
	return rows
}

// BeaconRows renders beacons as table cells: minion, name, enabled, settings.
func BeaconRows(rows []BeaconRow) [][]string {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{r.Minion, r.Name, yesNo(r.Enabled), r.Summary})
	}
	return out
}

// ScheduleRows renders schedules as table cells: minion, name, enabled, when.
func ScheduleRows(rows []ScheduleRow) [][]string {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{r.Minion, r.Name, yesNo(r.Enabled), r.Next})
	}
	return out
}

// yesNo renders a bool as the table word.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
