// Package elastic is the Elasticsearch backend (SPEC §8.5): saved log sources,
// the `_search` query DSL with `query_string`, `search_after` paging,
// `_field_caps` field completion, `_resolve/index` and `_cat/indices` for the
// source browser, plus the `GET /` version probe.
//
// It is read-only and talks to Elasticsearch through an
// internal/transport/httpx client, so proxies, TLS, auth helpers and retries
// are the connection profile's business, not a backend's. The client refuses
// every path except the five read endpoints of SPEC §8.5.
package elastic

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"jumper/internal/resource"
)

// SearchSize is how many hits one `_search` asks for (SPEC §8.5).
const SearchSize = 500

// hostChunk is the most values one `terms` clause carries.
const hostChunk = 1000

// SearchOptions is one page of one log source: the window, the hosts and the
// text come from the session and the investigation.
type SearchOptions struct {
	// Source is the configured log source the page is read from.
	Source LogSource
	// Hosts are the session targets. Both the full name and the short name of
	// every target are matched (SPEC §8.5).
	Hosts []resource.HostID
	// QueryString is the text the user typed. It is optional and always sent
	// through encoding/json.
	QueryString string
	// From and To bound the search on the source's time field. A zero From
	// leaves the lower bound out; a zero To leaves the upper bound out.
	From time.Time
	To   time.Time
	// Columns are extra `_source` fields the user added as columns.
	Columns []string
	// SearchAfter is the sort values of the last hit of the previous page.
	SearchAfter []any
	// PIT is an open point-in-time id. Empty searches the index directly.
	PIT string
	// Size is the page size. Zero means SearchSize.
	Size int
}

// SearchBody is the JSON body of one `_search`. Field order is stable so a
// request can be asserted as JSON.
type SearchBody struct {
	Size           int          `json:"size"`
	TrackTotalHits bool         `json:"track_total_hits"`
	Source         []string     `json:"_source,omitempty"`
	Query          Query        `json:"query"`
	Sort           []SortField  `json:"sort"`
	SearchAfter    []any        `json:"search_after,omitempty"`
	PIT            *PointInTime `json:"pit,omitempty"`
}

// Query is the `query` part of a search. jumper only builds bool queries:
// every clause it adds is a filter or a must, never a scoring change.
type Query struct {
	Bool Bool `json:"bool"`
}

// Bool is a bool query. Filter clauses AND together.
type Bool struct {
	Filter []any `json:"filter"`
	Must   []any `json:"must,omitempty"`
}

// SortField is one sort key, e.g. {"@timestamp": {"order": "asc"}}.
type SortField map[string]SortOrder

// SortOrder is the direction of one sort key.
type SortOrder struct {
	Order string `json:"order"`
}

// PointInTime is the `pit` part of a search.
type PointInTime struct {
	ID        string `json:"id"`
	KeepAlive string `json:"keep_alive"`
}

// BuildSearch returns the body one page sends. The clauses are, in order, the
// time range, the host terms and the user's `query_string`; the sort is the
// time field ascending with a tiebreaker so `search_after` can page.
func BuildSearch(o SearchOptions) SearchBody {
	src := o.Source.Normalized()
	size := o.Size
	if size <= 0 {
		size = SearchSize
	}

	filter := []any{rangeClause(src.TimeField, o.From, o.To)}
	if clause := hostClause(src.HostField, o.Hosts); clause != nil {
		filter = append(filter, clause)
	}

	body := SearchBody{
		Size:           size,
		TrackTotalHits: false,
		Source:         sourceFields(src, o.Columns),
		Query:          Query{Bool: Bool{Filter: filter}},
		Sort:           sortFields(src.TimeField, o.PIT),
		SearchAfter:    o.SearchAfter,
	}
	if q := strings.TrimSpace(o.QueryString); q != "" {
		body.Query.Bool.Must = []any{queryStringClause(q)}
	}
	if o.PIT != "" {
		body.PIT = &PointInTime{ID: o.PIT, KeepAlive: pitKeepAlive}
	}
	return body
}

// pitKeepAlive is how long a point in time stays open; the client never closes
// one (closing is a DELETE, which the read-only rule refuses).
const pitKeepAlive = "1m"

// rangeClause is the time filter on field.
func rangeClause(field string, from, to time.Time) map[string]any {
	bounds := map[string]any{}
	if !from.IsZero() {
		bounds["gte"] = from.UTC().Format(time.RFC3339Nano)
	}
	if !to.IsZero() {
		bounds["lte"] = to.UTC().Format(time.RFC3339Nano)
	}
	return map[string]any{"range": map[string]any{field: bounds}}
}

