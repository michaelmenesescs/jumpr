package mux

import (
	"errors"
	"reflect"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/errs"
)

func mustWindow(t *testing.T, s *Session, name string, w, h int) (*Window, PaneID) {
	t.Helper()
	s.SetSize(w, h)
	win, first := s.NewWindow(name)
	if win == nil {
		t.Fatal("NewWindow returned nil window")
	}
	if s.ActiveWindow() != win {
		t.Fatal("new window is not active")
	}
	if !win.Layout.Contains(first) || win.Layout.Count() != 1 {
		t.Fatalf("new window layout: panes %v", win.Layout.Panes())
	}
	if win.Focus != first || win.Last != first {
		t.Fatalf("new window focus %d last %d, pane %d", win.Focus, win.Last, first)
	}
	return win, first
}

func mustSplit(t *testing.T, s *Session, horizontal bool) PaneID {
	t.Helper()
	id, err := s.Split(horizontal)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	return id
}

func TestSplitFocusesNew(t *testing.T) {
	s := NewSession("main")
	w, first := mustWindow(t, s, "hosts", 120, 40)

	n := mustSplit(t, s, true)
	if w.Focus != n {
		t.Fatalf("focus %d, want new pane %d", w.Focus, n)
	}
	if w.Last != first {
		t.Fatalf("last %d, want previous pane %d", w.Last, first)
	}
	if !w.Layout.Contains(n) || w.Layout.Count() != 2 {
		t.Fatalf("panes %v", w.Layout.Panes())
	}
	if w.Zoomed {
		t.Fatal("split must clear zoom")
	}
	rs := s.Rects(Rect{0, 0, 120, 40})
	if len(rs) != 2 || rs[first].W != 60 || rs[n].W != 60 {
		t.Fatalf("rects %+v", rs)
	}

	// Below the minimum: 15 wide split in two gives panes under 10 cells.
	small := NewSession("small")
	sw, sp := mustWindow(t, small, "w", 15, 10)
	_, err := small.Split(true)
	if err == nil {
		t.Fatal("expected pane too small")
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Kind != errs.Config || e.Msg != "pane too small" {
		t.Fatalf("error %v", err)
	}
	if sw.Focus != sp || sw.Layout.Count() != 1 {
		t.Fatalf("refused split changed the window: focus %d panes %v", sw.Focus, sw.Layout.Panes())
	}

	// Too small for vertical as well: 6 high gives 3/3 exactly, 5 high does not.
	small.SetSize(80, 5)
	if _, err := small.Split(false); err == nil {
		t.Fatal("expected pane too small vertically")
	}
}

func TestKillPaneFocusesSibling(t *testing.T) {
	s := NewSession("main")
	w, first := mustWindow(t, s, "hosts", 120, 40)
	second := mustSplit(t, s, true)
	third := mustSplit(t, s, false)

	s.ToggleZoom()
	if !w.Zoomed {
		t.Fatal("want zoomed")
	}

	windowClosed, sessionEmpty := s.KillPane(third)
	if windowClosed || sessionEmpty {
		t.Fatalf("windowClosed=%v sessionEmpty=%v", windowClosed, sessionEmpty)
	}
	if w.Zoomed {
		t.Fatal("closing a zoomed pane must clear zoom")
	}
	if w.Focus != second {
		t.Fatalf("focus %d, want sibling %d", w.Focus, second)
	}
	if w.Focus == third || w.Layout.Contains(third) {
		t.Fatal("killed pane still present")
	}
	if got := w.Layout.Count(); got != 2 {
		t.Fatalf("panes %d", got)
	}
	if !w.Layout.Contains(first) || !w.Layout.Contains(second) {
		t.Fatal("live panes disappeared")
	}
	// The last-focused pane is live, so `last` still works.
	s.Focus("last")
	if w.Focus != second {
		t.Fatalf("after kill, focus(last) = %d", w.Focus)
	}

	// Killing an unknown pane is a no-op.
	if c, e := s.KillPane(9999); c || e {
		t.Fatalf("unknown pane: %v %v", c, e)
	}
}

func TestKillLastPaneClosesWindow(t *testing.T) {
	s := NewSession("main")
	s.SetSize(120, 40)
	w0, _ := s.NewWindow("one")
	w1, last := s.NewWindow("two")

	windowClosed, sessionEmpty := s.KillPane(last)
	if !windowClosed {
		t.Fatal("want windowClosed")
	}
	if sessionEmpty {
		t.Fatal("session must not be empty")
	}
	if len(s.Windows) != 1 || s.Windows[0] != w0 {
		t.Fatalf("windows %d", len(s.Windows))
	}
	if s.Active != 0 || s.ActiveWindow() != w0 {
		t.Fatalf("active %d, want previous window", s.Active)
	}
	if w1.Layout.Contains(last) {
		t.Fatal("window still holds the pane")
	}

	// The previous window keeps working.
	if _, err := s.Split(true); err != nil {
		t.Fatalf("split after window close: %v", err)
	}
}

func TestKillLastWindowEmpty(t *testing.T) {
	s := NewSession("main")
	_, only := mustWindow(t, s, "w", 80, 24)

	windowClosed, sessionEmpty := s.KillPane(only)
	if !windowClosed || !sessionEmpty {
		t.Fatalf("windowClosed=%v sessionEmpty=%v", windowClosed, sessionEmpty)
	}
	if len(s.Windows) != 0 {
		t.Fatalf("windows %d", len(s.Windows))
	}
	if s.Active != -1 || s.ActiveWindow() != nil {
		t.Fatalf("active %d", s.Active)
	}
	if rs := s.Rects(Rect{0, 0, 80, 24}); len(rs) != 0 {
		t.Fatalf("rects %+v", rs)
	}
	if got := s.AllPanes(); len(got) != 0 {
		t.Fatalf("all panes %v", got)
	}
}

func TestZoomRects(t *testing.T) {
	s := NewSession("main")
	w, first := mustWindow(t, s, "hosts", 120, 40)
	other := mustSplit(t, s, true)

	area := Rect{1, 2, 100, 30}
	rs := s.Rects(area)
	if len(rs) != 2 || rs[first] == (Rect{}) || rs[other] == (Rect{}) {
		t.Fatalf("unzoomed rects %+v", rs)
	}

	s.ToggleZoom()
	if !w.Zoomed {
		t.Fatal("want zoomed")
	}
	rs = s.Rects(area)
	if len(rs) != 1 {
		t.Fatalf("zoomed rects %+v", rs)
	}
	if r, ok := rs[w.Focus]; !ok || r != area {
		t.Fatalf("zoomed rect for focus %d: %+v", w.Focus, r)
	}

	s.ToggleZoom()
	if w.Zoomed {
		t.Fatal("want unzoomed")
	}
	if len(s.Rects(area)) != 2 {
		t.Fatal("unzoom must restore both panes")
	}

	// A single pane cannot zoom.
	s2 := NewSession("solo")
	mustWindow(t, s2, "w", 80, 24)
	s2.ToggleZoom()
	if s2.ActiveWindow().Zoomed {
		t.Fatal("single pane must not zoom")
	}
}

func TestFocusLast(t *testing.T) {
	s := NewSession("main")
	w, first := mustWindow(t, s, "hosts", 120, 40)
	second := mustSplit(t, s, true)

	if w.Focus != second {
		t.Fatalf("focus %d", w.Focus)
	}
	s.Focus("last")
	if w.Focus != first {
		t.Fatalf("focus(last) = %d, want %d", w.Focus, first)
	}
	s.Focus("last")
	if w.Focus != second {
		t.Fatalf("focus(last) again = %d, want %d", w.Focus, second)
	}
	s.Focus("next")
	if w.Focus != first {
		t.Fatalf("focus(next) = %d, want %d", w.Focus, first)
	}
	s.Focus("next")
	if w.Focus != second {
		t.Fatalf("focus(next) wraps to %d, want %d", w.Focus, second)
	}

	// Directional focus follows the layout.
	s.Focus("left")
	if w.Focus != first {
		t.Fatalf("focus(left) = %d, want %d", w.Focus, first)
	}
	s.Focus("right")
	if w.Focus != second {
		t.Fatalf("focus(right) = %d, want %d", w.Focus, second)
	}
	s.Focus("bogus")
	if w.Focus != second {
		t.Fatal("unknown direction must not move focus")
	}
}

func TestWindowSelectBounds(t *testing.T) {
	s := NewSession("main")
	s.SetSize(120, 40)
	var wins []*Window
	for i, n := range []string{"a", "b", "c"} {
		w, _ := s.NewWindow(n)
		if s.Active != i {
			t.Fatalf("active %d after window %d", s.Active, i)
		}
		wins = append(wins, w)
	}

	if !s.SelectWindow(1) || s.Active != 1 || s.ActiveWindow() != wins[1] {
		t.Fatalf("select 1: %v active %d", true, s.Active)
	}
	for _, bad := range []int{-1, 3, 99} {
		if s.SelectWindow(bad) {
			t.Fatalf("select %d must fail", bad)
		}
		if s.Active != 1 {
			t.Fatalf("select %d moved active to %d", bad, s.Active)
		}
	}

	s.NextWindow()
	if s.Active != 2 {
		t.Fatalf("next window %d", s.Active)
	}
	s.NextWindow()
	if s.Active != 0 {
		t.Fatalf("next window must wrap, got %d", s.Active)
	}
	s.PrevWindow()
	if s.Active != 2 {
		t.Fatalf("prev window must wrap, got %d", s.Active)
	}

	s.KillWindow()
	if len(s.Windows) != 2 || s.Active != 1 {
		t.Fatalf("kill window: %d windows, active %d", len(s.Windows), s.Active)
	}
	s.KillWindow()
	s.KillWindow()
	if s.KillWindow() != true {
		t.Fatal("killing an empty session reports empty")
	}
}

func TestIDsNeverReused(t *testing.T) {
	s := NewSession("main")
	s.SetSize(120, 40)
	w0, p1 := mustWindow(t, s, "a", 120, 40)
	p2 := mustSplit(t, s, true)
	s.KillPane(p2)
	w1, p3 := mustWindow(t, s, "b", 120, 40)
	s.KillWindow()
	w2, p4 := s.NewWindow("c")

	if p2 <= p1 || p3 <= p2 || p4 <= p3 {
		t.Fatalf("pane ids reused: %d %d %d %d", p1, p2, p3, p4)
	}
	if w1.ID <= w0.ID || w2.ID <= w1.ID {
		t.Fatalf("window ids reused: %d %d %d", w0.ID, w1.ID, w2.ID)
	}

	// FindPane and AllPanes see exactly the live panes, in window order.
	got := s.AllPanes()
	if !reflect.DeepEqual(got, []PaneID{p1, p4}) {
		t.Fatalf("all panes %v", got)
	}
	if win, ok := s.FindPane(p4); !ok || win != w2 {
		t.Fatalf("find pane %d: %v %v", p4, win, ok)
	}
	if win, ok := s.FindPane(p1); !ok || win != w0 {
		t.Fatalf("find pane %d: %v %v", p1, win, ok)
	}
	if _, ok := s.FindPane(p2); ok {
		t.Fatal("killed pane still found")
	}
	// Three windows: panes are listed per window, in order.
	s.SetSize(120, 40)
	wa, pa := s.NewWindow("d")
	_ = wa
	pb := mustSplit(t, s, true)
	if got := s.AllPanes(); !reflect.DeepEqual(got, []PaneID{p1, p4, pa, pb}) {
		t.Fatalf("all panes with three windows %v", got)
	}
	_ = w2
}

func TestNextLayout(t *testing.T) {
	s := NewSession("main")
	w, first := mustWindow(t, s, "hosts", 120, 40)
	second := mustSplit(t, s, true)
	w.Focus = second // resize the divider so the tree is uneven
	w.Last = first
	s.Resize("right", 20)
	uneven := s.Rects(Rect{0, 0, 120, 40})
	if uneven[first].W == uneven[second].W {
		t.Fatalf("expected an uneven split, got %+v", uneven)
	}

	s.NextLayout() // even-horizontal
	rs := s.Rects(Rect{0, 0, 120, 40})
	if rs[first].W != rs[second].W || rs[first].H != 40 {
		t.Fatalf("even-horizontal %+v", rs)
	}

	s.NextLayout() // even-vertical
	rs = s.Rects(Rect{0, 0, 120, 40})
	if rs[first].W != rs[second].W || rs[first].H != rs[second].H {
		t.Fatalf("even-vertical %+v", rs)
	}

	s.NextLayout() // main-left
	rs = s.Rects(Rect{0, 0, 120, 40})
	if rs[first].W != 72 || rs[first].H != 40 || rs[first].X != 0 {
		t.Fatalf("main-left first pane %+v", rs[first])
	}
	if rs[second].X != 72 || rs[second].W != 48 || rs[second].H != 40 {
		t.Fatalf("main-left second pane %+v", rs[second])
	}

	// Three panes: first stays wide, the rest stack on the right.
	s.NextLayout() // wraps back to even-horizontal
	third := mustSplit(t, s, false)
	s.NextLayout() // even-vertical
	s.NextLayout() // main-left
	rs = s.Rects(Rect{0, 0, 120, 40})
	if len(rs) != 3 {
		t.Fatalf("rects %+v", rs)
	}
	if rs[first].W != 72 || rs[first].H != 40 {
		t.Fatalf("main-left column %+v", rs[first])
	}
	for _, id := range []PaneID{second, third} {
		if rs[id].X != 72 || rs[id].W != 48 || rs[id].H != 20 {
			t.Fatalf("main-left stack pane %d %+v", id, rs[id])
		}
	}
}

func TestForPaneWrapsBatch(t *testing.T) {
	if cmd := ForPane(3, nil); cmd != nil {
		t.Fatal("nil command must stay nil")
	}

	got := ForPane(7, func() tea.Msg { return "hi" })()
	pm, ok := got.(PaneMsg)
	if !ok || pm.Pane != 7 || pm.Msg != "hi" {
		t.Fatalf("wrapped message %#v", got)
	}

	got = ForPane(5, func() tea.Msg { return nil })()
	if got != nil {
		t.Fatalf("nil message must not be wrapped: %#v", got)
	}

	batch := ForPane(4, func() tea.Msg {
		return tea.BatchMsg{
			func() tea.Msg { return "a" },
			func() tea.Msg { return "b" },
			nil,
		}
	})()
	bm, ok := batch.(tea.BatchMsg)
	if !ok {
		t.Fatalf("want tea.BatchMsg, got %T", batch)
	}
	if len(bm) != 2 {
		t.Fatalf("batch length %d", len(bm))
	}
	for i, want := range []string{"a", "b"} {
		inner, ok := bm[i]().(PaneMsg)
		if !ok || inner.Pane != 4 || inner.Msg != want {
			t.Fatalf("batch[%d] %#v", i, inner)
		}
	}
}
