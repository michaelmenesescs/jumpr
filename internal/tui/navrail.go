package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/render"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
)

// The tool rail is the always-visible map of what jumpr can do: one section
// per tool (fleet, systemd, Kubernetes, network, service map, checks, logs)
// and under it the views of that tool. Every entry is a `:` command line, so
// the rail never knows more than the command bar does; it only makes the
// commands visible. `tab` moves the keyboard to the rail and back.
const (
	// railWidth is the rail's width in cells, separator included.
	railWidth = 19
	// railMinWidth is the narrowest pane interior that still shows the rail.
	railMinWidth = 72
	// railSeparator is the trailing column that divides rail from view.
	railSeparator = " │"
)

// asciiOnly reports whether the session draws with the ASCII glyph set, in
// which case the shell's own lines and markers must be ASCII too.
func asciiOnly(env *Env) bool {
	return env != nil && env.Glyphs.Name == "ascii"
}

// shellGlyphs returns the column separator and the active-entry marker.
func shellGlyphs(env *Env) (bar, mark string) {
	if asciiOnly(env) {
		return "|", "> "
	}
	return "│", "▸ "
}

// railEntry is one line of the rail. A header has no Line and cannot be
// selected. Needs names the kind or command that must be registered for the
// entry to work; an entry whose need is missing is drawn faint.
type railEntry struct {
	Title  string
	Line   string
	Needs  string
	Header bool
	// Ask is true for a command that needs arguments: the entry opens the
	// command bar with Line typed in instead of running it.
	Ask bool
}

// railEntries is the tool map. The terms are the ones each system uses for
// itself: a host is a machine reached over SSH, a unit is a systemd unit, a
// node is a Kubernetes node, a service under Kubernetes is a Service object.
func railEntries() []railEntry {
	return []railEntry{
		{Title: "FLEET", Header: true},
		{Title: "hosts", Line: "host", Needs: "host"},
		{Title: "targets", Line: "targets", Needs: "targets"},
		{Title: "SYSTEMD", Header: true},
		{Title: "units", Line: "unit", Needs: "unit"},
		{Title: "timers", Line: "timer", Needs: "timer"},
		{Title: "journal", Line: "journal", Needs: "journal"},
		{Title: "processes", Line: "process", Needs: "process"},
		{Title: "KUBERNETES", Header: true},
		{Title: "pods", Line: "pod", Needs: "pod"},
		{Title: "deployments", Line: "deployment", Needs: "deployment"},
		{Title: "services", Line: "service", Needs: "service"},
		{Title: "ingresses", Line: "ingress", Needs: "ingress"},
		{Title: "nodes", Line: "node", Needs: "node"},
		{Title: "events", Line: "event", Needs: "event"},
		{Title: "NETWORK", Header: true},
		{Title: "sockets", Line: "socket", Needs: "socket"},
		{Title: "interfaces", Line: "interface", Needs: "interface"},
		{Title: "routes", Line: "route", Needs: "route"},
		{Title: "neighbours", Line: "neigh", Needs: "neigh"},
		{Title: "firewall", Line: "probe firewall", Needs: "probe"},
		{Title: "connections", Line: "probe connections", Needs: "probe"},
		{Title: "reach", Line: "reach ", Needs: "reach", Ask: true},
		{Title: "trace path", Line: "trace ", Needs: "trace", Ask: true},
		{Title: "endpoints", Line: "endpoints", Needs: "endpoints"},
		{Title: "net policies", Line: "networkpolicy", Needs: "networkpolicy"},
		{Title: "SERVICE MAP", Header: true},
		{Title: "map", Line: "map", Needs: "map"},
		{Title: "CHECKS", Header: true},
		{Title: "salt dry run", Line: "salt state", Needs: "salt"},
		{Title: "chef drift", Line: "chef drift", Needs: "chef"},
		{Title: "probes", Line: "probe", Needs: "probe"},
		{Title: "salt minions", Line: "salt-minion", Needs: "salt-minion"},
		{Title: "salt states", Line: "salt-sls", Needs: "salt-sls"},
		{Title: "chef nodes", Line: "chef-node", Needs: "chef-node"},
		{Title: "LOGS", Header: true},
		{Title: "kernel", Line: "kernel", Needs: "kernel"},
	}
}

// navRail is the rail's state: the entries, the cursor, the entry that was
// opened last and whether the rail owns the keyboard.
type navRail struct {
	env     *Env
	entries []railEntry
	cursor  int
	active  int
	focused bool
	top     int
}

