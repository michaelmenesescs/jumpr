package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend"
	"jumper/internal/render"
	"jumper/internal/tui/theme"
)

// WindowTab is one window in the status line.
type WindowTab struct {
	Index  int
	Name   string
	Active bool
}

// HealthDot is one backend's health indicator.
type HealthDot struct {
	Name  string
	State backend.HealthState
}

// StatusData is everything the status line shows.
type StatusData struct {
	Session       string
	Windows       []WindowTab
	Prefix        string
	Context       string
	Investigation string
	Window        string
	Targets       int
	Health        []HealthDot
	ReadOnly      bool
	Sync          int
	// Glyphs is the active set; a zero set renders with unicode.
	Glyphs render.GlyphSet
}

// statusGlyphs returns the set the status line draws with: the one it was
// handed, or unicode for a zero value.
func statusGlyphs(d StatusData) render.GlyphSet {
	if d.Glyphs.Name != "" {
		return d.Glyphs
	}
	set, _ := render.Glyphs("unicode")
	return set
}

// healthGlyph returns the dot glyph and its styled form for a state, using the
// unicode set. RenderStatus passes the active set to healthGlyphWith instead.
func healthGlyph(t *theme.Theme, s backend.HealthState) (string, string) {
	set, _ := render.Glyphs("unicode")
	return healthGlyphWith(set, t, s)
}

// healthGlyphWith returns the dot glyph and its styled form for a state, taken
// from set's status markers.
func healthGlyphWith(set render.GlyphSet, t *theme.Theme, s backend.HealthState) (string, string) {
	switch s {
	case backend.HealthOK:
		g := string(set.StatusOK)
		return g, t.OK.Render(g)
	case backend.HealthDegraded:
		g := string(set.StatusDegraded)
		return g, t.Warn.Render(g)
	case backend.HealthDown:
		g := string(set.StatusCrit)
		return g, t.Crit.Render(g)
	default:
		g := string(set.StatusNA)
		return g, t.Faint.Render(g)
	}
}

// RenderStatus renders the one-line status bar, exactly w cells wide. When w is
// too small it drops, in order: context, window label, health names, window
// tabs except the active one.
func RenderStatus(t *theme.Theme, d StatusData, w int) string {
	if w <= 0 {
		return ""
	}
	set := statusGlyphs(d)
	for level := 0; level <= 4; level++ {
		left, right := statusParts(set, t, d, level)
		lw, rw := render.Width(left), render.Width(right)
		if lw+rw+1 <= w || level == 4 {
			if lw+rw+1 > w {
				// Still too wide: give the right side priority.
				return render.PadRight(render.Truncate(left, max(w-rw-1, 0))+" "+right, w)
			}
			return left + strings.Repeat(" ", w-lw-rw) + right
		}
	}
	return strings.Repeat(" ", w)
}

func statusParts(set render.GlyphSet, t *theme.Theme, d StatusData, level int) (string, string) {
	parts := []string{t.Accent.Bold(true).Render("jumper")}
	if d.Session != "" {
		parts = append(parts, t.Bold.Render(d.Session))
	}
	var tabs []string
	for _, wt := range d.Windows {
		if level >= 4 && !wt.Active {
			continue
		}
		s := fmt.Sprintf("%d:%s", wt.Index, wt.Name)
		if wt.Active {
			tabs = append(tabs, t.Selected.Render(s+"*"))
		} else {
			tabs = append(tabs, t.Dim.Render(s))
		}
	}
	if len(tabs) > 0 {
		parts = append(parts, strings.Join(tabs, " "))
	}
	if level < 1 && d.Context != "" {
		parts = append(parts, t.Dim.Render(d.Context))
	}
	if d.Investigation != "" {
		parts = append(parts, t.Accent.Render("inv:"+d.Investigation))
	}
	if level < 2 && d.Window != "" {
		parts = append(parts, t.Dim.Render("window:"+d.Window))
	}
	parts = append(parts, t.Dim.Render(fmt.Sprintf("targets:%d", d.Targets)))
	left := strings.Join(parts, "  ")

	var r []string
	if d.ReadOnly {
		r = append(r, t.Warn.Bold(true).Render("RO"))
	}
	if d.Sync > 0 {
		mark := "×"
		if set.ASCII() {
			mark = "x"
		}
		r = append(r, t.Crit.Bold(true).Render(fmt.Sprintf("SYNC %s%d", mark, d.Sync)))
	}
	if d.Prefix != "" {
		r = append(r, t.Dim.Render(d.Prefix))
	}
	var dots []string
	for _, h := range d.Health {
		_, g := healthGlyphWith(set, t, h.State)
		if level < 3 {
			g += t.Dim.Render(h.Name)
		}
		dots = append(dots, g)
	}
	if len(dots) > 0 {
		r = append(r, strings.Join(dots, " "))
	}
	return left, strings.Join(r, "  ")
}

// healthTimeout bounds one backend health check.
const healthTimeout = 5 * time.Second

// HealthPoll checks every backend's Health concurrently (5 s timeout each) and
// returns one HealthMsg per backend. The root re-arms it with tea.Tick every
// interval; the command itself runs once.
func HealthPoll(reg *backend.Registry, interval time.Duration) tea.Cmd {
	_ = interval
	if reg == nil {
		return nil
	}
	var cmds []tea.Cmd
	for _, b := range reg.Backends() {
		b := b
		cmds = append(cmds, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), healthTimeout)
			defer cancel()
			done := make(chan backend.Health, 1)
			go func() { done <- b.Health(ctx) }()
			select {
			case h := <-done:
				return HealthMsg{Backend: b.Name(), Health: h}
			case <-ctx.Done():
				return HealthMsg{Backend: b.Name(), Health: backend.Health{
					State: backend.HealthDown, Detail: "health check timed out", Checked: time.Now(),
				}}
			}
		})
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}
