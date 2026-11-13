package render

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"jumper/internal/errs"
)

func TestGlyphsNames(t *testing.T) {
	got := GlyphNames()
	want := []string{"ascii", "nerd", "unicode"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GlyphNames() = %v, want %v", got, want)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("GlyphNames() not sorted: %v", got)
		}
	}
}

func TestGlyphsKnown(t *testing.T) {
	for _, name := range GlyphNames() {
		set, err := Glyphs(name)
		if err != nil {
			t.Fatalf("Glyphs(%q): %v", name, err)
		}
		if set.Name != name {
			t.Errorf("Glyphs(%q).Name = %q", name, set.Name)
		}
		// Every set carries a usable ramp, bar pair, ellipsis and markers.
		if len(set.Spark) < 2 {
			t.Errorf("%s: Spark has %d runes", name, len(set.Spark))
		}
		if set.BarFull == 0 || set.BarEmpty == 0 || set.Ellipsis == 0 ||
			set.StatusOK == 0 || set.StatusWarn == 0 || set.StatusCrit == 0 ||
			set.StatusNA == 0 || set.StatusUnknown == 0 || set.Separator == 0 || set.Arrow == 0 {
			t.Errorf("%s: a rune is unset: %+v", name, set)
		}
	}
}

func TestGlyphsUnknown(t *testing.T) {
	for _, name := range []string{"nope", "Nerd", "UTF8", "blocks"} {
		_, err := Glyphs(name)
		if err == nil {
			t.Fatalf("Glyphs(%q) returned no error", name)
		}
		if !errs.Is(err, errs.Config) {
			t.Errorf("Glyphs(%q) kind = %q, want %q", name, errs.KindOf(err), errs.Config)
		}
	}
}

func TestGlyphsExactRunes(t *testing.T) {
	unicode, err := Glyphs("unicode")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(unicode.Spark), "▁▂▃▄▅▆▇█"; got != want {
		t.Errorf("unicode.Spark = %q, want %q", got, want)
	}
	if unicode.BarFull != '█' || unicode.BarEmpty != '·' {
		t.Errorf("unicode bar = %q/%q, want █/·", unicode.BarFull, unicode.BarEmpty)
	}
	if unicode.Ellipsis != '…' || unicode.Separator != '·' || unicode.Arrow != '→' {
		t.Errorf("unicode decorations = %q %q %q", unicode.Ellipsis, unicode.Separator, unicode.Arrow)
	}
	if unicode.StatusOK != '●' || unicode.StatusWarn != '●' || unicode.StatusCrit != '●' ||
		unicode.StatusNA != '○' || unicode.StatusUnknown != '?' {
		t.Errorf("unicode status markers = %c %c %c %c %c",
			unicode.StatusOK, unicode.StatusWarn, unicode.StatusCrit, unicode.StatusNA, unicode.StatusUnknown)
	}

	nerd, err := Glyphs("nerd")
	if err != nil {
		t.Fatal(err)
	}
	if string(nerd.Spark) != string(unicode.Spark) {
		t.Errorf("nerd.Spark = %q, want the unicode ramp", string(nerd.Spark))
	}
	if nerd.StatusOK != '\U000f012c' || nerd.StatusWarn != '\U000f0026' ||
		nerd.StatusCrit != '\U000f015a' || nerd.StatusNA != '\U000f0131' ||
		nerd.StatusUnknown != '\U000f0131' {
		t.Errorf("nerd status markers = %U %U %U %U %U",
			nerd.StatusOK, nerd.StatusWarn, nerd.StatusCrit, nerd.StatusNA, nerd.StatusUnknown)
	}
	if nerd.Separator != '│' {
		t.Errorf("nerd.Separator = %q, want │", nerd.Separator)
	}
	if nerd.Ellipsis != unicode.Ellipsis || nerd.Arrow != unicode.Arrow {
		t.Errorf("nerd decorations = %q %q, want the unicode ones", nerd.Ellipsis, nerd.Arrow)
	}

	ascii, err := Glyphs("ascii")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(ascii.Spark), "_.,-~+=#"; got != want {
		t.Errorf("ascii.Spark = %q, want %q", got, want)
	}
	if ascii.BarFull != '#' || ascii.BarEmpty != '-' {
		t.Errorf("ascii bar = %q/%q, want #/-", ascii.BarFull, ascii.BarEmpty)
	}
	if ascii.Ellipsis != '~' || ascii.Separator != '|' || ascii.Arrow != '>' {
		t.Errorf("ascii decorations = %q %q %q", ascii.Ellipsis, ascii.Separator, ascii.Arrow)
	}
	if ascii.StatusOK != '+' || ascii.StatusWarn != '!' || ascii.StatusCrit != 'x' ||
		ascii.StatusNA != '.' || ascii.StatusUnknown != '?' {
		t.Errorf("ascii status markers = %c %c %c %c %c",
			ascii.StatusOK, ascii.StatusWarn, ascii.StatusCrit, ascii.StatusNA, ascii.StatusUnknown)
	}
}

