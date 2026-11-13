package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"jumper/internal/backend"
	"jumper/internal/clip"
	"jumper/internal/mux"
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
	// defaultPrefix is the prefix key used when the caller passes none.
	defaultPrefix = "ctrl+a"
)

// flashTickMsg expires the flash with the matching sequence number. A newer
// flash bumps the sequence, so a stale tick is ignored.
type flashTickMsg struct{ seq int }

// copyDoneMsg reports the result of the clipboard write the root started when
// the user yanked. Copy is I/O, so it never runs inside Update.
type copyDoneMsg struct {
	lines int
	err   error
}

// Root is the top-level tea.Model: it owns the multiplexer, the command bar,
// the modal dialog, the flash line and the status line. It does no I/O; every
// remote call runs in a tea.Cmd. The root never imports internal/tui/views
// (that package imports tui), so the default view factory is injected.
type Root struct {
	env     *Env
	cmds    *Commands
	factory Factory
	mux     *mux.Model
	prefix  string
	bar     *CmdBar
	dialog  Dialog
	prompt  Dialog   // interactive question (host key, passphrase); above dialog
	pending []Dialog // questions that arrived while prompt was on screen
	w, h    int

	flash    FlashMsg
	flashOn  bool
	flashSeq int
	health   map[string]backend.HealthState

	start    resource.Query
	paneKind resource.KindName
	panes    int
	initCmd  tea.Cmd
	startErr string

	clip     clip.Clipboard
	copy     *mux.CopyMode
	copyPane mux.PaneID
}

var _ tea.Model = (*Root)(nil)

// NewRoot builds the root model over one mux session. defaultFactory builds any
// view the env has no specific factory for; app passes a closure around
// views.NewTable. start is the first query, usually the host list. prefixKey is
// the Bubble Tea key string of the session prefix ("ctrl+a"); empty means the
// default.
func NewRoot(env *Env, cmds *Commands, defaultFactory Factory, start resource.Query, prefixKey string) *Root {
	if prefixKey == "" {
		prefixKey = defaultPrefix
	}
	if env != nil && env.Session != nil && start.Window.IsZero() {
		start.Window = env.Session.Window
	}
	r := &Root{
		env:      env,
		cmds:     cmds,
		factory:  defaultFactory,
		prefix:   prefixKey,
		bar:      NewCmdBar(env, cmds),
		health:   make(map[string]backend.HealthState),
		start:    start,
		paneKind: start.Kind,
	}
	name := "main"
	if env != nil && env.Session != nil && env.Session.Name != "" {
		name = env.Session.Name
	}
	m := mux.NewModel(mux.NewSession(name), r.paneContent, prefixKey, stylesFor(env))
	m.SetRootPredicate(IsRootMsg)
	r.mux = m
	_, r.initCmd = m.AddPane(mux.ContentState{}, "h")
	return r
}

// SetClipboard installs the clipboard the root yanks into. app builds
// clip.Multi(pbcopy, osc52(out, inTmux)) and calls this once; without one a
// yank flashes a warning instead.
func (r *Root) SetClipboard(c clip.Clipboard) { r.clip = c }

// stylesFor builds the four mux pane styles from the theme.
func stylesFor(env *Env) mux.Styles {
	if env == nil || env.Theme == nil {
		return mux.Styles{}
	}
	t := env.Theme
	return mux.Styles{
		Border:        t.Border,
		BorderFocused: t.BorderFocused,
		Title:         t.Title,
		TitleFocused:  t.Accent.Bold(true),
	}
}

