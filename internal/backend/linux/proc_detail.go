package linux

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/probe"
	"jumpr/internal/resource"
	"jumpr/internal/shq"
	"jumpr/internal/transport"
)

// KindProcessDetail is the single-process drill-in kind: `enter` on a process
// row opens it. It is not a listing kind — the view loads it through ProcInfo.
const KindProcessDetail resource.KindName = "process-detail"

// Section IDs of the /proc batch. They are the probe markers, the Op of a
// section-level error and the keys of ProcDetail.Failed.
const (
	secProcStatus  = "status"
	secProcLimits  = "limits"
	secProcCgroup  = "cgroup"
	secProcFDs     = "fds"
	secProcExe     = "exe"
	secProcCwd     = "cwd"
	secProcEnviron = "environ"
	secProcIO      = "io"
	secProcOOM     = "oom-score"
	secProcStart   = "start"
)

const (
	// procOp is the Op of every error built here.
	procOp = "linux.ProcInfo"
	// procJournalOp is the Op of the process journal errors.
	procJournalOp = "linux.ProcJournal"
	// procGoneText is what the kernel answers for a PID that no longer exists.
	procGoneText = "No such file"
	// fdWarnPercent is the share of the soft open-file limit above which the
	// view warns: 80 %.
	fdWarnPercent = 80
	// procJournalLimit is the default number of journal records one drill-in
	// asks for.
	procJournalLimit = 200
	// unlimitedLimit is the value of FDSoft when the limit is "unlimited".
	unlimitedLimit = -1
)

// /proc/PID/limits is fixed-width: the name fills the first 25 cells, then the
// soft and hard limits (19 cells each) and the units.
const (
	limitsNameEnd   = 25
	limitsSoftFrom  = 26
	limitsSoftEnd   = 45
	limitsHardFrom  = 46
	limitsHardEnd   = 65
	limitsUnitsFrom = 66
)

// ProcLimit is one row of /proc/PID/limits. Soft and Hard stay strings because
// the kernel prints "unlimited" for a limit it does not impose.
type ProcLimit struct {
	Name  string
	Soft  string
	Hard  string
	Units string
}

// ProcCgroup is the process's cgroup membership and the systemd unit the path
// names, when it names one.
type ProcCgroup struct {
	Path        string
	Controllers string
	Unit        string
}

// ProcIO is the /proc/PID/io counter set.
type ProcIO struct {
	RChar      int64
	WChar      int64
	SysCR      int64
	SysCW      int64
	ReadBytes  int64
	WriteBytes int64
}

// ProcDetail is everything the process drill-in shows: the parsed status
// fields, the limits, the cgroup, the open-file count, the environment keys
// (never values), the IO counters, the OOM score and the start time.
//
// Failed holds the sections that could not be read or parsed. A section
// failure never hides the rest of the drill-in; only a process that no longer
// exists fails whole (see ProcessExited).
type ProcDetail struct {
	PID      int
	Status   map[string]string
	Name     string
	State    string
	Threads  int
	RSS      int64 // bytes, from VmRSS
	VMSize   int64 // bytes, from VmSize
	FDs      int   // open file descriptors
	FDSoft   int64 // "Max open files" soft limit; unlimitedLimit when unlimited, 0 when unknown
	FDWarn   bool  // FDs above 80 % of FDSoft
	OOMScore int
	Start    time.Time
	Exe      string
	Cwd      string
	Cgroup   ProcCgroup
	Limits   []ProcLimit
	EnvKeys  []string
	IO       ProcIO
	Failed   map[string]error
}

// procSections is the one privileged batch of the drill-in: status, limits,
// cgroup, open-file count, exe, cwd, environment keys, IO, OOM score and start
// time. pid is validated before this is called and every path is quoted with
// shq. The environment is cut to keys on the remote side, so values — which
// often carry secrets — never reach the laptop.
func procSections(pid string) []probe.Section {
	return []probe.Section{
		{ID: secProcStatus, Cmd: shq.Join("cat", procPath(pid, "status")), Sudo: true},
		{ID: secProcLimits, Cmd: shq.Join("cat", procPath(pid, "limits")), Sudo: true},
		{ID: secProcCgroup, Cmd: shq.Join("cat", procPath(pid, "cgroup")), Sudo: true},
		{ID: secProcFDs, Cmd: shq.Join("ls", procPath(pid, "fd")) + " | wc -l", Sudo: true},
		{ID: secProcExe, Cmd: shq.Join("readlink", procPath(pid, "exe")), Sudo: true},
		{ID: secProcCwd, Cmd: shq.Join("readlink", procPath(pid, "cwd")), Sudo: true},
		{ID: secProcEnviron, Cmd: "tr '\\0' '\\n' < " + procPath(pid, "environ") + " | cut -d= -f1 | sort", Sudo: true},
		{ID: secProcIO, Cmd: shq.Join("cat", procPath(pid, "io")), Sudo: true},
		{ID: secProcOOM, Cmd: shq.Join("cat", procPath(pid, "oom_score")), Sudo: true},
		{ID: secProcStart, Cmd: shq.Join("ps", "-o", "lstart=", "-p", pid), Sudo: true},
	}
}

