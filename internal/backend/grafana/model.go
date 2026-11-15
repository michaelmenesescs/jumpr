// Dashboard model: the parts of a Grafana dashboard JSON document jumpr
// understands (SPEC §8.4).
//
// The dashboard schema varies by Grafana version: `datasource` is a name string
// in old documents and an object with `uid`/`type` in new ones; legacy documents
// keep panels under `rows` instead of `panels`; `targets` is absent on row
// panels. Every decoder here accepts both shapes and skips what it does not
// understand instead of failing the whole dashboard.

package grafana

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"jumpr/internal/resource"
)

// datasourceTypes are the datasource type names a legacy `datasource` string can
// carry. A string of one of these is a type, anything else is a uid or name.
var datasourceTypes = map[string]bool{
	"prometheus":    true,
	"elasticsearch": true,
	"loki":          true,
	"graphite":      true,
	"influxdb":      true,
	"cloudwatch":    true,
	"sql":           true,
}

// supportedPanelTypes are the panel types task 120 renders inside the TUI
// (SPEC §8.4). Everything else is shown with its title and opened in a browser.
var supportedPanelTypes = map[string]bool{
	"timeseries": true,
	// "graph" is what Grafana called timeseries before 8.0; the same panel
	// type backed by the same queries.
	"graph": true,
	"stat":  true,
	"gauge": true,
}

// Datasource identifies a Grafana datasource. It is the both-shapes value:
// an old document's name string or a new document's {uid,type} object.
type Datasource struct {
	UID  string
	Type string
	Name string
}

// UnmarshalJSON accepts a datasource name string, a {uid,type} object, or null.
func (d *Datasource) UnmarshalJSON(b []byte) error {
	*d = Datasource{}
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return nil
	}
	if s[0] == '"' {
		var name string
		if err := json.Unmarshal(b, &name); err != nil {
			return err
		}
		d.Name = name
		if datasourceTypes[strings.ToLower(name)] {
			d.Type = strings.ToLower(name)
			return nil
		}
		// A name or a uid: either way it identifies the datasource.
		d.UID = name
		return nil
	}
	var obj struct {
		UID  string `json:"uid"`
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		// An array or a number is not a datasource; ignore it.
		return nil
	}
	d.UID, d.Type, d.Name = obj.UID, obj.Type, obj.Name
	return nil
}

// MarshalJSON renders the unified-query shape, using the name as the uid when
// the document only carried a legacy name string.
func (d Datasource) MarshalJSON() ([]byte, error) {
	uid := d.UID
	if uid == "" {
		uid = d.Name
	}
	out := map[string]string{}
	if uid != "" {
		out["uid"] = uid
	}
	if d.Type != "" {
		out["type"] = d.Type
	}
	return json.Marshal(out)
}

// IsZero reports whether the datasource carries no identity at all.
func (d Datasource) IsZero() bool { return d.UID == "" && d.Type == "" && d.Name == "" }

// Target is one query of a panel.
type Target struct {
	RefID      string
	Expr       string
	Query      string
	Datasource Datasource
	Hidden     bool
}

// Expression returns the query text, whichever field carried it.
func (t Target) Expression() string {
	if strings.TrimSpace(t.Expr) != "" {
		return strings.TrimSpace(t.Expr)
	}
	return strings.TrimSpace(t.Query)
}

// UnmarshalJSON decodes a target defensively: expr and query may be strings or
// numbers, and a target without a refId still counts.
func (t *Target) UnmarshalJSON(b []byte) error {
	var raw struct {
		RefID      json.RawMessage `json:"refId"`
		Expr       json.RawMessage `json:"expr"`
		Query      json.RawMessage `json:"query"`
		Datasource Datasource      `json:"datasource"`
		Hide       bool            `json:"hide"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil // a malformed target is skipped, not fatal
	}
	t.RefID = text(raw.RefID)
	t.Expr = text(raw.Expr)
	t.Query = text(raw.Query)
	t.Datasource = raw.Datasource
	t.Hidden = raw.Hide
	return nil
}

// GridPos is a panel's place in the dashboard grid.
type GridPos struct {
	H int `json:"h"`
	W int `json:"w"`
	X int `json:"x"`
	Y int `json:"y"`
}

// Panel is one dashboard panel. A row panel has Type "row" and its children in
// Panels; row panels carry no targets.
type Panel struct {
	ID         int
	Title      string
	Type       string
	Datasource Datasource
	Targets    []Target
	Panels     []Panel
	GridPos    GridPos
}

// UnmarshalJSON accepts a numeric or string id and every datasource shape.
func (p *Panel) UnmarshalJSON(b []byte) error {
	var raw struct {
		ID         json.RawMessage `json:"id"`
		Title      string          `json:"title"`
		Type       string          `json:"type"`
		Datasource Datasource      `json:"datasource"`
		Targets    []Target        `json:"targets"`
		Panels     []Panel         `json:"panels"`
		GridPos    GridPos         `json:"gridPos"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil
	}
	if n, err := strconv.Atoi(text(raw.ID)); err == nil {
		p.ID = n
	}
	p.Title = raw.Title
	p.Type = raw.Type
	p.Datasource = raw.Datasource
	p.Targets = raw.Targets
	p.Panels = raw.Panels
	p.GridPos = raw.GridPos
	return nil
}

