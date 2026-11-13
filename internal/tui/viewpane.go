package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"jumper/internal/mux"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/tui/theme"
)

// contextSetter is implemented by a view that starts background work and must
// stop it when its pane closes. ViewPane hands its own context to every view it
// pushes, before that view's Init runs.
type contextSetter interface{ SetContext(ctx context.Context) }

// filterState is implemented by a view that wants its filter text shown on the
// pane's breadcrumb line.
type filterState interface{ FilterState() string }

// commandLiner is implemented by a view that can describe itself as a `:`
// command line, so the pane can be rebuilt after a restart.
type commandLiner interface{ CommandLine() string }

// ViewPane is the mux.Content that shows jumper views: one Stack of them, one
// breadcrumb line on top. It owns a context derived from the session's; Close
// cancels it, which stops every stream the views started.
type ViewPane struct {
	env     *Env
	id      mux.PaneID
	ctx     context.Context
	cancel  context.CancelFunc
	stack   *Stack
	focused bool
}

var _ mux.Content = (*ViewPane)(nil)

// NewViewPane builds a pane over root, the view at the bottom of the stack.
// The pane derives its context from ctx; a nil ctx means context.Background.
func NewViewPane(env *Env, id mux.PaneID, ctx context.Context, root View) *ViewPane {
	if ctx == nil {
		ctx = context.Background()
	}
	pctx, cancel := context.WithCancel(ctx)
	p := &ViewPane{
		env:    env,
		id:     id,
		ctx:    pctx,
		cancel: cancel,
		stack:  NewStack(root),
	}
	p.attach(root)
	return p
}

// attach gives v the pane context when it can take one.
func (p *ViewPane) attach(v View) {
	if cs, ok := v.(contextSetter); ok {
		cs.SetContext(p.ctx)
	}
}

// Init starts the top view. Its messages come back wrapped for this pane.
func (p *ViewPane) Init() tea.Cmd {
	top := p.stack.Top()
	if top == nil {
		return nil
	}
	return mux.ForPane(p.id, top.Init())
}

// Update resolves OpenQueryMsg here, routes everything else to the stack and
// wraps the result for this pane in exactly one place. The mux unwraps a
// PaneMsg whose content is a root-level message and forwards it to the root
// (see IsRootMsg).
func (p *ViewPane) Update(msg tea.Msg) (mux.Content, tea.Cmd) {
	if q, ok := msg.(OpenQueryMsg); ok {
		v, err := p.viewFor(q.Query)
		if err != nil {
			return p, mux.ForPane(p.id, Flash(FlashError, "%v", err))
		}
		p.attach(v)
		return p, mux.ForPane(p.id, p.stack.Push(v))
	}
	return p, mux.ForPane(p.id, p.stack.Update(msg))
}

// viewFor resolves a query through the registry and the env's factories. It
// does no I/O and never returns a nil view.
func (p *ViewPane) viewFor(q resource.Query) (View, error) {
	if p.env == nil || p.env.Registry == nil {
		return nil, fmt.Errorf("no backends configured")
	}
	_, spec, ok := p.env.Registry.Lookup(string(q.Kind))
	if !ok {
		return nil, fmt.Errorf("unknown kind: %s", q.Kind)
	}
	q.Kind = spec.Name
	if q.Window.IsZero() && p.env.Session != nil {
		q.Window = p.env.Session.Window
	}
	f := p.env.Factories[spec.Name]
	if f == nil {
		return nil, fmt.Errorf("no view factory for kind %s", spec.Name)
	}
	v, err := f(p.env, q)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, fmt.Errorf("no view for kind %s", spec.Name)
	}
	return v, nil
}

// View returns the breadcrumb line followed by the stack's h-1 lines.
func (p *ViewPane) View(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	line := p.crumbLine(w)
	if h == 1 {
		return line
	}
	return line + "\n" + p.stack.Render(w, h-1)
}

// crumbLine renders the crumbs with the last one bold, then the top view's
// filter, exactly w cells wide. Too much breadcrumb is cut from the left with
// an ellipsis, so the deepest (most specific) crumb stays visible.
func (p *ViewPane) crumbLine(w int) string {
	crumbs := make([]string, 0, 4)
	for _, c := range p.stack.Crumbs() {
		if c = strings.TrimSpace(c); c != "" {
			crumbs = append(crumbs, c)
		}
	}
	var t *theme.Theme
	if p.env != nil {
		t = p.env.Theme
	}

	var b strings.Builder
	for i, c := range crumbs {
		if i > 0 {
			b.WriteString(crumbSeparator)
		}
		switch {
		case t == nil:
			b.WriteString(c)
		case i == len(crumbs)-1:
			b.WriteString(t.Bold.Render(c))
		default:
			b.WriteString(t.Crumb.Render(c))
		}
	}
	if f := p.filterText(); f != "" {
		if len(crumbs) > 0 {
			b.WriteString(" · ")
		}
		text := "/" + f
		if t != nil {
			text = t.Dim.Render(text)
		}
		b.WriteString(text)
	}

	s := b.String()
	if width := render.Width(s); width > w {
		s = ansi.TruncateLeft(s, width-(w-1), "…")
	}
	return render.PadRight(s, w)
}

// filterText is the top view's filter, for the views that report one.
func (p *ViewPane) filterText() string {
	top := p.stack.Top()
	if top == nil {
		return ""
	}
	if fs, ok := top.(filterState); ok {
		return fs.FilterState()
	}
	return ""
}

// Title is the innermost crumb.
func (p *ViewPane) Title() string {
	if top := p.stack.Top(); top != nil {
		return top.Crumb()
	}
	return ""
}

// SetFocus records the pane focus. Views see keys only when their pane is
// focused; the mux decides that before calling Update.
func (p *ViewPane) SetFocus(focused bool) { p.focused = focused }

// RawKeys is false: a view pane lets the global keys through.
func (p *ViewPane) RawKeys() bool { return false }

// CopyLines is the top view's plain text.
func (p *ViewPane) CopyLines() []string {
	if top := p.stack.Top(); top != nil {
		return top.CopyLines()
	}
	return nil
}

// State describes the pane as the `:` command line of each stack entry, bottom
// first, stopping at the first entry that cannot describe itself.
func (p *ViewPane) State() mux.ContentState {
	st := mux.ContentState{Type: "view"}
	for _, v := range p.stack.views {
		cl, ok := v.(commandLiner)
		if !ok {
			break
		}
		st.Commands = append(st.Commands, cl.CommandLine())
	}
	return st
}

// Close cancels the pane context, which stops every stream the views started.
func (p *ViewPane) Close() error {
	p.cancel()
	return nil
}

// Capturing is true while the top view owns the keyboard.
func (p *ViewPane) Capturing() bool {
	if top := p.stack.Top(); top != nil {
		return top.Capturing()
	}
	return false
}

// Hints are the top view's hints.
func (p *ViewPane) Hints() []KeyHint {
	if top := p.stack.Top(); top != nil {
		return top.Hints()
	}
	return nil
}

// Subject is what the top view is about.
func (p *ViewPane) Subject() Subject {
	if top := p.stack.Top(); top != nil {
		return top.Subject()
	}
	return Subject{}
}

// IsRootMsg reports whether msg belongs to the root model rather than to the
// pane it was wrapped for. The mux model installs it as its root predicate.
func IsRootMsg(msg tea.Msg) bool {
	switch msg.(type) {
	case FlashMsg, OpenDialogMsg, CloseDialogMsg, RunCommandMsg, WindowChangedMsg, mux.OpMsg, tea.QuitMsg:
		return true
	}
	return false
}
