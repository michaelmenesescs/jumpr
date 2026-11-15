package linux

import (
	"jumpr/internal/probe"
	"jumpr/internal/resource"
)

// Section IDs. They are also the Op of a section-level error and the left half
// of a "_degraded" label ("psi: parse").
const (
	secLoadavg   = "loadavg"
	secMeminfo   = "meminfo"
	secPSI       = "psi"
	secOS        = "os"
	secKernel    = "kernel"
	secUptime    = "uptime"
	secNproc     = "nproc"
	secSysState  = "sysstate"
	secFailed    = "failed"
	secUnits     = "units"
	secProcs     = "procs"
	secSockets   = "sockets"
	secDF        = "df"
	secDFInode   = "df-inodes"
	secLsblk     = "lsblk"
	secDiskstats = "diskstats"
	secIPLink    = ipProbeLink
	secIPAddr    = ipProbeAddr
	secIPRoute   = ipProbeRoute
	secIPRoute6  = ipProbeRoute6
	secIPNeigh   = ipProbeNeigh
	secCgroup    = "cgroup"
	secPkg       = "pkg"
)

// Read-only remote commands. They are plain POSIX sh, run through probe so one
// SSH round trip serves a whole kind.
const (
	cmdLoadavg = "cat /proc/loadavg"
	cmdMeminfo = "cat /proc/meminfo"
	// cmdPSI reads the three /proc/pressure files. The trailing `|| true` is
	// load-bearing: a kernel built without CONFIG_PSI has no such files, and
	// without it the section's exit status is 1 (from the last cat), which the
	// backend reports as a failed section instead of "not available" — found
	// against a real Ubuntu 24.04 host on an OrbStack kernel (task 144).
	cmdPSI      = `for f in cpu memory io; do echo "== $f"; cat /proc/pressure/$f 2>/dev/null || true; done`
	cmdOS       = "cat /etc/os-release"
	cmdKernel   = "uname -r"
	cmdUptime   = "cat /proc/uptime"
	cmdNproc    = "nproc"
	cmdSysState = "systemctl is-system-running"
	cmdFailed   = "systemctl list-units --state=failed --plain --no-legend --no-pager"
	cmdUnits    = "systemctl list-units --all --plain --no-legend --no-pager"
	cmdProcs    = "ps -eo pid=,ppid=,user:32=,stat=,pcpu=,pmem=,rss=,etimes=,args="
	cmdSockets  = "ss -H -tuanp"
	cmdDF       = "df -PTk"
	cmdDFInode  = "df -Pi"
	cmdLsblk    = "lsblk -J -b -o NAME,TYPE,SIZE,FSTYPE,MOUNTPOINT,ROTA,RO,MODEL"
	// cmdDiskstats reads two snapshots a second apart inside one section so the
	// rates come from the same round trip as the device list.
	cmdDiskstats = "cat /proc/diskstats; sleep 1; cat /proc/diskstats"
	// Network sections. `ip -j` is available on RHEL 8+ and Ubuntu 20.04+;
	// `-s` nests the counters under stats64.rx/tx.
	cmdIPLink   = "ip -j -s link"
	cmdIPAddr   = "ip -j addr"
	cmdIPRoute  = "ip -j route show table main"
	cmdIPRoute6 = "ip -j -6 route"
	cmdIPNeigh  = "ip -j neigh"
	// cmdCgroup prints two iterations a second apart; the parser keeps the
	// second, where the input/output rates are populated.
	cmdCgroup = "systemd-cgtop -b -n 2 -d 1 --order=memory"
	// cmdPkg detects the package manager from /etc/os-release in the same round
	// trip and prints a "@@family rpm|deb" line before the list, so the parser
	// knows whether the fourth field is an install time (rpm) or a status
	// abbrev (deb). Read-only on both families.
	cmdPkg = `. /etc/os-release 2>/dev/null; ` +
		`case " $ID $ID_LIKE " in ` +
		`*rhel*|*fedora*|*centos*|*rocky*|*alma*|*suse*|*rpm*) ` +
		`echo "` + familyPrefix + `rpm"; ` +
		`rpm -qa --qf '%{NAME}	%{VERSION}-%{RELEASE}	%{ARCH}	%{INSTALLTIME}\n' ;; ` +
		`*) echo "` + familyPrefix + `deb"; ` +
		`dpkg-query -W -f '${Package}	${Version}	${Architecture}	${db:Status-Abbrev}\n' ;; esac`
)

// overviewSections is the host kind's batch: everything the overview row
// needs, in one round trip.
func overviewSections() []probe.Section {
	return []probe.Section{
		{ID: secLoadavg, Cmd: cmdLoadavg},
		{ID: secMeminfo, Cmd: cmdMeminfo},
		{ID: secPSI, Cmd: cmdPSI},
		{ID: secOS, Cmd: cmdOS},
		{ID: secKernel, Cmd: cmdKernel},
		{ID: secUptime, Cmd: cmdUptime},
		{ID: secNproc, Cmd: cmdNproc},
		{ID: secSysState, Cmd: cmdSysState},
		{ID: secFailed, Cmd: cmdFailed},
	}
}

