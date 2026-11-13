// Data-frame decoding: the `results.<refId>.frames` of a /api/ds/query answer
// turned into the series the dashboard grid draws (SPEC §8.4, task 120).
//
// A frame is a schema plus one column of values per schema field. The schema
// carries per-field metadata the query types in client.go deliberately do not
// keep — the series labels and the display name — so frames are decoded from
// their raw JSON here and the typed DSFrame stays the label_values path's shape.

package grafana

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Point is one sample of a decoded series. A sample the datasource answered
// with null or a non-number keeps its place as a NaN gap, so a chart draws a
// hole instead of joining across the missing value.
type Point struct {
	T time.Time
	V float64
}

// Series is one decoded data frame field: a time column paired with a number
// column, plus the labels the frame carried for it.
type Series struct {
	RefID  string
	Name   string
	Labels map[string]string
	Points []Point
}

// Title renders the legend text of the series: its display name with its
// labels in braces, the way a Grafana legend shows it.
func (s Series) Title() string {
	name := strings.TrimSpace(s.Name)
	if len(s.Labels) > 0 {
		if name == "" || strings.EqualFold(name, "value") {
			return LabelString(s.Labels)
		}
		return name + LabelString(s.Labels)
	}
	if name == "" {
		return "value"
	}
	return name
}

// Last returns the last finite value of the series.
func (s Series) Last() (float64, bool) {
	for i := len(s.Points) - 1; i >= 0; i-- {
		if v := s.Points[i].V; isFinite(v) {
			return v, true
		}
	}
	return 0, false
}

// Bounds returns the smallest and largest finite value of the series.
func (s Series) Bounds() (min, max float64, ok bool) {
	for _, p := range s.Points {
		if !isFinite(p.V) {
			continue
		}
		if !ok {
			min, max, ok = p.V, p.V, true
			continue
		}
		if p.V < min {
			min = p.V
		}
		if p.V > max {
			max = p.V
		}
	}
	return min, max, ok
}

// Values returns the y values with their NaN gaps, for a sparkline.
func (s Series) Values() []float64 {
	out := make([]float64, 0, len(s.Points))
	for _, p := range s.Points {
		out = append(out, p.V)
	}
	return out
}

// FrameField is one column of a data frame.
type FrameField struct {
	Name        string
	Type        string            // "time", "number", "string", "boolean", ... as Grafana declares it
	Labels      map[string]string // series identity from the schema
	DisplayName string            // config.displayNameFromDS, else config.displayName
}

// Frame is one decoded data frame: its fields, their columns and the values
// the decoder could read.
type Frame struct {
	RefID   string
	Name    string // schema.name, when the frame carries one
	Fields  []FrameField
	Columns [][]any
}

// rawFrame is one frame of a /api/ds/query answer with every part the decoder
// needs still in its wire form.
type rawFrame struct {
	Schema struct {
		RefID  string `json:"refId"`
		Name   string `json:"name"`
		Fields []struct {
			Name   string            `json:"name"`
			Type   string            `json:"type"`
			Labels map[string]string `json:"labels"`
			Config struct {
				DisplayName       string `json:"displayName"`
				DisplayNameFromDS string `json:"displayNameFromDS"`
			} `json:"config"`
		} `json:"fields"`
	} `json:"schema"`
	Data struct {
		Values []json.RawMessage `json:"values"`
	} `json:"data"`
}

// DecodeFrame decodes one frame. ok is false when the bytes are not a frame at
// all; a frame with unusable columns decodes to a frame with no series.
func DecodeFrame(raw json.RawMessage) (Frame, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return Frame{}, false
	}
	var wire rawFrame
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Frame{}, false
	}
	f := Frame{RefID: strings.TrimSpace(wire.Schema.RefID), Name: strings.TrimSpace(wire.Schema.Name)}
	for _, wf := range wire.Schema.Fields {
		disp := strings.TrimSpace(wf.Config.DisplayNameFromDS)
		if disp == "" {
			disp = strings.TrimSpace(wf.Config.DisplayName)
		}
		f.Fields = append(f.Fields, FrameField{
			Name:        strings.TrimSpace(wf.Name),
			Type:        strings.ToLower(strings.TrimSpace(wf.Type)),
			Labels:      wf.Labels,
			DisplayName: disp,
		})
	}
	for _, col := range wire.Data.Values {
		f.Columns = append(f.Columns, decodeColumn(col))
	}
	if len(f.Fields) == 0 && len(f.Columns) == 0 {
		return Frame{}, false
	}
	// A frame with fewer fields than columns still decodes: the extra columns
	// are unnamed and take their type from their values.
	for len(f.Fields) < len(f.Columns) {
		f.Fields = append(f.Fields, FrameField{})
	}
	return f, true
}

