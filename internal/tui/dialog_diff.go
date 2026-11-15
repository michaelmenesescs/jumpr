package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/render"
	"jumpr/internal/tui/theme"
)

// diffFooter is the dialog's last line.
const diffFooter = "up/down scroll  esc close"

// diffDialog shows a unified diff in a scrollable box: added lines green,
// removed lines red, hunk headers accented.
type diffDialog struct {
	t     *theme.Theme
	title string
	lines []string
	top   int
	h     int
}

var _ Dialog = (*diffDialog)(nil)

// NewDiffDialog builds a scrollable diff dialog over diff, the output of
// group.UnifiedDiff. An empty diff still renders (as no differences) so a
// caller that skips the check cannot draw an empty box.
func NewDiffDialog(t *theme.Theme, title, diff string) Dialog {
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(diff, "\r\n", "\n"), "\n"), "\n")
	if len(lines) == 1 && strings.TrimSpace(lines[0]) == "" {
		lines = []string{"no differences"}
	}
	return &diffDialog{t: t, title: title, lines: lines}
}

// Title is the dialog's border title.
func (d *diffDialog) Title() string { return d.title }

// Update scrolls the diff or closes the dialog.
func (d *diffDialog) Update(msg tea.Msg) (Dialog, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return d, nil
	}
	switch k.String() {
	case "esc", "q":
		return d, CloseDialog()
	case "up", "k":
		d.top--
	case "down", "j":
		d.top++
	case "pgup":
		d.top -= max(1, d.page())
	case "pgdown":
		d.top += max(1, d.page())
	case "home", "g":
		d.top = 0
	case "end", "G":
		d.top = len(d.lines) - 1
	}
	d.clamp()
	return d, nil
}

// page is the visible line count, one line of which is the footer.
func (d *diffDialog) page() int {
	rows := d.h - 1
	if rows < 1 {
		return 1
	}
	return rows
}

// clamp keeps the scroll position inside the diff.
func (d *diffDialog) clamp() {
	if d.top < 0 {
		d.top = 0
	}
	if last := len(d.lines) - d.page(); d.top > last {
		d.top = max(0, last)
	}
	if d.top > len(d.lines)-1 {
		d.top = max(0, len(d.lines)-1)
	}
}

// View renders the visible diff lines and the footer, exactly maxW cells wide.
func (d *diffDialog) View(maxW, maxH int) string {
	if maxW < 1 || maxH < 1 {
		return ""
	}
	d.h = maxH
	d.clamp()
	rows := maxH - 1
	if rows < 1 {
		rows = 1
	}
	lines := make([]string, 0, rows+1)
	for i := 0; i < rows; i++ {
		idx := d.top + i
		if idx >= len(d.lines) {
			lines = append(lines, "")
			continue
		}
		lines = append(lines, render.PadRight(render.Truncate(d.styleLine(d.lines[idx]), maxW), maxW))
	}
	footer := diffFooter
	if d.t != nil {
		footer = d.t.Dim.Render(footer)
	}
	lines = append(lines, render.PadRight(footer, maxW))
	return strings.Join(lines, "\n")
}

// styleLine colours one diff line by its role.
func (d *diffDialog) styleLine(line string) string {
	if d.t == nil {
		return line
	}
	switch {
	case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		return d.t.Bold.Render(line)
	case strings.HasPrefix(line, "+"):
		return d.t.OK.Render(line)
	case strings.HasPrefix(line, "-"):
		return d.t.Crit.Render(line)
	case strings.HasPrefix(line, "@@"):
		return d.t.Accent.Render(line)
	}
	return d.t.Base.Render(line)
}
