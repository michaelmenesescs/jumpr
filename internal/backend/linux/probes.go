package linux

import (
	"jumper/internal/probe"
	"jumper/internal/resource"
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
)

// Read-only remote commands. They are plain POSIX sh, run through probe so one
// SSH round trip serves a whole kind.
const (
	cmdLoadavg  = "cat /proc/loadavg"
	cmdMeminfo  = "cat /proc/meminfo"
	cmdPSI      = `for f in cpu memory io; do echo "== $f"; cat /proc/pressure/$f 2>/dev/null; done`
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
