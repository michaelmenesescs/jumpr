package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"jumpr/internal/mux"
	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
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

// ViewPane is the mux.Content that shows jumpr views: one Stack of them, one
// breadcrumb line on top. It owns a context derived from the session's; Close
// cancels it, which stops every stream the views started.
type ViewPane struct {
	env     *Env
	id      mux.PaneID
	ctx     context.Context
	cancel  context.CancelFunc
	stack   *Stack
	focused bool

	// panel is the knowledge side panel; nil when no knowledge root is
	// configured. panelOn is the K toggle (on by default) and w is the last
	// interior width the pane was drawn at, which decides whether the panel
	// fits at all.
	panel   *sidePanel
	panelOn bool
	w       int

	// rail is the tool rail on the left; resetNext makes the next view the
	// rail opened replace the whole stack instead of piling onto it.
	rail      *navRail
	railOn    bool
	resetNext bool
	// preview is the live detail of the selected row, right of a list.
	preview preview
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
		env:     env,
		id:      id,
		ctx:     pctx,
		cancel:  cancel,
		stack:   NewStack(root),
		rail:    newNavRail(env),
		railOn:  true,
		preview: preview{on: true},
	}
	if root := knowledgeRoot(env); root != "" {
		p.panel = newSidePanel(env, root)
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

// Init starts the top view and, when there is one, the knowledge panel. Its
// messages come back wrapped for this pane.
func (p *ViewPane) Init() tea.Cmd {
	top := p.stack.Top()
	var cmd tea.Cmd
	if top != nil {
		cmd = mux.ForPane(p.id, top.Init())
	}
	cmd = tea.Batch(cmd, mux.ForPane(p.id, p.trackPreview()))
	if p.panel == nil {
		return cmd
	}
	return tea.Batch(cmd,
		mux.ForPane(p.id, p.panel.Init()),
		mux.ForPane(p.id, p.panel.trackSubject(knowledgeSubject(p.Subject()))))
}

// Update resolves OpenQueryMsg here, routes everything else to the stack and
// wraps the result for this pane in exactly one place. The mux unwraps a
// PaneMsg whose content is a root-level message and forwards it to the root
// (see IsRootMsg).
func (p *ViewPane) Update(msg tea.Msg) (mux.Content, tea.Cmd) {
	if r, ok := msg.(knowledgeRememberMsg); ok {
		return p, mux.ForPane(p.id, p.rememberCmd(r))
	}
	if cmd, handled := p.handlePreviewMsg(msg); handled {
		return p, mux.ForPane(p.id, cmd)
	}
	if k, ok := msg.(tea.KeyMsg); ok {
		if cmd, handled := p.handleShellKey(k); handled {
			return p, mux.ForPane(p.id, cmd)
		}
	}
	if p.panel != nil {
		if cmd, handled := p.panel.handleMsg(msg); handled {
			return p, mux.ForPane(p.id, cmd)
		}
		if k, ok := msg.(tea.KeyMsg); ok {
			if cmd, handled := p.handlePanelKey(k); handled {
				return p, mux.ForPane(p.id, cmd)
			}
		}
	}
	var cmd tea.Cmd
	switch m := msg.(type) {
	case OpenQueryMsg:
		v, err := p.viewFor(m.Query)
		if err != nil {
			cmd = Flash(FlashError, "%v", err)
		} else {
			p.attach(v)
			cmd = p.open(v)
		}
	case PushViewMsg:
		if p.resetNext && m.View != nil {
			p.attach(m.View)
			cmd = p.open(m.View)
		} else {
			cmd = p.stack.Update(msg)
		}
	default:
		cmd = p.stack.Update(msg)
	}
	if pc := p.trackPreview(); pc != nil {
		cmd = tea.Batch(cmd, pc)
	}
	return p, mux.ForPane(p.id, p.trackSubject(cmd))
}

// open pushes v, or makes it the only view of the stack when the tool rail
// asked for it: choosing a tool starts a fresh trail of breadcrumbs.
func (p *ViewPane) open(v View) tea.Cmd {
	if p.resetNext {
		p.resetNext = false
		return p.stack.Reset(v)
	}
	return p.stack.Push(v)
}

// handleShellKey gives the rail its keys: tab enters it, and while it has
// focus it owns every key. ctrl+p toggles the preview, ctrl+n the rail.
func (p *ViewPane) handleShellKey(k tea.KeyMsg) (tea.Cmd, bool) {
	if p.rail == nil {
		return nil, false
	}
	key := k.String()
	if p.rail.focused {
		cmd, opened := p.rail.handleKey(k)
		if opened {
			p.resetNext = true
		}
		return cmd, true
	}
	// Any key typed into the view ends a rail request that opened nothing.
	p.resetNext = false
	if p.Capturing() || (p.panel != nil && p.panel.focused) {
		return nil, false
	}
	switch key {
	case "tab":
		if p.railVisible() {
			p.rail.focused = true
			return nil, true
		}
	case "ctrl+p":
		p.preview.on = !p.preview.on
		return tea.Batch(p.trackPreview(), Flash(FlashInfo, "preview %s", onOff(p.preview.on))), true
	case "ctrl+n":
		p.railOn = !p.railOn
		return Flash(FlashInfo, "tool rail %s", onOff(p.railOn)), true
	}
	return nil, false
}

// railVisible reports whether the rail fits and is switched on.
func (p *ViewPane) railVisible() bool {
	return p.rail != nil && p.railOn && p.w >= railMinWidth
}

// trackSubject asks the panel to re-match when the top view's subject changed,
// batching the view's own command in front of it.
func (p *ViewPane) trackSubject(prev tea.Cmd) tea.Cmd {
	if p.panel == nil {
		return prev
	}
	next := p.panel.trackSubject(knowledgeSubject(p.Subject()))
	switch {
	case prev == nil:
		return next
	case next == nil:
		return prev
	}
	return tea.Batch(prev, next)
}

// handlePanelKey gives the keys that belong to the panel to the panel: tab
// enters it, K toggles it, and while it has focus it owns every key.
func (p *ViewPane) handlePanelKey(k tea.KeyMsg) (tea.Cmd, bool) {
	key := k.String()
	if cmd, handled := p.panel.handleKey(k); handled {
		return cmd, true
	}
	if p.Capturing() {
		return nil, false
	}
	keys := keymap.Default()
	if p.env != nil && p.env.Keys != nil {
		keys = p.env.Keys
	}
	if keys.Knowledge.Matches(key) {
		// K shows the panel and moves into it; K again hides it.
		p.panelOn = !p.panelOn
		p.panel.focused = p.panelOn && p.w >= panelMinWidth
		return Flash(FlashInfo, "knowledge panel %s", onOff(p.panelOn)), true
	}
	return nil, false
}

// onOff renders a toggle state for the flash line.
func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// panelVisible reports whether the panel fits and is switched on.
func (p *ViewPane) panelVisible() bool {
	return p.panel != nil && p.panelOn && p.w >= panelMinWidth
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

// View returns the breadcrumb line followed by the stack's h-1 lines, with the
// knowledge panel on the right when it fits and is on.
func (p *ViewPane) View(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	p.w = w
	line := p.crumbLine(w)
	if h == 1 {
		return line
	}
	bh := h - 1
	mainW := w
	var left, right string
	if p.railVisible() {
		left = p.rail.Render(railWidth, bh)
		mainW -= railWidth
	}
	switch {
	case p.panelVisible():
		right = p.panel.Render(panelWidth, bh)
		mainW -= panelWidth
	case p.previewVisible(mainW):
		pw := mainW * 55 / 100
		right = p.renderPreview(pw, bh)
		mainW -= pw
	}
	body := p.stack.Render(mainW, bh)
	if left != "" {
		body = joinRows(left, body, bh)
	}
	if right != "" {
		body = joinRows(body, right, bh)
	}
	return line + "\n" + body
}

// joinRows puts two equally tall blocks side by side.
func joinRows(left, right string, h int) string {
	l := strings.Split(left, "\n")
	r := strings.Split(right, "\n")
	out := make([]string, h)
	for i := 0; i < h; i++ {
		var a, b string
		if i < len(l) {
			a = l[i]
		}
		if i < len(r) {
			b = r[i]
		}
		out[i] = a + b
	}
	return strings.Join(out, "\n")
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
	p.preview.close()
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
	if p.rail != nil && p.rail.focused {
		return p.rail.hints()
	}
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
	case FlashMsg, OpenDialogMsg, CloseDialogMsg, RunCommandMsg, OpenCommandBarMsg, WindowChangedMsg, mux.OpMsg, tea.QuitMsg:
		return true
	}
	return false
}
