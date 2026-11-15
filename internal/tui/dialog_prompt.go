package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/render"
	"jumpr/internal/tui/theme"
)

// promptDialog is the one-line text prompt: enter submits the trimmed value,
// esc cancels. The root uses it to rename a window or the session.
type promptDialog struct {
	t      *theme.Theme
	title  string
	label  string
	input  textinput.Model
	submit func(string) tea.Cmd
}

var _ Dialog = (*promptDialog)(nil)

// NewPromptDialog returns a dialog asking for one line of text. initial is
// shown and selected at the end. submit runs with the trimmed value when the
// user presses enter and the value is not empty; nil submit just closes.
func NewPromptDialog(t *theme.Theme, title, label, initial string, submit func(string) tea.Cmd) Dialog {
	in := textinput.New()
	in.Prompt = ""
	in.SetValue(initial)
	in.CursorEnd()
	in.Focus()
	return &promptDialog{t: t, title: title, label: label, input: in, submit: submit}
}

// Title is the dialog's border title.
func (p *promptDialog) Title() string { return p.title }

// Update feeds the input, or closes the dialog on enter and esc.
func (p *promptDialog) Update(msg tea.Msg) (Dialog, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		var next tea.Cmd
		p.input, next = p.input.Update(msg)
		return p, next
	}
	switch k.Type { //nolint:exhaustive // every other key goes to the input
	case tea.KeyEsc:
		return p, CloseDialog()
	case tea.KeyEnter:
		value := strings.TrimSpace(p.input.Value())
		if p.submit == nil || value == "" {
			return p, CloseDialog()
		}
		return p, tea.Batch(CloseDialog(), p.submit(value))
	}
	var next tea.Cmd
	p.input, next = p.input.Update(k)
	return p, next
}

// View renders the label, the input and a footer, exactly maxW cells wide.
func (p *promptDialog) View(maxW, maxH int) string {
	if maxW < 1 || maxH < 1 {
		return ""
	}
	lines := make([]string, 0, 4)
	if p.label != "" {
		lines = append(lines, p.label)
	}
	p.input.Width = max(1, maxW-2)
	lines = append(lines, p.input.View())
	for len(lines) < maxH-1 {
		lines = append(lines, "")
	}
	footer := "enter confirm  esc cancel"
	if p.t != nil {
		footer = p.t.Dim.Render(footer)
	}
	lines = append(lines, footer)
	if len(lines) > maxH {
		lines = lines[:maxH]
	}
	for i := range lines {
		lines[i] = render.PadRight(lines[i], maxW)
	}
	return strings.Join(lines, "\n")
}

// confirmDialog is the y/N question asked before killing busy content.
type confirmDialog struct {
	t     *theme.Theme
	title string
	text  string
	yes   tea.Cmd
}

var _ Dialog = (*confirmDialog)(nil)

// NewConfirmDialog returns a y/N dialog. yes runs on y, Y or enter; n, N, q
// and esc just close it.
func NewConfirmDialog(t *theme.Theme, title, text string, yes tea.Cmd) Dialog {
	return &confirmDialog{t: t, title: title, text: text, yes: yes}
}

// Title is the dialog's border title.
func (c *confirmDialog) Title() string { return c.title }

// Update answers the question.
func (c *confirmDialog) Update(msg tea.Msg) (Dialog, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return c, nil
	}
	switch k.String() {
	case "y", "Y", "enter":
		if c.yes == nil {
			return c, CloseDialog()
		}
		return c, tea.Batch(CloseDialog(), c.yes)
	case "n", "N", "q", "esc":
		return c, CloseDialog()
	}
	return c, nil
}

// View renders the question and the footer, exactly maxW cells wide.
func (c *confirmDialog) View(maxW, maxH int) string {
	if maxW < 1 || maxH < 1 {
		return ""
	}
	lines := make([]string, 0, 4)
	for _, l := range strings.Split(c.text, "\n") {
		lines = append(lines, render.Truncate(l, maxW))
	}
	for len(lines) < maxH-1 {
		lines = append(lines, "")
	}
	footer := "y/N"
	if c.t != nil {
		footer = c.t.Dim.Render(footer)
	}
	lines = append(lines, footer)
	if len(lines) > maxH {
		lines = lines[:maxH]
	}
	for i := range lines {
		lines[i] = render.PadRight(lines[i], maxW)
	}
	return strings.Join(lines, "\n")
}
