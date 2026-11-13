package elastic

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"jumper/internal/resource"
)

// bodyOf encodes a search body for a structural assertion.
func bodyOf(t *testing.T, o SearchOptions) map[string]any {
	t.Helper()
	b, err := json.Marshal(BuildSearch(o))
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	return out
}

// filters returns the filter clauses of a body.
func filters(t *testing.T, body map[string]any) []any {
	t.Helper()
	q, ok := body["query"].(map[string]any)
	if !ok {
		t.Fatalf("body has no query: %v", body)
	}
	b, ok := q["bool"].(map[string]any)
	if !ok {
		t.Fatalf("query is not a bool query: %v", q)
	}
	f, _ := b["filter"].([]any)
	return f
}

func TestBuildSearchDefaults(t *testing.T) {
	body := bodyOf(t, SearchOptions{Source: testSource()})

	if got, want := body["size"], float64(SearchSize); got != want {
		t.Errorf("size = %v, want %v", got, want)
	}
	if got, want := body["track_total_hits"], false; got != want {
		t.Errorf("track_total_hits = %v, want false (a count of 10k+ is useless)", got)
	}
	want := []any{"@timestamp", "host.name", "message", "log.level"}
	if got := body["_source"]; !reflect.DeepEqual(got, want) {
		t.Errorf("_source = %v, want %v", got, want)
	}
	// No hosts, no query text: only the range clause, and the range keeps only
	// the bounds that were given.
	f := filters(t, body)
	if len(f) != 1 {
		t.Fatalf("filter = %v, want one clause", f)
	}
	rng := f[0].(map[string]any)["range"].(map[string]any)["@timestamp"].(map[string]any)
	if len(rng) != 0 {
		t.Errorf("range = %v, want no bounds for a zero window", rng)
	}
	boolQ := body["query"].(map[string]any)["bool"].(map[string]any)
	if _, ok := boolQ["must"]; ok {
		t.Errorf("bool query carries must without a query string: %v", boolQ)
	}
	if _, ok := body["search_after"]; ok {
		t.Errorf("body carries search_after: %v", body)
	}
}

// TestBuildSearchTimeField checks a configured time field and a half-open window.
func TestBuildSearchTimeField(t *testing.T) {
	src := testSource()
	src.TimeField = "event.created"
	from := time.Date(2026, 11, 12, 9, 0, 0, 0, time.FixedZone("CET", 3600))

	body := bodyOf(t, SearchOptions{Source: src, From: from})
	rng := filters(t, body)[0].(map[string]any)["range"].(map[string]any)
	bounds, ok := rng["event.created"].(map[string]any)
	if !ok {
		t.Fatalf("range does not use event.created: %v", rng)
	}
	if got, want := bounds["gte"], "2026-11-12T08:00:00Z"; got != want {
		t.Errorf("gte = %v, want %v (UTC)", got, want)
	}
	if _, ok := bounds["lte"]; ok {
		t.Error("a zero To produced an upper bound")
	}
}

// TestBuildSearchHostChunks checks a fleet larger than one terms clause is split
// at 1,000 values and OR-ed, and that every value is present exactly once.
func TestBuildSearchHostChunks(t *testing.T) {
	hosts := make([]resource.HostID, 0, 1500)
	for i := 0; i < 1500; i++ {
		hosts = append(hosts, resource.HostID("host-"+pad4(i)+".example.internal"))
	}
	body := bodyOf(t, SearchOptions{Source: testSource(), Hosts: hosts})

	f := filters(t, body)
	if len(f) != 2 {
		t.Fatalf("filter = %d clauses, want range + one host clause", len(f))
	}
	clause := f[1].(map[string]any)
	should, ok := clause["bool"].(map[string]any)["should"].([]any)
	if !ok {
		t.Fatalf("host clause is not a should: %v", clause)
	}
	if kept := clause["bool"].(map[string]any)["minimum_should_match"]; kept != float64(1) {
		t.Errorf("minimum_should_match = %v, want 1", kept)
	}
	total := 0
	for _, s := range should {
		values := s.(map[string]any)["terms"].(map[string]any)["host.name"].([]any)
		if len(values) > hostChunk {
			t.Errorf("a chunk carries %d values, want at most %d", len(values), hostChunk)
		}
		total += len(values)
	}
	if total != 3000 {
		t.Errorf("host clause carries %d values, want 3000 (full and short name)", total)
	}
}

// TestBuildSearchSourceColumns checks extra columns are added to _source once.
func TestBuildSearchSourceColumns(t *testing.T) {
	body := bodyOf(t, SearchOptions{
		Source:  testSource(),
		Columns: []string{"status", "log.level", "", "http.response.status_code"},
	})
	want := []any{"@timestamp", "host.name", "message", "log.level", "status", "http.response.status_code"}
	if got := body["_source"]; !reflect.DeepEqual(got, want) {
		t.Errorf("_source = %v, want %v", got, want)
	}
}

