package chef

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"jumpr/internal/errs"
	"jumpr/internal/redact"
)

const (
	// snippetLen is how much unparsable output an error message carries.
	snippetLen = 200
	// maxAttrLines caps one precedence level's attribute lines.
	maxAttrLines = 200
	// maxAttrLineBytes caps one attribute line after redaction.
	maxAttrLineBytes = 400
)

// precedenceOrder is the order the node detail view shows Chef's attribute
// precedence levels, strongest first.
var precedenceOrder = []string{"normal", "override", "default", "automatic"}

// FirstJSON skips leading non-JSON lines and returns the input from the first
// line that starts (after spaces) with { or [. knife prints warnings before the
// JSON, and a workstation's shell rc files can print a banner.
func FirstJSON(b []byte) ([]byte, error) {
	for i := 0; i < len(b); {
		j := i
		for j < len(b) && (b[j] == ' ' || b[j] == '\t' || b[j] == '\r') {
			j++
		}
		if j < len(b) && (b[j] == '{' || b[j] == '[') {
			return b[j:], nil
		}
		nl := bytes.IndexByte(b[i:], '\n')
		if nl < 0 {
			break
		}
		i += nl + 1
	}
	return nil, errs.New(errs.Parse, "chef.decode", "no JSON in output: "+snippet(b))
}

// snippet is the first snippetLen bytes of b with trailing whitespace removed.
func snippet(b []byte) string {
	if len(b) > snippetLen {
		b = b[:snippetLen]
	}
	return strings.TrimSpace(string(b))
}

// DecodeInto decodes the first JSON value in b into v.
func DecodeInto(b []byte, v any) error {
	data, err := FirstJSON(b)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return errs.New(errs.Parse, "chef.decode", err.Error())
	}
	return nil
}

// StatusNode is one row of `knife status -F json`: a registered node with its
// last check-in. OhaiTime is the zero time when the output carried no usable
// ohai_time, which the caller renders as unknown rather than old.
type StatusNode struct {
	Name            string
	Environment     string
	Platform        string
	PlatformVersion string
	RunList         []string
	FQDN            string
	IP              string
	OhaiTime        time.Time
}

// ParseStatus decodes `knife status -F json`. The command answers with an array
// of node objects; an object keyed by node name is accepted too, because that is
// what an older knife prints. Rows the parser cannot understand are skipped.
func ParseStatus(b []byte) ([]StatusNode, error) {
	data, err := FirstJSON(b)
	if err != nil {
		return nil, err
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err == nil {
		out := make([]StatusNode, 0, len(arr))
		for _, raw := range arr {
			if n, ok := statusFromRaw("", raw); ok {
				out = append(out, n)
			}
		}
		return out, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, errs.New(errs.Parse, "chef.status", "unexpected knife status output: "+snippet(data))
	}
	names := make([]string, 0, len(obj))
	for name := range obj {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]StatusNode, 0, len(names))
	for _, name := range names {
		n, ok := statusFromRaw(name, obj[name])
		if !ok {
			continue
		}
		if n.Name == "" {
			n.Name = name
		}
		out = append(out, n)
	}
	return out, nil
}

// statusFromRaw parses one status row. fallbackName is the map key when the row
// is keyed by name; a row that is not an object or a string is skipped.
func statusFromRaw(fallbackName string, raw json.RawMessage) (StatusNode, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if fallbackName != "" {
			s = fallbackName
		}
		return StatusNode{Name: s}, s != ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return StatusNode{}, false
	}
	n := StatusNode{
		Name:            firstField(m, "name"),
		Environment:     firstField(m, "chef_environment", "environment"),
		Platform:        firstField(m, "platform"),
		PlatformVersion: firstField(m, "platform_version"),
		RunList:         firstList(m, "run_list"),
		FQDN:            firstField(m, "fqdn"),
		IP:              firstField(m, "ip", "ipaddress"),
		OhaiTime:        firstTime(m, "ohai_time"),
	}
	if n.Name == "" {
		n.Name = fallbackName
	}
	return n, n.Name != ""
}

