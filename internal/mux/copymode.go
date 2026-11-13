package mux

import (
	"strings"

	"github.com/mattn/go-runewidth"

	"jumper/internal/render"
)

// ANSI sequences copy mode uses for the selection and the cursor cell. They are
// written straight into the rendered pane, so copy mode needs no theme.
const (
	reverseOn  = "\x1b[7m"
	reverseOff = "\x1b[0m"
)

// selMode is the kind of selection copy mode currently has.
type selMode int

const (
	selNone selMode = iota
	selChar
	selLine
)

// cellPos is a position in the frozen text: a line and a cell column.
type cellPos struct{ y, x int }

// CopyMode is a frozen snapshot of one pane's text that the user can move
// around, search and yank from. Positions are in cells; the text is in runes,
// so wide characters are selected whole (SPEC §21.3).
type CopyMode struct {
	lines []string
	w, h  int

	cy, cx    int // cursor: line and cell column
	top, left int // first visible line and first visible cell

	mode   selMode
	anchor cellPos // fixed end of the selection

	typing bool   // a search pattern is being typed
	query  string // the last (or current) search pattern
}

// NewCopyMode freezes lines and opens a copy mode window of w×h cells. The
// cursor starts on the last line, like tmux.
func NewCopyMode(lines []string, w, h int) *CopyMode {
	c := &CopyMode{lines: lines, w: w, h: h}
	c.cy = len(lines) - 1
	if c.cy < 0 {
		c.cy = 0
	}
	return c
}

// SetSize resizes the window, keeping the cursor where it is.
func (c *CopyMode) SetSize(w, h int) {
	c.w, c.h = w, h
}

// Cursor returns the cursor's line and cell column.
func (c *CopyMode) Cursor() (line, col int) { return c.cy, c.cx }

// Update handles one key, in Bubble Tea's key string form. It reports whether
// copy mode is finished, and the text to yank when it is (empty on quit).
func (c *CopyMode) Update(key string) (done bool, yank string) {
	if c.typing {
		return c.updateSearch(key)
	}
	switch key {
	case "q":
		return true, ""
	case "esc":
		if c.mode != selNone {
			c.mode = selNone
			return false, ""
		}
		return true, ""
	case "y", "enter":
		return true, c.yankText()
	case "h", "left":
		c.moveX(-1)
	case "l", "right":
		c.moveX(1)
	case "k", "up":
		c.moveY(-1)
	case "j", "down":
		c.moveY(1)
	case "0":
		c.cx = 0
	case "$":
		c.cx = lastCell(c.line())
	case "g":
		c.moveTo(0)
	case "G":
		c.moveTo(len(c.lines) - 1)
	case "ctrl+u":
		c.moveY(-c.halfPage())
	case "ctrl+d":
		c.moveY(c.halfPage())
	case "w":
		c.moveWord(true)
	case "b":
		c.moveWord(false)
	case "v":
		c.toggle(selChar)
	case "V":
		c.toggle(selLine)
	case "/":
		c.typing = true
		c.query = ""
	case "n":
		c.searchForward()
	case "N":
		c.searchBackward()
	}
	return false, ""
}

// View renders exactly w cells on each of exactly h lines. The last line is the
// status line.
func (c *CopyMode) View() string {
	if c.w <= 0 || c.h <= 0 {
		return ""
	}
	textH := c.h - 1
	if textH < 0 {
		textH = 0
	}
	c.scroll(textH)
	out := make([]string, 0, c.h)
	for i := 0; i < textH; i++ {
		if li := c.top + i; li >= 0 && li < len(c.lines) {
			out = append(out, c.renderLine(li))
		} else {
			out = append(out, "")
		}
	}
	if c.h > 0 {
		out = append(out, render.Truncate(c.status(), c.w))
	}
	return render.Fit(strings.Join(out, "\n"), c.w, c.h)
}

// status is the last line: position, mode hint and the keys that finish.
func (c *CopyMode) status() string {
	var b strings.Builder
	b.WriteString("[copy] ")
	b.WriteString(render.Int(int64(c.cy + 1)))
	b.WriteString("/")
	b.WriteString(render.Int(int64(len(c.lines))))
	switch {
	case c.typing:
		b.WriteString("  /" + c.query)
	case c.mode == selLine:
		b.WriteString("  V:lines")
	default:
		b.WriteString("  v:select")
	}
	b.WriteString("  y:yank  q:quit")
	return b.String()
}

// line is the line the cursor is on ("" when there is none).
func (c *CopyMode) line() string {
	if c.cy < 0 || c.cy >= len(c.lines) {
		return ""
	}
	return c.lines[c.cy]
}

// halfPage is the distance ctrl+u and ctrl+d move.
func (c *CopyMode) halfPage() int {
	half := (c.h - 1) / 2
	if half < 1 {
		half = 1
	}
	return half
}

