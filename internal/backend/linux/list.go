package linux

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"jumper/internal/errs"
	"jumper/internal/probe"
	"jumper/internal/resource"
	"jumper/internal/shq"
	"jumper/internal/transport"
)

// hostInfo is the typed Raw payload of a host row: one pointer per probe
// section, nil when that section failed or did not parse.
type hostInfo struct {
	Load        *Load
	Mem         *Mem
	PSI         *PSI
	OS          *OS
	Kernel      string
	Uptime      time.Duration
	NProc       int
	State       string
	FailedUnits []Unit
}

// hostSectionOrder is the display order used for "_degraded" labels, so the
// label of a host does not depend on map iteration order.
var hostSectionOrder = []string{
	secLoadavg, secMeminfo, secPSI, secOS, secKernel,
	secUptime, secNproc, secSysState, secFailed,
}

// sectionErr converts one section's outcome into a section-level error, or nil
// when the section succeeded. A non-zero exit code is a failure unless the tool
// uses it as a state (see toleratesNonZero).
func sectionErr(host resource.HostID, id string, sec probe.SectionResult, tolerate bool) error {
	if sec.Err != nil {
		return errs.New(errs.KindOf(sec.Err), id, sec.Err.Error()).WithHost(string(host))
	}
	if sec.Missing {
		return errs.New(errs.Remote, id, "no result").WithHost(string(host))
	}
	if sec.RC != 0 && !(tolerate && sec.RC == 1) {
		return errs.New(errs.Remote, id, fmt.Sprintf("exit %d", sec.RC)).WithHost(string(host))
	}
	return nil
}

// collect splits one host's reply into sections and classifies each section.
// bad holds the errors that fail the host (single-section kinds) or degrade
// the affected cells (host kind). retried holds errors that were worked around
// but still make the rows degraded, such as a sudo failure followed by an
// unprivileged retry.
func (b *Backend) collect(ctx context.Context, kind resource.KindName, host resource.HostID, batch probe.Batch, res transport.ExecResult) (map[string]probe.SectionResult, map[string]error, map[string]error) {
	sec := batch.Split(res.Stdout, res.Stderr)
	bad := make(map[string]error, len(batch.Sections))
	for _, s := range batch.Sections {
		if e := sectionErr(host, s.ID, sec[s.ID], toleratesNonZero(s.ID)); e != nil {
			bad[s.ID] = e
		}
	}
	retried := make(map[string]error)
	if kind == KindSocket {
		if e := bad[secSockets]; errs.KindOf(e) == errs.Sudo {
			if again, ok := b.retrySockets(ctx, host); ok {
				sec[secSockets] = again
				delete(bad, secSockets)
				retried[secSockets] = e
			}
		}
	}
	return sec, bad, retried
}

// retrySockets runs the socket probe again without sudo for one host. The
// reply is the same shape; only the process column is lost.
func (b *Backend) retrySockets(ctx context.Context, host resource.HostID) (probe.SectionResult, bool) {
	batch, err := socketRetryBatch(b.newNonce())
	if err != nil {
		return probe.SectionResult{}, false
	}
	cctx, cancel := context.WithTimeout(ctx, b.timeoutDuration())
	defer cancel()
	res, err := b.exec.Exec(cctx, host, batch.Command(b.timeoutDuration()))
	if err != nil {
		return probe.SectionResult{}, false
	}
	sec := batch.Split(res.Stdout, res.Stderr)[secSockets]
	if sectionErr(host, secSockets, sec, false) != nil {
		return probe.SectionResult{}, false
	}
	return sec, true
}

// rows turns one host's sections into resources. A single-section kind fails
// the host when its section failed; the host kind degrades the affected cells
// instead.
func (b *Backend) rows(kind resource.KindName, host resource.HostID, q resource.Query, sec map[string]probe.SectionResult, bad, retried map[string]error) ([]resource.Resource, error) {
	switch kind {
	case KindHost:
		return []resource.Resource{b.hostRow(host, sec, bad, retried)}, nil
	case KindUnit:
		return b.unitRows(host, q, sec, bad, retried)
	case KindProcess:
		return b.processRows(host, sec, bad, retried)
	case KindSocket:
		return b.socketRows(host, sec, bad, retried)
	case KindMount:
		return b.mountRows(host, q, sec, bad, retried)
	case KindDisk:
		return b.diskRows(host, q, sec, bad, retried)
	case KindInterface:
		return b.interfaceRows(host, sec, bad, retried)
	case KindRoute:
		return b.routeRows(host, sec, bad, retried)
	case KindNeigh:
		return b.neighRows(host, sec, bad, retried)
	case KindCgroup:
		return b.cgroupRows(host, sec, bad, retried)
	case KindPkg:
		return b.pkgRows(host, sec, bad, retried)
	case KindUnitFleet:
		return b.unitFleetRows(host, q, sec, bad, retried)
	default:
		return nil, errs.New(errs.Config, "linux.List", fmt.Sprintf("unknown kind %q", kind))
	}
}

