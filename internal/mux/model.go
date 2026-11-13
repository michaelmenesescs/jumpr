package mux

import (
	"strconv"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Factory creates the content of a new pane. A zero ContentState asks for the
// default pane. The returned command is usually the content's initial I/O; the
// mux calls Init itself and batches both.
type Factory func(id PaneID, st ContentState) (Content, tea.Cmd)

// Styles are the styles the mux draws panes with. BorderSync marks the panes
// raw synchronized input is going to (SPEC §11.1).
type Styles struct {
	Border        lipgloss.Style
	BorderFocused lipgloss.Style
	BorderSync    lipgloss.Style
	Title         lipgloss.Style
	TitleFocused  lipgloss.Style
}

// Tab is one entry of the window list shown by the status line.
type Tab struct {
	Index  int
	Name   string
	Active bool
	Zoomed bool
}

// resizeCells is how far one resize key moves a divider.
const resizeCells = 2

// Model owns a Session and its contents, draws the active window and routes
// messages, keys and operations.
type Model struct {
	sess    *Session
	factory Factory
	prefix  *Prefix
	styles  Styles
	isRoot  func(tea.Msg) bool
	panes   map[PaneID]Content
	// syncPanes is the raw synchronized-input fan-out set of the active
	// window; empty while the mode is off (task 064).
	syncPanes []PaneID

	width, height int
}

// NewModel returns a model over s. f may be nil only in tests that never
// create a pane.
func NewModel(s *Session, f Factory, prefixKey string, styles Styles) *Model {
	if s == nil {
		s = NewSession("")
	}
	return &Model{
		sess:    s,
		factory: f,
		prefix:  NewPrefix(prefixKey, DefaultKeyTable(), nil),
		styles:  styles,
		panes:   map[PaneID]Content{},
	}
}

// SetSize gives the model the body area. The owner derives it from
// tea.WindowSizeMsg, which the mux deliberately does not handle itself.
func (m *Model) SetSize(w, h int) {
	m.width, m.height = w, h
	m.sess.SetSize(w, h)
}

// SetRootPredicate installs the test that decides whether a message wrapped in
// a PaneMsg actually belongs to the root (the owner) instead of the pane.
func (m *Model) SetRootPredicate(f func(tea.Msg) bool) { m.isRoot = f }

// Update routes one message. root is a message the owner must handle; cmd is
// always safe to run. A nil root means the mux handled it.
func (m *Model) Update(msg tea.Msg) (root tea.Msg, cmd tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Sizes come from the owner through SetSize; never broadcast them.
		return nil, nil
	case PaneMsg:
		return m.paneMsg(msg)
	case tea.KeyMsg:
		return m.keyMsg(msg)
	case OpMsg:
		return m.applyOp(msg)
	default:
		return nil, m.broadcast(msg)
	}
}

// paneMsg delivers a pane-scoped message, or hands it back to the owner when
// the root predicate claims it.
func (m *Model) paneMsg(pm PaneMsg) (tea.Msg, tea.Cmd) {
	if m.isRoot != nil && m.isRoot(pm.Msg) {
		return pm.Msg, nil
	}
	c, ok := m.panes[pm.Pane]
	if !ok || c == nil {
		return nil, nil
	}
	m.updateContent(pm.Pane, c, pm.Msg)
	return nil, nil
}

// keyMsg feeds the prefix machine, applies an operation or forwards the key to
// the focused content. While raw synchronized input is on, the key goes to
// every terminal pane of the set instead (SPEC §11.1).
func (m *Model) keyMsg(k tea.KeyMsg) (tea.Msg, tea.Cmd) {
	op, consumed := m.prefix.Feed(k.String())
	switch {
	case op != nil && op.Op == OpSendPrefix:
		// A doubled prefix belongs to the focused content, as typed.
		return nil, m.toFocused(k)
	case op != nil:
		return m.applyOp(*op)
	case consumed:
		return nil, nil
	}
	if m.syncActive() {
		return nil, m.syncKey(k)
	}
	return nil, m.toFocused(k)
}

// SetSync turns raw synchronized input on for panes, or off when panes is
// empty. Only the panes handed in receive keys; the mode is cleared whenever
// the window changes or a pane is added or removed (task 064).
func (m *Model) SetSync(panes []PaneID) {
	if len(panes) == 0 {
		m.syncPanes = nil
		return
	}
	m.syncPanes = append([]PaneID(nil), panes...)
}

// SyncPanes is a copy of the fan-out set; empty while the mode is off.
func (m *Model) SyncPanes() []PaneID {
	return append([]PaneID(nil), m.syncPanes...)
}

// SyncOn reports whether raw synchronized input is on.
func (m *Model) SyncOn() bool { return len(m.syncPanes) > 0 }

// SyncCount is the number of panes keys are delivered to; 0 while off.
func (m *Model) SyncCount() int { return len(m.syncPanes) }

// SyncTo delivers one key press to the panes of the sync set, the way a key
// typed into a synchronized window reaches them all. It returns nil while the
// mode is off.
func (m *Model) SyncTo(k tea.KeyMsg) tea.Cmd {
	if !m.syncActive() {
		return nil
	}
	return m.syncKey(k)
}

