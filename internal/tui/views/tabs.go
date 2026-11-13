package views

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/render"
	"jumper/internal/tui"
	"jumper/internal/tui/theme"
)

// Tab is one entry of a tab strip: the number key that selects it, the label
// shown in the strip and the view it shows.
type Tab struct {
	Key   string
	Title string
	View  tui.View
}

// Tabs is the reusable tab container: a strip of `1 status  2 show  3 cat` on
// top and one tui.View below. `tab`/`shift+tab` cycle and the number keys jump.
// It forwards Capturing, Hints, CopyLines, Crumb and Subject of the active tab,
// and initialises a tab lazily the first time it is shown, so an expensive tab
// (the journal) only loads when it is actually visited.
type Tabs struct {
	env    *tui.Env
	tabs   []Tab
	inited []bool
	active int
	w, h   int
}

var _ tui.View = (*Tabs)(nil)

// NewTabs builds a tab container over tabs. It does not touch the views.
func NewTabs(env *tui.Env, tabs []Tab) *Tabs {
	return &Tabs{
		env:    env,
		tabs:   append([]Tab(nil), tabs...),
		inited: make([]bool, len(tabs)),
	}
}

// Len is the number of tabs.
func (t *Tabs) Len() int { return len(t.tabs) }

// Active is the index of the shown tab.
func (t *Tabs) Active() int { return t.active }

// Visited reports whether tab i has been shown (and therefore initialised).
func (t *Tabs) Visited(i int) bool {
	return i >= 0 && i < len(t.inited) && t.inited[i]
}

// SetView replaces the view of tab i, keeping its visited state.
func (t *Tabs) SetView(i int, v tui.View) {
	if i < 0 || i >= len(t.tabs) {
		return
	}
	t.tabs[i].View = v
}

// Init initialises the shown tab. Unit detail builds its tab views after the
// batch arrives, so the first tab's Init is usually nil.
func (t *Tabs) Init() tea.Cmd { return t.initActive() }

// initActive initialises the active tab the first time it is shown.
func (t *Tabs) initActive() tea.Cmd {
	if t.active < 0 || t.active >= len(t.tabs) || t.inited[t.active] {
		return nil
	}
	t.inited[t.active] = true
	if t.tabs[t.active].View == nil {
		return nil
	}
	return t.tabs[t.active].View.Init()
}

// Select shows tab i, wrapping around, and initialises it on first visit.
func (t *Tabs) Select(i int) tea.Cmd {
	n := len(t.tabs)
	if n == 0 {
		return nil
	}
	i = ((i % n) + n) % n
	if i == t.active {
		return t.initActive()
	}
	t.active = i
	return t.initActive()
}

// Update routes keys. While the active tab captures the keyboard every key is
// the tab's; otherwise the strip's own keys are handled first.
func (t *Tabs) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	if len(t.tabs) == 0 || t.active >= len(t.tabs) || t.tabs[t.active].View == nil {
		return t, nil
	}
	if k, ok := msg.(tea.KeyMsg); ok && !t.Capturing() {
		if cmd, handled := t.key(k); handled {
			return t, cmd
		}
	}
	v, cmd := t.tabs[t.active].View.Update(msg)
	if v != nil {
		t.tabs[t.active].View = v
	}
	return t, cmd
}

// key handles the strip's keys: tab, shift+tab and the number keys.
func (t *Tabs) key(m tea.KeyMsg) (tea.Cmd, bool) {
	switch key := m.String(); {
	case key == "tab":
		return t.Select(t.active + 1), true
	case key == "shift+tab":
		return t.Select(t.active - 1), true
	case len(key) == 1 && key[0] >= '1' && key[0] <= '9':
		i := int(key[0] - '1')
		if i < len(t.tabs) {
			return t.Select(i), true
		}
	}
	return nil, false
}

// theme returns the palette, falling back to the default one.
func (t *Tabs) theme() *theme.Theme {
	if t.env != nil && t.env.Theme != nil {
		return t.env.Theme
	}
	return theme.Dark()
}

// strip renders the tab bar: the active tab on its selected background, the
// rest dim.
func (t *Tabs) strip(w int) string {
	th := t.theme()
	parts := make([]string, len(t.tabs))
	for i, tab := range t.tabs {
		label := tab.Key + " " + tab.Title
		if i == t.active {
			parts[i] = th.Selected.Render(" " + label + " ")
			continue
		}
		parts[i] = th.Dim.Render(label)
	}
	return render.Truncate(strings.Join(parts, "  "), w)
}

// Render returns exactly h lines of exactly w cells: the strip and the active
// tab's frame.
func (t *Tabs) Render(w, h int) string {
	t.w, t.h = w, h
	if w <= 0 || h <= 0 || len(t.tabs) == 0 {
		return ""
	}
	lines := []string{t.strip(w)}
	if h > 1 && t.tabs[t.active].View != nil {
		lines = append(lines, t.tabs[t.active].View.Render(w, h-1))
	}
	return render.Fit(strings.Join(lines, "\n"), w, h)
}

// Crumb is the active tab's breadcrumb.
func (t *Tabs) Crumb() string {
	if v := t.activeView(); v != nil {
		return v.Crumb()
	}
	return ""
}

// Hints are the active tab's hints.
func (t *Tabs) Hints() []tui.KeyHint {
	if v := t.activeView(); v != nil {
		return v.Hints()
	}
	return nil
}

// Subject is the active tab's subject.
func (t *Tabs) Subject() tui.Subject {
	if v := t.activeView(); v != nil {
		return v.Subject()
	}
	return tui.Subject{}
}

// Capturing is true while the active tab owns the keyboard.
func (t *Tabs) Capturing() bool {
	if v := t.activeView(); v != nil {
		return v.Capturing()
	}
	return false
}

// CopyLines are the active tab's plain-text lines.
func (t *Tabs) CopyLines() []string {
	if v := t.activeView(); v != nil {
		return v.CopyLines()
	}
	return nil
}

// Close closes every tab that owns a resource (the journal's streams).
func (t *Tabs) Close() error {
	for _, tab := range t.tabs {
		if c, ok := tab.View.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	}
	return nil
}

// activeView is the shown tab's view, or nil.
func (t *Tabs) activeView() tui.View {
	if t.active < 0 || t.active >= len(t.tabs) {
		return nil
	}
	return t.tabs[t.active].View
}