// paneContent is the mux pane factory. The first pane shows the start query;
// later panes open the kind of the pane they were split from.
func (r *Root) paneContent(id mux.PaneID, st mux.ContentState) (mux.Content, tea.Cmd) {
	q := r.start
	if r.panes > 0 {
		q = resource.Query{Kind: r.paneKind}
		if q.Kind == "" {
			q.Kind = r.start.Kind
		}
		if r.env != nil && r.env.Session != nil {
			q.Window = r.env.Session.Window
		}
	}
	r.panes++
	v, err := r.viewFor(q)
	if err != nil {
		if r.startErr == "" {
			r.startErr = err.Error()
		}
		v = r.noticeView("error", []string{err.Error()})
	}
	_ = st // persisted pane state is restored by task 024
	return NewViewPane(r.env, id, nil, v), nil
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

// Init starts the first pane, the first health poll and reports a failed start
// query.
func (r *Root) Init() tea.Cmd {
	var cmds []tea.Cmd
	if r.startErr != "" {
		cmds = append(cmds, Flash(FlashError, "%s", r.startErr))
	}
	if r.initCmd != nil {
		cmds = append(cmds, r.initCmd)
	}
	if c := HealthPoll(r.backends(), healthInterval); c != nil {
		cmds = append(cmds, c)
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// glyphs returns the active set: the one internal/app resolved from config and
// the environment, or unicode when no set was handed in.
func (r *Root) glyphs() render.GlyphSet {
	if r.env != nil && r.env.Glyphs.Name != "" {
		return r.env.Glyphs
	}
	set, _ := render.Glyphs("unicode")
	return set
}

// backends returns the registry of the environment, if any.
func (r *Root) backends() *backend.Registry {
	if r.env == nil {
		return nil
	}
	return r.env.Registry
}

// Update implements tea.Model. Key routing is: open dialog, command bar, the
// mux for prefix and raw-key keys, the global keys, then the mux.
func (r *Root) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		return r, r.handleKey(k)
	}
	return r, r.handleMsg(msg)
}

func (r *Root) handleKey(k tea.KeyMsg) tea.Cmd {
	if r.prompt != nil {
		// A prompt owns every key: it is the one thing a background job is
		// waiting for. It answers esc itself (reject / cancel).
		d, cmd := r.prompt.Update(k)
		r.prompt = d
		return cmd
	}
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
	if r.copy != nil && k.String() != r.prefix {
		return r.copyKey(k)
	}
	if r.muxTakesKey(k) {
		return r.routeMux(k)
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
		if r.atRest() {
			return tea.Quit
		}
	}
	return r.routeMux(k)
}

// muxTakesKey reports whether a key must go to the mux before the root's global
// keys apply: the prefix is armed, the key is the prefix itself, or the focused
// content wants every key.
func (r *Root) muxTakesKey(k tea.KeyMsg) bool {
	if r.mux == nil {
		return false
	}
	if r.mux.PrefixArmed() || k.String() == r.prefix {
		return true
	}
	_, c := r.mux.Focused()
	if c == nil {
		return false
	}
	if c.RawKeys() {
		return true
	}
	if cp, ok := c.(interface{ Capturing() bool }); ok && cp.Capturing() {
		return true
	}
	return false
}

// atRest reports whether a `q` would lose nothing: one window, one pane and a
// single view on the pane's stack.
func (r *Root) atRest() bool {
	sess := r.mux.Session()
	if sess == nil || len(sess.Windows) != 1 {
		return false
	}
	w := sess.ActiveWindow()
	if w == nil || w.Layout.Count() != 1 {
		return false
	}
	_, c := r.mux.Focused()
	vp, ok := c.(*ViewPane)
	if !ok {
		return false
	}
	return vp.stack.Len() == 1
}

// routeMux hands msg to the mux and handles anything the mux hands back.
func (r *Root) routeMux(msg tea.Msg) tea.Cmd {
	if r.mux == nil {
		return nil
	}
	out, cmd := r.mux.Update(msg)
	if out == nil {
		return cmd
	}
	var next tea.Cmd
	if k, ok := out.(tea.KeyMsg); ok {
		next = r.handleKey(k)
	} else {
		next = r.handleMsg(out)
	}
	return tea.Batch(cmd, next)
}

// toFocused sends msg to the focused pane only.
func (r *Root) toFocused(msg tea.Msg) tea.Cmd {
	id, c := r.mux.Focused()
	if c == nil {
		return nil
	}
	return r.handlePaneMsg(mux.PaneMsg{Pane: id, Msg: msg})
}

// handlePaneMsg delivers a pane-scoped message to its content and returns the
// command the content produced. The root routes these itself instead of going
// through mux.Update: the mux stores the updated content but drops that
// command, and dropping it would stop a view's stream after its first batch.
// Contents are values owned by the mux; ViewPane always returns itself, so the
// stored value stays current.
func (r *Root) handlePaneMsg(pm mux.PaneMsg) tea.Cmd {
	if IsRootMsg(pm.Msg) {
		return r.handleMsg(pm.Msg)
	}
	c, ok := r.mux.Contents()[pm.Pane]
	if !ok || c == nil {
		return nil
	}
	_, cmd := c.Update(pm.Msg)
	return cmd
}

func (r *Root) handleMsg(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		r.w, r.h = m.Width, m.Height
		r.mux.SetSize(r.w, r.bodyHeight())
		if r.copy != nil {
			if rect, ok := r.copyInterior(); ok {
				r.copy.SetSize(rect.W, rect.H)
			} else {
				r.copy = nil
			}
		}
		return nil
	case tea.QuitMsg:
		return tea.Quit
	case CloseDialogMsg:
		if r.prompt != nil {
			r.prompt = r.nextPrompt()
			return nil
		}
		r.dialog = nil
		return nil
	case PromptMsg:
		return r.showPrompt(m)
	case OpenDialogMsg:
		r.dialog = m.Dialog
		return nil
	case RunCommandMsg:
		if r.cmds == nil {
			return nil
		}
		return r.cmds.Exec(r.env, m.Line)
	case OpenQueryMsg:
		if m.Query.Kind != "" {
			r.paneKind = m.Query.Kind
		}
		return r.toFocused(m)
	case mux.OpMsg:
		return r.handleOp(m)
	case mux.PaneMsg:
		return r.handlePaneMsg(m)
	case forcedOpMsg:
		return r.routeMux(m.Op)
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
		r.routeMux(msg)
		return Flash(FlashInfo, "window: %s", m.Window.String())
	case HelpRequestedMsg:
		return r.openHelp()
	case KindsRequestedMsg:
		return r.pushFocused(r.kindsView())
	case copyDoneMsg:
		if m.err != nil {
			return Flash(FlashWarn, "copy: %v", m.err)
		}
		if m.lines == 1 {
			return Flash(FlashOK, "yanked 1 line")
		}
		return Flash(FlashOK, "yanked %d lines", m.lines)
	}
	return r.routeMux(msg)
}

