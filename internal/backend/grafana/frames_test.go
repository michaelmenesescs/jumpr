package grafana

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// frameJSON builds a one-frame answer of a data frame from its schema fields
// and columns, so a test reads as the JSON it is.
func frameJSON(t *testing.T, fields string, columns string) json.RawMessage {
	t.Helper()
	raw := `{"schema":{` + fields + `},"data":{"values":[` + columns + `]}}`
	if !json.Valid([]byte(raw)) {
		t.Fatalf("the test's frame is not JSON: %s", raw)
	}
	return json.RawMessage(raw)
}

func TestFramesDecodePrometheus(t *testing.T) {
	raw := frameJSON(t,
		`"refId":"A","fields":[{"name":"Time","type":"time"},`+
			`{"name":"Value","type":"number","labels":{"instance":"web-01.example.internal","job":"node"}}]`,
		`[1763100000000,1763100060000,1763100120000],[0.5,1.5,null]`)

	series := DecodeFrames("A", []json.RawMessage{raw})
	if len(series) != 1 {
		t.Fatalf("got %d series, want 1", len(series))
	}
	s := series[0]
	if s.RefID != "A" {
		t.Errorf("refId = %q, want A", s.RefID)
	}
	if got := s.Labels["instance"]; got != "web-01.example.internal" {
		t.Errorf("instance label = %q", got)
	}
	if len(s.Points) != 3 {
		t.Fatalf("got %d points, want 3", len(s.Points))
	}
	if got := s.Points[0].T; !got.Equal(time.UnixMilli(1763100000000)) {
		t.Errorf("first timestamp = %s, want the epoch millisecond", got)
	}
	if got := s.Points[1].V; got != 1.5 {
		t.Errorf("second value = %v, want 1.5", got)
	}
	if !math.IsNaN(s.Points[2].V) {
		t.Errorf("a null sample should be a NaN gap, got %v", s.Points[2].V)
	}
	if got, want := s.Title(), `{instance="web-01.example.internal", job="node"}`; got != want {
		t.Errorf("Title() = %q, want %q", got, want)
	}
}

func TestFramesDecodeDisplayName(t *testing.T) {
	raw := frameJSON(t,
		`"fields":[{"name":"Time","type":"time"},`+
			`{"name":"Value","type":"number","config":{"displayNameFromDS":"node load 1m"}}]`,
		`[1763100000000],[2.5]`)

	series := DecodeFrames("B", []json.RawMessage{raw})
	if len(series) != 1 {
		t.Fatalf("got %d series, want 1", len(series))
	}
	if got := series[0].Name; got != "node load 1m" {
		t.Errorf("Name = %q, want the display name from the DS", got)
	}
	if got := series[0].Title(); got != "node load 1m" {
		t.Errorf("Title() = %q", got)
	}
}

func TestFramesDecodeWideTable(t *testing.T) {
	raw := frameJSON(t,
		`"fields":[{"name":"Time","type":"time"},`+
			`{"name":"rx","type":"number"},{"name":"tx","type":"number"}]`,
		`[1763100000000,1763100060000],[10,20],[30,40]`)

	series := DecodeFrames("A", []json.RawMessage{raw})
	if len(series) != 2 {
		t.Fatalf("got %d series, want one per number field", len(series))
	}
	if series[0].Name != "rx" || series[1].Name != "tx" {
		t.Fatalf("names = %q, %q, want rx, tx", series[0].Name, series[1].Name)
	}
	if got := series[1].Points[1].V; got != 40 {
		t.Errorf("tx last value = %v, want 40", got)
	}
}

func TestFramesDecodeLongFormat(t *testing.T) {
	raw := frameJSON(t,
		`"fields":[{"name":"Time","type":"time"},`+
			`{"name":"instance","type":"string"},{"name":"Value","type":"number"}]`,
		`[1763100000000,1763100000000,1763100060000,1763100060000],`+
			`["web-01.example.internal","db-01.example.internal","web-01.example.internal","db-01.example.internal"],`+
			`[1,2,3,4]`)

	series := DecodeFrames("A", []json.RawMessage{raw})
	if len(series) != 2 {
		t.Fatalf("got %d series, want one per label combination", len(series))
	}
	// Sorted by the label combination, so the grid is stable between loads.
	if got := series[0].Labels["instance"]; got != "db-01.example.internal" {
		t.Fatalf("first series is %q, want the db host", got)
	}
	if got := series[0].Points[1].V; got != 4 {
		t.Errorf("db last value = %v, want 4", got)
	}
	if got := series[1].Points[0].V; got != 1 {
		t.Errorf("web first value = %v, want 1", got)
	}
}

func TestFramesDecodeGarbage(t *testing.T) {
	cases := map[string]json.RawMessage{
		"not json":         json.RawMessage(`{`),
		"no schema":        json.RawMessage(`{"schema":{"fields":[]}}`),
		"empty object":     json.RawMessage(`{}`),
		"values not array": json.RawMessage(`{"schema":{"fields":[{"name":"t","type":"time"}]},"data":{"values":[7]}}`),
	}
	for name, raw := range cases {
		// A frame that does not parse must never panic and never yield series.
		if got := DecodeFrames("A", []json.RawMessage{raw}); len(got) != 0 {
			t.Errorf("%s: got %d series, want none", name, len(got))
		}
	}
}

func TestFramesSeriesStats(t *testing.T) {
	raw := frameJSON(t,
		`"fields":[{"name":"Time","type":"time"},{"name":"Value","type":"number"}]`,
		`[1763100000000,1763100060000,1763100120000,1763100180000],[3,null,7,5]`)

	series := DecodeFrames("A", []json.RawMessage{raw})
	if len(series) != 1 {
		t.Fatalf("got %d series, want 1", len(series))
	}
	s := series[0]
	if v, ok := s.Last(); !ok || v != 5 {
		t.Errorf("Last() = %v, %v, want 5, true", v, ok)
	}
	min, max, ok := s.Bounds()
	if !ok || min != 3 || max != 7 {
		t.Errorf("Bounds() = %v, %v, %v, want 3, 7, true", min, max, ok)
	}
	vals := s.Values()
	if len(vals) != 4 || !math.IsNaN(vals[1]) {
		t.Errorf("Values() = %v, want four with a NaN gap", vals)
	}
}

func TestFramesSkipUnparseableFrameInBatch(t *testing.T) {
	good := frameJSON(t,
		`"fields":[{"name":"Time","type":"time"},{"name":"Value","type":"number"}]`,
		`[1763100000000],[1]`)
	series := DecodeFrames("A", []json.RawMessage{json.RawMessage(`{"schema":`), good})
	if len(series) != 1 {
		t.Fatalf("got %d series, want the good frame only", len(series))
	}
}
