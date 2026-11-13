package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/tui/theme"
)

const (
	// flashDuration is how long a flash line stays on screen.
	flashDuration = 5 * time.Second
	// healthInterval is the status line's health poll period.
	healthInterval = 30 * time.Second
	// crumbSeparator joins the view stack's breadcrumbs.
	crumbSeparator = " › "
)

// flashTickMsg expires the flash with the matching sequence number. A newer
// flash bumps the sequence, so a stale tick is ignored.
type flashTickMsg struct{ seq int }

// Root is the top-level tea.Model: it owns the one view stack, the command
// bar, the modal dialog, the flash line and the status line. It does no I/O;
// every remote call runs in a tea.Cmd. The root never imports internal/tui/
// views (that package imports tui), so the default view factory is injected.
type Root struct {
	env      *Env
	cmds     *Commands
	factory  Factory
	stack    *Stack
	bar      *CmdBar
	dialog   Dialog
	w, h     int
	flash    FlashMsg
	flashOn  bool
	flashSeq int
	health   map[string]backend.HealthState
	startErr string
}

var _ tea.Model = (*Root)(nil)

// NewRoot builds the root model. defaultFactory builds any view the env has no
// specific factory for; app passes a closure around views.NewTable. start is
// the first query, usually the host list.
func NewRoot(env *Env, cmds *Commands, defaultFactory Factory, start resource.Query) *Root {
	r := &Root{
		env:     env,
		cmds:    cmds,
		factory: defaultFactory,
		bar:     NewCmdBar(env, cmds),
		health:  make(map[string]backend.HealthState),
	}
	if env != nil && env.Session != nil && start.Window.IsZero() {
		start.Window = env.Session.Window
	}
	v, err := r.viewFor(start)
	if err != nil {
		r.startErr = err.Error()
		v = r.noticeView("error", []string{err.Error()})
	}
	r.stack = NewStack(v)
	return r
}

// viewFor resolves a query through the registry and the factories and returns
// the view for it. It does no I/O.
func (r *Root) viewFor(q resource.Query) (View, error) {
	if r.env == nil || r.env.Registry == nil {
		return nil, fmt.Errorf("no backends configured")
	}
	_, spec, ok := r.env.Registry.Lookup(string(q.Kind))
	if !ok {
		return nil, fmt.Errorf("unknown kind: %s", q.Kind)
	}
	if q.Backend != "" {
		if _, found := r.env.Registry.Get(q.Backend); !found {
			return nil, fmt.Errorf("unknown backend: %s", q.Backend)
		}
	}
	q.Kind = spec.Name
	if q.Window.IsZero() && r.env.Session != nil {
		q.Window = r.env.Session.Window
	}
	f := r.factory
	if r.env.Factories != nil {
		if named, found := r.env.Factories[spec.Name]; found && named != nil {
			f = named
		}
	}
	if f == nil {
		return nil, fmt.Errorf("no view factory for kind %s", spec.Name)
	}
	v, err := f(r.env, q)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, fmt.Errorf("no view for kind %s", spec.Name)
	}
	return v, nil
}

// Init starts the first listing, the first health poll and reports a failed
// start query.
func (r *Root) Init() tea.Cmd {
	var cmds []tea.Cmd
	if r.startErr != "" {
		cmds = append(cmds, Flash(FlashError, "%s", r.startErr))
	}
	if top := r.stack.Top(); top != nil {
		if c := top.Init(); c != nil {
			cmds = append(cmds, c)
		}
	}
	if c := HealthPoll(r.backends(), healthInterval); c != nil {
		cmds = append(cmds, c)
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// backends returns the registry of the environment, if any.
func (r *Root) backends() *backend.Registry {
	if r.env == nil {
		return nil
	}
	return r.env.Registry
}

// Update implements tea.Model. Key routing is: open dialog, command bar, the
// capturing view, then the global keys, then the stack.
func (r *Root) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		return r, r.handleKey(k)
	}
	return r, r.handleMsg(msg)
}