// firstField returns the first key present as a string or a number, formatted.
func firstField(m map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		if v, ok := rawString(raw); ok {
			return v
		}
	}
	return ""
}

// firstList returns the first key present that holds a list of scalars. A single
// scalar is accepted as a one-element list.
func firstList(m map[string]json.RawMessage, keys ...string) []string {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var list []string
		if err := json.Unmarshal(raw, &list); err == nil {
			return cleanList(list)
		}
		var one string
		if err := json.Unmarshal(raw, &one); err == nil {
			return cleanList([]string{one})
		}
	}
	return nil
}

// rawString formats a JSON scalar as a string.
func rawString(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return strconv.FormatFloat(f, 'g', -1, 64), true
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return strconv.FormatBool(b), true
	}
	return "", false
}

// firstTime returns the first key that holds a usable timestamp: epoch seconds
// as a number or a numeric string, or an RFC 3339 string.
func firstTime(m map[string]json.RawMessage, keys ...string) time.Time {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		if t, ok := parseTime(raw); ok {
			return t
		}
	}
	return time.Time{}
}

// parseTime decodes one timestamp value.
func parseTime(raw json.RawMessage) (time.Time, bool) {
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return epochTime(f), true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return time.Time{}, false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return epochTime(f), true
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05 -0700", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// epochTime turns epoch seconds with a fractional part into a time.
func epochTime(f float64) time.Time {
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, nsec)
}

// cleanList trims, drops empty entries and de-duplicates, keeping order.
func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// AttrGroup is one precedence level of a node's attributes, flattened to
// "a.b.c = value" lines. Dropped counts the lines the cap removed.
type AttrGroup struct {
	Precedence string
	Lines      []string
	Dropped    int
}

// NodeDetail is what `knife node show <n> -l -F json` returns, arranged for the
// detail view's tabs: a summary, the run list and the attributes by precedence
// level. Attribute lines are redacted and capped before they are stored
// (BUILD-PLAN §7, §12).
type NodeDetail struct {
	Name            string
	Environment     string
	Platform        string
	PlatformVersion string
	ChefType        string
	RunList         []string
	Roles           []string
	Attributes      []AttrGroup
	FetchedAt       time.Time
}

// ParseNodeDetail decodes `knife node show -l -F json`. red may be nil, in
// which case attributes are only capped.
func ParseNodeDetail(b []byte, red *redact.Redactor) (NodeDetail, error) {
	data, err := FirstJSON(b)
	if err != nil {
		return NodeDetail{}, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return NodeDetail{}, errs.New(errs.Parse, "chef.node", "unexpected knife node show output: "+snippet(data))
	}
	d := NodeDetail{
		Name:            firstField(m, "name"),
		Environment:     firstField(m, "chef_environment", "environment"),
		Platform:        firstField(m, "platform"),
		PlatformVersion: firstField(m, "platform_version"),
		ChefType:        firstField(m, "chef_type"),
		RunList:         firstList(m, "run_list"),
	}
	d.Roles = rolesFromRunList(d.RunList)
	var automatic map[string]any
	for _, prec := range precedenceOrder {
		raw, ok := m[prec]
		if !ok {
			continue
		}
		var attrs map[string]any
		if err := json.Unmarshal(raw, &attrs); err != nil {
			continue
		}
		if prec == "automatic" {
			automatic = attrs
		}
		d.Attributes = append(d.Attributes, attrGroup(prec, attrs, red))
	}
	// Platform lives in the automatic attributes, not at the top level.
	if d.Platform == "" {
		d.Platform = attrString(automatic, "platform")
	}
	if d.PlatformVersion == "" {
		d.PlatformVersion = attrString(automatic, "platform_version")
	}
	return d, nil
}

