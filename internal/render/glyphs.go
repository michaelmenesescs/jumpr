package render

import (
	"fmt"
	"math"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"jumper/internal/errs"
)

// GlyphSet is one complete, declared set of the glyphs jumper draws. Every
// rune the UI emits outside resource data comes from the active set, so a
// terminal font that lacks one family (Nerd Font icons, Unicode blocks) still
// gets a readable frame after switching sets.
//
// The ascii set is guaranteed printable in any font: every rune in it is below
// 0x80.
type GlyphSet struct {
	Name string

	// Spark is the ascending ramp used by Spark, from lowest to highest.
	Spark []rune
	// BarFull and BarEmpty draw a Bar's filled and empty cells.
	BarFull, BarEmpty rune
	// Ellipsis replaces text cut by Truncate and marks pending work.
	Ellipsis rune
	// Empty is the placeholder for a value that is absent or unknown.
	Empty rune
	// StatusOK, StatusWarn, StatusDegraded, StatusCrit, StatusNA and
	// StatusUnknown are the one-cell status markers.
	StatusOK, StatusWarn, StatusDegraded, StatusCrit, StatusNA, StatusUnknown rune
	// Separator joins parts of one line; Arrow prefixes a drill-in hint.
	Separator, Arrow rune
}

// unicodeGlyphs is the default set: Unicode block sparklines and solid status
// dots, which every modern terminal font carries.
func unicodeGlyphs() GlyphSet {
	return GlyphSet{
		Name:           "unicode",
		Spark:          []rune("▁▂▃▄▅▆▇█"),
		BarFull:        '█',
		BarEmpty:       '·',
		Ellipsis:       '…',
		Empty:          '—',
		StatusOK:       '●',
		StatusWarn:     '●',
		StatusDegraded: '◐',
		StatusCrit:     '●',
		StatusNA:       '○',
		StatusUnknown:  '?',
		Separator:      '·',
		Arrow:          '→',
	}
}

// nerdGlyphs is the opt-in Nerd Font set. The status markers and the separator
// are Private Use Area codepoints, so they only render in a terminal whose font
// is a Nerd Font (JetBrainsMono NF and friends). Nothing detects the font.
func nerdGlyphs() GlyphSet {
	set := unicodeGlyphs()
	set.Name = "nerd"
	set.StatusOK = '\U000f012c'      // nf-md-check_circle
	set.StatusWarn = '\U000f0026'    // nf-md-alert
	set.StatusCrit = '\U000f015a'    // nf-md-close_circle
	set.StatusNA = '\U000f0131'      // nf-md-circle_outline
	set.StatusUnknown = '\U000f0131' // nf-md-circle_outline
	set.Separator = '│'
	return set
}

// asciiGlyphs is the set that renders in any font, on any console, over any
// ssh connection: every rune is below 0x80.
func asciiGlyphs() GlyphSet {
	return GlyphSet{
		Name:           "ascii",
		Spark:          []rune("_.,-~+=#"),
		BarFull:        '#',
		BarEmpty:       '-',
		Ellipsis:       '~',
		Empty:          '-',
		StatusOK:       '+',
		StatusWarn:     '!',
		StatusDegraded: '!',
		StatusCrit:     'x',
		StatusNA:       '.',
		StatusUnknown:  '?',
		Separator:      '|',
		Arrow:          '>',
	}
}

// GlyphNames returns every set name, sorted. "auto" is a config value, not a
// set, so it is not listed.
func GlyphNames() []string {
	return []string{"ascii", "nerd", "unicode"}
}

// ASCII reports whether the set draws only runes below 0x80. The ascii set is
// the one set for which that holds, and its Empty placeholder is the field
// every other set fills with a wide rune, so Empty is the discriminator.
func (g GlyphSet) ASCII() bool {
	return g.Empty > 0 && g.Empty < 0x80
}

// Glyphs returns the named set. "auto" and "" resolve to unicode: auto is
// resolved once at startup by AutoGlyphs, which takes the environment value as
// an argument so this package stays pure. An unknown name is an errs.Config
// error.
func Glyphs(name string) (GlyphSet, error) {
	switch name {
	case "auto", "", "unicode":
		return unicodeGlyphs(), nil
	case "nerd":
		return nerdGlyphs(), nil
	case "ascii":
		return asciiGlyphs(), nil
	}
	return GlyphSet{}, errs.New(errs.Config, "render.glyphs", fmt.Sprintf("unknown glyph set %q (want auto, unicode, nerd or ascii)", name))
}

// AutoGlyphs resolves the auto setting against an environment value, normally
// $JUMPER_GLYPHS. nerd and ascii are explicit overrides; anything else —
// including empty — is unicode. It reads no environment itself and never probes
// the terminal font: auto is deterministic and pure.
func AutoGlyphs(env string) GlyphSet {
	switch env {
	case "nerd":
		return nerdGlyphs()
	case "ascii":
		return asciiGlyphs()
	}
	return unicodeGlyphs()
}