func TestGlyphsAsciiIsPrintable(t *testing.T) {
	set, err := Glyphs("ascii")
	if err != nil {
		t.Fatal(err)
	}
	check := func(field string, r rune) {
		if r >= 0x80 {
			t.Errorf("ascii %s = %U is not ASCII", field, r)
		}
	}
	v := reflect.ValueOf(set)
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		switch f := v.Field(i).Interface().(type) {
		case rune:
			check(name, f)
		case []rune:
			for _, r := range f {
				check(name, r)
			}
		case string:
			for _, r := range f {
				check(name, r)
			}
		}
	}
}

func TestGlyphsAuto(t *testing.T) {
	cases := []struct {
		env  string
		want string
	}{
		{"", "unicode"},
		{"unicode", "unicode"},
		{"nerd", "nerd"},
		{"ascii", "ascii"},
		{"bogus", "unicode"},
	}
	for _, c := range cases {
		if got := AutoGlyphs(c.env).Name; got != c.want {
			t.Errorf("AutoGlyphs(%q).Name = %q, want %q", c.env, got, c.want)
		}
	}
	// auto is a config value, not a set, and resolves to unicode when asked
	// directly: the environment override is AutoGlyphs's job.
	set, err := Glyphs("auto")
	if err != nil {
		t.Fatalf("Glyphs(auto): %v", err)
	}
	if set.Name != "unicode" {
		t.Errorf("Glyphs(auto).Name = %q, want unicode", set.Name)
	}
}

func TestGlyphsSparkWithWidth(t *testing.T) {
	ascii, err := Glyphs("ascii")
	if err != nil {
		t.Fatal(err)
	}
	series := [][]float64{
		nil,
		{},
		{0},
		{1, 2, 3},
		{math.NaN(), math.NaN()},
		{math.NaN(), 1, math.NaN(), 4},
		{-1, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11},
		make([]float64, 100),
	}
	if len(series) != 8 {
		t.Fatalf("series = %d, want 8", len(series))
	}
	for _, vals := range series {
		for width := 1; width <= 20; width++ {
			got := SparkWith(ascii, vals, width)
			if w := Width(got); w != width {
				t.Fatalf("Width(SparkWith(ascii, %v, %d)) = %d, want %d (%q)", vals, width, w, width, got)
			}
			for _, r := range got {
				if r >= 0x80 {
					t.Fatalf("SparkWith(ascii, %v, %d) = %q contains %U", vals, width, got, r)
				}
			}
		}
	}
}

func TestGlyphsBarWith(t *testing.T) {
	ascii, err := Glyphs("ascii")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		fraction float64
		width    int
		want     string
	}{
		{0, 4, "----"},
		{0.5, 4, "##--"},
		{1, 4, "####"},
		{2, 4, "####"},
		{-0.5, 3, "---"},
		{math.NaN(), 2, "--"},
		{1, 0, ""},
	}
	for _, c := range cases {
		got := BarWith(ascii, c.fraction, c.width)
		if got != c.want {
			t.Errorf("BarWith(ascii, %v, %d) = %q, want %q", c.fraction, c.width, got, c.want)
		}
	}
	// The unicode set keeps Bar's existing output.
	if got, want := BarWith(unicodeGlyphs(), 0.5, 4), "██··"; got != want {
		t.Errorf("BarWith(unicode, 0.5, 4) = %q, want %q", got, want)
	}
}

func TestGlyphsFitWith(t *testing.T) {
	ascii, err := Glyphs("ascii")
	if err != nil {
		t.Fatal(err)
	}
	got := FitWith(ascii, "line one is much too long\nshort", 6, 3)
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("FitWith returned %d lines, want 3", len(lines))
	}
	for i, line := range lines {
		if w := Width(line); w != 6 {
			t.Errorf("line %d = %q is %d cells, want 6", i, line, w)
		}
		for _, r := range line {
			if r >= 0x80 {
				t.Errorf("line %d = %q contains %U", i, line, r)
			}
		}
	}
	if !strings.HasSuffix(lines[0], "~") {
		t.Errorf("truncated line %q does not end with the ascii ellipsis", lines[0])
	}
}
