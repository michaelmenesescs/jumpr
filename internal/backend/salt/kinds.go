package salt

import (
	"strings"

	"jumper/internal/resource"
)

// Canonical kind names served by this backend.
const (
	KindMinion resource.KindName = "salt-minion"
	KindKey    resource.KindName = "salt-key"
)

// normalizeKind maps a canonical name or one of its aliases onto the canonical
// kind. Unknown kinds report false.
func normalizeKind(kind resource.KindName) (resource.KindName, bool) {
	switch strings.ToLower(strings.TrimSpace(string(kind))) {
	case "salt-minion", "minions", "minion":
		return KindMinion, true
	case "salt-key", "keys":
		return KindKey, true
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
	}
}
