package mux

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/render"
)

// stub is a Content that renders its pane id and records everything it is sent.
type stub struct {
	id       PaneID
	title    string
	msgs     []tea.Msg
	keys     []string
	focus    []bool
	views    [][2]int
	initRuns int
	closed   bool
}

var _ Content = (*stub)(nil)

func (s *stub) Init() tea.Cmd { s.initRuns++; return nil }

func (s *stub) Update(msg tea.Msg) (Content, tea.Cmd) {
	s.msgs = append(s.msgs, msg)
	if k, ok := msg.(tea.KeyMsg); ok {
		s.keys = append(s.keys, k.String())
	}
	return s, nil
}

func (s *stub) View(w, h int) string {
	if w <= 0 || h <= 0 {
		panic("Content.View called with a non-positive size")
	}
	s.views = append(s.views, [2]int{w, h})
	return render.Fit("pane"+itoa(int(s.id)), w, h)
}

func (s *stub) Title() string { return s.title }

func (s *stub) SetFocus(focused bool) { s.focus = append(s.focus, focused) }

func (s *stub) RawKeys() bool { return false }

func (s *stub) CopyLines() []string { return nil }

func (s *stub) State() ContentState { return ContentState{Type: "view"} }

func (s *stub) Close() error { s.closed = true; return nil }

func (s *stub) sawMsg(msg tea.Msg) bool {
	for _, m := range s.msgs {
		if m == msg {
			return true
		}
	}
	return false
}