// SupportedPanelType reports whether the TUI renders the panel type itself.
func SupportedPanelType(typ string) bool {
	return supportedPanelTypes[strings.ToLower(strings.TrimSpace(typ))]
}

// IsRow reports whether the panel is a row container.
func (p Panel) IsRow() bool { return strings.EqualFold(p.Type, "row") }

// VariableOption is one choice of a template variable.
type VariableOption struct {
	Text     string
	Value    string
	Selected bool
}

// VariableQuery is a template variable's query: a string like
// "label_values(node_load1, instance)" or the object form that carries the same
// text plus a refId and a datasource.
type VariableQuery struct {
	Expr  string
	RefID string
}

// String returns the query text without its object wrapper.
func (q VariableQuery) String() string { return strings.TrimSpace(q.Expr) }

// Variable is one dashboard template variable (SPEC §8.4). Current holds the
// effective value; Options hold the choices a select-type variable offers.
type Variable struct {
	Name       string
	Type       string
	Current    []string
	Options    []VariableOption
	Query      VariableQuery
	Datasource Datasource
	// Auto marks a variable jumpr filled from the session target rather than
	// one the user chose.
	Auto bool
}

// labelValuesRE matches the simple label_values(form, label) form jumpr
// resolves through /api/ds/query: a bare metric name and label, no selector
// and no function. Anything richer is free text.
var labelValuesRE = regexp.MustCompile(`^label_values\(\s*[A-Za-z_:][A-Za-z0-9_:]*\s*,\s*[A-Za-z_][A-Za-z0-9_]*\s*\)$`)

// UnmarshalJSON accepts the current value as a string, a list, or Grafana's
// {text,value} object; options as strings or objects; and the query as a string
// or an object.
func (v *Variable) UnmarshalJSON(b []byte) error {
	var raw struct {
		Name       string            `json:"name"`
		Type       string            `json:"type"`
		Current    json.RawMessage   `json:"current"`
		Options    []json.RawMessage `json:"options"`
		Query      json.RawMessage   `json:"query"`
		Datasource Datasource        `json:"datasource"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil
	}
	v.Name = raw.Name
	v.Type = raw.Type
	v.Datasource = raw.Datasource
	v.Current = parseVariableValue(raw.Current)
	for _, o := range raw.Options {
		if opt, ok := parseVariableOption(o); ok {
			v.Options = append(v.Options, opt)
		}
	}
	v.Query = parseVariableQuery(raw.Query)
	return nil
}

// parseVariableValue decodes a current value: a string, a list of strings, or a
// {text,value} object whose fields may themselves be strings or lists.
func parseVariableValue(raw json.RawMessage) []string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	switch s[0] {
	case '[':
		var list []string
		if err := json.Unmarshal(raw, &list); err == nil {
			return compact(list)
		}
		var anys []any
		if err := json.Unmarshal(raw, &anys); err == nil {
			out := make([]string, 0, len(anys))
			for _, a := range anys {
				out = append(out, scalar(a))
			}
			return compact(out)
		}
	case '{':
		var obj struct {
			Text  json.RawMessage `json:"text"`
			Value json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil
		}
		if vals := parseVariableValue(obj.Value); len(vals) > 0 {
			return vals
		}
		return parseVariableValue(obj.Text)
	case '"':
		var one string
		if err := json.Unmarshal(raw, &one); err == nil {
			return compact([]string{one})
		}
	}
	return nil
}

// parseVariableOption decodes one option: a plain string or an object.
func parseVariableOption(raw json.RawMessage) (VariableOption, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return VariableOption{}, false
	}
	if s[0] == '"' {
		var one string
		if err := json.Unmarshal(raw, &one); err != nil {
			return VariableOption{}, false
		}
		return VariableOption{Text: one, Value: one}, true
	}
	var obj struct {
		Text     json.RawMessage `json:"text"`
		Value    json.RawMessage `json:"value"`
		Selected bool            `json:"selected"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return VariableOption{}, false
	}
	opt := VariableOption{Text: text(obj.Text), Value: text(obj.Value), Selected: obj.Selected}
	if opt.Text == "" {
		opt.Text = opt.Value
	}
	if opt.Value == "" {
		opt.Value = opt.Text
	}
	return opt, true
}