func TestHostValues(t *testing.T) {
	cases := []struct {
		name string
		in   []resource.HostID
		want []string
	}{
		{"empty", nil, nil},
		{"short only", []resource.HostID{"web-01"}, []string{"web-01"}},
		{"fqdn expands", []resource.HostID{"Web-01.Example.Internal"}, []string{"web-01", "web-01.example.internal"}},
		{"dedupe", []resource.HostID{"web-01", "web-01.example.internal"}, []string{"web-01", "web-01.example.internal"}},
		{"sorted", []resource.HostID{"b.example.internal", "a"}, []string{"a", "b", "b.example.internal"}},
		{"blank skipped", []resource.HostID{"  ", "a"}, []string{"a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostValues(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("hostValues = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestChunk(t *testing.T) {
	vals := []string{"a", "b", "c", "d", "e"}
	got := chunk(vals, 2)
	if len(got) != 3 || len(got[2]) != 1 {
		t.Fatalf("chunk = %v, want 3 chunks with the last of 1", got)
	}
	if len(chunk(vals, 0)) != 1 {
		t.Errorf("chunk with n=0 should not split")
	}
	if chunk(nil, 10) != nil {
		t.Errorf("chunk(nil) should be nil")
	}
}

func TestLookupPath(t *testing.T) {
	doc := map[string]any{
		"@timestamp": "2026-11-12T14:31:02Z",
		"host":       map[string]any{"name": "web-01"},
		"flat.name":  "flat",
		"nested":     map[string]any{"a": map[string]any{"b": float64(3)}},
		"list":       []any{"x", "y"},
	}
	cases := []struct{ path, want string }{
		{"host.name", "web-01"},
		{"flat.name", "flat"},
		{"nested.a.b", "3"},
		{"list", `["x","y"]`},
		{"missing", ""},
		{"", ""},
		{"host.missing", ""},
		{"@timestamp", "2026-11-12T14:31:02Z"},
	}
	for _, tc := range cases {
		if got := textValue(lookupPath(doc, tc.path)); got != tc.want {
			t.Errorf("textValue(lookupPath(%q)) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestTextValue(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"s", "s"},
		{float64(9104), "9104"},
		{float64(1.5), "1.5"},
		{true, "true"},
		{json.Number("12345678901"), "12345678901"},
		{map[string]any{"a": float64(1)}, `{"a":1}`},
	}
	for _, tc := range cases {
		if got := textValue(tc.in); got != tc.want {
			t.Errorf("textValue(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTimeValue(t *testing.T) {
	want := time.Date(2026, 11, 12, 14, 31, 2, 117000000, time.UTC)
	cases := []struct {
		name string
		in   any
		want time.Time
	}{
		{"rfc3339", "2026-11-12T14:31:02.117Z", want},
		{"rfc3339 nanos", "2026-11-12T14:31:02.117000Z", want},
		{"elasticsearch default", "2026-11-12 14:31:02.117", want},
		{"epoch millis", float64(1794493862117), want},
		{"epoch seconds", float64(1794493862), want.Truncate(time.Second)},
		{"json number millis", json.Number("1794493862117"), want},
		{"garbage", "not a time", time.Time{}},
		{"nil", nil, time.Time{}},
		{"negative", float64(-1), time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := timeValue(tc.in); !got.Equal(tc.want) {
				t.Errorf("timeValue(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestDocString(t *testing.T) {
	doc := map[string]any{"a": "b", "n": float64(1)}
	out := docString(doc)
	if !strings.Contains(out, `"a": "b"`) || !strings.Contains(out, "\n") {
		t.Errorf("docString = %q, want indented JSON", out)
	}
	if docString(nil) != "" {
		t.Error("docString(nil) should be empty")
	}
}

// TestBuildSearchQueryStringIsEncoded checks the user's text is JSON-encoded, so
// a quote cannot break the body.
func TestBuildSearchQueryStringIsEncoded(t *testing.T) {
	body := bodyOf(t, SearchOptions{Source: testSource(), QueryString: `status:"500" OR "`})
	must, ok := body["query"].(map[string]any)["bool"].(map[string]any)["must"].([]any)
	if !ok || len(must) != 1 {
		t.Fatalf("must = %v, want one query_string clause", must)
	}
	qs := must[0].(map[string]any)["query_string"].(map[string]any)["query"]
	if qs != `status:"500" OR "` {
		t.Errorf("query = %q, want the text unchanged", qs)
	}
}

// pad4 left-pads i to four digits.
func pad4(i int) string {
	return fmt.Sprintf("%04d", i)
}
