package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/render"
)

// stubView is a minimal View that records what reaches it.
type stubView struct {
	crumb   string
	render  string
	inits   int
	updates []tea.Msg
}

func (v *stubView) Init() tea.Cmd {
	v.inits++
	return nil
}

func (v *stubView) Update(msg tea.Msg) (View, tea.Cmd) {
	v.updates = append(v.updates, msg)
	return v, nil
}

func (v *stubView) Render(w, h int) string { return v.render }

func (v *stubView) Crumb() string       { return v.crumb }
func (v *stubView) Hints() []KeyHint    { return nil }
func (v *stubView) Subject() Subject    { return Subject{} }
func (v *stubView) Capturing() bool     { return false }
func (v *stubView) CopyLines() []string { return nil }

var _ View = (*stubView)(nil)

// closerView is a stubView that records Close calls.
type closerView struct {
	stubView
	closed int
}

func (v *closerView) Close() error {
	v.closed++
	return nil
}

var _ View = (*closerView)(nil)

func TestStackClosesPoppedAndReplacedViews(t *testing.T) {
	root := &stubView{crumb: "root"}
	s := NewStack(root)

	child := &closerView{stubView: stubView{crumb: "child"}}
	s.Push(child)

	other := &closerView{stubView: stubView{crumb: "other"}}
	s.Replace(other)
	if child.closed != 1 {
		t.Fatalf("Replace did not close the replaced view (closed = %d)", child.closed)
	}
	if other.closed != 0 {
		t.Fatalf("Replace closed the new view (closed = %d)", other.closed)
	}

	if !s.Pop() {
		t.Fatal("Pop returned false with a child on the stack")
	}
	if other.closed != 1 {
		t.Fatalf("Pop did not close the popped view (closed = %d)", other.closed)
	}

	// The root is never popped, so it is never closed.
	if s.Pop() {
		t.Fatal("Pop popped the root")
	}
}

func TestStackPushPopReplace(t *testing.T) {
	root := &stubView{crumb: "root"}
	s := NewStack(root)
	if s.Len() != 1 || s.Top() != root {
		t.Fatalf("new stack: Len = %d, Top = %v", s.Len(), s.Top())
	}
	if root.inits != 0 {
		t.Fatal("NewStack called Init")
	}

	child := &stubView{crumb: "child"}
	s.Push(child)
	if child.inits != 1 {
		t.Fatalf("Push did not call Init (inits = %d)", child.inits)
	}
	if s.Len() != 2 || s.Top() != child {
		t.Fatalf("after Push: Len = %d, Top = %v", s.Len(), s.Top())
	}

	other := &stubView{crumb: "other"}
	s.Replace(other)
	if s.Len() != 2 || s.Top() != other {
		t.Fatalf("after Replace: Len = %d, Top = %v", s.Len(), s.Top())
	}
	if other.inits != 1 {
		t.Fatalf("Replace did not call Init (inits = %d)", other.inits)
	}

	if !s.Pop() {
		t.Fatal("Pop returned false with a child on the stack")
	}
	if s.Len() != 1 || s.Top() != root {
		t.Fatalf("after Pop: Len = %d, Top = %v", s.Len(), s.Top())
	}
}

func TestStackCannotPopRoot(t *testing.T) {
	root := &stubView{crumb: "root"}
	s := NewStack(root)
	if s.Pop() {
		t.Fatal("Pop popped the root")
	}
	if s.Len() != 1 || s.Top() != root {
		t.Fatalf("root changed: Len = %d, Top = %v", s.Len(), s.Top())
	}
	// A replace on the root is allowed; the stack stays at one view.
	replacement := &stubView{crumb: "replacement"}
	s.Replace(replacement)
	if s.Len() != 1 || s.Top() != replacement {
		t.Fatalf("replace root: Len = %d, Top = %v", s.Len(), s.Top())
	}
}

