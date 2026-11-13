package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/render"
	"jumper/internal/tui/theme"
)

// Dialog is a modal box owned by the root. It closes itself by returning a
// command that yields CloseDialogMsg.
type Dialog interface {
	Update(msg tea.Msg) (Dialog, tea.Cmd)
	View(maxW, maxH int) string
	Title() string
}

// CloseDialog returns a command that yields CloseDialogMsg.
func CloseDialog() tea.Cmd {
	return func() tea.Msg { return CloseDialogMsg{} }
}

// RenderDialog draws d centred in a rounded border over background. The result
// is exactly screenW × screenH.
func RenderDialog(t *theme.Theme, d Dialog, screenW, screenH int, background string) string {
	bg := render.Fit(background, screenW, screenH)
	if screenW < 5 || screenH < 3 {
		return bg
	}
	maxW, maxH := screenW-2, screenH-2
	body := strings.Split(d.View(maxW-2, maxH), "\n")
	if len(body) > maxH {
		body = body[:maxH]
	}
	title := d.Title()
	inner := render.Width(title) + 2
	for _, l := range body {
		inner = max(inner, render.Width(l))
	}
	inner = min(inner, maxW)

	bs := t.BorderFocused
	var box []string
	tt := ""
	if title != "" {
		tt = " " + render.Truncate(title, inner-2) + " "
	}
	fill := inner - render.Width(tt)
	box = append(box, bs.Render("╭")+t.Title.Render(tt)+bs.Render(strings.Repeat("─", fill)+"╮"))
	for _, l := range body {
		box = append(box, bs.Render("│")+render.PadRight(l, inner)+bs.Render("│"))
	}
	box = append(box, bs.Render("╰"+strings.Repeat("─", inner)+"╯"))

	bw := inner + 2
	x := (screenW - bw) / 2
	y := (screenH - len(box)) / 2
	lines := strings.Split(bg, "\n")
	for i, bl := range box {
		row := lines[y+i]
		left := render.PadRight(ansi.Truncate(row, x, ""), x)
		right := ansi.TruncateLeft(row, x+bw, "")
		lines[y+i] = render.PadRight(left+"\x1b[0m"+bl+"\x1b[0m"+right, screenW)
	}
	return strings.Join(lines, "\n")
}
