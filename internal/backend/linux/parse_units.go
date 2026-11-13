package linux

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"jumper/internal/resource"
)

// Unit is one row of `systemctl list-units --all --plain --no-legend --no-pager`.
type Unit struct {
	Name        string
	Load        string
	Active      string
	Sub         string
	Description string
}

// Detail is the subset of `systemctl show` properties the unit detail view needs.
type Detail struct {
	ActiveState            string
	SubState               string
	Result                 string
	NRestarts              int
	ExecMainStatus         int
	ExecMainPID            string
	MemoryCurrent          int64 // -1 when not set / not representable
	TasksCurrent           int
	ActiveEnterTimestamp   time.Time
	InactiveEnterTimestamp time.Time
	FragmentPath           string
	DropInPaths            []string
	UnitFileState          string
}

// Type is the unit type: the suffix after the last dot ("service", "timer", ...).
// A name without a dot has no type and yields "".
func (u Unit) Type() string { return unitXType(u.Name) }

// Status maps the unit's active/sub state onto a resource status.
func (u Unit) Status() resource.Status { return unitXStatus(u.Active, u.Sub) }

func unitXType(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return ""
}

func unitXStatus(active, sub string) resource.Status {
	switch active {
	case "failed":
		return resource.StatusCrit
	case "activating", "deactivating", "reloading":
		return resource.StatusWarn
	}
	if sub == "auto-restart" {
		return resource.StatusWarn
	}
	if active == "active" {
		return resource.StatusOK
	}
	return resource.StatusUnknown
}

// ParseListUnits parses the plain-text output of
// `systemctl list-units --all --plain --no-legend --no-pager`.
// Lines with fewer than four fields are skipped; a leading bullet or "*" on a
// failed unit is dropped; the remaining fields form the description. Blank
// output yields an empty slice.
func ParseListUnits(out string) ([]Unit, error) {
	var units []Unit
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && (fields[0] == "●" || fields[0] == "*") {
			fields = fields[1:]
		}
		if len(fields) < 4 {
			continue
		}
		units = append(units, Unit{
			Name:        fields[0],
			Load:        fields[1],
			Active:      fields[2],
			Sub:         fields[3],
			Description: strings.Join(fields[4:], " "),
		})
	}
	return units, nil
}

// ParseShow parses `systemctl show <unit> --no-pager` properties. Each line is
// split at its first "="; repeated keys are joined with "\n" in output order.
func ParseShow(out string) map[string]string {
	props := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSuffix(line, "\r")
		i := strings.IndexByte(line, '=')
		if i < 0 {
			continue
		}
		key, val := line[:i], line[i+1:]
		if prev, ok := props[key]; ok {
			props[key] = prev + "\n" + val
			continue
		}
		props[key] = val
	}
	return props
}

// UnitDetail extracts the fields of a `systemctl show` property map that the unit
// detail view displays. Missing or unparseable values are left at their zero
// value; MemoryCurrent is -1 when not set or out of int64 range.
func UnitDetail(props map[string]string) Detail {
	var d Detail
	d.ActiveState = props["ActiveState"]
	d.SubState = props["SubState"]
	d.Result = props["Result"]
	d.NRestarts = unitXInt(props["NRestarts"])
	d.ExecMainStatus = unitXInt(props["ExecMainStatus"])
	d.ExecMainPID = props["ExecMainPID"]
	d.MemoryCurrent = unitXInt64OrNotSet(props["MemoryCurrent"])
	d.TasksCurrent = unitXInt(props["TasksCurrent"])
	d.ActiveEnterTimestamp = unitXTime(props["ActiveEnterTimestamp"])
	d.InactiveEnterTimestamp = unitXTime(props["InactiveEnterTimestamp"])
	d.FragmentPath = props["FragmentPath"]
	if v := props["DropInPaths"]; strings.TrimSpace(v) != "" {
		d.DropInPaths = strings.Fields(v)
	}
	d.UnitFileState = props["UnitFileState"]
	return d
}

const unitXTimeLayout = "Mon 2006-01-02 15:04:05 MST"

func unitXInt(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

func unitXInt64OrNotSet(s string) int64 {
	if s == "" || s == "[not set]" {
		return -1
	}
	u, err := strconv.ParseUint(s, 10, 64)
	if err != nil || u > math.MaxInt64 {
		return -1
	}
	return int64(u)
}

func unitXTime(s string) time.Time {
	t, err := time.Parse(unitXTimeLayout, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}
	}
	return t
}

// SortUnits orders units failed first, then services, then by type and name
// (type ties broken alphabetically, then by unit name).
func SortUnits(us []Unit) {
	sort.SliceStable(us, func(i, j int) bool {
		a, b := us[i], us[j]
		if fa, fb := a.Active == "failed", b.Active == "failed"; fa != fb {
			return fa
		}
		ta, tb := a.Type(), b.Type()
		ra, rb := unitXTypeRank(ta), unitXTypeRank(tb)
		if ra != rb {
			return ra < rb
		}
		if ta != tb {
			return ta < tb
		}
		return a.Name < b.Name
	})
}

func unitXTypeRank(typ string) int {
	if typ == "service" {
		return 0
	}
	return 1
}
