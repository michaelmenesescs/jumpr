package mux

import (
	"sort"
	"strconv"
	"strings"

	"jumpr/internal/render"
)

// Box characters. Rounded, like tmux's default pane border.
const (
	boxTL = "╭"
	boxTR = "╮"
	boxBL = "╰"
	boxBR = "╯"
	boxH  = "─"
	boxV  = "│"
)

// View renders the active window in exactly the area given to SetSize. One
// visible pane (a single pane or a zoomed window) is drawn without a border;
// several panes each get their own box and are composed row by row.
func (m *Model) View() string {
	w, h := m.width, m.height
	if w <= 0 || h <= 0 {
		return ""
	}
	rects := m.sess.Rects(Rect{0, 0, w, h})
	if len(rects) == 0 {
		return render.Fit("", w, h)
	}
	if len(rects) == 1 {
		for id := range rects {
			c := m.panes[id]
			if c == nil {
				return render.Fit("", w, h)
			}
			return render.Fit(c.View(w, h), w, h)
		}
	}

	ids := make([]PaneID, 0, len(rects))
	for id := range rects {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := rects[ids[i]], rects[ids[j]]
		if a.X != b.X {
			return a.X < b.X
		}
		return a.Y < b.Y
	})

	focused, _ := m.Focused()
	boxes := make(map[PaneID][]string, len(ids))
	for _, id := range ids {
		boxes[id] = m.box(id, rects[id], id == focused)
	}

	rows := make([]string, h)
	for y := 0; y < h; y++ {
		var b strings.Builder
		for _, id := range ids {
			r := rects[id]
			if y < r.Y || y >= r.Y+r.H {
				continue
			}
			b.WriteString(boxes[id][y-r.Y])
		}
		rows[y] = b.String()
	}
	return render.Fit(strings.Join(rows, "\n"), w, h)
}

// box draws one pane rectangle, borders included: exactly r.H lines of exactly
// r.W cells each.
func (m *Model) box(id PaneID, r Rect, focused bool) []string {
	if r.W < 2 || r.H < 2 {
		return strings.Split(render.Fit("", r.W, r.H), "\n")
	}
	border, titleStyle := m.styles.Border, m.styles.Title
	if focused {
		border, titleStyle = m.styles.BorderFocused, m.styles.TitleFocused
	}
	if m.inSync(id) && m.styles.BorderSync.GetForeground() != nil {
		// A pane raw synchronized input goes to is drawn in the Crit colour.
		border = m.styles.BorderSync
	}

	innerW, innerH := r.W-2, r.H-2
	title := ""
	if c := m.panes[id]; c != nil {
		title = c.Title()
	}
	label := render.Truncate(" "+strconv.Itoa(int(id))+":"+title+" ", innerW)

	lines := make([]string, 0, r.H)
	lines = append(lines, border.Render(boxTL)+titleStyle.Render(label)+
		border.Render(strings.Repeat(boxH, innerW-render.Width(label)))+border.Render(boxTR))

	body := ""
	if innerW > 0 && innerH > 0 {
		if c := m.panes[id]; c != nil {
			body = render.Fit(c.View(innerW, innerH), innerW, innerH)
		}
	}
	bodyLines := strings.Split(body, "\n")
	for i := 0; i < innerH; i++ {
		line := ""
		if i < len(bodyLines) {
			line = bodyLines[i]
		}
		lines = append(lines, border.Render(boxV)+render.PadRight(line, innerW)+border.Render(boxV))
	}

	lines = append(lines, border.Render(boxBL)+border.Render(strings.Repeat(boxH, innerW))+border.Render(boxBR))
	return lines
}