// parseVariableQuery decodes a query: a string, or an object with query/expr.
func parseVariableQuery(raw json.RawMessage) VariableQuery {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return VariableQuery{}
	}
	if s[0] == '"' {
		var one string
		if err := json.Unmarshal(raw, &one); err == nil {
			return VariableQuery{Expr: one}
		}
		return VariableQuery{}
	}
	var obj struct {
		Query json.RawMessage `json:"query"`
		Expr  json.RawMessage `json:"expr"`
		RefID string          `json:"refId"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return VariableQuery{}
	}
	q := VariableQuery{RefID: obj.RefID}
	if q.Expr = text(obj.Query); q.Expr == "" {
		q.Expr = text(obj.Expr)
	}
	return q
}

// TimeDefaults is the time range a dashboard opens with, and its auto-refresh.
type TimeDefaults struct {
	From    string
	To      string
	Refresh string
}

// Dashboard is a dashboard's definition: what the panel grid and the variable
// header need. It is the Raw payload of the dashboard kind's rows.
type Dashboard struct {
	UID       string
	Title     string
	Slug      string
	Folder    string
	Tags      []string
	Panels    []Panel
	Variables []Variable
	Time      TimeDefaults
	URL       string
}

// UnmarshalJSON decodes the dashboard object of /api/dashboards/uid/<uid>.
// Legacy documents keep their rows under "rows"; each becomes a row panel so
// PanelRows walks one shape.
func (d *Dashboard) UnmarshalJSON(b []byte) error {
	var raw struct {
		UID    string `json:"uid"`
		Title  string `json:"title"`
		Slug   string `json:"slug"`
		Tags   []string
		Panels []Panel `json:"panels"`
		Rows   []struct {
			Title  string  `json:"title"`
			Panels []Panel `json:"panels"`
		} `json:"rows"`
		Templating struct {
			List []Variable `json:"list"`
		} `json:"templating"`
		Time struct {
			From    string `json:"from"`
			To      string `json:"to"`
			Refresh string `json:"refresh"`
		} `json:"time"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	d.UID = raw.UID
	d.Title = raw.Title
	d.Slug = raw.Slug
	d.Tags = raw.Tags
	d.Panels = raw.Panels
	d.Variables = raw.Templating.List
	d.Time = TimeDefaults{From: raw.Time.From, To: raw.Time.To, Refresh: raw.Time.Refresh}
	if len(d.Panels) == 0 && len(raw.Rows) > 0 {
		rows := make([]Panel, 0, len(raw.Rows))
		for _, r := range raw.Rows {
			rows = append(rows, Panel{Type: "row", Title: r.Title, Panels: r.Panels})
		}
		d.Panels = rows
	}
	return nil
}

// PanelRow is one cell of the dashboard panel grid: a panel's title, type and
// whether the TUI can render it. Section is the row panel it sits under.
type PanelRow struct {
	ID        int
	Title     string
	Type      string
	Section   string
	Supported bool
}

// PanelRows flattens the panel grid. Row panels are containers, so they become
// the Section of the panels they hold instead of grid cells themselves.
func (d *Dashboard) PanelRows() []PanelRow {
	if d == nil {
		return nil
	}
	var out []PanelRow
	var walk func(panels []Panel, section string)
	walk = func(panels []Panel, section string) {
		for _, p := range panels {
			if p.IsRow() {
				title := p.Title
				if title == "" {
					title = section
				}
				walk(p.Panels, title)
				continue
			}
			out = append(out, PanelRow{
				ID:        p.ID,
				Title:     p.Title,
				Type:      p.Type,
				Section:   section,
				Supported: SupportedPanelType(p.Type),
			})
			if len(p.Panels) > 0 {
				walk(p.Panels, section)
			}
		}
	}
	walk(d.Panels, "")
	return out
}