// showPrompt puts an interactive question on screen, above any dialog. A
// question that arrives while another is up waits its turn; Ask serializes
// callers anyway, so this only guards against a second source.
func (r *Root) showPrompt(m PromptMsg) tea.Cmd {
	d := NewInteractDialog(themeOf(r.env), m.Prompt, m.Reply)
	if r.prompt != nil {
		r.pending = append(r.pending, d)
		return nil
	}
	r.prompt = d
	return nil
}

// nextPrompt pops the next queued question, or returns nil when none is left.
func (r *Root) nextPrompt() Dialog {
	if len(r.pending) == 0 {
		return nil
	}
	d := r.pending[0]
	r.pending = r.pending[1:]
	return d
}

// pushFocused pushes v onto the focused pane's view stack.
func (r *Root) pushFocused(v View) tea.Cmd {
	if v == nil {
		return nil
	}
	return r.toFocused(PushViewMsg{View: v})
}

// handleOp executes the operations the mux hands back to its owner, and routes
// the ones the mux executes itself.
func (r *Root) handleOp(m mux.OpMsg) tea.Cmd {
	switch m.Op {
	case mux.OpCommand:
		r.bar.Open("")
		return nil
	case mux.OpHelp:
		return r.openHelp()
	case mux.OpRenameWindow:
		if name := strings.TrimSpace(m.Arg); name != "" {
			r.mux.Session().RenameWindow(name)
			return Flash(FlashOK, "window renamed: %s", name)
		}
		return r.openRenameWindow()
	case mux.OpRenameSession:
		if name := strings.TrimSpace(m.Arg); name != "" {
			r.renameSession(name)
			return Flash(FlashOK, "session renamed: %s", name)
		}
		return r.openRenameSession()
	case mux.OpWindowList:
		return r.openWindowList()
	case mux.OpDetach:
		return Flash(FlashInfo, "detach needs the session server (coming in M5); use :q to quit")
	case mux.OpCopyMode:
		return r.openCopyMode()
	case mux.OpSync, mux.OpSSH, mux.OpSessionList:
		return Flash(FlashInfo, "not available yet")
	case mux.OpKillPane:
		if r.focusedBusy() {
			r.dialog = NewConfirmDialog(themeOf(r.env), "kill pane",
				"This pane has work running. Kill it anyway?", confirmOp(mux.OpKillPane))
			return nil
		}
		return r.routeMux(m)
	case mux.OpKillWindow:
		if r.windowBusy() {
			r.dialog = NewConfirmDialog(themeOf(r.env), "kill window",
				"A pane in this window has work running. Kill the window anyway?", confirmOp(mux.OpKillWindow))
			return nil
		}
		return r.routeMux(m)
	case mux.OpSplitH, mux.OpSplitV, mux.OpZoom, mux.OpFocus, mux.OpResize, mux.OpSwap,
		mux.OpLayoutNext, mux.OpNewWindow, mux.OpSelectWindow, mux.OpNextWindow,
		mux.OpPrevWindow, mux.OpSendPrefix:
		return r.routeMux(m)
	}
	return nil
}

