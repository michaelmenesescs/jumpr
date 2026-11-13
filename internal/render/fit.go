// Package render holds the pure text-layout primitives every view is built on.
// Nothing here does I/O, keeps state or imports a styling library: callers pass
// strings in and get exactly-sized strings back.
package render

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// ellipsis is the single-cell tail Truncate appends when it cuts text.
const ellipsis = "…"

const reset = "\x1b[0m"

// Width returns the display width of s in terminal cells. ANSI escape
// sequences are ignored and wide runes (CJK, emoji) count as two cells.
func Width(s string) int {
	return ansi.StringWidth(s)
}

// Truncate cuts s to at most w cells, ANSI-aware. If anything was cut, the last
// cell is an ellipsis. A coloured string that was cut is closed with a reset so
// the colour never leaks past the cell. w <= 0 returns "".
func Truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if Width(s) <= w {
		return s
	}
	out := ansi.Truncate(s, w, ellipsis)
	// ansi.Truncate keeps escape sequences it has already seen but never
	// closes an SGR opened before the cut; do it ourselves.
	if strings.ContainsRune(s, '\x1b') {
		out += reset
	}
	return out
}

// PadRight truncates s to w cells and pads it on the right with spaces so the
// result is exactly w cells wide.
func PadRight(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = Truncate(s, w)
	if pad := w - Width(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// PadLeft truncates s to w cells and pads it on the left with spaces so the
// result is exactly w cells wide.
func PadLeft(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = Truncate(s, w)
	if pad := w - Width(s); pad > 0 {
		s = strings.Repeat(" ", pad) + s
	}
	return s
}

// Fit returns exactly h lines of exactly w cells each: every line of s is
// truncated or padded to w, surplus lines are dropped and missing lines are
// blank. The result has h-1 newlines and no trailing newline. If h <= 0 or
// w <= 0 the result is "".
func Fit(s string, w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	out := make([]string, 0, h)
	for _, line := range lines {
		out = append(out, PadRight(line, w))
	}
	blank := strings.Repeat(" ", w)
	for len(out) < h {
		out = append(out, blank)
	}
	return strings.Join(out, "\n")
}
