package linux

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"jumpr/internal/errs"
	"jumpr/internal/probe"
	"jumpr/internal/resource"
	"jumpr/internal/shq"
)

// KindUnitDetail is the single-unit view kind: `enter` on a unit row opens it.
// It is not a listing kind — the detail view loads it through UnitInfo.
const KindUnitDetail resource.KindName = "unit-detail"

// The unit detail batch's section IDs. They are also the probe markers.
const (
	secUnitStatus = "status"
	secUnitShow   = "show"
	secUnitCat    = "cat"
)

// systemctl exit codes the detail batch treats as normal: status is 3 for an
// inactive or failed unit and 4 when the unit does not exist; cat is 1 when the
// unit has no unit file. Only 4 means "no such unit".
const (
	unitRCOK         = 0
	unitRCNoFile     = 1
	unitRCInactive   = 3
	unitRCNoSuchUnit = 4
)

// UnitInfo is everything the unit detail view shows: the status text, the
// `systemctl show` properties, the fields of those properties the view renders
// first, the unit files and their drop-ins.
type UnitInfo struct {
	Status  string
	Props   map[string]string
	Detail  Detail
	Cat     string
	DropIns []string
}

// unitDetailSpec is the KindSpec of the unit-detail kind. The kind has a
// dedicated view, so the columns only matter to `:kinds` and the fallback
// table.
func unitDetailSpec() resource.KindSpec {
	return resource.KindSpec{
		Name:    KindUnitDetail,
		Aliases: []string{"unitdetail"},
		Title:   "Unit detail",
		Scope:   resource.ScopeHost,
		Columns: []resource.Column{
			{ID: "unit", Title: "UNIT", Width: 24},
			{ID: "state", Title: "STATE", Width: 12},
		},
	}
}

// unitSections is the detail batch: one round trip for status, show and cat.
// The unit name is already validated, and every command is assembled with shq.
func unitSections(unit string) []probe.Section {
	return []probe.Section{
		{ID: secUnitStatus, Cmd: shq.Join("systemctl", "status", unit, "--no-pager", "-n", "0", "--full")},
		{ID: secUnitShow, Cmd: shq.Join("systemctl", "show", unit, "--no-pager")},
		{ID: secUnitCat, Cmd: shq.Join("systemctl", "cat", unit, "--no-pager")},
	}
}

// UnitInfo runs the detail batch for one unit on one host and returns the
// parsed result. It is one round trip; a unit that does not exist is
// errs.NotAvailable, an inactive unit is a normal result.
func (b *Backend) UnitInfo(ctx context.Context, host resource.HostID, unit string) (UnitInfo, error) {
	const op = "linux.UnitInfo"
	host = resource.NormalizeHostID(string(host))
	if host == "" {
		return UnitInfo{}, errs.New(errs.Config, op, "unit detail needs a host")
	}
	unit = strings.TrimSpace(unit)
	if !shq.ValidUnitName(unit) {
		return UnitInfo{}, errs.New(errs.Config, op, "invalid unit name "+strconv.Quote(unit)).WithHost(string(host))
	}
	if b.exec == nil {
		return UnitInfo{}, errs.New(errs.Config, op, "no transport executor configured").WithHost(string(host))
	}
	batch, err := probe.NewBatch(b.newNonce(), unitSections(unit)...)
	if err != nil {
		return UnitInfo{}, err
	}

	cctx, cancel := context.WithTimeout(ctx, b.timeoutDuration())
	defer cancel()
	res, err := b.exec.Exec(cctx, host, batch.Command(b.timeoutDuration()))
	if err != nil {
		return UnitInfo{}, err
	}

	sec := batch.Split(res.Stdout, res.Stderr)
	if err := unitSectionErr(host, secUnitStatus, sec[secUnitStatus], unitRCOK, unitRCInactive); err != nil {
		return UnitInfo{}, err
	}
	if err := unitSectionErr(host, secUnitShow, sec[secUnitShow], unitRCOK); err != nil {
		return UnitInfo{}, err
	}
	if err := unitSectionErr(host, secUnitCat, sec[secUnitCat], unitRCOK, unitRCNoFile); err != nil {
		return UnitInfo{}, err
	}

	props := ParseShow(sec[secUnitShow].Stdout)
	cat := strings.TrimRight(sec[secUnitCat].Stdout, "\n")
	info := UnitInfo{
		Status: strings.TrimRight(sec[secUnitStatus].Stdout, "\n"),
		Props:  props,
		Detail: UnitDetail(props),
		Cat:    cat,
	}
	info.DropIns = dropInPaths(cat, info.Detail.DropInPaths)
	return info, nil
}

// unitSectionErr classifies one section of the detail batch. okRC lists the
// exit codes that are normal for that section; rc 4 always means the unit does
// not exist.
func unitSectionErr(host resource.HostID, id string, sec probe.SectionResult, okRC ...int) error {
	if sec.Err != nil {
		if typed, ok := sec.Err.(*errs.Error); ok {
			return typed.WithHost(string(host))
		}
		return errs.Wrap(errs.Remote, id, sec.Err).WithHost(string(host))
	}
	for _, rc := range okRC {
		if sec.RC == rc {
			return nil
		}
	}
	if sec.RC == unitRCNoSuchUnit {
		return errs.New(errs.NotAvailable, id, "no such unit").WithHost(string(host))
	}
	return errs.New(errs.Remote, id, fmt.Sprintf("systemctl exit %d", sec.RC)).WithHost(string(host))
}

// dropInPaths returns the drop-in files of a unit: the `# /path` headers of
// `systemctl cat` that live in a drop-in directory, then any path systemd
// reported in DropInPaths that the cat output did not already name.
func dropInPaths(cat string, known []string) []string {
	seen := make(map[string]bool, len(known))
	out := make([]string, 0, len(known))
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, line := range strings.Split(cat, "\n") {
		if p, ok := catHeaderPath(line); ok && isDropInPath(p) {
			add(p)
		}
	}
	for _, p := range known {
		add(strings.TrimSpace(p))
	}
	return out
}

// catHeaderPath is the file path of a `systemctl cat` header line ("# /path").
func catHeaderPath(line string) (string, bool) {
	line = strings.TrimRight(line, "\r")
	if !strings.HasPrefix(line, "# /") {
		return "", false
	}
	p := strings.TrimSpace(strings.TrimPrefix(line, "#"))
	return p, p != ""
}

// isDropInPath reports whether p is a drop-in file rather than a unit file.
func isDropInPath(p string) bool { return strings.Contains(p, ".d/") }