func (r *Root) handleKey(k tea.KeyMsg) tea.Cmd {
	if r.dialog != nil {
		// A modal is dismissed with esc whatever it shows; closing it here
		// rather than through CloseDialog keeps the dismissal immediate.
		if k.String() == "esc" {
			r.dialog = nil
			return nil
		}
		d, cmd := r.dialog.Update(k)
		r.dialog = d
		return cmd
	}
	if r.bar != nil && r.bar.Active() {
		cmd, _ := r.bar.Update(k)
		return cmd
	}
	if top := r.stack.Top(); top != nil && top.Capturing() {
		return r.stack.Update(k)
	}
	switch k.String() {
	case ":":
		r.bar.Open("")
		return nil
	case "?":
		return r.openHelp()
	case "ctrl+c":
		return tea.Quit
	case "q":
		if r.stack.Len() == 1 {
			return tea.Quit
		}
	}
	return r.stack.Update(k)
}

func (r *Root) handleMsg(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		r.w, r.h = m.Width, m.Height
		return nil
	case CloseDialogMsg:
		r.dialog = nil
		return nil
	case OpenDialogMsg:
		r.dialog = m.Dialog
		return nil
	case RunCommandMsg:
		if r.cmds == nil {
			return nil
		}
		return r.cmds.Exec(r.env, m.Line)
	case OpenQueryMsg:
		v, err := r.viewFor(m.Query)
		if err != nil {
			return Flash(FlashError, "%v", err)
		}
		return r.stack.Push(v)
	case FlashMsg:
		r.flash = m
		r.flashOn = true
		r.flashSeq++
		seq := r.flashSeq
		return tea.Tick(flashDuration, func(time.Time) tea.Msg { return flashTickMsg{seq: seq} })
	case flashTickMsg:
		if m.seq == r.flashSeq {
			r.flashOn = false
		}
		return nil
	case HealthMsg:
		r.health[m.Backend] = m.Health.State
		return nil
	case WindowChangedMsg:
		if r.env != nil && r.env.Session != nil {
			r.env.Session.Window = m.Window
		}
		r.stack.Update(msg)
		return Flash(FlashInfo, "window: %s", m.Window.String())
	case HelpRequestedMsg:
		return r.openHelp()
	case KindsRequestedMsg:
		return r.stack.Push(r.kindsView())
	}
	return r.stack.Update(msg)
}

// openHelp shows the help dialog: the global keys, then the focused view's
// hints.
func (r *Root) openHelp() tea.Cmd {
	sections := []HelpSection{{Title: "Global", Hints: globalHints()}}
	if top := r.stack.Top(); top != nil {
		if hints := top.Hints(); len(hints) > 0 {
			sections = append(sections, HelpSection{Title: top.Crumb(), Hints: hints})
		}
	}
	r.dialog = NewHelpDialog(r.env.Theme, "help", sections)
	return nil
}

// globalHints are the keys the root handles whatever the view.
func globalHints() []KeyHint {
	return []KeyHint{
		{Key: ":", Text: "command"},
		{Key: "?", Text: "help"},
		{Key: "ctrl+c", Text: "quit"},
		{Key: "q", Text: "quit"},
	}
}

