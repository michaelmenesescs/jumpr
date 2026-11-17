package tui

import (
	"io"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/render"
)

// Stack is the view history of one view pane. The root view is always at the
// bottom and can never be popped. Views are interface values: every Update
// result is stored back, because a view may return a new value.
type Stack struct {
	views []View
}

// NewStack returns a stack whose single view is root.
func NewStack(root View) *Stack {
	return &Stack{views: []View{root}}
}

// Len returns the number of views on the stack (at least one).
func (s *Stack) Len() int {
	return len(s.views)
}

// Top returns the focused view, or nil for a zero Stack.
func (s *Stack) Top() View {
	if len(s.views) == 0 {
		return nil
	}
	return s.views[len(s.views)-1]
}

// Crumbs returns the breadcrumb of every view, root first.
func (s *Stack) Crumbs() []string {
	out := make([]string, 0, len(s.views))
	for _, v := range s.views {
		out = append(out, v.Crumb())
	}
	return out
}

// Push puts v on top and returns its Init command.
func (s *Stack) Push(v View) tea.Cmd {
	if v == nil {
		return nil
	}
	s.views = append(s.views, v)
	return v.Init()
}

// Pop drops the top view, closing it when it is an io.Closer. It refuses to pop
// the root and reports false then.
func (s *Stack) Pop() bool {
	if len(s.views) <= 1 {
		return false
	}
	last := len(s.views) - 1
	s.closeView(s.views[last])
	s.views[last] = nil
	s.views = s.views[:last]
	return true
}

// Replace swaps the top view for v, closes the view it replaces when that view
// is an io.Closer, and returns v's Init command. Replacing the root is allowed:
// the stack always keeps exactly one view.
func (s *Stack) Replace(v View) tea.Cmd {
	if v == nil {
		return nil
	}
	if len(s.views) == 0 {
		s.views = append(s.views, v)
	} else {
		if old := s.views[len(s.views)-1]; old != nil && old != v {
			s.closeView(old)
		}
		s.views[len(s.views)-1] = v
	}
	return v.Init()
}

// Reset closes every view and makes v the only one, returning its Init
// command. The tool rail uses it: a new tool starts a new breadcrumb trail.
func (s *Stack) Reset(v View) tea.Cmd {
	if v == nil {
		return nil
	}
	for i, old := range s.views {
		if old != v {
			s.closeView(old)
		}
		s.views[i] = nil
	}
	s.views = append(s.views[:0], v)
	return v.Init()
}

// closeView releases a view's resources: views that hold streams cancel them in
// Close (io.Closer), so a popped journal stops following.
func (s *Stack) closeView(v View) {
	if v == nil {
		return
	}
	if c, ok := v.(io.Closer); ok {
		_ = c.Close()
	}
}

// Update handles the navigation messages itself and routes everything else to
// the top view only, storing the view that Update returned back into the stack.
func (s *Stack) Update(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case PushViewMsg:
		return s.Push(m.View)
	case PopViewMsg:
		s.Pop()
		return nil
	case ReplaceViewMsg:
		return s.Replace(m.View)
	}
	top := s.Top()
	if top == nil {
		return nil
	}
	v, cmd := top.Update(msg)
	if v != nil {
		s.views[len(s.views)-1] = v
	}
	return cmd
}

// Render delegates to the top view and fits the result to w×h as a safety net.
func (s *Stack) Render(w, h int) string {
	top := s.Top()
	if top == nil {
		return render.Fit("", w, h)
	}
	return render.Fit(top.Render(w, h), w, h)
}
