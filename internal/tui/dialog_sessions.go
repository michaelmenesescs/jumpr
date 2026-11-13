package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/render"
	"jumper/internal/tui/theme"
)

// sessionsDialog is the session list of task 082: every session with its window
// count, whether a client is attached to it and when it was last active.
// Dormant sessions — saved to disk, revived on first attach — are marked
// "(saved)". enter switches this client, n creates a session, x kills one.
type sessionsDialog struct {
	t      *theme.Theme
	items  []SessionSummary
	sel    int
	offset int
	pick   func(int) tea.Cmd // enter
	onNew  func() tea.Cmd    // n
	onKill func(int) tea.Cmd // x
	now    func() time.Time
}

var _ Dialog = (*sessionsDialog)(nil)

// NewSessionsDialog returns the list over items. pick runs with the highlighted
// row on enter, onNew on n and onKill on x. A nil pick, onNew or onKill only
// closes the dialog, which is what a read-only caller wants.
func NewSessionsDialog(t *theme.Theme, items []SessionSummary, pick func(int) tea.Cmd, onNew func() tea.Cmd, onKill func(int) tea.Cmd) Dialog {
	return &sessionsDialog{t: t, items: items, pick: pick, onNew: onNew, onKill: onKill, now: time.Now}
}

// Title is the dialog's border title.
func (d *sessionsDialog) Title() string { return "sessions" }

// Update moves the selection and acts on enter, n and x.
func (d *sessionsDialog) Update(msg tea.Msg) (Dialog, tea.Cmd) {
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
		return d, tea.Batch(CloseDialog(), d.pick(d.sel))
	case "n":
		if d.onNew == nil {
			return d, nil
		}
		// The prompt replaces the list: one modal at a time.
		return d, tea.Batch(CloseDialog(), d.onNew())
	case "x":
		if d.onKill == nil || d.sel < 0 || d.sel >= len(d.items) {
			return d, nil
		}
		return d, tea.Batch(CloseDialog(), d.onKill(d.sel))
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
func (d *sessionsDialog) View(maxW, maxH int) string {
	if maxW < 1 || maxH < 1 {
		return ""
	}
	now := d.now()
	if len(d.items) == 0 {
		rows := make([]string, 0, maxH)
		rows = append(rows, render.Truncate("no sessions", maxW))
		return d.finish(rows, maxW, maxH)
	}
	rows := maxH - 1
	if rows < 1 {
		rows = 1
	}
	if d.sel < d.offset {
		d.offset = d.sel
	}
	if d.sel >= d.offset+rows {
		d.offset = d.sel - rows + 1
	}
	if d.offset < 0 {
		d.offset = 0
	}
	lines := make([]string, 0, rows)
	for i := 0; i < rows; i++ {
		idx := d.offset + i
		if idx >= len(d.items) {
			lines = append(lines, "")
			continue
		}
		text := sessionLine(d.items[idx], now)
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
	return d.finish(lines, maxW, maxH)
}

// finish appends the footer and pads every row to maxW.
func (d *sessionsDialog) finish(lines []string, maxW, maxH int) string {
	for len(lines) < maxH-1 {
		lines = append(lines, "")
	}
	footer := "enter switch  n new  x kill  esc close"
	if d.t != nil {
		footer = d.t.Dim.Render(footer)
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

// sessionLine renders one session: its name, its window count, whether a client
// is on it or it is dormant, and how long ago it was last active.
func sessionLine(s SessionSummary, now time.Time) string {
	unit := "windows"
	if s.Windows == 1 {
		unit = "window"
	}
	line := fmt.Sprintf("%s  %d %s", s.Name, s.Windows, unit)
	switch {
	case s.Dormant:
		line += "  (saved)"
	case s.Attached:
		line += "  (attached)"
	}
	if !s.Activity.IsZero() {
		if age := sessionAge(s.Activity, now); age != "" {
			line += "  " + age
		}
	}
	return line
}

// sessionAge is how long ago t was, in the compact form the list uses:
// "just now", "4m", "2h", "3d".
func sessionAge(t, now time.Time) string {
	if t.IsZero() || now.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// endSessionDialog confirms ending a session on the session server, where
// quitting is destructive and detaching is not (task 082 requirement 2): y ends
// it, d detaches instead, anything else cancels.
type endSessionDialog struct {
	t      *theme.Theme
	name   string
	yes    tea.Cmd
	detach tea.Cmd
}

var _ Dialog = (*endSessionDialog)(nil)

// NewEndSessionDialog asks whether to end session name. yes runs on y, Y or
// enter, detach on d, and n, N, q and esc just close the dialog.
func NewEndSessionDialog(t *theme.Theme, name string, yes, detach tea.Cmd) Dialog {
	return &endSessionDialog{t: t, name: name, yes: yes, detach: detach}
}

// Title is the dialog's border title.
func (d *endSessionDialog) Title() string { return "end session" }

// Update answers the question.
func (d *endSessionDialog) Update(msg tea.Msg) (Dialog, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return d, nil
	}
	switch k.String() {
	case "y", "Y", "enter":
		if d.yes == nil {
			return d, CloseDialog()
		}
		return d, tea.Batch(CloseDialog(), d.yes)
	case "d":
		if d.detach == nil {
			return d, CloseDialog()
		}
		return d, tea.Batch(CloseDialog(), d.detach)
	case "n", "N", "q", "esc":
		return d, CloseDialog()
	}
	return d, nil
}

// View renders the question and the footer, exactly maxW cells wide.
func (d *endSessionDialog) View(maxW, maxH int) string {
	if maxW < 1 || maxH < 1 {
		return ""
	}
	lines := make([]string, 0, 5)
	question := fmt.Sprintf("end session %q? shells and follows will stop (y/N)", d.name)
	for _, l := range strings.Split(question, "\n") {
		lines = append(lines, render.Truncate(l, maxW))
	}
	hint := "detaching is the non-destructive default: d = detach instead"
	if d.t != nil {
		hint = d.t.Dim.Render(hint)
	}
	lines = append(lines, render.Truncate(hint, maxW))
	for len(lines) < maxH-1 {
		lines = append(lines, "")
	}
	footer := "y/N"
	if d.t != nil {
		footer = d.t.Dim.Render(footer)
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