// DecodeFrames decodes every frame of one refId and returns their series, in
// frame order. A frame that does not parse is skipped, never fatal.
func DecodeFrames(refID string, raws []json.RawMessage) []Series {
	var out []Series
	for _, raw := range raws {
		f, ok := DecodeFrame(raw)
		if !ok {
			continue
		}
		if f.RefID == "" {
			f.RefID = refID
		}
		out = append(out, f.Series()...)
	}
	return out
}

// Series turns the frame into series: one per number column, or — when the
// frame carries a single number column beside label columns — one per distinct
// label combination (the long form a table-format datasource answers with).
func (f Frame) Series() []Series {
	timeIdx := -1
	var numbers, labels []int
	for i := range f.Fields {
		switch f.kind(i) {
		case "time":
			if timeIdx < 0 {
				timeIdx = i
			}
		case "number":
			numbers = append(numbers, i)
		case "string":
			labels = append(labels, i)
		}
	}
	if len(numbers) == 0 {
		return nil
	}
	if len(numbers) == 1 && len(labels) > 0 {
		return f.longSeries(numbers[0], timeIdx, labels)
	}
	out := make([]Series, 0, len(numbers))
	for _, idx := range numbers {
		out = append(out, Series{
			RefID:  f.RefID,
			Name:   f.fieldName(idx),
			Labels: copyLabels(f.Fields[idx].Labels),
			Points: f.points(idx, timeIdx),
		})
	}
	return out
}

// kind is the column's role: "time", "number", "string", or "" when the column
// carries nothing usable. A field without a declared type takes it from its
// first non-null value.
func (f Frame) kind(i int) string {
	if i < 0 || i >= len(f.Fields) {
		return ""
	}
	switch t := f.Fields[i].Type; t {
	case "time":
		return "time"
	case "number":
		return "number"
	case "string", "boolean", "enum", "other":
		return "string"
	}
	if i >= len(f.Columns) {
		return ""
	}
	for _, v := range f.Columns[i] {
		if v == nil {
			continue
		}
		if _, ok := v.(float64); ok {
			return "number"
		}
		return "string"
	}
	return ""
}

// rows is the longest column's length: the number of samples the frame holds.
func (f Frame) rows() int {
	n := 0
	for _, col := range f.Columns {
		if len(col) > n {
			n = len(col)
		}
	}
	return n
}

// at returns one cell, or nil past a column's end.
func (f Frame) at(col, row int) any {
	if col < 0 || col >= len(f.Columns) {
		return nil
	}
	if row < 0 || row >= len(f.Columns[col]) {
		return nil
	}
	return f.Columns[col][row]
}

// fieldName is the display name of one column: the schema's display name, its
// field name, the frame's name, then "value".
func (f Frame) fieldName(i int) string {
	var ff FrameField
	if i >= 0 && i < len(f.Fields) {
		ff = f.Fields[i]
	}
	name := strings.TrimSpace(ff.DisplayName)
	if name == "" {
		name = strings.TrimSpace(ff.Name)
	}
	if name != "" && !strings.EqualFold(name, "value") {
		return name
	}
	if frame := strings.TrimSpace(f.Name); frame != "" {
		return frame
	}
	if name != "" {
		return name
	}
	return "value"
}

// labelName is the key a label column contributes: its field name.
func (f Frame) labelName(i int) string {
	if i >= 0 && i < len(f.Fields) {
		if n := strings.TrimSpace(f.Fields[i].Name); n != "" {
			return n
		}
		if n := strings.TrimSpace(f.Fields[i].DisplayName); n != "" {
			return n
		}
	}
	return "label"
}