// openCopyMode freezes the focused pane's text and opens copy mode over that
// pane. An empty pane flashes instead of opening an empty window.
func (r *Root) openCopyMode() tea.Cmd {
	if r.mux == nil {
		return nil
	}
	id, c := r.mux.Focused()
	if c == nil {
		return nil
	}
	lines := c.CopyLines()
	if len(lines) == 0 {
		return Flash(FlashInfo, "nothing to copy")
	}
	r.copyPane = id
	rect, ok := r.copyInterior()
	if !ok {
		return Flash(FlashWarn, "pane too small for copy mode")
	}
	r.copy = mux.NewCopyMode(lines, rect.W, rect.H)
	return nil
}

// copyKey feeds one key to copy mode and finishes it on y or quit. The
// clipboard write is I/O, so it runs in a tea.Cmd and reports back with
// copyDoneMsg; a yank with no selection of anything is dropped.
func (r *Root) copyKey(k tea.KeyMsg) tea.Cmd {
	if r.copy == nil {
		return nil
	}
	done, text := r.copy.Update(k.String())
	if !done {
		return nil
	}
	r.copy = nil
	if text == "" {
		return nil
	}
	c := r.clip
	if c == nil {
		return Flash(FlashWarn, "no clipboard configured")
	}
	lines := strings.Count(text, "\n") + 1
	return func() tea.Msg { return copyDoneMsg{lines: lines, err: c.Copy(text)} }
}

// copyInterior is the cell area copy mode draws in: the focused pane's interior,
// borders excluded. It reports false when that pane is gone or too small.
func (r *Root) copyInterior() (mux.Rect, bool) {
	if r.mux == nil || r.mux.Session() == nil {
		return mux.Rect{}, false
	}
	rects := r.mux.Session().Rects(mux.Rect{X: 0, Y: 0, W: r.w, H: r.bodyHeight()})
	rect, ok := rects[r.copyPane]
	if !ok || rect.W <= 0 || rect.H <= 0 {
		return mux.Rect{}, false
	}
	if len(rects) > 1 {
		rect = mux.Rect{X: rect.X + 1, Y: rect.Y + 1, W: rect.W - 2, H: rect.H - 2}
	}
	if rect.W <= 0 || rect.H <= 0 {
		return mux.Rect{}, false
	}
	return rect, true
}

