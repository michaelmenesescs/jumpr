package linux

import (
	"jumper/internal/group"
)

// Probe is one read-only catalog probe: a name, a one-line help, the remote
// command and an optional normalizer applied before outputs are grouped.
//
// Sudo marks the probes that need root for a complete answer. Every command in
// the catalog only reads: nothing writes a file, deletes anything or pipes into
// another program that changes state.
type Probe struct {
	Name      string
	Help      string
	Cmd       string
	Sudo      bool
	Normalize func(string) string
}

// Catalog probe commands. They are plain POSIX sh, run through transport, one
// per `:probe` invocation, and every one of them is read-only.
const (
	// cmdCatalogFailedUnits lists the names of failed units, one per line.
	cmdCatalogFailedUnits = "systemctl list-units --state=failed --plain --no-legend --no-pager | awk '{print $1}' | sort"
	// cmdCatalogUptime is the three load averages; no normalization, because
	// the point of the probe is that they differ between hosts.
	cmdCatalogUptime = "cat /proc/loadavg | cut -d' ' -f1-3"
	// cmdCatalogKernel is the running kernel release.
	cmdCatalogKernel = "uname -r"
	// cmdCatalogOS is the pretty distribution name.
	cmdCatalogOS = `. /etc/os-release; echo "$PRETTY_NAME"`
	// cmdCatalogListeners is the listening TCP socket addresses and their
	// processes; the process ids are stripped before grouping because they
	// differ on every host.
	cmdCatalogListeners = "ss -H -tlnp | awk '{print $4, $6}' | sort"
	// cmdCatalogDiskFull is the filesystems at 85% use or more. The use
	// percentage is matched as a pattern rather than compared with `>`, which
	// keeps the catalog free of redirection characters (see the catalog test).
	cmdCatalogDiskFull = "df -PT -x tmpfs -x devtmpfs | awk 'NR!=1 && int($6) ~ /^(8[5-9]|9[0-9]|100)$/ {print $7, $6}'"
	// cmdCatalogTimeSync is systemd's NTP synchronization verdict.
	cmdCatalogTimeSync = "timedatectl show -p NTPSynchronized --value"
	// cmdCatalogSaltMinion is the salt-minion service state.
	cmdCatalogSaltMinion = "systemctl is-active salt-minion"
)

// Catalog returns the built-in read-only probes, in display order. `unit-file
// <unit>` joins the catalog in task 086.
func Catalog() []Probe {
	return []Probe{
		{
			Name: "failed-units",
			Help: "failed systemd units, one name per host",
			Cmd:  cmdCatalogFailedUnits,
		},
		{
			Name: "uptime",
			Help: "the three load averages (expect many groups)",
			Cmd:  cmdCatalogUptime,
		},
		{
			Name: "kernel",
			Help: "running kernel release",
			Cmd:  cmdCatalogKernel,
		},
		{
			Name: "os",
			Help: "distribution pretty name",
			Cmd:  cmdCatalogOS,
		},
		{
			Name:      "listeners",
			Help:      "listening TCP sockets and their process",
			Cmd:       cmdCatalogListeners,
			Sudo:      true,
			Normalize: group.StripPIDs,
		},
		{
			Name: "disk-full",
			Help: "filesystems at 85% use or more",
			Cmd:  cmdCatalogDiskFull,
		},
		{
			Name: "time-sync",
			Help: "NTP synchronization verdict",
			Cmd:  cmdCatalogTimeSync,
		},
		{
			Name: "salt-minion",
			Help: "salt-minion service state",
			Cmd:  cmdCatalogSaltMinion,
		},
	}
}