// moveTo moves to line y, clamping both coordinates.
func (c *CopyMode) moveTo(y int) {
	if len(c.lines) == 0 {
		return
	}
	if y < 0 {
		y = 0
	}
	if y > len(c.lines)-1 {
		y = len(c.lines) - 1
	}
	c.cy = y
	c.clampX()
}

// moveY moves n lines up (negative) or down.
func (c *CopyMode) moveY(n int) { c.moveTo(c.cy + n) }

// moveX moves n cells left (negative) or right, clamped to the line.
func (c *CopyMode) moveX(n int) {
	c.cx += n
	c.clampX()
}

// clampX keeps the cursor column inside the current line.
func (c *CopyMode) clampX() {
	if c.cx < 0 {
		c.cx = 0
	}
	if last := lastCell(c.line()); c.cx > last {
		c.cx = last
	}
}

// toggle starts, switches or stops a selection anchored at the cursor.
func (c *CopyMode) toggle(m selMode) {
	if c.mode == m {
		c.mode = selNone
		return
	}
	c.mode = m
	c.anchor = cellPos{c.cy, c.cx}
}

// selection returns the selection's two ends in text order.
func (c *CopyMode) selection() (cellPos, cellPos) {
	cur := cellPos{c.cy, c.cx}
	a, b := c.anchor, cur
	if a.y > b.y || (a.y == b.y && a.x > b.x) {
		return b, a
	}
	return a, b
}

// yankText is the text the selection covers, or the current line when there is
// no selection.
func (c *CopyMode) yankText() string {
	if c.mode == selNone || len(c.lines) == 0 {
		return c.line()
	}
	a, b := c.selection()
	if c.mode == selLine {
		return strings.Join(c.lines[a.y:b.y+1], "\n")
	}
	if a.y == b.y {
		return cutCells(c.lines[a.y], a.x, b.x)
	}
	parts := []string{cutCells(c.lines[a.y], a.x, lineWidth(c.lines[a.y])-1)}
	parts = append(parts, c.lines[a.y+1:b.y]...)
	parts = append(parts, cutCells(c.lines[b.y], 0, b.x))
	return strings.Join(parts, "\n")
}

// selected reports whether the rune starting at start is selected.
func (c *CopyMode) selected(li, start int) bool {
	if c.mode == selNone {
		return false
	}
	a, b := c.selection()
	if li < a.y || li > b.y {
		return false
	}
	if c.mode == selLine {
		return true
	}
	switch {
	case a.y == b.y:
		return start >= a.x && start <= b.x
	case li == a.y:
		return start >= a.x
	case li == b.y:
		return start <= b.x
	}
	return true
}

// highlight reports whether a rune is drawn in reverse video: the rune under
// the cursor, or any selected rune.
func (c *CopyMode) highlight(li, start, w int) bool {
	if li == c.cy && start <= c.cx && c.cx < start+w {
		return true
	}
	return c.selected(li, start)
}

// renderLine draws one line of text with the selection and the cursor cell in
// reverse video, exactly w cells wide.
func (c *CopyMode) renderLine(li int) string {
	var b strings.Builder
	cell := 0
	styled := false
	setStyle := func(on bool) {
		if on == styled {
			return
		}
		if on {
			b.WriteString(reverseOn)
		} else {
			b.WriteString(reverseOff)
		}
		styled = on
	}
	for _, r := range c.lines[li] {
		w := runeWidth(r)
		if w == 0 {
			// Combining marks belong to the previous cell.
			if cell > c.left && cell <= c.left+c.w {
				b.WriteRune(r)
			}
			continue
		}
		start := cell
		cell += w
		if start+w <= c.left {
			continue // wholly left of the window
		}
		if start < c.left {
			// A wide rune straddling the left edge: pad the part that is cut.
			setStyle(false)
			b.WriteString(strings.Repeat(" ", start+w-c.left))
			continue
		}
		if start >= c.left+c.w {
			break
		}
		setStyle(c.highlight(li, start, w))
		b.WriteRune(r)
	}
	setStyle(false)
	return render.PadRight(b.String(), c.w)
}

// scroll keeps the cursor inside the visible window.
func (c *CopyMode) scroll(textH int) {
	if textH <= 0 {
		c.top = c.cy
		return
	}
	if c.cy < c.top {
		c.top = c.cy
	}
	if c.cy >= c.top+textH {
		c.top = c.cy - textH + 1
	}
	if maxTop := len(c.lines) - textH; c.top > maxTop {
		c.top = maxTop
	}
	if c.top < 0 {
		c.top = 0
	}
	if c.cx < c.left {
		c.left = c.cx
	}
	if c.cx >= c.left+c.w {
		c.left = c.cx - c.w + 1
	}
	if c.left < 0 {
		c.left = 0
	}
}

