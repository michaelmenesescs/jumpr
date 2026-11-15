package prom

import "jumpr/internal/resource"

// Kind names this backend serves.
const (
	// KindTarget is one scrape target of one job.
	KindTarget resource.KindName = "target"
	// KindRule is one rule of one rule group.
	KindRule resource.KindName = "rule"
	// KindSeries is the result table of an instant query: one row per series.
	KindSeries resource.KindName = "series"
	// KindAlert is one Alertmanager alert. The fake backend registers the same
	// kind name in --demo only.
	KindAlert resource.KindName = "alert"
	// KindSilence is one Alertmanager silence.
	KindSilence resource.KindName = "silence"
)

// resource.Query argument keys this backend reads.
const (
	// ArgExpr is the PromQL expression a series list evaluates.
	ArgExpr = "expr"
	// ArgServer limits a list to one configured server by name.
	ArgServer = "server"
)

// Kind aliases accepted in the command bar.
const (
	aliasTargets  = "targets"
	aliasUp       = "up"
	aliasRules    = "rules"
	aliasPromQL   = "promql"
	aliasAlerts   = "alerts"
	aliasAlert    = "al"
	aliasSilences = "silences"
	aliasSilence  = "si"
)

// kindSpecs returns the three kinds, in the order the command bar lists them.
func kindSpecs() []resource.KindSpec {
	return []resource.KindSpec{
		{
			Name:        KindTarget,
			Aliases:     []string{aliasTargets, aliasUp},
			Title:       "Prometheus targets",
			Scope:       resource.ScopeGlobal,
			DefaultSort: "job",
			Columns: []resource.Column{
				{ID: "job", Title: "JOB", Width: 18},
				{ID: "instance", Title: "INSTANCE", Width: 0},
				{ID: "health", Title: "HEALTH", Width: 8, Kind: resource.ColStatus},
				{ID: "scrape", Title: "LAST SCRAPE", Width: 12, Kind: resource.ColTime},
				{ID: "error", Title: "ERROR", Width: 0},
			},
		},
		{
			Name:        KindRule,
			Aliases:     []string{aliasRules},
			Title:       "Prometheus rules",
			Scope:       resource.ScopeGlobal,
			DefaultSort: "group",
			Columns: []resource.Column{
				{ID: "group", Title: "GROUP", Width: 20},
				{ID: "name", Title: "NAME", Width: 0},
				{ID: "state", Title: "STATE", Width: 9},
				{ID: "health", Title: "HEALTH", Width: 8},
			},
		},
		{
			Name:        KindSeries,
			Aliases:     []string{aliasPromQL},
			Title:       "PromQL result",
			Scope:       resource.ScopeGlobal,
			DefaultSort: "labels",
			Columns: []resource.Column{
				{ID: "labels", Title: "LABELS", Width: 0},
				{ID: "value", Title: "VALUE", Width: 14, Align: resource.AlignRight, Kind: resource.ColNumber},
				{ID: "spark", Title: "WINDOW", Width: 24, Kind: resource.ColSpark},
			},
		},
	}
}

// normalizeKind maps a kind name or alias onto its canonical name.
func normalizeKind(k resource.KindName) (resource.KindName, bool) {
	switch k {
	case KindTarget, aliasTargets, aliasUp:
		return KindTarget, true
	case KindRule, aliasRules:
		return KindRule, true
	case KindSeries, aliasPromQL:
		return KindSeries, true
	}
	return "", false
}

// amKindSpecs returns the two Alertmanager kinds, in the order the command bar
// lists them. They are global: one row per alert or silence of every configured
// Alertmanager.
func amKindSpecs() []resource.KindSpec {
	return []resource.KindSpec{
		{
			Name:        KindAlert,
			Aliases:     []string{aliasAlerts, aliasAlert},
			Title:       "Alerts",
			Scope:       resource.ScopeGlobal,
			DefaultSort: "start",
			Columns: []resource.Column{
				{ID: "sev", Title: "SEV", Width: 10},
				{ID: "alert", Title: "ALERT", Width: 20},
				{ID: "labels", Title: "LABELS", Width: 34},
				{ID: "source", Title: "SOURCE", Width: 13},
				{ID: "start", Title: "START", Width: 20, Kind: resource.ColTime},
				{ID: "state", Title: "STATE", Width: 10},
			},
		},
		{
			Name:        KindSilence,
			Aliases:     []string{aliasSilences, aliasSilence},
			Title:       "Silences",
			Scope:       resource.ScopeGlobal,
			DefaultSort: "-starts",
			Columns: []resource.Column{
				{ID: "id", Title: "ID", Width: 10},
				{ID: "matchers", Title: "MATCHERS", Width: 34},
				{ID: "creator", Title: "CREATOR", Width: 12},
				{ID: "comment", Title: "COMMENT", Width: 0},
				{ID: "starts", Title: "STARTS", Width: 20, Kind: resource.ColTime},
				{ID: "ends", Title: "ENDS", Width: 20, Kind: resource.ColTime},
				{ID: "state", Title: "STATE", Width: 9},
			},
		},
	}
}

// normalizeAMKind maps an Alertmanager kind name or alias onto its canonical
// name. The Prometheus kinds are not Alertmanager kinds, so they do not match.
func normalizeAMKind(k resource.KindName) (resource.KindName, bool) {
	switch k {
	case KindAlert, aliasAlerts, aliasAlert:
		return KindAlert, true
	case KindSilence, aliasSilences, aliasSilence:
		return KindSilence, true
	}
	return "", false
}

// targetStatus maps a target health onto the row status the table colours.
func targetStatus(health string) resource.Status {
	switch health {
	case "up":
		return resource.StatusOK
	case "down":
		return resource.StatusCrit
	case "unknown":
		return resource.StatusWarn
	}
	return resource.StatusUnknown
}

// ruleStatus maps a rule health onto the row status.
func ruleStatus(health string) resource.Status {
	switch health {
	case "ok":
		return resource.StatusOK
	case "err", "error":
		return resource.StatusCrit
	case "unknown":
		return resource.StatusWarn
	}
	return resource.StatusUnknown
}