// procPath is /proc/<pid>/<name> as one shell word. pid is a validated decimal
// number and the path is quoted with shq like every other remote argument.
func procPath(pid, name string) string { return shq.Quote("/proc/" + pid + "/" + name) }

// ProcInfo runs the detail batch for one PID on one host and returns the
// parsed result. It is one round trip. A PID that no longer exists is
// errs.NotAvailable ("process exited", see ProcessExited); an unreadable or
// unparsable section is recorded in ProcDetail.Failed and does not fail the
// rest.
func (b *Backend) ProcInfo(ctx context.Context, host resource.HostID, pid string) (ProcDetail, error) {
	host = resource.NormalizeHostID(string(host))
	if host == "" {
		return ProcDetail{}, errs.New(errs.Config, procOp, "process detail needs a host")
	}
	canonical, ok := parsePID(strings.TrimSpace(pid))
	if !ok {
		return ProcDetail{}, errs.New(errs.Config, procOp, "invalid pid "+strconv.Quote(pid)).WithHost(string(host))
	}
	if b.exec == nil {
		return ProcDetail{}, errs.New(errs.Config, procOp, "no transport executor configured").WithHost(string(host))
	}
	batch, err := probe.NewBatch(b.newNonce(), procSections(canonical)...)
	if err != nil {
		return ProcDetail{}, err
	}

	cctx, cancel := context.WithTimeout(ctx, b.timeoutDuration())
	defer cancel()
	res, err := b.exec.Exec(cctx, host, batch.Command(b.timeoutDuration()))
	if err != nil {
		return ProcDetail{}, err
	}
	sec := batch.Split(res.Stdout, res.Stderr)

	pidNum, _ := strconv.Atoi(canonical)
	if procGone(sec) {
		return ProcDetail{PID: pidNum}, errs.New(errs.NotAvailable, procOp, "process exited").WithHost(string(host))
	}

	info := ProcDetail{PID: pidNum, Failed: make(map[string]error)}
	fail := func(id string, e error) {
		if e == nil {
			return
		}
		if _, dup := info.Failed[id]; !dup {
			info.Failed[id] = e
		}
	}

	if e := procSectionErr(host, secProcStatus, sec[secProcStatus]); e != nil {
		fail(secProcStatus, e)
	} else if status, perr := ParseProcStatus(sec[secProcStatus].Stdout); perr != nil {
		fail(secProcStatus, perr)
	} else {
		info.Status = status
		info.Name = statusValue(status, "Name")
		info.State = statusValue(status, "State")
		info.Threads = statusInt(status, "Threads")
		info.RSS = statusKB(status, "VmRSS")
		info.VMSize = statusKB(status, "VmSize")
	}

	if e := procSectionErr(host, secProcLimits, sec[secProcLimits]); e != nil {
		fail(secProcLimits, e)
	} else if limits, perr := ParseProcLimits(sec[secProcLimits].Stdout); perr != nil {
		fail(secProcLimits, perr)
	} else {
		info.Limits = limits
		info.FDSoft = openFileLimit(limits)
	}

	if e := procSectionErr(host, secProcCgroup, sec[secProcCgroup]); e != nil {
		fail(secProcCgroup, e)
	} else if cg, perr := ParseProcCgroup(sec[secProcCgroup].Stdout); perr != nil {
		fail(secProcCgroup, perr)
	} else {
		info.Cgroup = cg
	}

	if e := procSectionErr(host, secProcFDs, sec[secProcFDs]); e != nil {
		fail(secProcFDs, e)
	} else if n, perr := strconv.Atoi(firstLine(sec[secProcFDs].Stdout)); perr == nil && n >= 0 {
		info.FDs = n
	} else {
		fail(secProcFDs, errs.New(errs.Parse, secProcFDs, "no descriptor count").WithHost(string(host)))
	}

	if e := procSectionErr(host, secProcExe, sec[secProcExe]); e != nil {
		fail(secProcExe, e)
	} else {
		info.Exe = strings.TrimSpace(sec[secProcExe].Stdout)
	}

	if e := procSectionErr(host, secProcCwd, sec[secProcCwd]); e != nil {
		fail(secProcCwd, e)
	} else {
		info.Cwd = strings.TrimSpace(sec[secProcCwd].Stdout)
	}

	if e := procSectionErr(host, secProcEnviron, sec[secProcEnviron]); e != nil {
		fail(secProcEnviron, e)
	} else {
		info.EnvKeys = ParseProcEnvironKeys(sec[secProcEnviron].Stdout)
	}

	if e := procSectionErr(host, secProcIO, sec[secProcIO]); e != nil {
		fail(secProcIO, e)
	} else if io, perr := ParseProcIO(sec[secProcIO].Stdout); perr != nil {
		fail(secProcIO, perr)
	} else {
		info.IO = io
	}

	if e := procSectionErr(host, secProcOOM, sec[secProcOOM]); e != nil {
		fail(secProcOOM, e)
	} else if n, perr := ParseProcOOMScore(sec[secProcOOM].Stdout); perr != nil {
		fail(secProcOOM, perr)
	} else {
		info.OOMScore = n
	}

	if e := procSectionErr(host, secProcStart, sec[secProcStart]); e != nil {
		fail(secProcStart, e)
	} else if t, perr := ParseProcStartTime(sec[secProcStart].Stdout); perr != nil {
		fail(secProcStart, perr)
	} else {
		info.Start = t
	}

	info.FDWarn = fdsWarn(info.FDs, info.FDSoft)
	return info, nil
}