// kindsView lists every registered kind.
func (r *Root) kindsView() View {
	var lines []string
	if r.env != nil && r.env.Registry != nil {
		for _, spec := range r.env.Registry.Kinds() {
			line := string(spec.Name)
			if len(spec.Aliases) > 0 {
				line += "  " + strings.Join(spec.Aliases, ",")
			}
			line += "  " + spec.Title
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		lines = []string{"no kinds registered"}
	}
	return r.noticeView("kinds", lines)
}

// noticeView is a read-only list of lines with a title; esc pops it.
func (r *Root) noticeView(title string, lines []string) View {
	var t *theme.Theme
	if r.env != nil {
		t = r.env.Theme
	}
	return &noticeView{t: t, title: title, lines: lines}
}

// View renders exactly width×height cells. Before the first WindowSizeMsg it
// returns "".
func (r *Root) View() string {
	if r.w <= 0 || r.h <= 0 {
		return ""
	}
	w, h := r.w, r.h

	var b strings.Builder
	b.WriteString(RenderStatus(r.env.Theme, r.statusData(), w))
	b.WriteString("\n")

	bodyH := max(h-3, 0)
	listH := max(bodyH-1, 0)
	if bodyH > 0 {
		b.WriteString(r.renderCrumbs(w))
		b.WriteString("\n")
	}
	if listH > 0 {
		b.WriteString(r.stack.Render(w, listH))
		b.WriteString("\n")
	}
	b.WriteString(r.renderInputLine(w))
	b.WriteString("\n")
	b.WriteString(RenderHints(r.env.Theme, r.hints(), w))

	out := render.Fit(b.String(), w, h)
	if r.dialog != nil {
		return RenderDialog(r.env.Theme, r.dialog, w, h, out)
	}
	return out
}

// renderInputLine is the command bar while it is open, the flash otherwise.
func (r *Root) renderInputLine(w int) string {
	if r.bar != nil && r.bar.Active() {
		return r.bar.View(w)
	}
	if r.flashOn {
		return RenderFlash(r.env.Theme, r.flash, w)
	}
	return render.PadRight("", w)
}

// hints are the focused view's hints, or the global ones when it has none.
func (r *Root) hints() []KeyHint {
	if top := r.stack.Top(); top != nil {
		if h := top.Hints(); len(h) > 0 {
			return h
		}
	}
	return globalHints()
}

// renderCrumbs renders the breadcrumb line exactly w cells wide, with the last
// crumb bold.
func (r *Root) renderCrumbs(w int) string {
	crumbs := make([]string, 0, 4)
	for _, c := range r.stack.Crumbs() {
		if c = strings.TrimSpace(c); c != "" {
			crumbs = append(crumbs, c)
		}
	}
	if len(crumbs) == 0 {
		return render.PadRight("", w)
	}
	var t *theme.Theme
	if r.env != nil {
		t = r.env.Theme
	}
	parts := make([]string, len(crumbs))
	for i, c := range crumbs {
		last := i == len(crumbs)-1
		if t == nil {
			parts[i] = c
			continue
		}
		if last {
			parts[i] = t.Bold.Render(c)
		} else {
			parts[i] = t.Crumb.Render(c)
		}
	}
	return render.PadRight(render.Truncate(strings.Join(parts, crumbSeparator), w), w)
}

// statusData builds the status line's contents from the session and the last
// health results.
func (r *Root) statusData() StatusData {
	d := StatusData{}
	if r.env != nil && r.env.Session != nil {
		d.Session = r.env.Session.Name
		d.ReadOnly = r.env.Session.ReadOnly
		if !r.env.Session.Window.IsZero() {
			d.Window = r.env.Session.Window.String()
		}
	}
	if r.backends() != nil {
		for _, b := range r.backends().Backends() {
			state, ok := r.health[b.Name()]
			if !ok {
				state = backend.HealthUnconfigured
			}
			d.Health = append(d.Health, HealthDot{Name: b.Name(), State: state})
		}
	}
	return d
}

// noticeView is a minimal View for the start error and the kinds list. It is
// deliberately in package tui: the root may not import internal/tui/views.
type noticeView struct {
	t     *theme.Theme
	title string
	lines []string
}

var _ View = (*noticeView)(nil)

func (n *noticeView) Init() tea.Cmd { return nil }

func (n *noticeView) Update(msg tea.Msg) (View, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return n, nil
	}
	switch k.String() {
	case "esc", "q":
		return n, Pop()
	}
	return n, nil
}

func (n *noticeView) Render(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	lines := make([]string, 0, len(n.lines)+2)
	title := n.title
	if n.t != nil {
		title = n.t.Header.Render(n.title)
	}
	lines = append(lines, title)
	lines = append(lines, "")
	lines = append(lines, n.lines...)
	return render.Fit(strings.Join(lines, "\n"), w, h)
}

func (n *noticeView) Crumb() string { return n.title }

func (n *noticeView) Hints() []KeyHint {
	return []KeyHint{{Key: "esc", Text: "back"}}
}

func (n *noticeView) Subject() Subject { return Subject{} }

func (n *noticeView) Capturing() bool { return false }

func (n *noticeView) CopyLines() []string {
	return append([]string{strings.ToUpper(n.title)}, n.lines...)
}
