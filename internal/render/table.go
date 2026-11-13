package render

import "strings"

// Cell is one cell of a table row. Style may be nil; when set it is applied to
// the already-padded text, so background colours cover the whole cell.
type Cell struct {
	Text  string
	Style func(string) string
}

// TableColumn describes one column of a table. Width is the fixed cell width;
// a Width of 0 makes the column flexible (ColumnWidths shares the leftover).
type TableColumn struct {
	Title      string
	Width      int
	AlignRight bool
}

// minFixedWidth and minFlexWidth are the floors ColumnWidths will not shrink
// below.
const (
	minFixedWidth = 3
	minFlexWidth  = 4
)

// ColumnWidths computes the cell width of every column for a total width of
// total cells. Columns are separated by one space. Fixed widths are honoured
// while they fit; the remaining width is split evenly across flexible columns
// (minimum 4), with leftover cells going to the last flexible column. Fixed
// columns that do not fit are shrunk from the right down to a minimum of 3.
func ColumnWidths(cols []TableColumn, total int) []int {
	n := len(cols)
	widths := make([]int, n)
	if n == 0 {
		return widths
	}

	avail := total - (n - 1) // cells left for content after separators
	if avail < 0 {
		avail = 0
	}

	fixedTotal := 0
	flex := make([]int, 0, n)
	for i, c := range cols {
		if c.Width > 0 {
			widths[i] = c.Width
			fixedTotal += c.Width
		} else {
			flex = append(flex, i)
		}
	}

	// Fixed columns that do not fit: shrink the rightmost ones first.
	for i := n - 1; i >= 0 && fixedTotal > avail; i-- {
		if cols[i].Width <= 0 {
			continue
		}
		room := widths[i] - minFixedWidth
		if room <= 0 {
			continue
		}
		take := fixedTotal - avail
		if take > room {
			take = room
		}
		widths[i] -= take
		fixedTotal -= take
	}

	if len(flex) == 0 {
		return widths
	}

	remaining := avail - fixedTotal
	if remaining < 0 {
		remaining = 0
	}
	if remaining < len(flex)*minFlexWidth {
		for _, i := range flex {
			widths[i] = minFlexWidth
		}
		return widths
	}

	base := remaining / len(flex)
	for k, i := range flex {
		if k == len(flex)-1 {
			// Leftover cells go to the last flexible column.
			widths[i] = remaining - base*(len(flex)-1)
		} else {
			widths[i] = base
		}
	}
	return widths
}

// Row renders one table line. The result is exactly sum(widths)+len(widths)-1
// cells wide: every cell is truncated/padded to its width, right-aligned when
// the column says so, and styled after padding. Extra cells are ignored and
// missing cells render blank.
func Row(cells []Cell, widths []int, cols []TableColumn) string {
	if len(widths) == 0 {
		return ""
	}
	var b strings.Builder
	for i, w := range widths {
		if i > 0 {
			b.WriteByte(' ')
		}
		var text string
		var style func(string) string
		if i < len(cells) {
			text = cells[i].Text
			style = cells[i].Style
		}
		right := i < len(cols) && cols[i].AlignRight
		var cell string
		if right {
			cell = PadLeft(text, w)
		} else {
			cell = PadRight(text, w)
		}
		if style != nil {
			cell = style(cell)
		}
		b.WriteString(cell)
	}
	return b.String()
}

// Header renders the title row of a table, with the same widths and alignment
// as Row.
func Header(cols []TableColumn, widths []int) string {
	cells := make([]Cell, len(cols))
	for i, c := range cols {
		cells[i] = Cell{Text: c.Title}
	}
	return Row(cells, widths, cols)
}