// sectionsFor returns the batch sections of one canonical kind.
func sectionsFor(kind resource.KindName) []probe.Section {
	switch kind {
	case KindHost:
		return overviewSections()
	case KindUnit:
		return []probe.Section{{ID: secUnits, Cmd: cmdUnits}}
	case KindProcess:
		return []probe.Section{{ID: secProcs, Cmd: cmdProcs}}
	case KindSocket:
		// Every socket should show its process, which needs root.
		return []probe.Section{{ID: secSockets, Cmd: cmdSockets, Sudo: true}}
	case KindMount:
		return []probe.Section{
			{ID: secDF, Cmd: cmdDF},
			{ID: secDFInode, Cmd: cmdDFInode},
		}
	case KindDisk:
		return []probe.Section{
			{ID: secLsblk, Cmd: cmdLsblk},
			{ID: secDiskstats, Cmd: cmdDiskstats},
		}
	case KindInterface:
		return []probe.Section{
			{ID: secIPLink, Cmd: cmdIPLink},
			{ID: secIPAddr, Cmd: cmdIPAddr},
		}
	case KindRoute:
		return []probe.Section{
			{ID: secIPRoute, Cmd: cmdIPRoute},
			{ID: secIPRoute6, Cmd: cmdIPRoute6},
		}
	case KindNeigh:
		return []probe.Section{{ID: secIPNeigh, Cmd: cmdIPNeigh}}
	case KindCgroup:
		return []probe.Section{{ID: secCgroup, Cmd: cmdCgroup}}
	case KindPkg:
		return []probe.Section{{ID: secPkg, Cmd: cmdPkg}}
	default:
		return nil
	}
}

// newBatch compiles the kind's sections under one nonce.
func newBatch(nonce string, kind resource.KindName) (probe.Batch, error) {
	return probe.NewBatch(nonce, sectionsFor(kind)...)
}

// socketRetryBatch is the unprivileged fallback for a host where `sudo -n`
// failed: the list is still useful, only the process column is lost.
func socketRetryBatch(nonce string) (probe.Batch, error) {
	return probe.NewBatch(nonce, probe.Section{ID: secSockets, Cmd: cmdSockets})
}

// toleratesNonZero reports whether a section's non-zero exit code is normal.
// `systemctl is-system-running` exits 1 for "degraded", which is a state, not a
// failure.
func toleratesNonZero(id string) bool { return id == secSysState }

// Section is one read-only probe command: the probe group it belongs to (also
// the testdata directory a capture writes to), the file name its stdout is
// stored under, whether it needs root, and the command line itself.
type Section struct {
	Probe string
	Name  string
	Cmd   string
	Sudo  bool
}

// RawCommands returns every read-only command the Linux probes run, keyed by
// "probe/name". A capture writes each one to
// testdata/linux/<Probe>/<distro>/<Name>.txt.
//
// Parameterized commands (per unit, per file, per timer) are absent on purpose:
// they need a subject the caller picks, so there is no single raw command to
// record. The journal entry is the list command the journal view runs.
func RawCommands() map[string]Section {
	journal := JournalCommand(JournalOptions{Limit: 500, Sudo: true})
	sections := []Section{
		{"overview", "loadavg", cmdLoadavg, false},
		{"overview", "meminfo", cmdMeminfo, false},
		{"overview", "psi", cmdPSI, false},
		{"overview", "os-release", cmdOS, false},
		{"overview", "kernel", cmdKernel, false},
		{"overview", "uptime", cmdUptime, false},
		{"overview", "nproc", cmdNproc, false},
		{"overview", "sysstate", cmdSysState, false},
		{"overview", "failed", cmdFailed, false},
		{"units", "list-units", cmdUnits, false},
		{"ps", "ps", cmdProcs, false},
		{"ss", "ss", cmdSockets, true},
		{"disk", "df", cmdDF, false},
		{"disk", "df-inodes", cmdDFInode, false},
		{"disk", "lsblk", cmdLsblk, false},
		{"disk", "diskstats", cmdDiskstats, false},
		{"ip", "link", cmdIPLink, false},
		{"ip", "addr", cmdIPAddr, false},
		{"ip", "route", cmdIPRoute, false},
		{"ip", "route6", cmdIPRoute6, false},
		{"ip", "neigh", cmdIPNeigh, false},
		{"cgroup", "cgroup", cmdCgroup, false},
		{"pkg", "pkg", cmdPkg, false},
		{"timers", "list-timers", cmdTimers, false},
		{"journal", "journal", journal, true},
	}
	out := make(map[string]Section, len(sections))
	for _, s := range sections {
		out[s.Probe+"/"+s.Name] = s
	}
	return out
}