// attrString returns one attribute of a decoded level as a string.
func attrString(attrs map[string]any, key string) string {
	v, ok := attrs[key]
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

// attrGroup flattens one precedence level, sorts its lines and applies the cap.
func attrGroup(prec string, attrs map[string]any, red *redact.Redactor) AttrGroup {
	lines := flattenAttrs("", attrs)
	sort.Strings(lines)
	g := AttrGroup{Precedence: prec}
	for i, line := range lines {
		if i >= maxAttrLines {
			g.Dropped = len(lines) - maxAttrLines
			break
		}
		g.Lines = append(g.Lines, capAttrLine(line, red))
	}
	return g
}

// flattenAttrs turns a nested attribute map into sorted "a.b.c = value" lines.
func flattenAttrs(prefix string, attrs map[string]any) []string {
	out := make([]string, 0, len(attrs))
	for k, v := range attrs {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if nested, ok := v.(map[string]any); ok {
			out = append(out, flattenAttrs(key, nested)...)
			continue
		}
		out = append(out, key+" = "+attrValue(v))
	}
	return out
}

// attrValue formats one attribute value.
func attrValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case json.Number:
		return t.String()
	case []any:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// capAttrLine redacts and truncates one attribute line.
func capAttrLine(line string, red *redact.Redactor) string {
	if red == nil {
		return truncateBytes(line, maxAttrLineBytes)
	}
	return red.Cap(line, maxAttrLineBytes)
}

// truncateBytes cuts s to at most max bytes on a rune boundary.
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// rolesFromRunList extracts the role names of a run list, in order.
func rolesFromRunList(runList []string) []string {
	out := make([]string, 0, len(runList))
	seen := make(map[string]bool, len(runList))
	for _, e := range runList {
		e = strings.TrimSpace(e)
		if !strings.HasPrefix(e, "role[") || !strings.HasSuffix(e, "]") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(e, "role["), "]")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// shortRunList renders a run list for a table cell: the entries as written,
// joined with ", ". It is the short form because role entries stay role[name]
// instead of being expanded into their recipes.
func shortRunList(runList []string) string { return strings.Join(runList, ", ") }

// ParseNodeNames decodes `knife node list -F json`: an array of names, or an
// object keyed by name.
func ParseNodeNames(b []byte) ([]string, error) {
	data, err := FirstJSON(b)
	if err != nil {
		return nil, err
	}
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		return cleanList(arr), nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err == nil {
		names := make([]string, 0, len(obj))
		for name := range obj {
			names = append(names, name)
		}
		sort.Strings(names)
		return cleanList(names), nil
	}
	return nil, errs.New(errs.Parse, "chef.names", "unexpected knife list output: "+snippet(data))
}

// SearchRow is one row of `knife search node <query> -a <attr>`: a node name
// and the attributes asked for, flattened to strings.
type SearchRow struct {
	Name  string
	Attrs map[string]string
}

// ParseSearch decodes `knife search … -F json`. The answer is
// {"total":n,"start":0,"rows":[{name: value}]} where a value is a scalar for one
// -a attribute and an object for several.
func ParseSearch(b []byte) ([]SearchRow, error) {
	data, err := FirstJSON(b)
	if err != nil {
		return nil, err
	}
	var res struct {
		Rows []map[string]json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, errs.New(errs.Parse, "chef.search", "unexpected knife search output: "+snippet(data))
	}
	out := make([]SearchRow, 0, len(res.Rows))
	for _, row := range res.Rows {
		names := make([]string, 0, len(row))
		for name := range row {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			out = append(out, SearchRow{Name: name, Attrs: searchAttrs(row[name])})
		}
	}
	return out, nil
}

// searchAttrs flattens one search row value into strings.
func searchAttrs(raw json.RawMessage) map[string]string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil {
		out := make(map[string]string, len(obj))
		for k, v := range obj {
			if s, ok := rawString(v); ok {
				out[k] = s
			}
		}
		return out
	}
	if s, ok := rawString(raw); ok {
		return map[string]string{"value": s}
	}
	return map[string]string{}
}
