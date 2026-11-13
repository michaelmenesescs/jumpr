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
)

// normalizeKind maps a canonical name or one of its aliases onto the canonical
// kind. Unknown kinds report false.
func normalizeKind(kind resource.KindName) (resource.KindName, bool) {
	switch strings.ToLower(strings.TrimSpace(string(kind))) {
	case "host", "hosts", "h":
		return KindHost, true
	case "unit", "units", "u":
		return KindUnit, true
	case "process", "procs", "ps", "p":
		return KindProcess, true
	case "socket", "sockets", "ss", "s":
		return KindSocket, true
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
	}
}