// ProcessExited reports whether err means the process no longer exists, so a
// view can say "process exited" instead of showing an error.
func ProcessExited(err error) bool {
	return errs.KindOf(err) == errs.NotAvailable && strings.Contains(err.Error(), "process exited")
}

// FDLimitText is the soft "Max open files" limit as a view shows it:
// "unlimited", the number, or "unknown" when the limits could not be read.
func (d ProcDetail) FDLimitText() string {
	switch {
	case d.FDSoft == unlimitedLimit:
		return "unlimited"
	case d.FDSoft > 0:
		return strconv.FormatInt(d.FDSoft, 10)
	default:
		return "unknown"
	}
}

// procGone reports whether the batch says the PID is gone: the status or
// limits file answered "No such file".
func procGone(sec map[string]probe.SectionResult) bool {
	for _, id := range []string{secProcStatus, secProcLimits} {
		s := sec[id]
		if s.RC == 0 {
			continue
		}
		if strings.Contains(s.Stderr, procGoneText) || strings.Contains(s.Stdout, procGoneText) {
			return true
		}
	}
	return false
}

// procSectionErr classifies one section of the /proc batch. No exit code is
// normal here, so every failure is an error; the caller records it.
func procSectionErr(host resource.HostID, id string, sec probe.SectionResult) error {
	if sec.Err != nil {
		if typed, ok := sec.Err.(*errs.Error); ok {
			return typed.WithHost(string(host))
		}
		return errs.Wrap(errs.KindOf(sec.Err), id, sec.Err).WithHost(string(host))
	}
	if sec.Missing {
		return errs.New(errs.Remote, id, "no result").WithHost(string(host))
	}
	if sec.RC != 0 {
		if e := shq.Classify(sec.RC, sec.Stderr, true); e != nil {
			if typed, ok := e.(*errs.Error); ok {
				return typed.WithHost(string(host))
			}
		}
		return errs.New(errs.Remote, id, fmt.Sprintf("exit %d", sec.RC)).WithHost(string(host))
	}
	return nil
}

// ParseProcStatus parses /proc/PID/status into its fields. Lines that are not
// "Key:\tvalue" are skipped.
func ParseProcStatus(out string) (map[string]string, error) {
	status := make(map[string]string, 32)
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(strings.TrimRight(line, "\r"), ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		status[key] = strings.TrimSpace(val)
	}
	if len(status) == 0 {
		return nil, errs.New(errs.Parse, secProcStatus, "empty status")
	}
	return status, nil
}

// statusValue is one status field, or "".
func statusValue(status map[string]string, key string) string { return status[key] }