// hostClause is the terms filter on the host field. Targets are matched as the
// full name and as the short name, so a fleet that logs either form is found.
// More than hostChunk values are split into chunks, OR-ed together.
func hostClause(field string, hosts []resource.HostID) any {
	if strings.TrimSpace(field) == "" {
		return nil
	}
	values := hostValues(hosts)
	if len(values) == 0 {
		return nil
	}
	chunks := chunk(values, hostChunk)
	if len(chunks) == 1 {
		return termsClause(field, chunks[0])
	}
	should := make([]any, 0, len(chunks))
	for _, c := range chunks {
		should = append(should, termsClause(field, c))
	}
	return map[string]any{"bool": map[string]any{
		"should":               should,
		"minimum_should_match": 1,
	}}
}

// termsClause is one terms filter.
func termsClause(field string, values []string) map[string]any {
	return map[string]any{"terms": map[string]any{field: values}}
}

// queryStringClause is the lucene text the user typed.
func queryStringClause(q string) map[string]any {
	return map[string]any{"query_string": map[string]any{"query": q}}
}

// sortFields is the time field ascending plus a tiebreaker. `_shard_doc` is the
// tiebreaker of a point-in-time search, `_doc` the one of a plain search.
func sortFields(field, pit string) []SortField {
	tie := "_doc"
	if pit != "" {
		tie = "_shard_doc"
	}
	return []SortField{
		{field: SortOrder{Order: "asc"}},
		{tie: SortOrder{Order: "asc"}},
	}
}

// sourceFields is the `_source` list: the configured fields, then the columns
// the user added, without duplicates.
func sourceFields(src LogSource, columns []string) []string {
	out := make([]string, 0, 4+len(columns))
	seen := map[string]bool{}
	add := func(f string) {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			return
		}
		seen[f] = true
		out = append(out, f)
	}
	add(src.TimeField)
	add(src.HostField)
	add(src.MessageField)
	add(src.LevelField)
	for _, c := range columns {
		add(c)
	}
	return out
}

// hostValues expands targets into the values the host field may carry: the
// name as given and its short form, lower-cased, de-duplicated and sorted.
func hostValues(hosts []resource.HostID) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(hosts)*2)
	add := func(v string) {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	for _, h := range hosts {
		name := resource.NormalizeHostID(string(h))
		add(string(name))
		if i := strings.IndexByte(string(name), '.'); i > 0 {
			add(string(name)[:i])
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// chunk splits values into slices of at most n.
func chunk(values []string, n int) [][]string {
	if n <= 0 {
		n = len(values)
	}
	var out [][]string
	for i := 0; i < len(values); i += n {
		end := i + n
		if end > len(values) {
			end = len(values)
		}
		out = append(out, values[i:end])
	}
	return out
}

// lookupPath reads a dotted field path from a decoded document. A document may
// carry the field flat ("host.name") or nested ({"host": {"name": ...}}); both
// are found. Anything else yields nil.
func lookupPath(doc map[string]any, path string) any {
	path = strings.TrimSpace(path)
	if path == "" || len(doc) == 0 {
		return nil
	}
	if v, ok := doc[path]; ok {
		return v
	}
	var cur any = doc
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[part]
		if !ok {
			return nil
		}
	}
	return cur
}

// textValue renders a document value as one line. Numbers and booleans keep
// their natural spelling; objects and arrays are compact JSON.
func textValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return trimFloat(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// trimFloat formats a JSON number without an exponent or trailing zeros.
func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// timeValue reads a timestamp out of a document: an RFC 3339 string, epoch
// milliseconds or epoch seconds. Anything else is the zero time, which sorts a
// row last instead of failing the page.
func timeValue(v any) time.Time {
	switch t := v.(type) {
	case string:
		s := strings.TrimSpace(t)
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05"} {
			if ts, err := time.Parse(layout, s); err == nil {
				return ts
			}
		}
	case float64:
		return epoch(t)
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return epoch(f)
		}
	}
	return time.Time{}
}

// epoch turns epoch seconds or milliseconds into a time, picking by magnitude.
func epoch(f float64) time.Time {
	if f <= 0 {
		return time.Time{}
	}
	if f < 1e11 {
		return time.Unix(int64(f), 0).UTC()
	}
	ms := int64(f)
	return time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond)).UTC()
}

// docString renders a document as indented JSON for the detail pane. A
// document that cannot be encoded yields an empty string, never a panic.
func docString(doc map[string]any) string {
	if len(doc) == 0 {
		return ""
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}
