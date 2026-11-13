package mux

import "jumper/internal/errs"

// Preset layouts cycled by NextLayout.
const (
	modeEvenH = iota
	modeEvenV
	modeMainLeft
	modeCount
)

// Window is one full-screen layout in a session.
type Window struct {
	ID       WindowID
	Name     string
	Layout   *Layout
	Focus    PaneID
	Last     PaneID
	Zoomed   bool
	AutoName bool
}

// Session is an ordered set of windows. Active is the index of the active
// window, or -1 when the session has none.
type Session struct {
	Name    string
	Windows []*Window
	Active  int

	nextPane   PaneID
	nextWindow WindowID
	width      int
	height     int
	layoutMode map[WindowID]int
}

// NewSession returns a session with no windows.
func NewSession(name string) *Session {
	return &Session{
		Name:       name,
		Active:     -1,
		nextPane:   1,
		layoutMode: map[WindowID]int{},
	}
}

// SetSize records the last known size, used for neighbour lookups, resizes and
// the minimum-size check on split.
func (s *Session) SetSize(w, h int) {
	s.width, s.height = w, h
}

func (s *Session) area() Rect { return Rect{0, 0, s.width, s.height} }

func (s *Session) allocPane() PaneID {
	id := s.nextPane
	s.nextPane++
	return id
}

// NewWindow appends a window holding one new pane and makes it active. An
// empty name marks the window as auto-named.
func (s *Session) NewWindow(name string) (*Window, PaneID) {
	id := s.nextWindow
	s.nextWindow++
	pane := s.allocPane()
	w := &Window{
		ID:       id,
		Name:     name,
		Layout:   NewLayout(pane),
		Focus:    pane,
		Last:     pane,
		AutoName: name == "",
	}
	s.Windows = append(s.Windows, w)
	s.Active = len(s.Windows) - 1
	s.layoutMode[id] = -1
	return w, pane
}

// ActiveWindow returns the active window, or nil when there is none.
func (s *Session) ActiveWindow() *Window {
	if s.Active < 0 || s.Active >= len(s.Windows) {
		return nil
	}
	return s.Windows[s.Active]
}

// Split splits the focused pane of the active window and focuses the new pane.
// horizontal puts the new pane to the right, otherwise below. The split is
// refused when a resulting pane would be under 10x3 at the last known size.
func (s *Session) Split(horizontal bool) (PaneID, error) {
	const op = "mux.split"
	w := s.ActiveWindow()
	if w == nil {
		return 0, errs.New(errs.Config, op, "no active window")
	}
	id := s.allocPane()
	var err error
	if horizontal {
		err = w.Layout.SplitH(w.Focus, id)
	} else {
		err = w.Layout.SplitV(w.Focus, id)
	}
	if err != nil {
		return 0, err
	}
	if !s.fits(w) {
		w.Layout.Close(id)
		return 0, errs.New(errs.Config, op, "pane too small")
	}
	w.Last = w.Focus
	w.Focus = id
	w.Zoomed = false
	return id, nil
}

// fits reports whether every pane of w is at least the minimum size at the last
// known size. Without a known size nothing is refused.
func (s *Session) fits(w *Window) bool {
	if s.width <= 0 || s.height <= 0 {
		return true
	}
	for _, r := range w.Layout.Rects(s.area()) {
		if r.W < minPaneW || r.H < minPaneH {
			return false
		}
	}
	return true
}

// indexOf returns the position of w in the session, or -1.
func (s *Session) indexOf(w *Window) int {
	for i, o := range s.Windows {
		if o == w {
			return i
		}
	}
	return -1
}

// removeWindow drops the window at idx and keeps Active on a valid window,
// preferring the previous one.
func (s *Session) removeWindow(idx int) bool {
	w := s.Windows[idx]
	s.Windows = append(s.Windows[:idx], s.Windows[idx+1:]...)
	delete(s.layoutMode, w.ID)
	if len(s.Windows) == 0 {
		s.Active = -1
		return true
	}
	switch {
	case idx < s.Active:
		s.Active--
	case idx == s.Active:
		s.Active = idx - 1
		if s.Active < 0 {
			s.Active = 0
		}
	}
	if s.Active >= len(s.Windows) {
		s.Active = len(s.Windows) - 1
	}
	return false
}

// KillPane closes a pane. A window left without panes is removed and the
// previous window selected. Unknown panes are a no-op.
func (s *Session) KillPane(id PaneID) (windowClosed, sessionEmpty bool) {
	w, ok := s.FindPane(id)
	if !ok {
		return false, false
	}
	if w.Zoomed {
		w.Zoomed = false
	}
	next, empty := w.Layout.Close(id)
	if empty {
		idx := s.indexOf(w)
		if idx < 0 {
			return false, false
		}
		return true, s.removeWindow(idx)
	}
	switch {
	case w.Focus == id:
		w.Focus = next
		if w.Last == id || !w.Layout.Contains(w.Last) {
			w.Last = next
		}
	case w.Last == id:
		w.Last = w.Focus
	}
	return false, false
}

// Focus moves the focus of the active window.
// d is "left", "right", "up", "down", "next" or "last"; other values are a no-op.
func (s *Session) Focus(d string) {
	w := s.ActiveWindow()
	if w == nil {
		return
	}
	var target PaneID
	var ok bool
	switch d {
	case "left", "right", "up", "down":
		dir, _ := direction(d)
		target, ok = w.Layout.Neighbor(w.Focus, dir, s.area())
	case "next":
		panes := w.Layout.Panes()
		for i, p := range panes {
			if p == w.Focus && len(panes) > 1 {
				target = panes[(i+1)%len(panes)]
				ok = true
				break
			}
		}
	case "last":
		if w.Last != w.Focus && w.Layout.Contains(w.Last) {
			target, ok = w.Last, true
		}
	default:
		return
	}
	if !ok || target == w.Focus {
		return
	}
	w.Last = w.Focus
	w.Focus = target
}

