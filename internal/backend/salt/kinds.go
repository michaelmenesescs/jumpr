package salt

import (
	"strings"

	"jumper/internal/resource"
)

// Canonical kind names served by this backend.
const (
	KindMinion resource.KindName = "salt-minion"
	KindKey    resource.KindName = "salt-key"
	KindJobs   resource.KindName = "salt-job"
	// KindSls is the state tree: one row per sls of every environment.
	KindSls resource.KindName = "salt-sls"
	// KindMine is the mine cache: one row per minion and mine function.
	KindMine resource.KindName = "salt-mine"
)

// normalizeKind maps a canonical name or one of its aliases onto the canonical
// kind. Unknown kinds report false.
func normalizeKind(kind resource.KindName) (resource.KindName, bool) {
	switch strings.ToLower(strings.TrimSpace(string(kind))) {
	case "salt-minion", "minions", "minion":
		return KindMinion, true
	case "salt-key", "keys":
		return KindKey, true
	case "salt-job", "jobs", "job":
		return KindJobs, true
	case "salt-sls", "states", "sls":
		return KindSls, true
	case "salt-mine", "mine":
		return KindMine, true
	default:
		return "", false
	}
}

// kindSpecs returns the KindSpec values for the two read-only kinds. Both are
// global: Salt answers for the whole master, not per host.
func kindSpecs() []resource.KindSpec {
	return []resource.KindSpec{
		{
			Name:    KindMinion,
			Aliases: []string{"minions", "minion"},
			Title:   "Salt minions",
			Scope:   resource.ScopeGlobal,
			Columns: []resource.Column{
				{ID: "minion", Title: "MINION", Width: 24},
				{ID: "status", Title: "STATUS", Width: 10},
				{ID: "key", Title: "KEY", Width: 10},
				{ID: "os", Title: "OS", Width: 18},
				{ID: "role", Title: "ROLE", Width: 10},
				{ID: "dc", Title: "DC", Width: 6},
				{ID: "version", Title: "VERSION", Width: 10},
				{ID: "host", Title: "HOST", Width: 0},
			},
			DefaultSort: "minion",
		},
		{
			Name:    KindKey,
			Aliases: []string{"keys"},
			Title:   "Salt keys",
			Scope:   resource.ScopeGlobal,
			Columns: []resource.Column{
				{ID: "id", Title: "ID", Width: 24},
				{ID: "state", Title: "STATE", Width: 10},
				{ID: "fingerprint", Title: "FINGERPRINT", Width: 0},
			},
			DefaultSort: "id",
		},
		{
			Name:    KindJobs,
			Aliases: []string{"jobs", "job"},
			Title:   "Salt jobs",
			Scope:   resource.ScopeGlobal,
			Columns: []resource.Column{
				{ID: "jid", Title: "JID", Width: 22},
				{ID: "start", Title: "START", Width: 9, Kind: resource.ColTime},
				{ID: "state", Title: "STATE", Width: 7},
				{ID: "function", Title: "FUNCTION", Width: 22},
				{ID: "target", Title: "TARGET", Width: 24},
				{ID: "user", Title: "USER", Width: 10},
				{ID: "returned", Title: "RETURNED", Width: 8, Align: resource.AlignRight},
			},
			// The backend already orders running jobs first, then newest
			// first; an empty DefaultSort keeps that order.
			DefaultSort: "",
		},
		{
			Name:    KindSls,
			Aliases: []string{"states", "sls"},
			Title:   "Salt states",
			Scope:   resource.ScopeGlobal,
			Columns: []resource.Column{
				{ID: "sls", Title: "SLS", Width: 28},
				{ID: "env", Title: "ENV", Width: 8},
				{ID: "path", Title: "PATH", Width: 0},
				{ID: "assigned", Title: "ASSIGNED", Width: 8, Align: resource.AlignRight, Kind: resource.ColNumber},
			},
			DefaultSort: "sls",
		},
		{
			Name:    KindMine,
			Aliases: []string{"mine"},
			Title:   "Salt mine",
			Scope:   resource.ScopeGlobal,
			Columns: []resource.Column{
				{ID: "minion", Title: "MINION", Width: 24},
				{ID: "function", Title: "FUNCTION", Width: 24},
				{ID: "value", Title: "VALUE", Width: 0},
				{ID: "size", Title: "SIZE", Width: 10, Align: resource.AlignRight, Kind: resource.ColBytes},
			},
			DefaultSort: "minion",
		},
	}
}