// overlayCopy draws copy mode over the focused pane's interior in body, leaving
// the rest of the mux view untouched.
func (r *Root) overlayCopy(body string) string {
	rect, ok := r.copyInterior()
	if !ok {
		r.copy = nil
		return body
	}
	lines := strings.Split(body, "\n")
	over := strings.Split(r.copy.View(), "\n")
	for i := 0; i < rect.H && i < len(over); i++ {
		y := rect.Y + i
		if y < 0 || y >= len(lines) {
			continue
		}
		line := lines[y]
		lines[y] = ansi.Cut(line, 0, rect.X) + over[i] + ansi.Cut(line, rect.X+rect.W, render.Width(line))
	}
	return strings.Join(lines, "\n")
}

// busyContent is the optional Content method that asks for a kill confirmation.
type busyContent interface{ Busy() bool }

// forcedOpMsg is an operation the user already confirmed. It skips the busy
// check so a confirmed kill cannot ask again.
type forcedOpMsg struct{ Op mux.OpMsg }

// confirmOp returns the command a y/N dialog runs when the user answers yes.
func confirmOp(op string) tea.Cmd {
	return func() tea.Msg { return forcedOpMsg{Op: mux.OpMsg{Op: op}} }
}

// focusedBusy reports whether the focused content has work running.
func (r *Root) focusedBusy() bool {
	_, c := r.mux.Focused()
	if b, ok := c.(busyContent); ok {
		return b.Busy()
	}
	return false
}

// windowBusy reports whether any content of the active window has work running.
func (r *Root) windowBusy() bool {
	w := r.mux.Session().ActiveWindow()
	if w == nil {
		return false
	}
	contents := r.mux.Contents()
	for _, id := range w.Layout.Panes() {
		if b, ok := contents[id].(busyContent); ok && b.Busy() {
			return true
		}
	}
	return false
}

// renameSession renames the session in both places that hold the name.
func (r *Root) renameSession(name string) {
	r.mux.Session().Rename(name)
	if r.env != nil && r.env.Session != nil {
		r.env.Session.Name = name
	}
}

// openRenameWindow prompts for the active window's name.
func (r *Root) openRenameWindow() tea.Cmd {
	cur := ""
	if w := r.mux.Session().ActiveWindow(); w != nil && !w.AutoName {
		cur = w.Name
	}
	r.dialog = NewPromptDialog(themeOf(r.env), "rename window", "window name:", cur, func(name string) tea.Cmd {
		return opCmd(mux.OpRenameWindow, name)
	})
	return nil
}

// openRenameSession prompts for the session's name.
func (r *Root) openRenameSession() tea.Cmd {
	cur := r.mux.Session().Name
	r.dialog = NewPromptDialog(themeOf(r.env), "rename session", "session name:", cur, func(name string) tea.Cmd {
		return opCmd(mux.OpRenameSession, name)
	})
	return nil
}

// openWindowList shows every window; enter selects the highlighted one.
func (r *Root) openWindowList() tea.Cmd {
	tabs := r.mux.Tabs()
	items := make([]ListItem, 0, len(tabs))
	for _, tab := range tabs {
		detail := ""
		if tab.Active {
			detail = "active"
		}
		items = append(items, ListItem{Label: fmt.Sprintf("%d: %s", tab.Index, tab.Name), Detail: detail})
	}
	r.dialog = NewListDialog(themeOf(r.env), "windows", items, func(i int) tea.Cmd {
		return opCmd(mux.OpSelectWindow, strconv.Itoa(i))
	})
	return nil
}