func newNavRail(env *Env) *navRail {
	n := &navRail{env: env, entries: railEntries(), active: 1, cursor: 1}
	return n
}

// available reports whether the entry's kind or command is registered. The
// command registry is not reachable from Env, so an entry that names neither
// a registered kind nor one of the built-in commands counts as unavailable.
func (n *navRail) available(e railEntry) bool {
	if e.Header {
		return false
	}
	if n.env == nil {
		return true
	}
	if n.env.RailCommands != nil && n.env.RailCommands(e.Needs) {
		return true
	}
	if n.env.Registry == nil {
		return false
	}
	_, _, ok := n.env.Registry.Lookup(e.Needs)
	return ok
}

// move steps the cursor over headers, staying inside the list.
func (n *navRail) move(delta int) {
	i := n.cursor
	for {
		i += delta
		if i < 0 || i >= len(n.entries) {
			return
		}
		if !n.entries[i].Header {
			n.cursor = i
			return
		}
	}
}

// handleKey handles one key while the rail has focus. It consumes every key,
// so a stray letter cannot reach the view beside it.
func (n *navRail) handleKey(k tea.KeyMsg) (cmd tea.Cmd, open bool) {
	key := k.String()
	keys := keymap.Default()
	if n.env != nil && n.env.Keys != nil {
		keys = n.env.Keys
	}
	switch {
	case key == "tab" || key == "esc" || key == "right" || key == "l":
		n.focused = false
	case keys.Up.Matches(key):
		n.move(-1)
	case keys.Down.Matches(key):
		n.move(1)
	case keys.Top.Matches(key):
		n.cursor = 0
		n.move(1)
	case keys.Bottom.Matches(key):
		n.cursor = len(n.entries)
		n.move(-1)
	case keys.Open.Matches(key):
		e := n.entries[n.cursor]
		if !n.available(e) {
			return Flash(FlashWarn, "%s is not available: nothing registered for %q", e.Title, e.Needs), false
		}
		n.focused = false
		line := e.Line
		if e.Ask {
			return func() tea.Msg { return OpenCommandBarMsg{Prefill: line} }, false
		}
		n.active = n.cursor
		return func() tea.Msg { return RunCommandMsg{Line: line} }, true
	}
	return nil, false
}

// hints are the rail's own key hints, shown while it has focus.
func (n *navRail) hints() []KeyHint {
	return []KeyHint{
		{Key: "j/k", Text: "move"},
		{Key: "enter", Text: "open tool"},
		{Key: "tab", Text: "back to view"},
		{Key: ":", Text: "cmd"},
		{Key: "?", Text: "help"},
	}
}

// Render draws the rail as exactly h lines of exactly w cells.
func (n *navRail) Render(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	content := w - render.Width(railSeparator)
	if content < 1 {
		content = 1
	}
	var t *theme.Theme
	if n.env != nil {
		t = n.env.Theme
	}
	// Keep the cursor inside the window when the list is taller than the pane.
	if n.cursor < n.top {
		n.top = n.cursor
	}
	if n.cursor >= n.top+h {
		n.top = n.cursor - h + 1
	}
	if n.top > 0 && n.entries[n.top-1].Header && n.cursor < n.top+h-1 {
		n.top--
	}
	if len(n.entries) <= h {
		n.top = 0
	}
	lines := make([]string, 0, h)
	for i := n.top; i < len(n.entries) && len(lines) < h; i++ {
		e := n.entries[i]
		var text string
		switch {
		case e.Header:
			text = render.PadRight(render.Truncate(e.Title, content), content)
			if t != nil {
				text = t.Header.Render(text)
			}
		default:
			mark := "  "
			if i == n.active {
				_, mark = shellGlyphs(n.env)
			}
			text = render.PadRight(render.Truncate(mark+e.Title, content), content)
			if t != nil {
				switch {
				case i == n.cursor && n.focused:
					text = t.Selected.Render(text)
				case i == n.active:
					text = t.Accent.Render(text)
				case !n.available(e):
					text = t.Faint.Render(text)
				}
			}
		}
		lines = append(lines, text)
	}
	body := render.Fit(strings.Join(lines, "\n"), content, h)
	rows := strings.Split(body, "\n")
	bar, _ := shellGlyphs(n.env)
	sep := " " + bar
	if t != nil {
		if n.focused {
			sep = t.BorderFocused.Render(sep)
		} else {
			sep = t.Border.Render(sep)
		}
	}
	for i, row := range rows {
		rows[i] = row + sep
	}
	return strings.Join(rows, "\n")
}
