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
}

// healthGlyph returns the dot glyph and its styled form for a state.
func healthGlyph(t *theme.Theme, s backend.HealthState) (string, string) {
	switch s {
	case backend.HealthOK:
		return "●", t.OK.Render("●")
	case backend.HealthDegraded:
		return "◐", t.Warn.Render("◐")
	case backend.HealthDown:
		return "●", t.Crit.Render("●")
	default:
		return "○", t.Faint.Render("○")
	}
}

// RenderStatus renders the one-line status bar, exactly w cells wide. When w is
// too small it drops, in order: context, window label, health names, window
// tabs except the active one.
func RenderStatus(t *theme.Theme, d StatusData, w int) string {
	if w <= 0 {
		return ""
	}
	for level := 0; level <= 4; level++ {
		left, right := statusParts(t, d, level)
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

func statusParts(t *theme.Theme, d StatusData, level int) (string, string) {
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
		r = append(r, t.Crit.Bold(true).Render(fmt.Sprintf("SYNC ×%d", d.Sync)))
	}
	if d.Prefix != "" {
		r = append(r, t.Dim.Render(d.Prefix))
	}
	var dots []string
	for _, h := range d.Health {
		_, g := healthGlyph(t, h.State)
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