// labels returns a fresh copy of the inventory host's labels, always carrying
// the "host" key.
func (b *Backend) labels(host resource.HostID) map[string]string {
	out := make(map[string]string, 4)
	if b.inv != nil {
		if h, ok := b.inv.Resolve(string(host)); ok {
			for k, v := range h.Labels {
				out[k] = v
			}
		}
	}
	if _, ok := out["host"]; !ok {
		out["host"] = string(host)
	}
	return out
}

// degradeLabel renders "<section>: <kind>", the value of the "_degraded" label.
func degradeLabel(id string, err error) string {
	return id + ": " + string(errs.KindOf(err))
}

// applyDegraded records the failed sections on the row's labels. Order is by
// hostSectionOrder, then any remaining IDs sorted, so the value is stable.
func applyDegraded(labels map[string]string, bad, retried map[string]error) {
	if len(bad) == 0 && len(retried) == 0 {
		return
	}
	var parts []string
	seen := make(map[string]bool, len(bad)+len(retried))
	for _, id := range hostSectionOrder {
		if e, ok := bad[id]; ok {
			parts = append(parts, degradeLabel(id, e))
			seen[id] = true
		}
	}
	var rest []string
	for id := range bad {
		if !seen[id] {
			rest = append(rest, id)
		}
	}
	for id := range retried {
		if !seen[id] {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	for _, id := range rest {
		if e, ok := bad[id]; ok {
			parts = append(parts, degradeLabel(id, e))
			continue
		}
		parts = append(parts, degradeLabel(id, retried[id]))
	}
	if len(parts) > 0 {
		labels["_degraded"] = strings.Join(parts, "; ")
	}
}

// hostRow builds the overview row. Missing sections leave their cells nil and
// mark the row degraded; the host is never failed by a section problem.
func (b *Backend) hostRow(host resource.HostID, sec map[string]probe.SectionResult, bad, retried map[string]error) resource.Resource {
	labels := b.labels(host)

	info := &hostInfo{}
	fields := map[string]any{"host": string(host)}
	degrade := func(id string, err error) {
		if _, dup := bad[id]; !dup {
			bad[id] = err
		}
		if b.log != nil {
			b.log.Debug("linux host section degraded", "host", string(host), "section", id, "kind", errs.KindOf(err))
		}
	}

	if _, failed := bad[secLoadavg]; !failed {
		if l, err := ParseLoadavg(sec[secLoadavg].Stdout); err != nil {
			degrade(secLoadavg, err)
		} else {
			info.Load = &l
			fields["load"] = l.L1
		}
	}
	if _, failed := bad[secMeminfo]; !failed {
		if m, err := ParseMeminfo(sec[secMeminfo].Stdout); err != nil {
			degrade(secMeminfo, err)
		} else {
			info.Mem = &m
			fields["mem"] = m.UsedPercent()
		}
	}
	if _, failed := bad[secPSI]; !failed {
		if p, err := ParsePSI(sec[secPSI].Stdout); err != nil {
			degrade(secPSI, err)
		} else {
			info.PSI = &p
			if p.IO.Present {
				fields["psi_io"] = p.IO.SomeAvg10
			}
		}
	}
	if _, failed := bad[secOS]; !failed {
		if o, err := ParseOSRelease(sec[secOS].Stdout); err != nil {
			degrade(secOS, err)
		} else {
			info.OS = &o
			fields["os"] = firstNonEmpty(o.PrettyName, o.ID)
		}
	}
	if _, failed := bad[secKernel]; !failed {
		if s := strings.TrimSpace(sec[secKernel].Stdout); s == "" {
			degrade(secKernel, errs.New(errs.Parse, secKernel, "empty kernel version").WithHost(string(host)))
		} else {
			info.Kernel = s
			fields["kernel"] = s
		}
	}
	if _, failed := bad[secUptime]; !failed {
		if d, err := ParseUptime(sec[secUptime].Stdout); err != nil {
			degrade(secUptime, err)
		} else {
			info.Uptime = d
			fields["uptime"] = d
		}
	}
	if _, failed := bad[secNproc]; !failed {
		if n, err := ParseNproc(sec[secNproc].Stdout); err != nil {
			degrade(secNproc, err)
		} else {
			info.NProc = n
		}
	}
	if _, failed := bad[secSysState]; !failed {
		if s, err := ParseSystemState(sec[secSysState].Stdout); err != nil {
			degrade(secSysState, err)
		} else {
			info.State = s
			fields["state"] = s
		}
	}
	if _, failed := bad[secFailed]; !failed {
		if us, err := ParseListUnits(sec[secFailed].Stdout); err != nil {
			degrade(secFailed, err)
		} else {
			info.FailedUnits = us
			fields["failed"] = int64(len(us))
			names := make([]string, 0, 3)
			for _, u := range us {
				if len(names) == 3 {
					break
				}
				names = append(names, u.Name)
			}
			fields["units"] = strings.Join(names, ", ")
		}
	}

	// The degrade closure may have added entries after applyDegraded ran.
	applyDegraded(labels, bad, retried)

	status := resource.StatusUnknown
	switch {
	case len(info.FailedUnits) > 0 || info.State == "degraded":
		status = resource.StatusCrit
	case info.Mem != nil && info.Mem.UsedPercent() > 90:
		status = resource.StatusWarn
	case info.PSI != nil && info.PSI.IO.Present && info.PSI.IO.SomeAvg10 > 10:
		status = resource.StatusWarn
	case info.Load != nil || info.Mem != nil || info.OS != nil:
		status = resource.StatusOK
	}

	return resource.Resource{
		Ref:       resource.Ref{Backend: backendName, Kind: KindHost, Host: host, Name: string(host)},
		Fields:    fields,
		Labels:    labels,
		Status:    status,
		Raw:       info,
		FetchedAt: b.now(),
	}
}

func (b *Backend) unitRows(host resource.HostID, q resource.Query, sec map[string]probe.SectionResult, bad, retried map[string]error) ([]resource.Resource, error) {
	if e := bad[secUnits]; e != nil {
		return nil, e
	}
	units, err := ParseListUnits(sec[secUnits].Stdout)
	if err != nil {
		return nil, err
	}
	if want := strings.TrimSpace(q.Args["unit"]); want != "" {
		kept := units[:0]
		for _, u := range units {
			if u.Name == want {
				kept = append(kept, u)
			}
		}
		units = kept
	}
	SortUnits(units)

	out := make([]resource.Resource, 0, len(units))
	for _, u := range units {
		labels := b.labels(host)
		applyDegraded(labels, nil, retried)
		out = append(out, resource.Resource{
			Ref: resource.Ref{Backend: backendName, Kind: KindUnit, Host: host, Name: u.Name},
			Fields: map[string]any{
				"host":        string(host),
				"unit":        u.Name,
				"load":        u.Load,
				"active":      u.Active,
				"sub":         u.Sub,
				"description": u.Description,
			},
			Labels:    labels,
			Status:    u.Status(),
			Raw:       u,
			FetchedAt: b.now(),
		})
	}
	return out, nil
}

func (b *Backend) processRows(host resource.HostID, sec map[string]probe.SectionResult, bad, retried map[string]error) ([]resource.Resource, error) {
	if e := bad[secProcs]; e != nil {
		return nil, e
	}
	procs, _ := ParsePS(sec[secProcs].Stdout)
	sort.SliceStable(procs, func(i, j int) bool { return procs[i].CPU > procs[j].CPU })

	out := make([]resource.Resource, 0, len(procs))
	for _, p := range procs {
		labels := b.labels(host)
		applyDegraded(labels, nil, retried)
		out = append(out, resource.Resource{
			Ref: resource.Ref{Backend: backendName, Kind: KindProcess, Host: host, Name: fmt.Sprintf("%d", p.PID)},
			Fields: map[string]any{
				"host":    string(host),
				"pid":     int64(p.PID),
				"user":    p.User,
				"cpu":     p.CPU,
				"mem":     p.Mem,
				"rss":     p.RSSKB * 1024,
				"elapsed": p.Elapsed,
				"command": p.Args,
			},
			Labels:    labels,
			Status:    resource.StatusUnknown,
			Raw:       p,
			FetchedAt: b.now(),
		})
	}
	return out, nil
}

func (b *Backend) socketRows(host resource.HostID, sec map[string]probe.SectionResult, bad, retried map[string]error) ([]resource.Resource, error) {
	if e := bad[secSockets]; e != nil {
		return nil, e
	}
	socks, _ := ParseSS(sec[secSockets].Stdout)

	out := make([]resource.Resource, 0, len(socks))
	for _, s := range socks {
		local := s.LocalAddr + ":" + s.LocalPort
		peer := s.PeerAddr + ":" + s.PeerPort
		fields := map[string]any{
			"host":  string(host),
			"proto": s.Proto,
			"state": s.State,
			"recvq": int64(s.RecvQ),
			"sendq": int64(s.SendQ),
			"local": local,
			"peer":  peer,
		}
		if names := sockProcNames(s); len(names) > 0 {
			fields["process"] = names
		}
		labels := b.labels(host)
		applyDegraded(labels, nil, retried)
		status := resource.StatusUnknown
		if s.Listening() {
			status = resource.StatusOK
		}
		out = append(out, resource.Resource{
			Ref:       resource.Ref{Backend: backendName, Kind: KindSocket, Host: host, Name: s.Proto + " " + local + " " + peer},
			Fields:    fields,
			Labels:    labels,
			Status:    status,
			Raw:       s,
			FetchedAt: b.now(),
		})
	}
	return out, nil
}

// sockProcNames returns the distinct process names holding a socket.
func sockProcNames(s Socket) string {
	var names []string
	seen := make(map[string]bool, len(s.Procs))
	for _, p := range s.Procs {
		if p.Name == "" || seen[p.Name] {
			continue
		}
		seen[p.Name] = true
		names = append(names, p.Name)
	}
	return strings.Join(names, ", ")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// secUnitFleet is the unit-fleet batch's one section (and probe marker).
const secUnitFleet = "fleet"

// unitFleetProps are the `systemctl show` properties one fleet row carries.
const unitFleetProps = "ActiveState,SubState,ActiveEnterTimestamp,NRestarts,UnitFileState,MemoryCurrent"

// unitFleetSections is the fleet batch: one `systemctl show` per host, built
// with shq because the unit name comes from the user.
func unitFleetSections(unit string) []probe.Section {
	return []probe.Section{{
		ID:  secUnitFleet,
		Cmd: shq.Join("systemctl", "show", unit, "-p", unitFleetProps, "--no-pager"),
	}}
}

// batchFor compiles a kind's batch when the command depends on the query, and
// otherwise defers to the fixed batch of probes.go. Only unit-fleet is
// parameterized: its `systemctl show` names the unit.
func (b *Backend) batchFor(kind resource.KindName, q resource.Query) (probe.Batch, error) {
	if kind != KindUnitFleet {
		return b.batch(kind)
	}
	unit := strings.TrimSpace(q.Args["unit"])
	if unit == "" {
		return probe.Batch{}, errs.New(errs.Config, "linux.List", "unit-fleet needs a unit")
	}
	if !shq.ValidUnitName(unit) {
		return probe.Batch{}, errs.New(errs.Config, "linux.List",
			"invalid unit name "+strconv.Quote(unit))
	}
	return probe.NewBatch(b.newNonce(), unitFleetSections(unit)...)
}

// unitFleetRows is the one row of one host: the same unit's state there. A host
// whose `systemctl show` failed fails its row; an unknown unit leaves the cells
// empty rather than inventing a state.
func (b *Backend) unitFleetRows(host resource.HostID, q resource.Query, sec map[string]probe.SectionResult, bad, retried map[string]error) ([]resource.Resource, error) {
	if e := bad[secUnitFleet]; e != nil {
		return nil, e
	}
	unit := strings.TrimSpace(q.Args["unit"])
	d := UnitDetail(ParseShow(sec[secUnitFleet].Stdout))

	labels := b.labels(host)
	applyDegraded(labels, nil, retried)
	fields := map[string]any{
		"host":     string(host),
		"unit":     unit,
		"active":   d.ActiveState,
		"sub":      d.SubState,
		"restarts": int64(d.NRestarts),
		"enabled":  d.UnitFileState,
	}
	if !d.ActiveEnterTimestamp.IsZero() {
		fields["since"] = d.ActiveEnterTimestamp
	}
	if d.MemoryCurrent >= 0 {
		fields["mem"] = d.MemoryCurrent
	}
	return []resource.Resource{{
		Ref:       resource.Ref{Backend: backendName, Kind: KindUnitFleet, Host: host, Name: string(host)},
		Fields:    fields,
		Labels:    labels,
		Status:    unitXStatus(d.ActiveState, d.SubState),
		Raw:       d,
		FetchedAt: b.now(),
	}}, nil
}