// syncActive reports whether keys typed at the focused pane have to be
// broadcast: the mode is on and the focused pane is a live terminal. Keys that
// go to a view are never broadcast (task 064 pitfalls).
func (m *Model) syncActive() bool {
	if len(m.syncPanes) == 0 {
		return false
	}
	_, c := m.Focused()
	return c != nil && c.RawKeys()
}

// syncKey sends one key press to every connected terminal pane of the sync set.
func (m *Model) syncKey(k tea.KeyMsg) tea.Cmd {
	var cmds []tea.Cmd
	for _, id := range m.syncPanes {
		c, ok := m.panes[id]
		if !ok || c == nil || !c.RawKeys() {
			continue
		}
		if cmd := m.updateContent(id, c, k); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// clearSync switches raw synchronized input off. It is called whenever the
// window changes or a pane is added or removed, so the mode can never outlive
// the pane set it was turned on for.
func (m *Model) clearSync() { m.syncPanes = nil }

// inSync reports whether id is one of the panes keys are broadcast to.
func (m *Model) inSync(id PaneID) bool {
	for _, p := range m.syncPanes {
		if p == id {
			return true
		}
	}
	return false
}

// toFocused forwards msg to the focused content of the active window.
func (m *Model) toFocused(msg tea.Msg) tea.Cmd {
	id, c := m.Focused()
	if c == nil {
		return nil
	}
	return m.updateContent(id, c, msg)
}

// updateContent stores a content's new value and returns its command.
func (m *Model) updateContent(id PaneID, c Content, msg tea.Msg) tea.Cmd {
	nc, cmd := c.Update(msg)
	if nc != nil {
		m.panes[id] = nc
	}
	return cmd
}

// broadcast delivers msg to every content of every window.
func (m *Model) broadcast(msg tea.Msg) tea.Cmd {
	var cmds []tea.Cmd
	for _, id := range m.sess.AllPanes() {
		c, ok := m.panes[id]
		if !ok || c == nil {
			continue
		}
		if cmd := m.updateContent(id, c, msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// applyOp executes one multiplexer operation.
func (m *Model) applyOp(op OpMsg) (tea.Msg, tea.Cmd) {
	switch op.Op {
	case OpNewWindow, OpKillWindow, OpSelectWindow, OpNextWindow, OpPrevWindow:
		// Synchronized input belongs to one pane set of one window.
		m.clearSync()
	}
	switch op.Op {
	case OpSplitH:
		return nil, m.split(true)
	case OpSplitV:
		return nil, m.split(false)
	case OpKillPane:
		return nil, m.killPane()
	case OpZoom:
		m.sess.ToggleZoom()
		return nil, nil
	case OpFocus:
		return nil, m.focus(op.Arg)
	case OpResize:
		m.sess.Resize(op.Arg, resizeCells)
		return nil, nil
	case OpSwap:
		m.sess.SwapPane(op.Arg)
		return nil, nil
	case OpLayoutNext:
		m.sess.NextLayout()
		return nil, nil
	case OpNewWindow:
		return nil, m.newWindow()
	case OpKillWindow:
		return nil, m.killWindow()
	case OpSelectWindow:
		i, err := strconv.Atoi(op.Arg)
		if err != nil {
			return nil, nil
		}
		return nil, m.selectWindow(i)
	case OpNextWindow, OpPrevWindow:
		old := m.sess.ActiveWindow()
		if op.Op == OpNextWindow {
			m.sess.NextWindow()
		} else {
			m.sess.PrevWindow()
		}
		if m.sess.ActiveWindow() == old {
			return nil, nil
		}
		m.focusContent(old, false)
		m.focusContent(m.sess.ActiveWindow(), true)
		return nil, nil
	case OpWindowList, OpRenameWindow, OpRenameSession, OpSessionList, OpSSH,
		OpCopyMode, OpSync, OpCommand, OpDetach, OpHelp:
		return op, nil
	}
	return nil, nil
}

// split adds a pane beside the focused one and focuses it.
func (m *Model) split(horizontal bool) tea.Cmd {
	w := m.sess.ActiveWindow()
	if w == nil {
		_, id := m.sess.NewWindow("")
		return m.create(id, ContentState{}, true)
	}
	old := w.Focus
	id, err := m.sess.Split(horizontal)
	if err != nil {
		return nil
	}
	m.focusContentID(old, false)
	return m.create(id, ContentState{}, true)
}

// killPane closes the focused pane; closing the last pane quits.
func (m *Model) killPane() tea.Cmd {
	w := m.sess.ActiveWindow()
	if w == nil {
		return nil
	}
	id := w.Focus
	m.closeContent(id)
	_, empty := m.sess.KillPane(id)
	if empty {
		return tea.Quit
	}
	m.focusContent(m.sess.ActiveWindow(), true)
	return nil
}

// focus moves the focus inside the active window.
func (m *Model) focus(dir string) tea.Cmd {
	w := m.sess.ActiveWindow()
	if w == nil {
		return nil
	}
	old := w.Focus
	m.sess.Focus(dir)
	if w.Focus == old {
		return nil
	}
	m.focusContentID(old, false)
	m.focusContentID(w.Focus, true)
	return nil
}

// newWindow creates a window with one pane and focuses it.
func (m *Model) newWindow() tea.Cmd {
	m.focusContent(m.sess.ActiveWindow(), false)
	_, id := m.sess.NewWindow("")
	return m.create(id, ContentState{}, true)
}

// killWindow closes every content of the active window.
func (m *Model) killWindow() tea.Cmd {
	w := m.sess.ActiveWindow()
	if w == nil {
		return nil
	}
	for _, id := range w.Layout.Panes() {
		m.closeContent(id)
	}
	if m.sess.KillWindow() {
		return tea.Quit
	}
	m.focusContent(m.sess.ActiveWindow(), true)
	return nil
}

// selectWindow makes window i active, if it exists.
func (m *Model) selectWindow(i int) tea.Cmd {
	old := m.sess.ActiveWindow()
	if !m.sess.SelectWindow(i) {
		return nil
	}
	nw := m.sess.ActiveWindow()
	if nw == old {
		return nil
	}
	m.focusContent(old, false)
	m.focusContent(nw, true)
	return nil
}

// create builds the content of a new pane and runs its init. Adding a pane ends
// synchronized input: the set it was turned on for is gone (task 064).
func (m *Model) create(id PaneID, st ContentState, focus bool) tea.Cmd {
	m.clearSync()
	if m.factory == nil {
		return nil
	}
	c, cmd := m.factory(id, st)
	if c == nil {
		return cmd
	}
	if focus {
		c.SetFocus(true)
	}
	m.panes[id] = c
	cmds := []tea.Cmd{cmd}
	if ic := c.Init(); ic != nil {
		cmds = append(cmds, ic)
	}
	return tea.Batch(cmds...)
}

// closeContent releases one content and forgets it. Removing a pane ends
// synchronized input.
func (m *Model) closeContent(id PaneID) {
	m.clearSync()
	c, ok := m.panes[id]
	if !ok {
		return
	}
	delete(m.panes, id)
	if c != nil {
		_ = c.Close()
	}
}

// focusContent tells a window's focused content whether it has focus.
func (m *Model) focusContent(w *Window, on bool) {
	if w == nil {
		return
	}
	m.focusContentID(w.Focus, on)
}

func (m *Model) focusContentID(id PaneID, on bool) {
	if c, ok := m.panes[id]; ok && c != nil {
		c.SetFocus(on)
	}
}

// Focused returns the focused pane of the active window.
func (m *Model) Focused() (PaneID, Content) {
	w := m.sess.ActiveWindow()
	if w == nil {
		return 0, nil
	}
	return w.Focus, m.panes[w.Focus]
}

// Tabs lists the windows for the status line. A zoomed window's name gains
// " Z".
func (m *Model) Tabs() []Tab {
	tabs := make([]Tab, 0, len(m.sess.Windows))
	for i, w := range m.sess.Windows {
		name := w.Name
		if w.Zoomed {
			name += " Z"
		}
		tabs = append(tabs, Tab{Index: i, Name: name, Active: i == m.sess.Active, Zoomed: w.Zoomed})
	}
	return tabs
}

// PrefixArmed reports whether the prefix key was pressed and a key is awaited.
func (m *Model) PrefixArmed() bool { return m.prefix.Armed() }

// SetPrefix replaces the session prefix key. A session server re-points it when
// a client attaches from a different environment: inside tmux the configured
// tmux prefix would collide with the outer one, so the alternate key applies.
func (m *Model) SetPrefix(key string) {
	if m == nil || m.prefix == nil || key == "" {
		return
	}
	m.prefix.SetPrefix(key)
}

// Session returns the session the model owns.
func (m *Model) Session() *Session { return m.sess }

// Contents returns a copy of the pane-to-content map.
func (m *Model) Contents() map[PaneID]Content {
	out := make(map[PaneID]Content, len(m.panes))
	for id, c := range m.panes {
		out[id] = c
	}
	return out
}

// AddPane creates a pane for a caller that already knows its state (:ssh,
// restore). split is "h" (right, the default) or "v" (below). With no window it
// opens a new one.
func (m *Model) AddPane(st ContentState, split string) (PaneID, tea.Cmd) {
	w := m.sess.ActiveWindow()
	if w == nil {
		_, id := m.sess.NewWindow("")
		return id, m.create(id, st, true)
	}
	horizontal := split != "v" && split != "split-v" && split != "vertical" && split != "down"
	old := w.Focus
	id, err := m.sess.Split(horizontal)
	if err != nil {
		return 0, nil
	}
	m.focusContentID(old, false)
	return id, m.create(id, st, true)
}

// CloseAll releases every content.
func (m *Model) CloseAll() {
	for id := range m.panes {
		m.closeContent(id)
	}
}