// direction maps a focus/resize direction name to a Direction.
func direction(d string) (Direction, bool) {
	switch d {
	case "left":
		return Left, true
	case "right":
		return Right, true
	case "up":
		return Up, true
	case "down":
		return Down, true
	}
	return 0, false
}

// Resize moves the divider nearest the focused pane by cells in direction d.
func (s *Session) Resize(d string, cells int) {
	w := s.ActiveWindow()
	if w == nil || cells == 0 {
		return
	}
	dir, ok := direction(d)
	if !ok {
		return
	}
	w.Layout.Resize(w.Focus, dir, cells, s.area())
}

// ToggleZoom zooms or unzooms the focused pane. A single pane cannot zoom.
func (s *Session) ToggleZoom() {
	w := s.ActiveWindow()
	if w == nil || w.Layout.Count() < 2 {
		return
	}
	w.Zoomed = !w.Zoomed
}

// SelectWindow makes Windows[i] active. Out-of-range indexes leave the session
// unchanged and report false.
func (s *Session) SelectWindow(i int) bool {
	if i < 0 || i >= len(s.Windows) {
		return false
	}
	s.Active = i
	return true
}

// NextWindow selects the next window, wrapping around.
func (s *Session) NextWindow() {
	if len(s.Windows) == 0 {
		return
	}
	s.Active = (s.Active + 1) % len(s.Windows)
}

// PrevWindow selects the previous window, wrapping around.
func (s *Session) PrevWindow() {
	if len(s.Windows) == 0 {
		return
	}
	s.Active = (s.Active - 1 + len(s.Windows)) % len(s.Windows)
}

// KillWindow closes the active window. It reports true when the session is left
// with no windows.
func (s *Session) KillWindow() (sessionEmpty bool) {
	w := s.ActiveWindow()
	if w == nil {
		return true
	}
	idx := s.indexOf(w)
	if idx < 0 {
		return len(s.Windows) == 0
	}
	return s.removeWindow(idx)
}

// RenameWindow names the active window; an empty name turns auto-naming on.
func (s *Session) RenameWindow(name string) {
	w := s.ActiveWindow()
	if w == nil {
		return
	}
	w.Name = name
	w.AutoName = name == ""
}

// Rename names the session.
func (s *Session) Rename(name string) { s.Name = name }

// SwapPane exchanges the focused pane with the previous ("prev") or next
// ("next") pane in layout order. Focus follows the pane.
func (s *Session) SwapPane(dir string) {
	w := s.ActiveWindow()
	if w == nil {
		return
	}
	panes := w.Layout.Panes()
	if len(panes) < 2 {
		return
	}
	i := -1
	for j, p := range panes {
		if p == w.Focus {
			i = j
			break
		}
	}
	if i < 0 {
		return
	}
	var k int
	switch dir {
	case "prev":
		k = (i - 1 + len(panes)) % len(panes)
	case "next":
		k = (i + 1) % len(panes)
	default:
		return
	}
	w.Layout.Swap(panes[i], panes[k])
}

// NextLayout cycles the active window through even-horizontal, even-vertical
// and main-left (first pane left at 60%, the rest stacked on the right).
func (s *Session) NextLayout() {
	w := s.ActiveWindow()
	if w == nil || w.Layout.Count() < 2 {
		return
	}
	mode := (s.layoutMode[w.ID] + 1) % modeCount
	switch mode {
	case modeEvenH:
		w.Layout.Even(true)
	case modeEvenV:
		w.Layout.Even(false)
	case modeMainLeft:
		w.Layout = mainLeft(w.Layout.Panes())
	}
	s.layoutMode[w.ID] = mode
}

// mainLeft rebuilds a layout with the first pane on the left at ratio 0.6 and
// the remaining panes stacked vertically on the right.
func mainLeft(panes []PaneID) *Layout {
	if len(panes) == 0 {
		return nil
	}
	if len(panes) == 1 {
		return NewLayout(panes[0])
	}
	return &Layout{root: &node{
		horizontal: true,
		ratio:      0.6,
		a:          &node{pane: panes[0]},
		b:          stack(panes[1:]),
	}}
}

// stack builds a vertical chain of panes, top first.
func stack(panes []PaneID) *node {
	if len(panes) == 0 {
		return nil
	}
	if len(panes) == 1 {
		return &node{pane: panes[0]}
	}
	return &node{
		ratio: 0.5,
		a:     &node{pane: panes[0]},
		b:     stack(panes[1:]),
	}
}

// FindPane returns the window holding id.
func (s *Session) FindPane(id PaneID) (*Window, bool) {
	for _, w := range s.Windows {
		if w.Layout.Contains(id) {
			return w, true
		}
	}
	return nil, false
}

// AllPanes lists every pane of every window, window order first.
func (s *Session) AllPanes() []PaneID {
	var ids []PaneID
	for _, w := range s.Windows {
		ids = append(ids, w.Layout.Panes()...)
	}
	return ids
}

// Rects tiles area with the active window's panes. When zoomed only the
// focused pane is returned, holding the whole area.
func (s *Session) Rects(area Rect) map[PaneID]Rect {
	w := s.ActiveWindow()
	if w == nil {
		return map[PaneID]Rect{}
	}
	if w.Zoomed {
		return map[PaneID]Rect{w.Focus: area}
	}
	return w.Layout.Rects(area)
}
