package linux

import (
	"strings"

	"jumper/internal/resource"
)

// Canonical kind names served by this backend. The aliases are listed in
// kindSpecs.
const (
	KindHost    resource.KindName = "host"
	KindUnit    resource.KindName = "unit"
	KindProcess resource.KindName = "process"
	KindSocket  resource.KindName = "socket"
	KindJournal resource.KindName = "journal"
	// KindHostOverview is the single-row view kind for one host: it is served
	// by the host kind's own overview batch and always yields the one row.
	KindHostOverview resource.KindName = "host-overview"
	KindMount        resource.KindName = "mount"
	KindDisk         resource.KindName = "disk"
)

// normalizeKind maps a canonical name or one of its aliases onto the canonical
// kind. Unknown kinds report false. The host-overview kind is served by the
// host batch, so it normalizes onto host.
func normalizeKind(kind resource.KindName) (resource.KindName, bool) {
	switch strings.ToLower(strings.TrimSpace(string(kind))) {
	case "host", "hosts", "h", "host-overview", "overview":
		return KindHost, true
	case "unit", "units", "u":
		return KindUnit, true
	case "process", "procs", "ps", "p":
		return KindProcess, true
	case "socket", "sockets", "ss", "s":
		return KindSocket, true
	case "journal", "journals", "j", "logs":
		return KindJournal, true
	case "mount", "mounts", "df":
		return KindMount, true
	case "disk", "disks", "lsblk":
		return KindDisk, true
	default:
		return "", false
	}
}

