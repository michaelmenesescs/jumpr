package render

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// chartPoints builds a series from values, one minute apart.
func chartPoints(vals ...float64) []Point {
	base := time.Unix(1763100000, 0)
	out := make([]Point, len(vals))
	for i, v := range vals {
		out[i] = Point{T: base.Add(time.Duration(i) * time.Minute), V: v}
	}
	return out
}

// lines splits a chart into lines.
func lines(s string) []string { return strings.Split(s, "\n") }

// brailleCount counts the drawn dots cells of one line.
func brailleCount(line string) int {
	n := 0
	for _, r := range line {
		if r > brailleBase && r <= brailleBase+0xff {
			n++
		}
	}
	return n
}

// brailleColumns returns the display column of every braille cell in a line.
func brailleColumns(line string) []int {
	var out []int
	col := 0
	for _, r := range ansi.Strip(line) {
		if r > brailleBase && r <= brailleBase+0xff {
			out = append(out, col)
			col++
			continue
		}
		col += Width(string(r))
	}
	return out
}

// rowsWithDots counts the lines that carry a braille cell.
func rowsWithDots(chart string) int {
	n := 0
	for _, line := range lines(chart) {
		if brailleCount(line) > 0 {
			n++
		}
	}
	return n
}

func TestChartExactSize(t *testing.T) {
	series := []Series{{Name: "up", Points: chartPoints(1, 2, 3, 4, 5)}}
	for _, size := range []struct{ w, h int }{
		{1, 1}, {2, 1}, {10, 4}, {40, 10}, {80, 20}, {120, 30},
	} {
		out := Chart(series, size.w, size.h)
		got := lines(out)
		if len(got) != size.h {
			t.Errorf("%dx%d: got %d lines, want %d", size.w, size.h, len(got), size.h)
		}
		for i, line := range got {
			if w := Width(line); w != size.w {
				t.Errorf("%dx%d: line %d is %d cells, want %d (%q)", size.w, size.h, i, w, size.w, line)
			}
		}
	}
	if out := Chart(series, 0, 10); out != "" {
		t.Errorf("Chart(0, 10) = %q, want empty", out)
	}
	if out := Chart(series, 10, 0); out != "" {
		t.Errorf("Chart(10, 0) = %q, want empty", out)
	}
}

func TestChartFlatLine(t *testing.T) {
	out := Chart([]Series{{Name: "flat", Points: chartPoints(3, 3, 3, 3, 3)}}, 40, 8)
	if got := rowsWithDots(out); got != 1 {
		t.Fatalf("a flat line drew %d rows, want 1:\n%s", got, out)
	}
	if !strings.Contains(out, "3") {
		t.Errorf("the y-axis labels do not show the value:\n%s", out)
	}
}

func TestChartSinglePoint(t *testing.T) {
	out := Chart([]Series{{Name: "one", Points: chartPoints(7.5)}}, 40, 8)
	if got := brailleCount(strings.Join(lines(out), "")); got != 1 {
		t.Fatalf("a single point drew %d cells, want 1:\n%s", got, out)
	}
	// The point sits at the right edge: now is the newest sample.
	cols := brailleColumns(lines(out)[4])
	if len(cols) == 0 {
		// The dot may sit on another row; search every line.
		for _, line := range lines(out) {
			cols = append(cols, brailleColumns(line)...)
		}
	}
	if len(cols) == 0 || cols[len(cols)-1] != 39 {
		t.Errorf("single point columns = %v, want the last column", cols)
	}
}

func TestChartNaNGap(t *testing.T) {
	out := Chart([]Series{{Name: "gap", Points: chartPoints(1, math.NaN(), 1)}}, 40, 8)
	all := strings.Join(lines(out), "")
	if got := brailleCount(all); got == 0 {
		t.Fatalf("no dots drawn:\n%s", out)
	}
	var cols []int
	for _, line := range lines(out) {
		cols = append(cols, brailleColumns(line)...)
	}
	for _, c := range cols {
		if c > 10 && c < 30 {
			t.Errorf("a dot sits at column %d, inside the NaN gap:\n%s", c, out)
		}
	}
	if len(cols) == 0 || cols[0] > 10 || cols[len(cols)-1] < 30 {
		t.Errorf("the samples on either side of the gap were not drawn: %v", cols)
	}
}

func TestChartMultipleSeries(t *testing.T) {
	series := []Series{
		{Name: "a", Color: 0, Points: chartPoints(1, 2, 3)},
		{Name: "b", Color: 1, Points: chartPoints(3, 2, 1)},
	}
	out := Chart(series, 40, 8)
	if !strings.Contains(out, chartPalette[0]) || !strings.Contains(out, chartPalette[1]) {
		t.Errorf("both series should be drawn in their own colour:\n%q", out)
	}
	for i, line := range lines(out) {
		if w := Width(line); w != 40 {
			t.Errorf("line %d is %d cells, want 40", i, w)
		}
	}
}

func TestChartEmpty(t *testing.T) {
	out := Chart(nil, 20, 5)
	if got := len(lines(out)); got != 5 {
		t.Fatalf("got %d lines, want 5", got)
	}
	for i, line := range lines(out) {
		if w := Width(line); w != 20 {
			t.Errorf("line %d is %d cells, want 20", i, w)
		}
		if strings.TrimSpace(line) != "" {
			t.Errorf("line %d = %q, want blank", i, line)
		}
	}
}

func TestChartIgnoresInfiniteValues(t *testing.T) {
	out := Chart([]Series{{Name: "inf", Points: chartPoints(1, math.Inf(1), 2)}}, 30, 6)
	if got := brailleCount(strings.Join(lines(out), "")); got == 0 {
		t.Fatalf("no dots drawn:\n%s", out)
	}
	if strings.Contains(out, "+Inf") {
		t.Errorf("+Inf should be a gap, not an axis label:\n%s", out)
	}
}
