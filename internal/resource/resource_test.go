package resource

import (
	"sort"
	"testing"
	"time"

	"sitrep/internal/errs"
)

func TestRefStringAndParse(t *testing.T) {
	ref := Ref{Backend: "linux", Kind: "unit", Host: "web-01", Name: "nginx.service"}
	want := "linux/unit/web-01//nginx.service"
	if got := ref.String(); got != want {
		t.Fatalf("Ref.String() = %q, want %q", got, want)
	}

	got, err := ParseRef(want)
	if err != nil {
		t.Fatalf("ParseRef(%q) error: %v", want, err)
	}
	if got != ref {
		t.Fatalf("ParseRef(%q) = %+v, want %+v", want, got, ref)
	}

	// Empty parts are kept, and round-trip.
	empty := Ref{Backend: "salt", Kind: "salt-job"}
	got, err = ParseRef(empty.String())
	if err != nil {
		t.Fatalf("ParseRef(%q) error: %v", empty.String(), err)
	}
	if got != empty {
		t.Fatalf("ParseRef(%q) = %+v, want %+v", empty.String(), got, empty)
	}
}

func TestParseRefErrors(t *testing.T) {
	for _, s := range []string{"", "a/b", "a/b/c/d", "a/b/c/d/e/f"} {
		_, err := ParseRef(s)
		if err == nil {
			t.Fatalf("ParseRef(%q) = nil error, want error", s)
		}
		if kind := errs.KindOf(err); kind != errs.Config {
			t.Fatalf("ParseRef(%q) kind = %q, want %q", s, kind, errs.Config)
		}
	}
}

func TestStr(t *testing.T) {
	when := time.Date(2026, 11, 12, 13, 4, 5, 0, time.UTC)
	r := Resource{Fields: map[string]any{
		"text":    "hello",
		"int":     int64(42),
		"plain":   int(7),
		"float":   float64(1.5),
		"whole":   float64(2),
		"bool":    true,
		"when":    when,
		"dur":     90 * time.Second,
		"spark":   []float64{1, 2.5, 3},
		"nil":     nil,
		"unknown": struct{ A int }{A: 1},
	}}

	cases := map[string]string{
		"text":    "hello",
		"int":     "42",
		"plain":   "7",
		"float":   "1.5",
		"whole":   "2",
		"bool":    "true",
		"when":    "2026-11-12T13:04:05Z",
		"dur":     "1m30s",
		"spark":   "1,2.5,3",
		"nil":     "",
		"missing": "",
		"unknown": "{1}",
	}
	for id, want := range cases {
		if got := r.Str(id); got != want {
			t.Errorf("Str(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestInt(t *testing.T) {
	r := Resource{Fields: map[string]any{
		"int64":  int64(-5),
		"int":    int(6),
		"int32":  int32(7),
		"float":  float64(8),
		"frac":   float64(8.5),
		"big":    float64(1 << 63),
		"string": "9",
		"dur":    time.Minute,
	}}
	cases := []struct {
		id   string
		want int64
		ok   bool
	}{
		{"int64", -5, true},
		{"int", 6, true},
		{"int32", 7, true},
		{"float", 8, true},
		{"frac", 0, false},
		{"big", 0, false},
		{"string", 0, false},
		{"dur", 0, false},
		{"missing", 0, false},
	}
	for _, c := range cases {
		got, ok := r.Int(c.id)
		if got != c.want || ok != c.ok {
			t.Errorf("Int(%q) = (%d, %v), want (%d, %v)", c.id, got, ok, c.want, c.ok)
		}
	}
}

func TestFloat(t *testing.T) {
	r := Resource{Fields: map[string]any{
		"float": 1.25,
		"int64": int64(3),
		"int":   int(4),
		"int32": int32(5),
		"bool":  true,
	}}
	cases := []struct {
		id   string
		want float64
		ok   bool
	}{
		{"float", 1.25, true},
		{"int64", 3, true},
		{"int", 4, true},
		{"int32", 5, true},
		{"bool", 0, false},
		{"missing", 0, false},
	}
	for _, c := range cases {
		got, ok := r.Float(c.id)
		if got != c.want || ok != c.ok {
			t.Errorf("Float(%q) = (%v, %v), want (%v, %v)", c.id, got, ok, c.want, c.ok)
		}
	}
}

func TestTime(t *testing.T) {
	when := time.Date(2026, 11, 12, 13, 4, 5, 0, time.UTC)
	r := Resource{Fields: map[string]any{
		"when": when,
		"text": "2026-11-12T13:04:05Z",
	}}
	if got, ok := r.Time("when"); !ok || !got.Equal(when) {
		t.Errorf("Time(when) = (%v, %v), want (%v, true)", got, ok, when)
	}
	if _, ok := r.Time("text"); ok {
		t.Errorf("Time(text) = _, true; want false")
	}
}

func TestCompare(t *testing.T) {
	t1 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	cases := []struct {
		name string
		a, b any
		want int
	}{
		{"ints", int64(1), int64(2), -1},
		{"ints equal", int64(2), int64(2), 0},
		{"ints reversed", int64(3), int64(2), 1},
		{"int and float", 2, 2.5, -1},
		{"float and float", 2.5, 2.5, 0},
		{"strings", "alpha", "beta", -1},
		{"strings equal", "a", "a", 0},
		{"times", t1, t2, -1},
		{"times reversed", t2, t1, 1},
		{"durations", 30 * time.Second, time.Minute, -1},
		{"bools", false, true, -1},
		{"series", []float64{1, 2}, []float64{1, 3}, -1},
		{"series prefix", []float64{1}, []float64{1, 0}, -1},
		{"nil last", "a", nil, -1},
		{"nil still last", nil, "a", 1},
		{"both nil", nil, nil, 0},
		{"mismatched types", int64(9), "10", 1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("%s: Compare(%v, %v) = %d, want %d", c.name, c.a, c.b, got, c.want)
		}
		if got := Compare(c.b, c.a); got != -c.want {
			t.Errorf("%s: Compare reversed = %d, want %d", c.name, got, -c.want)
		}
	}
}

func TestCompareSorts(t *testing.T) {
	values := []any{int64(3), nil, int64(1), int64(2)}
	sort.Slice(values, func(i, j int) bool { return Compare(values[i], values[j]) < 0 })
	want := []any{int64(1), int64(2), int64(3), nil}
	for i := range want {
		if values[i] != want[i] {
			t.Fatalf("sorted = %v, want %v", values, want)
		}
	}
}

func TestNormalizeHostID(t *testing.T) {
	cases := map[string]HostID{
		"Web-01.Example.Internal.": "web-01.example.internal",
		"  db-02  ":                "db-02",
		"plain":                    "plain",
		"":                         "",
		".":                        "",
	}
	for in, want := range cases {
		if got := NormalizeHostID(in); got != want {
			t.Errorf("NormalizeHostID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKindSpecColumn(t *testing.T) {
	spec := KindSpec{
		Name: "unit",
		Columns: []Column{
			{ID: "name", Title: "NAME"},
			{ID: "load", Title: "LOAD", Width: 8, Align: AlignRight, Kind: ColNumber},
		},
	}
	got, ok := spec.Column("load")
	if !ok {
		t.Fatal("Column(load) = _, false; want true")
	}
	if got.ID != "load" || got.Title != "LOAD" || got.Width != 8 || got.Align != AlignRight || got.Kind != ColNumber {
		t.Fatalf("Column(load) = %+v", got)
	}
	if _, ok := spec.Column("missing"); ok {
		t.Fatal("Column(missing) = _, true; want false")
	}
}
