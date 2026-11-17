package linux

import (
	"strconv"
	"strings"

	"jumpr/internal/errs"
	"jumpr/internal/group"
	"jumpr/internal/shq"
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
	// cmdCatalogFirewall prints the active rule set with whichever tool the
	// host has. `command -v` prints the tool's path first, which says which
	// packet filter the rules below belong to.
	cmdCatalogFirewall = "if command -v nft; then nft list ruleset; elif command -v iptables-save; then iptables-save; else echo no nft or iptables on this host; fi"
	// cmdCatalogConnections is every established TCP connection as
	// "local peer", the raw material of the host side of the service map.
	cmdCatalogConnections = "ss -H -tn state established | awk '{print $3, $4}' | sort"
	// cmdCatalogTimeSync is systemd's NTP synchronization verdict.
	cmdCatalogTimeSync = "timedatectl show -p NTPSynchronized --value"
	// cmdCatalogSaltMinion is the salt-minion service state.
	cmdCatalogSaltMinion = "systemctl is-active salt-minion"
)

// Catalog returns the built-in read-only probes, in display order. `unit-file`
// is parameterized (it takes a unit name), so it is built by UnitFileProbe and
// not listed here.
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
		{
			Name: "firewall",
			Help: "the packet filter rule set: nftables, else iptables",
			Cmd:  cmdCatalogFirewall,
			Sudo: true,
		},
		{
			Name: "connections",
			Help: "established TCP connections: local and peer address",
			Cmd:  cmdCatalogConnections,
		},
	}
}

// cmdCatalogUnitFileMissing is what the `unit-file` probe prints on a host that
// has no file for the unit, so those hosts form a group of their own instead of
// an empty one.
const cmdCatalogUnitFileMissing = "no such unit"

// unitFileCmd is `systemctl cat <unit> --no-pager`, or the missing marker when
// the host has no unit file. The unit name is already validated and quoted with
// shq; the `||` keeps the batch raw, with no redirection.
func unitFileCmd(unit string) string {
	return shq.Join("systemctl", "cat", unit, "--no-pager") + " || echo '" + cmdCatalogUnitFileMissing + "'"
}

// unitFileNormalize drops nothing but trailing whitespace: every line loses its
// trailing spaces, tabs and carriage return, and the text loses its trailing
// blank lines. Nothing else changes, so the `# /etc/systemd/system/...` headers
// of `systemctl cat` survive and a host with a drop-in never groups with one
// without.
func unitFileNormalize(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " 	\r")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// UnitFileProbe is the parameterized `unit-file` probe for one unit. It is not
// in Catalog(), whose entries take no argument: `:unitdiff <unit>` builds it,
// because the unit name is part of the command. An unusable unit name is an
// errs.Config error.
func UnitFileProbe(unit string) (Probe, error) {
	unit = strings.TrimSpace(unit)
	if !shq.ValidUnitName(unit) {
		return Probe{}, errs.New(errs.Config, "linux.UnitFileProbe",
			"invalid unit name "+strconv.Quote(unit))
	}
	return Probe{
		Name:      "unit-file",
		Help:      "one unit's files, drop-ins included",
		Cmd:       unitFileCmd(unit),
		Normalize: unitFileNormalize,
	}, nil
}
