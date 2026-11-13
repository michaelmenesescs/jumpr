package views

import (
	"strings"
	"testing"

	"jumper/internal/backend/fake"
	"jumper/internal/errs"
	"jumper/internal/render"
	"jumper/internal/resource"
)

// asciiSet returns the ascii glyph set every assertion here renders with.
func asciiSet(t *testing.T) render.GlyphSet {
	t.Helper()
	set, err := render.Glyphs("ascii")
	if err != nil {
		t.Fatalf("render.Glyphs(ascii): %v", err)
	}
	return set
}

// sparkColumn returns the fake host kind's spark column.
func sparkColumn(t *testing.T) resource.Column {
	t.Helper()
	b := fake.New(fake.Options{})
	for _, c := range hostKind(t, b).Columns {
		if c.Kind == resource.ColSpark {
			return c
		}
	}
	t.Fatal("fake host kind has no spark column")
	return resource.Column{}
}

func TestTableAsciiGlyphsSparkCell(t *testing.T) {
	col := sparkColumn(t)
	row := resource.Resource{Fields: map[string]any{col.ID: []float64{1, 4, 2, 8, 3}}}
	got := cellTextWith(asciiSet(t), col, row, fixedNow, col.Width)
	want := render.SparkWith(asciiSet(t), []float64{1, 4, 2, 8, 3}, col.Width)
	if got != want {
		t.Fatalf("spark cell = %q, want %q", got, want)
	}
	if w := render.Width(got); w != col.Width {
		t.Errorf("spark cell is %d cells, want %d", w, col.Width)
	}
	for _, r := range got {
		if r >= 0x80 {
			t.Errorf("spark cell %q contains %U", got, r)
		}
	}
	// The unicode default is untouched: cellText still draws Unicode blocks.
	unicodeCell := cellText(col, row, fixedNow, col.Width)
	if unicodeCell != render.Spark([]float64{1, 4, 2, 8, 3}, col.Width) {
		t.Errorf("unicode spark cell = %q, want the task 009 rendering", unicodeCell)
	}
}

func TestTableAsciiGlyphsFrame(t *testing.T) {
	b := fake.New(fake.Options{})
	tbl := NewTable(testEnv(), b, hostKind(t, b), resource.Query{Kind: "host"}).WithGlyphs(asciiSet(t))
	tbl = drain(t, tbl)

	for _, size := range [][2]int{{120, 30}, {80, 20}, {40, 10}, {160, 5}} {
		out := tbl.Render(size[0], size[1])
		for _, r := range out {
			if r > 0x7f {
				t.Fatalf("frame %dx%d contains %U\n%s", size[0], size[1], r, out)
			}
		}
		lines := strings.Split(out, "\n")
		if len(lines) != size[1] {
			t.Fatalf("frame %dx%d has %d lines", size[0], size[1], len(lines))
		}
		for _, line := range lines {
			if w := render.Width(line); w != size[0] {
				t.Fatalf("frame %dx%d line %q is %d cells", size[0], size[1], line, w)
			}
		}
	}
}

func TestTableAsciiGlyphsErrorFrame(t *testing.T) {
	opts := fake.Options{FailHosts: map[resource.HostID]errs.Kind{
		"web-ewr-007": errs.Unreachable,
		"web-ewr-012": errs.Timeout,
	}}
	b := fake.New(opts)
	tbl := NewTable(testEnv(), b, hostKind(t, b), resource.Query{Kind: "host"}).WithGlyphs(asciiSet(t))
	tbl = drain(t, tbl)
	if len(tbl.errs) == 0 {
		t.Fatal("no failed hosts to render")
	}

	for _, size := range [][2]int{{120, 30}, {60, 8}, {40, 6}} {
		out := tbl.Render(size[0], size[1])
		if !strings.Contains(out, "unreachable") {
			t.Fatalf("frame %dx%d does not show the error rows:\n%s", size[0], size[1], out)
		}
		for _, r := range out {
			if r > 0x7f {
				t.Fatalf("frame %dx%d contains %U\n%s", size[0], size[1], r, out)
			}
		}
		lines := strings.Split(out, "\n")
		if len(lines) != size[1] {
			t.Fatalf("frame %dx%d has %d lines", size[0], size[1], len(lines))
		}
		for _, line := range lines {
			if w := render.Width(line); w != size[0] {
				t.Fatalf("frame %dx%d line %q is %d cells", size[0], size[1], line, w)
			}
		}
	}
}

func TestTableAsciiGlyphsBannerSeparator(t *testing.T) {
	b := fake.New(fake.Options{})
	tbl := NewTable(testEnv(), b, hostKind(t, b), resource.Query{Kind: "host"}).WithGlyphs(asciiSet(t))
	tbl = drain(t, tbl)
	if got := tbl.banner(); !strings.Contains(got, "1,512 hosts | 0 errors |") {
		t.Errorf("banner = %q, want ascii separators", got)
	}
}