// themeOf returns env's theme, or nil when there is no env.
func themeOf(env *Env) *theme.Theme {
	if env == nil {
		return nil
	}
	return env.Theme
}

// openHelp shows the help dialog: the global keys, the mux key table and the
// focused view's hints.
func (r *Root) openHelp() tea.Cmd {
	sections := []HelpSection{
		{Title: "Global", Hints: globalHints()},
		MuxHelpSection(PrefixDisplay(r.prefix)),
	}
	if hints := r.paneHints(); len(hints) > 0 {
		title := ""
		if _, c := r.mux.Focused(); c != nil {
			if p, ok := c.(*ViewPane); ok {
				title = p.Title()
			}
		}
		sections = append(sections, HelpSection{Title: title, Hints: hints})
	}
	r.dialog = NewHelpDialog(themeOf(r.env), "help", sections)
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
	return &noticeView{t: themeOf(r.env), title: title, lines: lines}
}

// bodyHeight is the number of lines the mux may draw.
func (r *Root) bodyHeight() int {
	return max(r.h-3, 0)
}

// View renders exactly width×height cells. Before the first WindowSizeMsg it
// returns "".
func (r *Root) View() string {
	if r.w <= 0 || r.h <= 0 {
		return ""
	}
	w, h := r.w, r.h

	var b strings.Builder
	b.WriteString(RenderStatus(themeOf(r.env), r.statusData(), w))
	b.WriteString("\n")

	bodyH := r.bodyHeight()
	if bodyH > 0 {
		body := r.mux.View()
		if r.copy != nil {
			body = r.overlayCopy(body)
		}
		b.WriteString(body)
		b.WriteString("\n")
	}
	b.WriteString(r.renderInputLine(w))
	b.WriteString("\n")
	b.WriteString(RenderHints(themeOf(r.env), r.hints(), w))

	out := render.FitWith(r.glyphs(), b.String(), w, h)
	if r.dialog != nil {
		out = RenderDialog(themeOf(r.env), r.dialog, w, h, out)
	}
	if r.prompt != nil {
		out = RenderDialog(themeOf(r.env), r.prompt, w, h, out)
	}
	return out
}

// renderInputLine is the command bar while it is open, the flash otherwise.
func (r *Root) renderInputLine(w int) string {
	if r.bar != nil && r.bar.Active() {
		return r.bar.View(w)
	}
	if r.flashOn {
		return RenderFlash(themeOf(r.env), r.flash, w)
	}
	return render.PadRight("", w)
}

// paneHints are the focused content's hints, when it reports any.
func (r *Root) paneHints() []KeyHint {
	if r.mux == nil {
		return nil
	}
	_, c := r.mux.Focused()
	if c == nil {
		return nil
	}
	if h, ok := c.(interface{ Hints() []KeyHint }); ok {
		return h.Hints()
	}
	return nil
}

// hints are the focused pane's hints, or the global ones when it has none.
func (r *Root) hints() []KeyHint {
	if h := r.paneHints(); len(h) > 0 {
		return h
	}
	return globalHints()
}

// statusData builds the status line's contents from the session, the window
// tabs and the last health results.
func (r *Root) statusData() StatusData {
	d := StatusData{Prefix: PrefixDisplay(r.prefix), Glyphs: r.glyphs()}
	if sess := r.mux.Session(); sess != nil {
		d.Session = sess.Name
		for _, tab := range r.mux.Tabs() {
			d.Windows = append(d.Windows, WindowTab{Index: tab.Index, Name: tab.Name, Active: tab.Active})
		}
	}
	if r.env != nil && r.env.Session != nil {
		d.ReadOnly = r.env.Session.ReadOnly
		if !r.env.Session.Window.IsZero() {
			d.Window = r.env.Session.Window.String()
		}
	}
	if r.mux.PrefixArmed() {
		if t := themeOf(r.env); t != nil {
			d.Prefix = t.Accent.Bold(true).Render(d.Prefix)
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