// points pairs one number column with the time column. Without a time column
// the samples keep the zero time; a null number stays as a gap when there is a
// time column to align it with.
func (f Frame) points(numIdx, timeIdx int) []Point {
	rows := f.rows()
	out := make([]Point, 0, rows)
	for r := 0; r < rows; r++ {
		var t time.Time
		if timeIdx >= 0 {
			if ts, ok := timeAt(f.at(timeIdx, r)); ok {
				t = ts
			}
		}
		v, ok := numberAt(f.at(numIdx, r))
		if !ok {
			if timeIdx < 0 {
				continue
			}
			v = math.NaN()
		}
		out = append(out, Point{T: t, V: v})
	}
	return out
}

// longSeries groups the rows of a one-number-column frame by its label
// columns: one series per distinct combination, sorted by that combination so
// the grid is stable between loads.
func (f Frame) longSeries(numIdx, timeIdx int, labelIdx []int) []Series {
	type group struct {
		labels map[string]string
		points []Point
	}
	groups := make(map[string]*group)
	keys := make([]string, 0, 8)
	rows := f.rows()
	for r := 0; r < rows; r++ {
		labels := make(map[string]string, len(labelIdx))
		parts := make([]string, 0, len(labelIdx))
		for _, li := range labelIdx {
			val := stringAt(f.at(li, r))
			if val != "" {
				labels[f.labelName(li)] = val
			}
			parts = append(parts, val)
		}
		key := strings.Join(parts, "\x00")
		g, ok := groups[key]
		if !ok {
			g = &group{labels: labels}
			groups[key] = g
			keys = append(keys, key)
		}
		var t time.Time
		if timeIdx >= 0 {
			if ts, tok := timeAt(f.at(timeIdx, r)); tok {
				t = ts
			}
		}
		v, ok := numberAt(f.at(numIdx, r))
		if !ok {
			if timeIdx < 0 {
				continue
			}
			v = math.NaN()
		}
		g.points = append(g.points, Point{T: t, V: v})
	}
	sort.Strings(keys)
	out := make([]Series, 0, len(keys))
	for _, key := range keys {
		g := groups[key]
		out = append(out, Series{RefID: f.RefID, Name: f.fieldName(numIdx), Labels: g.labels, Points: g.points})
	}
	return out
}

// decodeColumn decodes one values array. Numbers stay float64; integers too
// large for one stay their source text rather than losing precision silently.
func decodeColumn(raw json.RawMessage) []any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var vals []any
	if err := dec.Decode(&vals); err != nil {
		return nil
	}
	for i, v := range vals {
		if n, ok := v.(json.Number); ok {
			if f, err := n.Float64(); err == nil {
				vals[i] = f
			} else {
				vals[i] = n.String()
			}
		}
	}
	return vals
}

// numberAt reads one cell as a number. NaN, +Inf and -Inf survive as
// themselves; null, a bool and unparseable text do not.
func numberAt(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "nan":
			return math.NaN(), true
		case "+inf", "inf", "infinity":
			return math.Inf(1), true
		case "-inf", "-infinity":
			return math.Inf(-1), true
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// stringAt reads one cell as text.
func stringAt(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		return t.String()
	}
	return ""
}

// timeAt reads one cell as a time: an epoch number (milliseconds when it is
// large enough to be one, seconds otherwise) or an RFC 3339 string.
func timeAt(v any) (time.Time, bool) {
	switch t := v.(type) {
	case float64:
		return epochTime(t), true
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return time.Time{}, false
		}
		return epochTime(f), true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return time.Time{}, false
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return epochTime(f), true
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if ts, err := time.Parse(layout, s); err == nil {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

// epochTime reads a numeric timestamp. Grafana sends time fields as epoch
// milliseconds; a value small enough to be epoch seconds is read as one.
func epochTime(v float64) time.Time {
	if math.Abs(v) >= 1e11 {
		return time.UnixMilli(int64(v))
	}
	sec := math.Trunc(v)
	ns := (v - sec) * float64(time.Second)
	return time.Unix(int64(sec), int64(ns))
}

// copyLabels copies a label map so a series never aliases the frame's.
func copyLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// isFinite reports whether v can be plotted.
func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
