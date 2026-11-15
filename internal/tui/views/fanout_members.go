package views

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend/linux"
	"jumpr/internal/group"
	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
	"jumpr/internal/tui/keymap"
)

// membersHostWidth is the HOST column of the member list.
const membersHostWidth = 24

// FanoutMembers lists the hosts of one group, one row each with the first line
// of its output. Enter on a host opens its overview; esc goes back.
type FanoutMembers struct {
	env    *tui.Env
	title  string
	g      group.Group
	keys   *keymap.Map
	glyphs render.GlyphSet
	cursor int
	h      int
}

var _ tui.View = (*FanoutMembers)(nil)

// NewFanoutMembers builds the member list of g under title.
func NewFanoutMembers(env *tui.Env, title string, g group.Group) *FanoutMembers {
	keys := keymap.Default()
	if env != nil && env.Keys != nil {
		keys = env.Keys
	}
	glyphs, _ := render.Glyphs("unicode")
	if env != nil && env.Glyphs.Name != "" {
		glyphs = env.Glyphs
	}
	hosts := append([]resource.HostID(nil), g.Hosts...)
	return &FanoutMembers{
		env:    env,
		title:  title,
		g:      group.Group{Key: g.Key, Output: g.Output, RC: g.RC, Hosts: hosts},
		keys:   keys,
		glyphs: glyphs,
	}
}

// Init does nothing: the group is already known.
func (m *FanoutMembers) Init() tea.Cmd { return nil }

// Update scrolls the list, opens a host overview or goes back.
func (m *FanoutMembers) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	key := k.String()
	switch {
	case m.keys.Back.Matches(key):
		return m, tui.Pop()
	case m.keys.Up.Matches(key):
		m.moveCursor(-1)
	case m.keys.Down.Matches(key):
		m.moveCursor(1)
	case m.keys.PageUp.Matches(key):
		m.moveCursor(-max(1, m.pageSize()))
	case m.keys.PageDown.Matches(key):
		m.moveCursor(max(1, m.pageSize()))
	case m.keys.Top.Matches(key):
		m.cursor = 0
	case m.keys.Bottom.Matches(key):
		m.cursor = max(0, len(m.g.Hosts)-1)
	case m.keys.Open.Matches(key):
		if m.cursor >= 0 && m.cursor < len(m.g.Hosts) {
			host := m.g.Hosts[m.cursor]
			return m, tui.OpenQuery(resource.Query{Kind: linux.KindHostOverview, Hosts: []resource.HostID{host}})
		}
	}
	return m, nil
}

// pageSize is the number of list rows a page key scrolls.
func (m *FanoutMembers) pageSize() int {
	if m.h <= 3 {
		return 1
	}
	return m.h - 3
}

// moveCursor moves the selection and keeps it inside the list.
func (m *FanoutMembers) moveCursor(delta int) {
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if last := len(m.g.Hosts) - 1; m.cursor > last {
		m.cursor = max(0, last)
	}
}

// Render draws the banner, the header and the visible host rows, exactly w × h.
func (m *FanoutMembers) Render(w, h int) string {
	m.h = h
	if w <= 0 || h <= 0 {
		return ""
	}
	lines := []string{m.banner(), m.header(w)}
	if visible := h - len(lines); visible > 0 {
		start := 0
		if m.cursor >= visible {
			start = m.cursor - visible + 1
		}
		out := firstOutputLine(m.g.Output)
		for i := start; i < start+visible && i < len(m.g.Hosts); i++ {
			line := render.PadRight(render.Truncate(string(m.g.Hosts[i]), membersHostWidth), membersHostWidth) +
				" " + out
			line = render.PadRight(render.Truncate(line, w), w)
			if m.env != nil && m.env.Theme != nil {
				if i == m.cursor {
					line = m.env.Theme.Selected.Render(line)
				} else {
					line = m.env.Theme.Base.Render(line)
				}
			}
			lines = append(lines, line)
		}
	}
	return render.FitWith(m.glyphs, strings.Join(lines, "\n"), w, h)
}

// banner names the group: how many hosts share it, their exit code and the
// representative output's first line.
func (m *FanoutMembers) banner() string {
	text := fmt.Sprintf("group of %d · rc %d · %s", len(m.g.Hosts), m.g.RC, firstOutputLine(m.g.Output))
	if m.env != nil && m.env.Theme != nil {
		text = m.env.Theme.Dim.Render(text)
	}
	return text
}

// header is the column title row.
func (m *FanoutMembers) header(w int) string {
	line := render.PadRight("HOST", membersHostWidth) + " OUTPUT"
	line = render.Truncate(line, w)
	if m.env != nil && m.env.Theme != nil {
		line = m.env.Theme.Header.Render(line)
	}
	return render.PadRight(line, w)
}

// Crumb is the run's title plus the group size.
func (m *FanoutMembers) Crumb() string {
	return fmt.Sprintf("%s · %d hosts", m.title, len(m.g.Hosts))
}

// Hints are the keys the member list answers.
func (m *FanoutMembers) Hints() []tui.KeyHint {
	return []tui.KeyHint{
		{Key: firstKey(m.keys.Open), Text: "host"},
		{Key: firstKey(m.keys.Back), Text: "back"},
	}
}

// Subject is the group's hosts.
func (m *FanoutMembers) Subject() tui.Subject {
	return tui.Subject{Hosts: append([]resource.HostID(nil), m.g.Hosts...)}
}

// Capturing is always false: the list has no input of its own.
func (m *FanoutMembers) Capturing() bool { return false }

// CopyLines returns one line per host with its output.
func (m *FanoutMembers) CopyLines() []string {
	out := firstOutputLine(m.g.Output)
	lines := make([]string, 0, len(m.g.Hosts))
	for _, h := range m.g.Hosts {
		lines = append(lines, string(h)+"  "+out)
	}
	return lines
}