// updateSearch handles keys while a search pattern is being typed.
func (c *CopyMode) updateSearch(key string) (done bool, yank string) {
	switch key {
	case "esc":
		c.typing = false
		c.query = ""
	case "enter":
		c.typing = false
		c.searchForward()
	case "backspace":
		if rs := []rune(c.query); len(rs) > 0 {
			c.query = string(rs[:len(rs)-1])
		}
	default:
		if rs := []rune(key); len(rs) == 1 {
			c.query += key
		}
	}
	return false, ""
}

// searchForward finds the next occurrence of the query, wrapping at the end of
// the text and starting just after the cursor.
func (c *CopyMode) searchForward() {
	n := len(c.lines)
	if n == 0 || c.query == "" {
		return
	}
	for i := 0; i < n; i++ {
		li := (c.cy + i) % n
		from := 0
		if i == 0 {
			from = c.cx + 1
		}
		if cell, ok := findIn(c.lines[li], c.query, from); ok {
			c.cy, c.cx = li, cell
			return
		}
	}
}

// searchBackward finds the previous occurrence of the query, wrapping at the
// start of the text.
func (c *CopyMode) searchBackward() {
	n := len(c.lines)
	if n == 0 || c.query == "" {
		return
	}
	for i := 0; i < n; i++ {
		li := (c.cy - i + n) % n
		before := lineWidth(c.lines[li])
		if i == 0 {
			before = c.cx
		}
		if cell, ok := findLast(c.lines[li], c.query, before); ok {
			c.cy, c.cx = li, cell
			return
		}
	}
}

// moveWord moves to the start of the next (forward) or previous word.
func (c *CopyMode) moveWord(forward bool) {
	cs := cells(c.line())
	if forward {
		for i, rc := range cs {
			if rc.off <= c.cx || isSpace(rc.r) {
				continue
			}
			if i == 0 || isSpace(cs[i-1].r) {
				c.cx = rc.off
				return
			}
		}
		c.cx = lastCell(c.line())
		return
	}
	best := -1
	for i, rc := range cs {
		if rc.off >= c.cx {
			break
		}
		if isSpace(rc.r) {
			continue
		}
		if i == 0 || isSpace(cs[i-1].r) {
			best = rc.off
		}
	}
	if best >= 0 {
		c.cx = best
		return
	}
	c.cx = 0
}

// runeCell is one rune of a line with its cell offset and width.
type runeCell struct {
	r      rune
	off, w int
}

// cells splits s into runes with cell offsets. Zero-width runes are kept with
// the current offset.
func cells(s string) []runeCell {
	out := make([]runeCell, 0, len(s))
	off := 0
	for _, r := range s {
		w := runeWidth(r)
		out = append(out, runeCell{r: r, off: off, w: w})
		off += w
	}
	return out
}

// isSpace reports whether r separates words.
func isSpace(r rune) bool {
	return r == ' ' || r == '\t'
}

// runeWidth is the number of cells r occupies; zero for combining marks.
func runeWidth(r rune) int {
	if w := runewidth.RuneWidth(r); w > 0 {
		return w
	}
	return 0
}

// lineWidth is the number of cells in s.
func lineWidth(s string) int { return render.Width(s) }

// lastCell is the last cell of a line; 0 for an empty line.
func lastCell(s string) int {
	if w := lineWidth(s); w > 0 {
		return w - 1
	}
	return 0
}

// cutCells returns the runes of s whose cells fall in [from, to]. A rune that
// straddles an edge is taken whole, so a wide character is never split.
func cutCells(s string, from, to int) string {
	if to < from {
		return ""
	}
	var b strings.Builder
	for _, rc := range cells(s) {
		if rc.w == 0 {
			continue
		}
		if rc.off+rc.w-1 >= from && rc.off <= to {
			b.WriteRune(rc.r)
		}
	}
	return b.String()
}

// cellToByte returns the byte offset of the rune covering cell.
func cellToByte(s string, cell int) int {
	if cell <= 0 {
		return 0
	}
	off := 0
	for i, r := range s {
		w := runeWidth(r)
		if w == 0 {
			continue
		}
		if off+w > cell {
			return i
		}
		off += w
	}
	return len(s)
}

// findIn returns the cell of the first query match at or after cell from.
func findIn(s, q string, from int) (int, bool) {
	b := cellToByte(s, from)
	if b > len(s) {
		return 0, false
	}
	i := strings.Index(s[b:], q)
	if i < 0 {
		return 0, false
	}
	return lineWidth(s[:b+i]), true
}

// findLast returns the cell of the last query match that starts before cell
// before.
func findLast(s, q string, before int) (int, bool) {
	best, ok := 0, false
	for off := 0; off <= len(s); {
		i := strings.Index(s[off:], q)
		if i < 0 {
			break
		}
		if cell := lineWidth(s[:off+i]); cell >= before {
			break
		} else {
			best, ok = cell, true
		}
		off += i + 1
	}
	return best, ok
}