// Variable returns the variable called name, with or without a leading "$".
func (d *Dashboard) Variable(name string) (Variable, bool) {
	if d == nil {
		return Variable{}, false
	}
	name = strings.TrimPrefix(strings.TrimSpace(name), "$")
	for i := range d.Variables {
		if d.Variables[i].Name == name {
			return d.Variables[i], true
		}
	}
	return Variable{}, false
}

// SetVariable sets a variable's current value and returns false when the
// dashboard has no such variable.
func (d *Dashboard) SetVariable(name, value string) bool {
	if d == nil {
		return false
	}
	name = strings.TrimPrefix(strings.TrimSpace(name), "$")
	for i := range d.Variables {
		if d.Variables[i].Name != name {
			continue
		}
		d.Variables[i].SetValues(value)
		d.Variables[i].Auto = false
		return true
	}
	return false
}

// SetValues replaces the current value and reflects it in the options.
func (v *Variable) SetValues(values ...string) {
	v.Current = compact(values)
	for i := range v.Options {
		v.Options[i].Selected = containsString(v.Current, v.Options[i].Value)
	}
	for _, val := range v.Current {
		if !v.hasOption(val) {
			v.Options = append(v.Options, VariableOption{Text: val, Value: val, Selected: true})
		}
	}
}

// hasOption reports whether an option with the value exists.
func (v *Variable) hasOption(value string) bool {
	for _, o := range v.Options {
		if o.Value == value {
			return true
		}
	}
	return false
}

// Value returns the first current value, or "".
func (v Variable) Value() string {
	if len(v.Current) == 0 {
		return ""
	}
	return v.Current[0]
}

// Values returns name → value for every variable, the form a query
// interpolator consumes (task 120).
func (d *Dashboard) Values() map[string]string {
	out := make(map[string]string, len(d.Variables))
	for _, v := range d.Variables {
		out[v.Name] = v.Value()
	}
	return out
}

// LabelValuesExpr returns the query text when the variable's query is the
// simple label_values(form, label) form /api/ds/query can resolve.
func (v Variable) LabelValuesExpr() (string, bool) {
	expr := v.Query.String()
	if !labelValuesRE.MatchString(expr) {
		return "", false
	}
	return expr, true
}

// autoVariableNames are the variables SPEC §8.4 fills from the session target.
var autoVariableNames = map[string]bool{"host": true, "instance": true, "node": true}

// AutoFill fills $host, $instance and $node from the first target host and
// marks them auto. A variable the user already set keeps its value; an empty
// hosts list changes nothing. It returns the names it filled.
func (d *Dashboard) AutoFill(hosts []resource.HostID) []string {
	if d == nil || len(hosts) == 0 {
		return nil
	}
	host := strings.TrimSpace(string(hosts[0]))
	if host == "" {
		return nil
	}
	var filled []string
	for i := range d.Variables {
		v := &d.Variables[i]
		if !autoVariableNames[strings.ToLower(v.Name)] {
			continue
		}
		if len(v.Current) > 0 && !v.Auto {
			continue
		}
		v.SetValues(host)
		v.Auto = true
		filled = append(filled, v.Name)
	}
	return filled
}

// DashboardPath is the browser path of a dashboard: /d/<uid>/<slug>.
func DashboardPath(uid, slug string) string {
	uid = strings.TrimSpace(uid)
	slug = strings.TrimSpace(slug)
	if uid == "" {
		return ""
	}
	if slug == "" {
		slug = uid
	}
	return "/d/" + uid + "/" + slug
}

// PanelPath is the browser path of one panel: the dashboard path with
// ?viewPanel=<id>.
func PanelPath(uid, slug string, panelID int) string {
	base := DashboardPath(uid, slug)
	if base == "" || panelID <= 0 {
		return base
	}
	return base + "?viewPanel=" + strconv.Itoa(panelID)
}

// text turns a JSON scalar into a string; an object or array becomes "".
func text(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	switch s[0] {
	case '"':
		var out string
		if err := json.Unmarshal(raw, &out); err == nil {
			return out
		}
	case '{', '[':
		return ""
	default:
		var num json.Number
		if err := json.Unmarshal(raw, &num); err == nil {
			return num.String()
		}
	}
	return ""
}

// scalar stringifies one decoded JSON value for a variable's current list.
func scalar(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

// compact drops empty strings and duplicates while keeping order.
func compact(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// containsString reports whether list holds s.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// LabelString renders a label set the way the alert row shows it: sorted
// key="value" pairs inside braces.
func LabelString(labels map[string]string) string {
	if len(labels) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(labels[k])
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}
