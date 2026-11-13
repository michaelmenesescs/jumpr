package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/render"
	"jumper/internal/tui/theme"
)

// HelpSection is one titled group of key hints in the help dialog.
type HelpSection struct {
	Title string
	Hints []KeyHint
}

type helpDialog struct {
	t      *theme.Theme
	title  string
	lines  []string
	offset int
	page   int
}

// NewHelpDialog returns a scrollable key-help dialog. up/down scroll; esc, q
// or ? close it.
func NewHelpDialog(t *theme.Theme, title string, sections []HelpSection) Dialog {
	keyW := 0
	for _, s := range sections {
		for _, h := range s.Hints {
			keyW = max(keyW, render.Width(h.Key))
		}
	}
	var lines []string
	for i, s := range sections {
		if i > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, t.Header.Render(s.Title))
		for _, h := range s.Hints {
			lines = append(lines, "  "+t.HintKey.Render(render.PadRight(h.Key, keyW))+"  "+t.HintText.Render(h.Text))
		}
	}
	return &helpDialog{t: t, title: title, lines: lines, page: 10}
}

func (h *helpDialog) Title() string { return h.title }

func (h *helpDialog) Update(msg tea.Msg) (Dialog, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return h, nil
	}
	switch k.String() {
	case "esc", "q", "?":
		return h, CloseDialog()
	case "up", "k":
		h.offset--
	case "down", "j":
		h.offset++
	case "pgup":
		h.offset -= h.page
	case "pgdown":
		h.offset += h.page
	case "home", "g":
		h.offset = 0
	case "end", "G":
		h.offset = len(h.lines)
	}
	h.offset = max(0, min(h.offset, len(h.lines)-1))
	return h, nil
}

func (h *helpDialog) View(maxW, maxH int) string {
	if maxW < 1 || maxH < 1 {
		return ""
	}
	rows := maxH - 1 // last row is the footer
	if rows < 1 {
		rows = 1
	}
	h.page = max(rows-1, 1)
	off := max(0, min(h.offset, max(len(h.lines)-rows, 0)))
	end := min(off+rows, len(h.lines))
	out := append([]string{}, h.lines[off:end]...)
	for len(out) < min(rows, len(h.lines)) {
		out = append(out, "")
	}
	out = append(out, h.t.Dim.Render("↑/↓ scroll  esc close"))
	for i := range out {
		out[i] = render.PadRight(out[i], maxW)
	}
	return joinLines(out)
}

func joinLines(l []string) string {
	s := ""
	for i, x := range l {
		if i > 0 {
			s += "\n"
		}
		s += x
	}
	return s
}