// SparkWith renders vals as a sparkline of exactly width cells using set's
// ramp. It behaves exactly like Spark otherwise.
func SparkWith(set GlyphSet, vals []float64, width int) string {
	if width <= 0 {
		return ""
	}
	blocks := set.Spark
	if len(blocks) == 0 {
		blocks = unicodeGlyphs().Spark
	}
	buckets := downsample(vals, width)

	peak := 0.0
	for _, v := range buckets {
		if !math.IsNaN(v) && v > peak {
			peak = v
		}
	}

	last := len(blocks) - 1
	var b strings.Builder
	b.Grow(width)
	for i := len(buckets); i < width; i++ {
		b.WriteByte(' ')
	}
	for _, v := range buckets {
		if math.IsNaN(v) {
			b.WriteByte(' ')
			continue
		}
		if peak <= 0 {
			b.WriteRune(blocks[0])
			continue
		}
		idx := int(v / peak * float64(last))
		if idx < 0 {
			idx = 0
		}
		if idx > last {
			idx = last
		}
		b.WriteRune(blocks[idx])
	}
	return b.String()
}

// BarWith renders a fraction as a width-cell bar with set's filled and empty
// runes. It behaves exactly like Bar otherwise.
func BarWith(set GlyphSet, fraction float64, width int) string {
	if width <= 0 {
		return ""
	}
	if math.IsNaN(fraction) || fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}
	filled := int(math.Round(fraction * float64(width)))
	if filled > width {
		filled = width
	}

	full, empty := set.BarFull, set.BarEmpty
	if full == 0 || empty == 0 {
		u := unicodeGlyphs()
		full, empty = u.BarFull, u.BarEmpty
	}

	var b strings.Builder
	b.Grow(width * 3)
	for i := 0; i < filled; i++ {
		b.WriteRune(full)
	}
	for i := filled; i < width; i++ {
		b.WriteRune(empty)
	}
	return b.String()
}

// TruncateWith cuts s to at most w cells and marks a cut with set's ellipsis.
// It is Truncate with the active set's ellipsis rune.
func TruncateWith(set GlyphSet, s string, w int) string {
	if w <= 0 {
		return ""
	}
	if Width(s) <= w {
		return s
	}
	mark := set.Ellipsis
	if mark == 0 {
		mark = unicodeGlyphs().Ellipsis
	}
	out := truncateMark(s, w, string(mark))
	if strings.ContainsRune(s, '\x1b') {
		out += reset
	}
	return out
}

// PadLeftWith truncates s to w cells with set's ellipsis and pads it on the
// left with spaces so the result is exactly w cells wide.
func PadLeftWith(set GlyphSet, s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = TruncateWith(set, s, w)
	if pad := w - Width(s); pad > 0 {
		s = strings.Repeat(" ", pad) + s
	}
	return s
}

// RowWith renders one table line with set's ellipsis, exactly like Row: every
// cell is truncated/padded to its width, right-aligned when the column says so,
// and styled after padding.
func RowWith(set GlyphSet, cells []Cell, widths []int, cols []TableColumn) string {
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
			cell = PadLeftWith(set, text, w)
		} else {
			cell = PadRightWith(set, text, w)
		}
		if style != nil {
			cell = style(cell)
		}
		b.WriteString(cell)
	}
	return b.String()
}

// HeaderWith renders the title row of a table with set's ellipsis, with the
// same widths and alignment as RowWith.
func HeaderWith(set GlyphSet, cols []TableColumn, widths []int) string {
	cells := make([]Cell, len(cols))
	for i, c := range cols {
		cells[i] = Cell{Text: c.Title}
	}
	return RowWith(set, cells, widths, cols)
}

// truncateMark cuts s to at most w cells, ending a cut with mark.
func truncateMark(s string, w int, mark string) string {
	return ansi.Truncate(s, w, mark)
}

// PadRightWith truncates s to w cells with set's ellipsis and pads it on the
// right with spaces so the result is exactly w cells wide.
func PadRightWith(set GlyphSet, s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = TruncateWith(set, s, w)
	if pad := w - Width(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// FitWith returns exactly h lines of exactly w cells each, like Fit, but marks
// any truncation with set's ellipsis.
func FitWith(set GlyphSet, s string, w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	out := make([]string, 0, h)
	for _, line := range lines {
		out = append(out, PadRightWith(set, line, w))
	}
	blank := strings.Repeat(" ", w)
	for len(out) < h {
		out = append(out, blank)
	}
	return strings.Join(out, "\n")
}
