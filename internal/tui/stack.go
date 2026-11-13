package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/render"
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

// Pop drops the top view. It refuses to pop the root and reports false then.
func (s *Stack) Pop() bool {
	if len(s.views) <= 1 {
		return false
	}
	last := len(s.views) - 1
	s.views[last] = nil
	s.views = s.views[:last]
	return true
}

// Replace swaps the top view for v and returns v's Init command. Replacing
// the root is allowed: the stack always keeps exactly one view.
func (s *Stack) Replace(v View) tea.Cmd {
	if v == nil {
		return nil
	}
	if len(s.views) == 0 {
		s.views = append(s.views, v)
	} else {
		s.views[len(s.views)-1] = v
	}
	return v.Init()
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
