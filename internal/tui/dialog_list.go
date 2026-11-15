package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/render"
	"jumpr/internal/tui/theme"
)

// ListItem is one row of a list dialog.
type ListItem struct {
	Label  string
	Detail string
}

// listDialog is a scrolling pick list: up/down move, enter picks, esc closes.
type listDialog struct {
	t      *theme.Theme
	title  string
	items  []ListItem
	sel    int
	offset int
	pick   func(int) tea.Cmd
}

var _ Dialog = (*listDialog)(nil)

// NewListDialog returns a pick list over items. pick runs with the selected
// index on enter; nil pick just closes the dialog.
func NewListDialog(t *theme.Theme, title string, items []ListItem, pick func(int) tea.Cmd) Dialog {
	return &listDialog{t: t, title: title, items: items, pick: pick}
}

// Title is the dialog's border title.
func (d *listDialog) Title() string { return d.title }

// Update moves the selection and picks on enter.
func (d *listDialog) Update(msg tea.Msg) (Dialog, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return d, nil
	}
	last := len(d.items) - 1
	switch k.String() {
	case "esc", "q":
		return d, CloseDialog()
	case "enter":
		if d.pick == nil || d.sel < 0 || d.sel >= len(d.items) {
			return d, CloseDialog()
		}
		sel := d.sel
		return d, tea.Batch(CloseDialog(), d.pick(sel))
	case "up", "k":
		if d.sel > 0 {
			d.sel--
		}
	case "down", "j":
		if d.sel < last {
			d.sel++
		}
	case "home", "g":
		d.sel = 0
	case "end", "G":
		if last >= 0 {
			d.sel = last
		}
	}
	return d, nil
}

// View renders the visible rows and a footer, exactly maxW cells wide.
func (d *listDialog) View(maxW, maxH int) string {
	if maxW < 1 || maxH < 1 {
		return ""
	}
	rows := maxH - 1
	if rows < 1 {
		rows = 1
	}
	// Keep the selection inside the window.
	if d.sel < d.offset {
		d.offset = d.sel
	}
	if d.sel >= d.offset+rows {
		d.offset = d.sel - rows + 1
	}
	if d.offset < 0 {
		d.offset = 0
	}
	lines := make([]string, 0, rows+1)
	for i := 0; i < rows; i++ {
		idx := d.offset + i
		if idx >= len(d.items) {
			lines = append(lines, "")
			continue
		}
		item := d.items[idx]
		text := item.Label
		if item.Detail != "" {
			text += "  " + item.Detail
		}
		text = render.Truncate(text, maxW)
		if d.t != nil {
			if idx == d.sel {
				text = d.t.Selected.Render(text)
			} else {
				text = d.t.Base.Render(text)
			}
		}
		lines = append(lines, render.PadRight(text, maxW))
	}
	footer := "enter select  esc close"
	if d.t != nil {
		footer = d.t.Dim.Render(footer)
	}
	lines = append(lines, render.PadRight(footer, maxW))
	return strings.Join(lines, "\n")
}