// statusInt reads an integer status field; an unparsable value is 0.
func statusInt(status map[string]string, key string) int {
	n, err := strconv.Atoi(status[key])
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// statusKB reads a size status field ("1234 kB") and returns bytes.
func statusKB(status map[string]string, key string) int64 {
	f := strings.Fields(status[key])
	if len(f) == 0 {
		return 0
	}
	v, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil || v < 0 {
		return 0
	}
	if len(f) > 1 && strings.EqualFold(f[1], "kB") {
		return v * 1024
	}
	return v
}

// ParseProcLimits parses /proc/PID/limits. It reads the fixed-width columns the
// kernel prints and skips a line it cannot place.
func ParseProcLimits(out string) ([]ProcLimit, error) {
	var limits []ProcLimit
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if lim, ok := parseLimitLine(line); ok {
			limits = append(limits, lim)
		}
	}
	if len(limits) == 0 {
		return nil, errs.New(errs.Parse, secProcLimits, "no limits in output")
	}
	return limits, nil
}

// parseLimitLine cuts one fixed-width limits row. The header and anything
// shorter than the four columns are skipped.
func parseLimitLine(line string) (ProcLimit, bool) {
	if len(line) < limitsUnitsFrom {
		return ProcLimit{}, false
	}
	name := strings.TrimSpace(line[:limitsNameEnd])
	if name == "" || name == "Limit" {
		return ProcLimit{}, false
	}
	return ProcLimit{
		Name:  name,
		Soft:  strings.TrimSpace(line[limitsSoftFrom:limitsSoftEnd]),
		Hard:  strings.TrimSpace(line[limitsHardFrom:limitsHardEnd]),
		Units: strings.TrimSpace(line[limitsUnitsFrom:]),
	}, true
}

// openFileLimit is the soft "Max open files" limit, unlimitedLimit for
// "unlimited" and 0 when the row is missing.
func openFileLimit(limits []ProcLimit) int64 {
	for _, lim := range limits {
		if lim.Name != "Max open files" {
			continue
		}
		return parseLimitValue(lim.Soft)
	}
	return 0
}