func (s *stub) lastFocus() (bool, bool) {
	if len(s.focus) == 0 {
		return false, false
	}
	return s.focus[len(s.focus)-1], true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// factory builds stubs and keeps them for assertions.
type factory struct {
	made map[PaneID]*stub
}

func (f *factory) make(id PaneID, st ContentState) (Content, tea.Cmd) {
	s := &stub{id: id, title: "t" + itoa(int(id))}
	f.made[id] = s
	return s, nil
}

// newTest builds a model with one window, a content for its first pane and one
// split per element of splits ("h" or "v").
func newModelTest(t *testing.T, w, h int, splits ...string) (*Model, *factory) {
	t.Helper()
	ses := NewSession("main")
	window, first := ses.NewWindow("main")
	f := &factory{made: map[PaneID]*stub{}}
	m := NewModel(ses, f.make, "ctrl+a", Styles{})
	m.SetSize(w, h)
	m.create(first, ContentState{}, true)
	for _, s := range splits {
		op := OpSplitH
		if s == "v" {
			op = OpSplitV
		}
		if _, cmd := m.Update(OpMsg{Op: op}); cmd != nil {
			t.Fatalf("split %q returned a command", s)
		}
	}
	if len(window.Layout.Panes()) != 1+len(splits) {
		t.Fatalf("got %d panes, want %d", len(window.Layout.Panes()), 1+len(splits))
	}
	return m, f
}

func runeKey(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

// checkSize asserts that s is exactly h lines of exactly w cells.
func checkSize(t *testing.T, s string, w, h int) {
	t.Helper()
	lines := strings.Split(s, "\n")
	if len(lines) != h {
		t.Fatalf("got %d lines, want %d", len(lines), h)
	}
	for i, line := range lines {
		if got := render.Width(line); got != w {
			t.Fatalf("line %d is %d cells, want %d", i, got, w)
		}
	}
}

func TestModelViewExactSize(t *testing.T) {
	layouts := map[int][]string{
		1: nil,
		2: {"h"},
		3: {"h", "v"},
		4: {"h", "v", "h"},
	}
	sizes := [][2]int{{80, 24}, {131, 37}}
	for n := 1; n <= 4; n++ {
		for _, size := range sizes {
			m, f := newModelTest(t, size[0], size[1], layouts[n]...)
			view := m.View()
			checkSize(t, view, size[0], size[1])
			if len(f.made) != n {
				t.Fatalf("%d panes: %d contents", n, len(f.made))
			}
			for id, c := range f.made {
				if len(c.views) == 0 {
					t.Fatalf("pane %d was never rendered", id)
				}
				if n > 1 {
					last := c.views[len(c.views)-1]
					// Borders take two cells off both axes.
					if last[0] <= 0 || last[1] <= 0 {
						t.Fatalf("pane %d rendered at %v", id, last)
					}
				}
			}
		}
	}
}

func TestModelBordersShowTitles(t *testing.T) {
	m, f := newModelTest(t, 80, 24, "h")
	view := m.View()
	for _, want := range []string{"1:t1", "2:t2", "╭", "╮", "╰", "╯"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view does not contain %q:\n%s", want, view)
		}
	}

	// A title wider than the top edge is cut, never overflowing the pane.
	m, f = newModelTest(t, 26, 10, "h")
	f.made[2].title = strings.Repeat("wide", 20)
	checkSize(t, m.View(), 26, 10)
}

func TestModelKeysGoToFocused(t *testing.T) {
	m, f := newModelTest(t, 80, 24, "h")
	focused, c := m.Focused()
	if c == nil || focused != 2 {
		t.Fatalf("focused pane is %d, want 2", focused)
	}
	m.Update(runeKey('a'))
	if len(f.made[1].keys) != 0 {
		t.Fatalf("unfocused pane received %v", f.made[1].keys)
	}
	if got := f.made[2].keys; len(got) != 1 || got[0] != "a" {
		t.Fatalf("focused pane keys = %v", got)
	}

	m.Update(OpMsg{Op: OpFocus, Arg: "last"})
	if id, _ := m.Focused(); id != 1 {
		t.Fatalf("focus did not move, pane %d", id)
	}
	m.Update(runeKey('b'))
	if got := f.made[1].keys; len(got) != 1 || got[0] != "b" {
		t.Fatalf("pane 1 keys = %v", got)
	}
	if len(f.made[2].keys) != 1 {
		t.Fatalf("pane 2 received more keys: %v", f.made[2].keys)
	}
}

func TestModelPrefixSplitCreatesPane(t *testing.T) {
	m, f := newModelTest(t, 80, 24)
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlA}); cmd != nil {
		t.Fatal("prefix key returned a command")
	}
	if !m.PrefixArmed() {
		t.Fatal("prefix not armed")
	}
	if len(m.Session().Windows[0].Layout.Panes()) != 1 {
		t.Fatal("prefix key created a pane")
	}
	if _, cmd := m.Update(runeKey('%')); cmd != nil {
		t.Fatal("split returned a command")
	}
	panes := m.Session().Windows[0].Layout.Panes()
	if len(panes) != 2 {
		t.Fatalf("got %d panes, want 2", len(panes))
	}
	id, c := m.Focused()
	if id != panes[1] || c == nil {
		t.Fatalf("new pane %d is not focused (focus %d)", panes[1], id)
	}
	if f.made[panes[1]].initRuns != 1 {
		t.Fatal("Init was not called on the new content")
	}
}

func TestModelPaneMsgRoutedToInactiveWindow(t *testing.T) {
	ses := NewSession("main")
	_, first := ses.NewWindow("a")
	f := &factory{made: map[PaneID]*stub{}}
	m := NewModel(ses, f.make, "ctrl+a", Styles{})
	m.SetSize(80, 24)
	m.create(first, ContentState{}, true)
	_, active := ses.NewWindow("b")
	m.create(active, ContentState{}, true)

	m.Update(PaneMsg{Pane: first, Msg: runeKey('x')})
	if got := f.made[first].keys; len(got) != 1 || got[0] != "x" {
		t.Fatalf("inactive pane keys = %v", got)
	}
	if len(f.made[active].keys) != 0 {
		t.Fatalf("active pane received the key: %v", f.made[active].keys)
	}
	// An unknown pane is dropped, not delivered anywhere.
	if root, cmd := m.Update(PaneMsg{Pane: 99, Msg: runeKey('y')}); root != nil || cmd != nil {
		t.Fatalf("unknown pane returned (%v, %v)", root, cmd)
	}
}