// kindSpecs returns the KindSpec values for the four core kinds.
//
// The unit kind carries a "host" field on every row; a view that lists units
// across more than one host prepends a host column using that field, since
// KindSpec is not query-dependent.
func kindSpecs() []resource.KindSpec {
	return []resource.KindSpec{
		{
			Name:    KindHost,
			Aliases: []string{"hosts", "h"},
			Title:   "Hosts",
			Scope:   resource.ScopeGlobal,
			Columns: []resource.Column{
				{ID: "host", Title: "HOST", Width: 16},
				{ID: "load", Title: "LOAD", Width: 6, Align: resource.AlignRight, Kind: resource.ColNumber},
				{ID: "mem", Title: "MEM%", Width: 6, Align: resource.AlignRight, Kind: resource.ColPercent},
				{ID: "psi_io", Title: "PSI-IO", Width: 7, Align: resource.AlignRight, Kind: resource.ColPercent},
				{ID: "failed", Title: "FAILED", Width: 7, Align: resource.AlignRight, Kind: resource.ColNumber},
				{ID: "units", Title: "UNITS", Width: 20},
				{ID: "os", Title: "OS", Width: 18},
				{ID: "kernel", Title: "KERNEL", Width: 12},
				{ID: "uptime", Title: "UPTIME", Width: 8, Align: resource.AlignRight, Kind: resource.ColDuration},
				{ID: "state", Title: "STATE", Width: 10},
			},
			DefaultSort: "host",
		},
		{
			// Single-row view kind for one host. It is served by the host batch
			// (see normalizeKind) and has a dedicated view, so these columns only
			// matter to the fallback table and to `:kinds`.
			Name:    KindHostOverview,
			Aliases: []string{"overview"},
			Title:   "Host overview",
			Scope:   resource.ScopeHost,
			Columns: []resource.Column{
				{ID: "host", Title: "HOST", Width: 0},
				{ID: "state", Title: "STATE", Width: 10},
				{ID: "failed", Title: "FAILED", Width: 7, Align: resource.AlignRight, Kind: resource.ColNumber},
			},
			DefaultSort: "host",
		},
		{
			Name:    KindUnit,
			Aliases: []string{"units", "u"},
			Title:   "Units",
			Scope:   resource.ScopeHost,
			Columns: []resource.Column{
				{ID: "unit", Title: "UNIT", Width: 24},
				{ID: "load", Title: "LOAD", Width: 8},
				{ID: "active", Title: "ACTIVE", Width: 9},
				{ID: "sub", Title: "SUB", Width: 9},
				{ID: "description", Title: "DESCRIPTION", Width: 0},
			},
		},
		{
			Name:    KindProcess,
			Aliases: []string{"procs", "ps", "p"},
			Title:   "Processes",
			Scope:   resource.ScopeHost,
			Columns: []resource.Column{
				{ID: "pid", Title: "PID", Width: 7, Align: resource.AlignRight, Kind: resource.ColNumber},
				{ID: "user", Title: "USER", Width: 10},
				{ID: "cpu", Title: "CPU%", Width: 6, Align: resource.AlignRight, Kind: resource.ColNumber},
				{ID: "mem", Title: "MEM%", Width: 6, Align: resource.AlignRight, Kind: resource.ColPercent},
				{ID: "rss", Title: "RSS", Width: 9, Align: resource.AlignRight, Kind: resource.ColBytes},
				{ID: "elapsed", Title: "ELAPSED", Width: 8, Align: resource.AlignRight, Kind: resource.ColDuration},
				{ID: "command", Title: "COMMAND", Width: 0},
			},
			DefaultSort: "-cpu",
		},
		{
			Name:    KindSocket,
			Aliases: []string{"sockets", "ss", "s"},
			Title:   "Sockets",
			Scope:   resource.ScopeHost,
			Columns: []resource.Column{
				{ID: "proto", Title: "PROTO", Width: 5},
				{ID: "state", Title: "STATE", Width: 8},
				{ID: "recvq", Title: "RECV-Q", Width: 7, Align: resource.AlignRight, Kind: resource.ColNumber},
				{ID: "sendq", Title: "SEND-Q", Width: 7, Align: resource.AlignRight, Kind: resource.ColNumber},
				{ID: "local", Title: "LOCAL", Width: 24},
				{ID: "peer", Title: "PEER", Width: 24},
				{ID: "process", Title: "PROCESS", Width: 0},
			},
		},
		{
			// Journal rows carry a "host" field as well; a view that lists more
			// than one host prepends a host column and merge-sorts by time,
			// because KindSpec is not query-dependent.
			Name:    KindJournal,
			Aliases: []string{"journals", "j", "logs"},
			Title:   "Journal",
			Scope:   resource.ScopeHost,
			Columns: []resource.Column{
				{ID: "time", Title: "TIME", Width: 8, Kind: resource.ColTime},
				{ID: "pri", Title: "PRI", Width: 4},
				{ID: "source", Title: "SOURCE", Width: 16},
				{ID: "message", Title: "MESSAGE", Width: 0},
			},
		},
		{
			Name:    KindMount,
			Aliases: []string{"mounts", "df"},
			Title:   "Mounts",
			Scope:   resource.ScopeHost,
			Columns: []resource.Column{
				{ID: "mount", Title: "MOUNT", Width: 26},
				{ID: "fstype", Title: "FSTYPE", Width: 8},
				{ID: "size", Title: "SIZE", Width: 9, Align: resource.AlignRight, Kind: resource.ColBytes},
				{ID: "used", Title: "USED", Width: 9, Align: resource.AlignRight, Kind: resource.ColBytes},
				{ID: "avail", Title: "AVAIL", Width: 9, Align: resource.AlignRight, Kind: resource.ColBytes},
				{ID: "use", Title: "USE%", Width: 6, Align: resource.AlignRight, Kind: resource.ColPercent},
				{ID: "iuse", Title: "IUSE%", Width: 6, Align: resource.AlignRight, Kind: resource.ColPercent},
				{ID: "source", Title: "SOURCE", Width: 0},
			},
			DefaultSort: "mount",
		},
		{
			Name:    KindDisk,
			Aliases: []string{"disks", "lsblk"},
			Title:   "Disks",
			Scope:   resource.ScopeHost,
			Columns: []resource.Column{
				{ID: "name", Title: "NAME", Width: 20},
				{ID: "type", Title: "TYPE", Width: 8},
				{ID: "size", Title: "SIZE", Width: 9, Align: resource.AlignRight, Kind: resource.ColBytes},
				{ID: "fstype", Title: "FSTYPE", Width: 8},
				{ID: "mount", Title: "MOUNT", Width: 22},
				{ID: "r/s", Title: "R/S", Width: 7, Align: resource.AlignRight, Kind: resource.ColNumber},
				{ID: "w/s", Title: "W/S", Width: 7, Align: resource.AlignRight, Kind: resource.ColNumber},
				{ID: "util", Title: "UTIL%", Width: 6, Align: resource.AlignRight, Kind: resource.ColPercent},
				{ID: "rota", Title: "ROTA", Width: 5},
				{ID: "ro", Title: "RO", Width: 3},
				{ID: "model", Title: "MODEL", Width: 0},
			},
			DefaultSort: "name",
		},
	}
}
