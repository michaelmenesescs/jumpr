package chef

import (
	"strings"

	"jumpr/internal/resource"
)

// Canonical kind names served by this backend.
const (
	KindNode resource.KindName = "chef-node"
	KindRole resource.KindName = "chef-role"
	KindEnv  resource.KindName = "chef-env"
)

// normalizeKind maps a canonical name or one of its aliases onto the canonical
// kind. Unknown kinds report false.
func normalizeKind(kind resource.KindName) (resource.KindName, bool) {
	switch strings.ToLower(strings.TrimSpace(string(kind))) {
	case "chef-node", "nodes", "node":
		return KindNode, true
	case "chef-role", "roles", "role":
		return KindRole, true
	case "chef-env", "envs", "env", "environments", "environment":
		return KindEnv, true
	default:
		return "", false
	}
}

// kindSpecs returns the read-only Chef kinds. All three are global: knife
// answers for the whole Chef server, not per host.
func kindSpecs() []resource.KindSpec {
	return []resource.KindSpec{
		{
			Name:    KindNode,
			Aliases: []string{"nodes", "node"},
			Title:   "Chef nodes",
			Scope:   resource.ScopeGlobal,
			Columns: []resource.Column{
				{ID: "name", Title: "NODE", Width: 26},
				{ID: "environment", Title: "ENV", Width: 10},
				{ID: "platform", Title: "PLATFORM", Width: 16},
				{ID: "roles", Title: "ROLES", Width: 22},
				{ID: "run_list", Title: "RUN LIST", Width: 0},
				{ID: "checkin", Title: "CHECK-IN", Width: 10, Align: resource.AlignRight, Kind: resource.ColDuration},
				{ID: "status", Title: "STATUS", Width: 7, Kind: resource.ColStatus},
			},
			DefaultSort: "name",
		},
		{
			Name:    KindRole,
			Aliases: []string{"roles", "role"},
			Title:   "Chef roles",
			Scope:   resource.ScopeGlobal,
			Columns: []resource.Column{
				{ID: "name", Title: "ROLE", Width: 28},
				{ID: "nodes", Title: "NODES", Width: 7, Align: resource.AlignRight, Kind: resource.ColNumber},
			},
			DefaultSort: "name",
		},
		{
			Name:    KindEnv,
			Aliases: []string{"envs", "env", "environments", "environment"},
			Title:   "Chef environments",
			Scope:   resource.ScopeGlobal,
			Columns: []resource.Column{
				{ID: "name", Title: "ENVIRONMENT", Width: 28},
				{ID: "nodes", Title: "NODES", Width: 7, Align: resource.AlignRight, Kind: resource.ColNumber},
			},
			DefaultSort: "name",
		},
	}
}
