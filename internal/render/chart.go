package render

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// Point is one sample of a charted series. A NaN or infinite value is a gap:
// nothing is drawn for it and a line never crosses it.
type Point struct {
	T time.Time
	V float64
}

// Series is one line of a chart. Color picks one of the four palette entries.
type Series struct {
	Name   string
	Color  int
	Points []Point
}

const (
	// MaxChartSeries is the most series a chart draws; extra series are
	// ignored.
	MaxChartSeries = 4
	// brailleBase is the code point of the empty braille cell; the dot bits are
	// added to it.
	brailleBase = 0x2800
)

// chartPalette holds the four foreground colours a chart uses, as ANSI 256
// sequences. render stays free of a styling library.
var chartPalette = [MaxChartSeries]string{
	"\x1b[38;5;75m",
	"\x1b[38;5;214m",
	"\x1b[38;5;203m",
	"\x1b[38;5;42m",
}

// brailleBits maps a dot's position inside a cell (row 0-3, column 0-1) to its
// bit in the braille code point.
var brailleBits = [4][2]byte{
	{0x01, 0x08},
	{0x02, 0x10},
	{0x04, 0x20},
	{0x40, 0x80},
}

// chartCell is one braille cell: the dots of every series in it and how many
// dots each series put there, so the cell takes the colour of the series that
// drew the most.
type chartCell struct {
	bits  [MaxChartSeries]byte
	count [MaxChartSeries]uint8
}

// Chart renders series as a braille line chart of exactly h lines of exactly w
// cells, with a left gutter of y-axis labels: the maximum on the top line, the
// minimum on the bottom line and the last value of the first series on its own
// line.
//
// Each cell holds a 2x4 dot grid, so the plot is twice as wide and four times
// as tall as its cell count. NaN and infinite samples leave a gap. A series
// whose values never change draws a flat line through the middle row, and a
// series with one point draws one dot. At most MaxChartSeries series are drawn,
// in the four palette colours. An empty or non-positive size returns "".
func Chart(series []Series, w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	if len(series) > MaxChartSeries {
		series = series[:MaxChartSeries]
	}

	min, max, any := chartBounds(series)
	if !any {
		// Nothing to plot: the whole pane is blank, with no axis to label.
		return Fit("", w, h)
	}
	top, bottom := chartLabel(max), chartLabel(min)
	lastText, lastRow := "", -1
	if v, ok := chartLast(series); ok {
		lastText = chartLabel(v)
		lastRow = chartYRow(v, min, max, h)
	}

	gutter := len(top)
	if len(bottom) > gutter {
		gutter = len(bottom)
	}
	if len(lastText) > gutter {
		gutter = len(lastText)
	}
	plotW := w
	if gutter > 0 && w > gutter+1 {
		plotW = w - gutter - 1
	} else {
		gutter = 0
	}

	dotW, dotH := plotW*2, h*4
	cells := make([]chartCell, h*plotW)
	for si, s := range series {
		prevX, prevY := -1, -1
		n := len(s.Points)
		for i, p := range s.Points {
			if !isFinite(p.V) {
				prevX, prevY = -1, -1
				continue
			}
			x := chartX(i, n, dotW)
			y := chartY(p.V, min, max, dotH)
			if prevX >= 0 {
				chartLine(cells, plotW, si, prevX, prevY, x, y)
			} else {
				chartPlot(cells, plotW, si, x, y)
			}
			prevX, prevY = x, y
		}
	}

	lines := make([]string, 0, h)
	for row := 0; row < h; row++ {
		label := ""
		switch {
		case row == 0:
			label = top
		case row == h-1:
			label = bottom
		case row == lastRow:
			label = lastText
		}
		var b strings.Builder
		if gutter > 0 {
			b.WriteString(PadLeft(label, gutter))
			b.WriteByte(' ')
		}
		for col := 0; col < plotW; col++ {
			c := cells[row*plotW+col]
			si := chartCellSeries(c)
			if si < 0 {
				b.WriteByte(' ')
				continue
			}
			b.WriteString(chartPalette[si])
			b.WriteRune(rune(brailleBase + int(c.bits[si])))
			b.WriteString(reset)
		}
		lines = append(lines, b.String())
	}
	return Fit(strings.Join(lines, "\n"), w, h)
}

// chartBounds returns the finite value range across every series.
func chartBounds(series []Series) (min, max float64, ok bool) {
	for _, s := range series {
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
	}
	return min, max, ok
}

// chartLast is the last finite value of the first series that has one.
func chartLast(series []Series) (float64, bool) {
	if len(series) == 0 {
		return 0, false
	}
	for i := len(series[0].Points) - 1; i >= 0; i-- {
		if v := series[0].Points[i].V; isFinite(v) {
			return v, true
		}
	}
	return 0, false
}

// chartX maps a sample index onto a dot column; a single sample sits at the
// right edge, where "now" is.
func chartX(i, n, dotW int) int {
	if dotW <= 1 {
		return 0
	}
	if n <= 1 {
		return dotW - 1
	}
	return i * (dotW - 1) / (n - 1)
}

// chartY maps a value onto a dot row, counting from the bottom.
func chartY(v, min, max float64, dotH int) int {
	if dotH <= 1 {
		return 0
	}
	frac := 0.5
	if max > min {
		frac = (v - min) / (max - min)
	}
	return int(math.Round(frac * float64(dotH-1)))
}

// chartYRow maps a value onto a cell row counted from the top, which is the
// y-axis label line.
func chartYRow(v, min, max float64, h int) int {
	if h <= 1 {
		return 0
	}
	y := chartY(v, min, max, h*4)
	return h - 1 - y/4
}

// chartPlot sets one dot.
func chartPlot(cells []chartCell, plotW, si, x, y int) {
	if x < 0 || y < 0 || plotW <= 0 {
		return
	}
	cx, cy := x/2, y/4
	if cx >= plotW || cy*plotW+cx >= len(cells) {
		return
	}
	c := &cells[cy*plotW+cx]
	c.bits[si] |= brailleBits[y%4][x%2]
	if c.count[si] < 255 {
		c.count[si]++
	}
}

// chartLine draws the dots between two points, one dot column at a time.
func chartLine(cells []chartCell, plotW, si, x0, y0, x1, y1 int) {
	if x1 < x0 {
		x0, x1, y0, y1 = x1, x0, y1, y0
	}
	for x := x0; x <= x1; x++ {
		y := y1
		if x1 > x0 {
			y = y0 + (y1-y0)*(x-x0)/(x1-x0)
		}
		chartPlot(cells, plotW, si, x, y)
	}
}

// chartCellSeries picks the series that owns a cell: the one with the most
// dots, the lowest index on a tie.
func chartCellSeries(c chartCell) int {
	best, bestCount := -1, uint8(0)
	for si := range c.count {
		if c.count[si] > bestCount {
			best, bestCount = si, c.count[si]
		}
	}
	return best
}

// chartLabel renders an axis value compactly.
func chartLabel(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case v == math.Trunc(v) && math.Abs(v) < 1e9:
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', 4, 64)
}

// isFinite reports whether v can be plotted.
func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
