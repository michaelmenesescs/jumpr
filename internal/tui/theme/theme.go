// Package theme holds the single set of colours and styles every view uses.
//
// Views never name a colour directly: they take styles from Theme. The colour
// profile is the one piece of package-level mutable state jumper allows
// (BUILD-PLAN §6.1 rule 7); it is fixed once per process by SetProfile.
package theme

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"jumper/internal/render"
	"jumper/internal/resource"
)

// Flash holds the styles for the four flash-line severities.
type Flash struct {
	Info  lipgloss.Style
	OK    lipgloss.Style
	Warn  lipgloss.Style
	Error lipgloss.Style
}

// Theme is the complete palette handed to every view and renderer.
type Theme struct {
	Base          lipgloss.Style
	Dim           lipgloss.Style
	Faint         lipgloss.Style
	Bold          lipgloss.Style
	Accent        lipgloss.Style
	OK            lipgloss.Style
	Warn          lipgloss.Style
	Crit          lipgloss.Style
	Header        lipgloss.Style
	Selected      lipgloss.Style
	Border        lipgloss.Style
	BorderFocused lipgloss.Style
	StatusBar     lipgloss.Style
	HintKey       lipgloss.Style
	HintText      lipgloss.Style
	Flash         Flash
	Title         lipgloss.Style
	Crumb         lipgloss.Style
}

// Dark returns the default dark, high-contrast theme. Colours are fixed
// 256-colour codes so the palette is identical on every terminal; the colour
// profile (not the palette) decides how they are encoded.
func Dark() *Theme {
	base := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))

	return &Theme{
		Base:          base,
		Dim:           lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		Faint:         lipgloss.NewStyle().Foreground(lipgloss.Color("240")),
		Bold:          base.Bold(true),
		Accent:        lipgloss.NewStyle().Foreground(lipgloss.Color("75")),
		OK:            lipgloss.NewStyle().Foreground(lipgloss.Color("42")),
		Warn:          lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
		Crit:          lipgloss.NewStyle().Foreground(lipgloss.Color("203")),
		Header:        lipgloss.NewStyle().Foreground(lipgloss.Color("75")).Bold(true),
		Selected:      base.Background(lipgloss.Color("237")),
		Border:        lipgloss.NewStyle().Foreground(lipgloss.Color("240")),
		BorderFocused: lipgloss.NewStyle().Foreground(lipgloss.Color("75")),
		StatusBar:     lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Background(lipgloss.Color("236")),
		HintKey:       lipgloss.NewStyle().Foreground(lipgloss.Color("75")).Bold(true),
		HintText:      lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		Flash: Flash{
			Info:  lipgloss.NewStyle().Foreground(lipgloss.Color("75")),
			OK:    lipgloss.NewStyle().Foreground(lipgloss.Color("42")),
			Warn:  lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
			Error: lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true),
		},
		Title: lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Bold(true),
		Crumb: lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
	}
}

// SetProfile fixes the colour profile for the process. Valid names are
// "auto" (keep the detected profile), "truecolor", "256", "16" and "none"
// (no colour or attributes at all). Anything else is ignored.
func SetProfile(name string) {
	switch name {
	case "auto", "":
		return
	case "truecolor":
		lipgloss.SetColorProfile(termenv.TrueColor)
	case "256":
		lipgloss.SetColorProfile(termenv.ANSI256)
	case "16":
		lipgloss.SetColorProfile(termenv.ANSI)
	case "none":
		lipgloss.SetColorProfile(termenv.Ascii)
	}
}

// StatusMarker returns set's one-cell marker for a resource status, styled the
// same way as StatusStyle styles the status text. The glyph set is passed in:
// the theme holds colours only, never the active set.
func (t *Theme) StatusMarker(set render.GlyphSet, s resource.Status) string {
	return t.StatusStyle(s).Render(string(marker(set, s)))
}

// marker returns the rune set draws for a status.
func marker(set render.GlyphSet, s resource.Status) rune {
	switch s {
	case resource.StatusOK:
		return set.StatusOK
	case resource.StatusWarn:
		return set.StatusWarn
	case resource.StatusCrit:
		return set.StatusCrit
	case resource.StatusNA:
		return set.StatusNA
	default:
		return set.StatusUnknown
	}
}

// Separator returns set's field separator, styled Dim.
func (t *Theme) Separator(set render.GlyphSet) string {
	if set.Separator == 0 {
		return ""
	}
	return t.Dim.Render(string(set.Separator))
}

// Arrow returns set's drill-in arrow, styled Dim.
func (t *Theme) Arrow(set render.GlyphSet) string {
	if set.Arrow == 0 {
		return ""
	}
	return t.Dim.Render(string(set.Arrow))
}

// StatusStyle returns the style for a resource status: Unknown is plain Base,
// OK/Warn/Crit map to their colours and NA is Faint.
func (t *Theme) StatusStyle(s resource.Status) lipgloss.Style {
	switch s {
	case resource.StatusOK:
		return t.OK
	case resource.StatusWarn:
		return t.Warn
	case resource.StatusCrit:
		return t.Crit
	case resource.StatusNA:
		return t.Faint
	default:
		return t.Base
	}
}