// parseLimitValue reads one limit cell: a decimal number or "unlimited".
func parseLimitValue(v string) int64 {
	if v == "unlimited" {
		return unlimitedLimit
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// fdsWarn reports whether used is above fdWarnPercent of soft. A missing or
// unlimited soft limit never warns.
func fdsWarn(used int, soft int64) bool {
	if soft <= 0 {
		return false
	}
	return int64(used)*100 > soft*fdWarnPercent
}

// ParseProcCgroup parses /proc/PID/cgroup. The unified (cgroup v2) line wins;
// on a hybrid host the line that names a systemd unit is preferred. Lines that
// do not carry an absolute path are skipped.
func ParseProcCgroup(out string) (ProcCgroup, error) {
	var best *ProcCgroup
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 || !strings.HasPrefix(parts[2], "/") {
			continue
		}
		cg := ProcCgroup{
			Controllers: strings.TrimSpace(parts[1]),
			Path:        strings.TrimSpace(parts[2]),
		}
		cg.Unit = cgroupUnit(cg.Path)
		if cg.Controllers == "" {
			return cg, nil
		}
		if best == nil || (best.Unit == "" && cg.Unit != "") {
			c := cg
			best = &c
		}
	}
	if best == nil {
		return ProcCgroup{}, errs.New(errs.Parse, secProcCgroup, "no cgroup path")
	}
	return *best, nil
}

// cgroupUnit is the systemd unit the last element of a cgroup path names, or
// "" when the path ends in something that is not a unit.
func cgroupUnit(path string) string {
	path = strings.TrimRight(path, "/")
	name := path[strings.LastIndexByte(path, '/')+1:]
	for _, suffix := range []string{".service", ".scope", ".slice"} {
		if strings.HasSuffix(name, suffix) {
			return name
		}
	}
	return ""
}

// ParseProcEnvironKeys reads the key list the remote side produced. Values are
// cut off before the output leaves the node; anything that still carries an
// "=" is not a key and is skipped.
func ParseProcEnvironKeys(out string) []string {
	seen := make(map[string]bool, 16)
	var keys []string
	for _, line := range strings.Split(out, "\n") {
		key := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if key == "" || strings.ContainsRune(key, '=') || seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	return keys
}

// ParseProcIO parses /proc/PID/io. Unknown counters are ignored.
func ParseProcIO(out string) (ProcIO, error) {
	var io ProcIO
	found := false
	for _, line := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(strings.TrimRight(line, "\r"), ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
		if err != nil {
			continue
		}
		found = true
		switch strings.TrimSpace(key) {
		case "rchar":
			io.RChar = n
		case "wchar":
			io.WChar = n
		case "syscr":
			io.SysCR = n
		case "syscw":
			io.SysCW = n
		case "read_bytes":
			io.ReadBytes = n
		case "write_bytes":
			io.WriteBytes = n
		}
	}
	if !found {
		return ProcIO{}, errs.New(errs.Parse, secProcIO, "no io counters")
	}
	return io, nil
}

// ParseProcOOMScore parses the single number of /proc/PID/oom_score.
func ParseProcOOMScore(out string) (int, error) {
	n, err := strconv.Atoi(firstLine(out))
	if err != nil || n < 0 {
		return 0, errs.New(errs.Parse, secProcOOM, "no oom score")
	}
	return n, nil
}

// ParseProcStartTime parses `ps -o lstart=`, whose layout depends on the
// locale. An empty answer is an unknown start, not an error.
func ParseProcStartTime(out string) (time.Time, error) {
	text := strings.Join(strings.Fields(out), " ")
	if text == "" {
		return time.Time{}, nil
	}
	for _, layout := range procStartLayouts() {
		if t, err := time.Parse(layout, text); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errs.New(errs.Parse, secProcStart, "unreadable start time")
}

// procStartLayouts are the `ps lstart` layouts tried in order.
func procStartLayouts() []string {
	return []string{
		"Mon Jan _2 15:04:05 2006",
		"Mon Jan 2 15:04:05 2006",
		time.RFC3339,
	}
}

// ProcJournal lists one process's journal records on one host: one bounded
// read-only journalctl call with the _PID= match, parsed with the journal
// parser. Local filtering happens only for bounds; the PID filter is remote.
func (b *Backend) ProcJournal(ctx context.Context, host resource.HostID, pid string, o JournalOptions) ([]Entry, error) {
	host = resource.NormalizeHostID(string(host))
	if host == "" {
		return nil, errs.New(errs.Config, procJournalOp, "journal needs a host")
	}
	canonical, ok := parsePID(strings.TrimSpace(pid))
	if !ok {
		return nil, errs.New(errs.Config, procJournalOp, "invalid pid "+strconv.Quote(pid)).WithHost(string(host))
	}
	if b.exec == nil {
		return nil, errs.New(errs.Config, procJournalOp, "no transport executor configured").WithHost(string(host))
	}
	if o.Now.IsZero() {
		o.Now = b.now()
	}

	cctx, cancel := context.WithTimeout(ctx, b.timeoutDuration())
	defer cancel()
	res, err := b.exec.Exec(cctx, host, transport.Command{
		Script:  ProcJournalCommand(canonical, o),
		Sudo:    o.Sudo,
		Timeout: b.timeoutDuration(),
	})
	if err != nil {
		return nil, err
	}
	if res.RC != 0 {
		if e := shq.Classify(res.RC, string(res.Stderr), o.Sudo); e != nil {
			if typed, ok := e.(*errs.Error); ok {
				return nil, typed.WithHost(string(host))
			}
		}
		return nil, errs.New(errs.Remote, procJournalOp, fmt.Sprintf("journalctl exit %d", res.RC)).WithHost(string(host))
	}
	entries, _ := ParseJournal(res.Stdout)
	return entries, nil
}

// ProcJournalCommand builds the journalctl line for one PID: the shape of
// JournalCommand plus the _PID= match. pid is a validated decimal number and
// every argument goes through shq.
func ProcJournalCommand(pid string, o JournalOptions) string {
	args := []string{"journalctl", "-o", "json", "--no-pager", "_PID=" + pid}
	if o.MaxPriority > 0 && o.MaxPriority < 7 {
		args = append(args, "-p", strconv.Itoa(o.MaxPriority))
	}
	from, to := o.Window.Bounds(o.Now)
	if !from.IsZero() {
		args = append(args, "--since", "@"+strconv.FormatInt(from.Unix(), 10))
		if !to.IsZero() {
			args = append(args, "--until", "@"+strconv.FormatInt(to.Unix(), 10))
		}
	}
	limit := o.Limit
	if limit <= 0 {
		limit = procJournalLimit
	}
	args = append(args, "-n", strconv.Itoa(limit))
	return shq.Join(args...)
}