func TestStackCrumbsOrder(t *testing.T) {
	root := &stubView{crumb: "hosts (24)"}
	s := NewStack(root)
	s.Push(&stubView{crumb: "unit nginx.service"})
	s.Push(&stubView{crumb: "journal"})

	got := s.Crumbs()
	want := []string{"hosts (24)", "unit nginx.service", "journal"}
	if len(got) != len(want) {
		t.Fatalf("Crumbs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Crumbs = %v, want %v", got, want)
		}
	}
}

func TestStackUpdateRoutesToTopOnly(t *testing.T) {
	root := &stubView{crumb: "root"}
	child := &stubView{crumb: "child"}
	s := NewStack(root)
	s.Push(child)

	msg := ListBatchMsg{Seq: 1}
	s.Update(msg)
	if len(root.updates) != 0 {
		t.Fatalf("root received %d updates, want 0", len(root.updates))
	}
	got, ok := child.updates[0].(ListBatchMsg)
	if len(child.updates) != 1 || !ok || got.Seq != 1 {
		t.Fatalf("child updates = %v, want the one message", child.updates)
	}
}

func TestStackUpdateNavigation(t *testing.T) {
	root := &stubView{crumb: "root"}
	s := NewStack(root)

	next := &stubView{crumb: "next"}
	s.Update(PushViewMsg{View: next})
	if s.Top() != next || s.Len() != 2 {
		t.Fatalf("PushViewMsg did not push: Len = %d", s.Len())
	}
	if len(root.updates) != 0 || len(next.updates) != 0 {
		t.Fatal("navigation message was routed to a view")
	}

	s.Update(PopViewMsg{})
	if s.Top() != root || s.Len() != 1 {
		t.Fatalf("PopViewMsg did not pop: Len = %d", s.Len())
	}

	replacement := &stubView{crumb: "replacement"}
	s.Update(ReplaceViewMsg{View: replacement})
	if s.Top() != replacement || s.Len() != 1 {
		t.Fatal("ReplaceViewMsg did not replace the top")
	}
}

func TestStackUpdateStoresReturnedView(t *testing.T) {
	root := &stubView{crumb: "root"}
	s := NewStack(root)

	// A View that returns a different value from Update (views are values,
	// not pointers, so this is the normal case).
	first := &valueView{crumb: "first"}
	s.Push(first)
	s.Update(RefreshMsg{})

	top, ok := s.Top().(*valueView)
	if !ok {
		t.Fatalf("Top is %T, want *valueView", s.Top())
	}
	if top.crumb != "second" {
		t.Fatalf("Top crumb = %q, want the view returned by Update", top.crumb)
	}
	if top.updates != 1 {
		t.Fatalf("new view got %d updates, want 1", top.updates)
	}
}

// RefreshMsg is a message only valueView treats specially.
type RefreshMsg struct{}

type valueView struct {
	crumb   string
	updates int
}

func (v *valueView) Init() tea.Cmd { return nil }

func (v *valueView) Update(tea.Msg) (View, tea.Cmd) {
	v.updates++
	next := *v
	next.crumb = "second"
	return &next, nil
}

func (v *valueView) Render(w, h int) string { return "value" }
func (v *valueView) Crumb() string          { return v.crumb }
func (v *valueView) Hints() []KeyHint       { return nil }
func (v *valueView) Subject() Subject       { return Subject{} }
func (v *valueView) Capturing() bool        { return false }
func (v *valueView) CopyLines() []string    { return nil }

var _ View = (*valueView)(nil)

func TestStackRenderFits(t *testing.T) {
	root := &stubView{crumb: "root", render: "0123456789\n\nsecond line that is too long"}
	s := NewStack(root)

	out := s.Render(4, 2)
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("Render returned %d lines, want 2", len(lines))
	}
	for i, line := range lines {
		if got := render.Width(line); got != 4 {
			t.Fatalf("line %d has width %d, want 4 (%q)", i, got, line)
		}
	}
	if out != render.Fit(root.render, 4, 2) {
		t.Fatalf("Render = %q, want the fitted view output", out)
	}
}