type rootMsg struct{ n int }

func TestModelRootMsgReturned(t *testing.T) {
	m, f := newModelTest(t, 80, 24)
	m.SetRootPredicate(func(msg tea.Msg) bool {
		_, ok := msg.(rootMsg)
		return ok
	})
	want := rootMsg{n: 7}
	root, cmd := m.Update(PaneMsg{Pane: 1, Msg: want})
	if root != want || cmd != nil {
		t.Fatalf("Update returned (%v, %v), want (%v, nil)", root, cmd, want)
	}
	if len(f.made[1].msgs) != 0 {
		t.Fatalf("root message reached the content: %v", f.made[1].msgs)
	}
}

func TestModelKillLastPaneQuits(t *testing.T) {
	m, f := newModelTest(t, 80, 24)
	_, cmd := m.Update(OpMsg{Op: OpKillPane})
	if cmd == nil {
		t.Fatal("killing the last pane returned no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("command is not tea.Quit: %T", cmd())
	}
	if len(m.Session().Windows) != 0 {
		t.Fatalf("session has %d windows", len(m.Session().Windows))
	}
	if !f.made[1].closed {
		t.Fatal("content was not closed")
	}
	if len(m.Contents()) != 0 {
		t.Fatalf("contents left: %v", m.Contents())
	}
}

func TestModelZoomHidesOthers(t *testing.T) {
	m, f := newModelTest(t, 80, 24, "h")
	m.Update(OpMsg{Op: OpZoom})
	if !m.Session().Windows[0].Zoomed {
		t.Fatal("window not zoomed")
	}
	view := m.View()
	checkSize(t, view, 80, 24)
	if !strings.Contains(view, "pane2") {
		t.Fatalf("focused pane not drawn:\n%s", view)
	}
	if strings.Contains(view, "pane1") {
		t.Fatalf("other pane drawn while zoomed:\n%s", view)
	}
	if len(f.made[1].views) != 0 {
		t.Fatal("unfocused pane was rendered while zoomed")
	}
	if last := f.made[1].lastFocusOrZero(); last != false {
		t.Fatal("unfocused pane believes it has focus")
	}
	tabs := m.Tabs()
	if len(tabs) != 1 || !tabs[0].Zoomed || tabs[0].Name != "main Z" {
		t.Fatalf("tabs = %+v", tabs)
	}
}

func TestModelBroadcastReachesAll(t *testing.T) {
	ses := NewSession("main")
	_, first := ses.NewWindow("a")
	f := &factory{made: map[PaneID]*stub{}}
	m := NewModel(ses, f.make, "ctrl+a", Styles{})
	m.SetSize(80, 24)
	m.create(first, ContentState{}, true)
	_, second := ses.NewWindow("b")
	m.create(second, ContentState{}, true)

	msg := rootMsg{n: 3}
	m.Update(msg)
	for _, id := range []PaneID{first, second} {
		if !f.made[id].sawMsg(msg) {
			t.Fatalf("pane %d did not get the broadcast", id)
		}
	}
}

func TestModelSendPrefixForwarded(t *testing.T) {
	m, f := newModelTest(t, 80, 24)
	prefix := tea.KeyMsg{Type: tea.KeyCtrlA}
	m.Update(prefix)
	if _, cmd := m.Update(prefix); cmd != nil {
		t.Fatal("send-prefix returned a command")
	}
	got := f.made[1].keys
	if len(got) != 1 || got[0] != "ctrl+a" {
		t.Fatalf("content keys = %v, want one ctrl+a", got)
	}
	if m.PrefixArmed() {
		t.Fatal("prefix still armed")
	}
}

// lastFocusOrZero reports the last SetFocus value, false when never called.
func (s *stub) lastFocusOrZero() bool {
	v, _ := s.lastFocus()
	return v
}
