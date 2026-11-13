package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"jumper/internal/backend"
	"jumper/internal/bus"
	"jumper/internal/clip"
	"jumper/internal/investigation"
	"jumper/internal/mux"
	"jumper/internal/mux/termpane"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/targets"
	"jumper/internal/transport"
	"jumper/internal/tui/keymap"
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
	// autosaveInterval is how often a dirty session is written. Saves are
	// debounced by the dirty flag, so a burst of splits costs one file write.
	autosaveInterval = time.Second
)

// autosaveMsg asks the root to write the session file if a change is pending.
type autosaveMsg struct{}

// autosaveTick schedules the next autosave check.
func autosaveTick() tea.Cmd {
	return tea.Tick(autosaveInterval, func(time.Time) tea.Msg { return autosaveMsg{} })
}

// sessionOp reports whether a multiplexer operation changes what a restart has
// to bring back.
func sessionOp(op string) bool {
	switch op {
	case mux.OpSplitH, mux.OpSplitV, mux.OpKillPane, mux.OpZoom, mux.OpFocus, mux.OpResize,
		mux.OpSwap, mux.OpLayoutNext, mux.OpNewWindow, mux.OpKillWindow, mux.OpSelectWindow,
		mux.OpNextWindow, mux.OpPrevWindow, mux.OpRenameWindow, mux.OpRenameSession, mux.OpSSH:
		return true
	}
	return false
}

// isNavMsg reports whether msg changes a view stack, so the session has to be
// saved again.
func isNavMsg(msg tea.Msg) bool {
	switch msg.(type) {
	case PushViewMsg, PopViewMsg, ReplaceViewMsg, OpenQueryMsg:
		return true
	}
	return false
}

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

	// Synchronized input (task 064): the line being reconstructed while raw
	// sync is on, the channel a bus step uses to switch the mode, the planner
	// registered on the bus, and the guard rails' policy and pane limit.
	sync     *syncState
	syncMode chan bool
	syncPl   *syncPlanner
	guard    *bus.Policy
	maxSync  int

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

	// Session persistence (task 024). sessionPath is
	// <cache>/sessions/<name>.json; dirty means a layout or stack change is
	// waiting for the next 1 s autosave tick.
	sessionPath string
	dirty       bool
	warns       []string

	// connectTerm is set while :ssh adds a pane, so the pane factory knows a
	// term pane it is building is a fresh connection, not a restored one.
	connectTerm bool

	// runs are the rollouts this session started, in start order; the run
	// dialog of one is r.dialog while it is on screen (task 061).
	runs   []*runDialog
	runSeq int
	// pendingRun is the run a list dialog asked to reopen: the dialog's own
	// CloseDialogMsg follows the pick, so the root consumes it there.
	pendingRun int
	// syncClose is how many CloseDialogMsg a broadcast prompt's submit has in
	// flight. The prompt closes itself and submits in one batch, so the close
	// can arrive after the plan dialog is already up; those are swallowed.
	syncClose int
	// planCancel cancels the planning round-trip of the actions menu; esc
	// cancels it while the planning spinner is up.
	planCancel context.CancelFunc
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
	r.registerDefaults()
	// The `:targets` command belongs to the root: it is the only model that
	// owns SessionState.Targets. A registry that already has it is fine.
	_ = RegisterTargetsCommands(cmds)
	// `:inv`, `:note` and `:timeline` drive the session's investigation, which
	// only the root mutates.
	_ = RegisterInvestigationCommands(cmds)
	// `:runs` lists the rollouts in flight, which only the root holds.
	_ = RegisterActionsCommands(cmds)
	m := mux.NewModel(mux.NewSession(name), r.paneContent, prefixKey, stylesFor(env))
	m.SetRootPredicate(IsRootMsg)
	r.mux = m
	// The synchronized-input specs belong to this model, so the planner is
	// built and registered here; a bus that already has one keeps its own.
	r.syncMode = make(chan bool, syncModeBuffer)
	r.syncPl = &syncPlanner{panes: m.Contents, mode: r.syncMode}
	if env != nil && env.Bus != nil {
		_ = env.Bus.Register("mux", r.syncPl)
	}
	_, r.initCmd = m.AddPane(mux.ContentState{}, "h")
	return r
}

// registerDefaults gives every kind without a specific factory the default one
// (BUILD-PLAN §4.8: "kinds without one get the generic table view"). The panes
// resolve `:` queries through Env.Factories themselves, so the fallback has to
// be visible in that map and not only in viewFor.
func (r *Root) registerDefaults() {
	// `:ssh` adds a pane, which only the root can do, so the command is
	// registered here rather than by the app's static table.
	_ = RegisterSSHCommands(r.cmds)
	if r.env == nil || r.env.Registry == nil || r.factory == nil {
		return
	}
	if r.env.Factories == nil {
		r.env.Factories = make(map[resource.KindName]Factory)
	}
	for _, spec := range r.env.Registry.Kinds() {
		if r.env.Factories[spec.Name] == nil {
			r.env.Factories[spec.Name] = r.factory
		}
	}
}

// SetClipboard installs the clipboard the root yanks into. app builds
// clip.Multi(pbcopy, osc52(out, inTmux)) and calls this once; without one a
// yank flashes a warning instead.
func (r *Root) SetClipboard(c clip.Clipboard) { r.clip = c }

// stylesFor builds the mux pane styles from the theme.
func stylesFor(env *Env) mux.Styles {
	if env == nil || env.Theme == nil {
		return mux.Styles{}
	}
	t := env.Theme
	return mux.Styles{
		Border:        t.Border,
		BorderFocused: t.BorderFocused,
		// Panes raw synchronized input goes to are drawn in the Crit colour.
		BorderSync:   t.Crit,
		Title:        t.Title,
		TitleFocused: t.Accent.Bold(true),
	}
}

// paneContent is the mux pane factory. A pane restored from a session file is
// rebuilt from its saved `:` command lines; the first pane shows the start
// query and later panes open the kind of the pane they were split from.
func (r *Root) paneContent(id mux.PaneID, st mux.ContentState) (mux.Content, tea.Cmd) {
	if st.Type == "term" {
		// A term pane from a session file comes back closed ("restored"); one
		// built for :ssh connects right away.
		r.panes++
		return r.termPane(id, st.Host, !r.connectTerm), nil
	}
	if p, ok := r.restoredPane(id, st); ok {
		r.panes++
		return p, nil
	}
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
	return NewViewPane(r.env, id, nil, v), nil
}

// termPane builds a terminal pane for host. restored is true for a pane that
// came back from a session file: it starts closed and does not reconnect.
func (r *Root) termPane(id mux.PaneID, host string, restored bool) mux.Content {
	var opener transport.ShellOpener
	now := time.Now
	if r.env != nil {
		opener = r.env.Shells
		if r.env.Now != nil {
			now = r.env.Now
		}
	}
	p := termpane.New(id, context.Background(), opener, resource.HostID(host), termpane.Options{
		Term: "xterm-256color",
		Now:  now,
	})
	// A protected host says so in the border title (SPEC §11.1).
	r.markProtected(p)
	if restored {
		p.MarkRestored()
	}
	return p
}

// restoredPane rebuilds a view pane from the `:` command lines saved in st: the
// first becomes the root view and the rest are pushed on top, in order, so the
// stack is exactly what the user had. It reports false when any line cannot be
// turned back into a view, and the caller then opens the default pane.
func (r *Root) restoredPane(id mux.PaneID, st mux.ContentState) (*ViewPane, bool) {
	if st.Type != "view" || len(st.Commands) == 0 || r.cmds == nil {
		return nil, false
	}
	views := make([]View, 0, len(st.Commands))
	for _, line := range st.Commands {
		v, ok := r.commandView(line)
		if !ok {
			return nil, false
		}
		views = append(views, v)
	}
	p := NewViewPane(r.env, id, nil, views[0])
	for _, v := range views[1:] {
		p.stack.Push(v)
	}
	return p, true
}

// commandView runs one saved `:` line through the command registry and returns
// the view it asks for. Only a line that opens a query can be restored; quit,
// help and an unknown word report false.
func (r *Root) commandView(line string) (View, bool) {
	cmd := r.cmds.Exec(r.env, line)
	if cmd == nil {
		return nil, false
	}
	q, ok := cmd().(OpenQueryMsg)
	if !ok || q.Query.Kind == "" {
		return nil, false
	}
	v, err := r.viewFor(q.Query)
	if err != nil {
		return nil, false
	}
	return v, true
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

// Init starts the first pane, the first health poll, the autosave tick and any
// warning the caller queued before the program started.
func (r *Root) Init() tea.Cmd {
	var cmds []tea.Cmd
	if r.startErr != "" {
		cmds = append(cmds, Flash(FlashError, "%s", r.startErr))
	}
	for _, w := range r.warns {
		cmds = append(cmds, Flash(FlashWarn, "%s", w))
	}
	if r.initCmd != nil {
		cmds = append(cmds, r.initCmd)
	}
	if c := HealthPoll(r.backends(), healthInterval); c != nil {
		cmds = append(cmds, c)
	}
	if r.sessionPath != "" {
		cmds = append(cmds, autosaveTick())
	}
	// The bus step that turns raw synchronized input on has to reach the UI.
	if c := r.waitSyncMode(); c != nil {
		cmds = append(cmds, c)
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// SetSessionPath sets where the session is autosaved (<cache>/sessions/
// <name>.json) and turns autosave on. An empty path turns it off, which is what
// the tests and a scratch run use.
func (r *Root) SetSessionPath(path string) { r.sessionPath = path }

// AddWarning queues a warning line shown once the program starts. app uses it
// for a session file it could not restore.
func (r *Root) AddWarning(text string) {
	if text != "" {
		r.warns = append(r.warns, text)
	}
}

// RestoreSession replaces the model's multiplexer with the one rebuilt from f.
// app calls it before Init, so the restored panes' first commands run with the
// program. A warning (some pane fell back to its default content) is returned
// with a working model; a nil model means the file could not be opened and the
// caller keeps the fresh session.
func (r *Root) RestoreSession(f mux.SessionFile) error {
	m, cmd, err := mux.Restore(f, r.paneContent, r.prefix, stylesFor(r.env))
	if m == nil {
		return err
	}
	m.SetRootPredicate(IsRootMsg)
	if r.w > 0 && r.h > 0 {
		m.SetSize(r.w, r.bodyHeight())
	}
	r.mux = m
	// The placeholder session's first pane is gone with it, so its init command
	// is dropped instead of being run against a pane that no longer exists.
	r.initCmd = cmd
	return err
}

// Snapshot describes the current session the way mux.Save writes it.
func (r *Root) Snapshot() mux.SessionFile {
	var history []string
	if r.bar != nil {
		history = r.bar.History()
	}
	now := time.Now
	if r.env != nil && r.env.Now != nil {
		now = r.env.Now
	}
	return mux.Snapshot(r.mux, history, now())
}

// SaveSession writes the session file now and reports the failure, so app can
// save once more when the program stops instead of losing the last change.
func (r *Root) SaveSession() error {
	if r.sessionPath == "" || r.mux == nil {
		return nil
	}
	r.dirty = false
	return mux.Save(r.sessionPath, r.Snapshot())
}

// saveCmd writes the dirty session. The disk write happens inside the command,
// never in Update. A failure is flashed; a successful save reports nothing.
func (r *Root) saveCmd() tea.Cmd {
	if r.sessionPath == "" || r.mux == nil {
		r.dirty = false
		return nil
	}
	f := r.Snapshot()
	path := r.sessionPath
	r.dirty = false
	return func() tea.Msg {
		if err := mux.Save(path, f); err != nil {
			return FlashMsg{Level: FlashWarn, Text: fmt.Sprintf("session not saved: %v", err)}
		}
		return nil
	}
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
		// rather than through CloseDialog keeps the dismissal immediate. A
		// dialog that owns esc (the planning spinner, the run dialog) is
		// asked first.
		if k.String() == "esc" {
			r.pendingRun = 0
			if r.planCancel != nil {
				r.planCancel()
				r.planCancel = nil
			}
			if ed, ok := r.dialog.(escDialog); ok {
				d, cmd := ed.Esc()
				r.dialog = d
				return cmd
			}
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
		return r.routeKey(k)
	}
	// `enter` on an alert row expands into an investigation. Checked before the
	// global keys and the view, so the table never flashes "no drill-in".
	if cmd := r.alertOpenKey(k); cmd != nil {
		return cmd
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
	if b := r.binding(keymapNote); b.Matches(k.String()) {
		return r.openNotePrompt()
	}
	if b := r.binding(keymapTimeline); b.Matches(k.String()) {
		return r.openTimeline()
	}
	return r.routeMux(k)
}

// Keymap slots the root handles itself, as opposed to the focused view.
const (
	keymapNote     = "note"
	keymapTimeline = "timeline"
)

// binding returns one named binding of the session key map, the default when
// the model has none.
func (r *Root) binding(name string) keymap.Binding {
	keys := keymap.Default()
	if r.env != nil && r.env.Keys != nil {
		keys = r.env.Keys
	}
	if name == keymapTimeline {
		return keys.Timeline
	}
	return keys.Note
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
	armed := r.mux.PrefixArmed()
	out, cmd := r.mux.Update(msg)
	if armed || isNavMsg(msg) {
		// A key sent after the prefix may have split, killed or renamed
		// something; a navigation message changed a view stack.
		r.dirty = true
	}
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

// openQuery hands a `:` query to the focused pane, which owns the stack the
// views live on. It is the only place OpenQueryMsg is turned into a view push:
// the root must not push as well, or one `:units` would open two views.
// Opening a host list with a selector also makes that selector the session's
// target set (BUILD-PLAN §4.7: the status line follows the hosts on screen).
func (r *Root) openQuery(m OpenQueryMsg) tea.Cmd {
	q := r.scoped(m.Query)
	if q.Kind != "" {
		r.paneKind = q.Kind
	}
	if _, c := r.mux.Focused(); c == nil {
		// Never swallow a query because no pane is focused yet (startup).
		return Flash(FlashWarn, "no pane to open %s in", q.Kind)
	}
	push := r.toFocused(OpenQueryMsg{Query: q})
	var cmds []tea.Cmd
	if r.env != nil {
		if c := r.env.RecordView(q); c != nil {
			cmds = append(cmds, c)
		}
	}
	if setCmd := r.targetsFromHostQuery(q); setCmd != nil {
		cmds = append(cmds, setCmd)
	}
	cmds = append(cmds, push)
	if len(cmds) == 1 {
		return push
	}
	return tea.Batch(cmds...)
}

// targetsFromHostQuery resolves the selector of a host list into a target set.
// It returns nil for other kinds and for a query without a selector: explicit
// hosts and unfiltered lists leave the session targets alone.
func (r *Root) targetsFromHostQuery(q resource.Query) tea.Cmd {
	if q.Kind != "host" || q.Selector.IsZero() || r.env == nil || r.env.Inventory == nil {
		return nil
	}
	env := r.env
	sel := q.Selector
	// Read the investigation's hosts here, in the model's goroutine: the
	// command must not touch mutable session state.
	invHosts := InvestigationTargets(env)
	return func() tea.Msg {
		set, err := targets.Resolve(env.Inventory, env.Sets, sel, invHosts)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: err.Error()}
		}
		return TargetsChangedMsg{Set: set}
	}
}

// scoped gives a host-scoped query the host of the view it was opened from when
// it names none, so `:units` on a host table means that host's units. Explicit
// hosts win, and global kinds are returned unchanged. With no host in the view
// and no selector, the session target set is the scope.
func (r *Root) scoped(q resource.Query) resource.Query {
	if q.Kind == "" || len(q.Hosts) > 0 {
		return q
	}
	spec, ok := r.kindSpec(q.Kind)
	if !ok || spec.Scope != resource.ScopeHost {
		return q
	}
	subj := r.focusedSubject()
	hosts := subj.Hosts
	if len(hosts) == 0 && subj.Ref.Host != "" {
		hosts = []resource.HostID{subj.Ref.Host}
	}
	if len(hosts) > 0 {
		q.Hosts = append([]resource.HostID(nil), hosts...)
		return q
	}
	if q.Selector.IsZero() {
		if ids := r.sessionTargets().Hosts; len(ids) > 0 {
			q.Hosts = append([]resource.HostID(nil), ids...)
		}
	}
	return q
}

// rootBound reports whether msg must be handled by the root even when it
// arrives wrapped in a mux.PaneMsg. TargetsChangedMsg is emitted by the table
// that owns the marks, but it changes session state, which only the root
// mutates.
func rootBound(msg tea.Msg) bool {
	switch msg.(type) {
	case TargetsChangedMsg, InvestigationChangedMsg, InvestigationAppendedMsg,
		TimelineRequestedMsg, InvestigationResolveRequestMsg, ActionsRequestMsg,
		RunListRequestedMsg:
		return true
	}
	return false
}

// applyTargets stores set in the session and rebroadcasts it to every pane.
func (r *Root) applyTargets(set targets.Set) {
	if r.env != nil && r.env.Session != nil {
		r.env.Session.Targets = set
	}
	r.routeMux(TargetsChangedMsg{Set: set})
}

// applySaltTargets is `:salt <native target>`: the set becomes the session
// targets, the counts flash replaces the generic target flash, and the host
// list opens on the mapped hosts.
func (r *Root) applySaltTargets(m saltTargetsMsg) tea.Cmd {
	r.applyTargets(m.Set)
	var cmds []tea.Cmd
	if m.Text != "" {
		cmds = append(cmds, Flash(FlashInfo, "%s", m.Text))
	}
	if m.Query.Kind != "" {
		if open := r.openQuery(OpenQueryMsg{Query: m.Query}); open != nil {
			cmds = append(cmds, open)
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// openSaltJob opens the grouped detail of the job `:salt ping` started and
// reports what the transport did the first time it is used in a session.
func (r *Root) openSaltJob(m saltPingMsg) tea.Cmd {
	var cmds []tea.Cmd
	if m.Unmapped > 0 {
		cmds = append(cmds, Flash(FlashWarn, "%d targets are not salt minions", m.Unmapped))
	}
	if m.First {
		cmds = append(cmds, Flash(FlashWarn, "%s", saltJobCacheWarning))
	}
	q := resource.Query{Kind: saltJobDetailKind, Args: map[string]string{"jid": m.JID}}
	cmds = append(cmds, saltViewCmd(r.env, saltJobDetailKind, q))
	return tea.Batch(cmds...)
}

// sessionTargets is the current target set; the zero set when there is no
// session.
func (r *Root) sessionTargets() targets.Set {
	if r.env == nil || r.env.Session == nil {
		return targets.Set{}
	}
	return r.env.Session.Targets
}

// targetsFlash is the one-line confirmation of a target change:
// "targets: 24 hosts (role=web dc=ewr)".
func targetsFlash(set targets.Set) string {
	noun := "hosts"
	if len(set.Hosts) == 1 {
		noun = "host"
	}
	text := fmt.Sprintf("targets: %d %s", len(set.Hosts), noun)
	if set.Label != "" {
		text += " (" + set.Label + ")"
	}
	return text
}

// targetsView is the text view `:targets` opens: the label, the count and the
// hosts themselves.
func (r *Root) targetsView() View {
	set := r.sessionTargets()
	label := set.Label
	if label == "" {
		label = "(none)"
	}
	lines := []string{
		"label: " + label,
		fmt.Sprintf("hosts: %d", len(set.Hosts)),
	}
	if len(set.Hosts) == 0 {
		lines = append(lines, "",
			"no targets: use :targets <selector>, :targets marked or :targets load <name>")
	} else {
		lines = append(lines, "")
		for _, h := range set.Hosts {
			lines = append(lines, string(h))
		}
	}
	return r.noticeView("targets", lines)
}

// kindSpec looks a kind up in the registry.
func (r *Root) kindSpec(kind resource.KindName) (resource.KindSpec, bool) {
	if r.env == nil || r.env.Registry == nil {
		return resource.KindSpec{}, false
	}
	_, spec, ok := r.env.Registry.Lookup(string(kind))
	return spec, ok
}

// focusedSubject is what the focused content is about, when it reports one.
func (r *Root) focusedSubject() Subject {
	if r.mux == nil {
		return Subject{}
	}
	_, c := r.mux.Focused()
	if s, ok := c.(interface{ Subject() Subject }); ok {
		return s.Subject()
	}
	return Subject{}
}

// handlePaneMsg delivers a pane-scoped message to its content and returns the
// command the content produced. The root routes these itself instead of going
// through mux.Update: the mux stores the updated content but drops that
// command, and dropping it would stop a view's stream after its first batch.
// Contents are values owned by the mux; ViewPane always returns itself, so the
// stored value stays current.
func (r *Root) handlePaneMsg(pm mux.PaneMsg) tea.Cmd {
	if IsRootMsg(pm.Msg) || rootBound(pm.Msg) {
		return r.handleMsg(pm.Msg)
	}
	if isNavMsg(pm.Msg) {
		r.dirty = true
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
		// A broadcast prompt submits and closes in one batch, so its close may
		// land after the plan dialog it asked for; that one clears nothing.
		if r.syncClose > 0 {
			r.syncClose--
			if _, ok := r.dialog.(*promptDialog); ok {
				r.dialog = nil
			}
			return nil
		}
		if r.prompt != nil {
			r.prompt = r.nextPrompt()
			return nil
		}
		if r.pendingRun != 0 {
			id := r.pendingRun
			r.pendingRun = 0
			return r.showRun(id)
		}
		r.dialog = nil
		return nil
	case PromptMsg:
		return r.showPrompt(m)
	case ActionsRequestMsg:
		return r.openActions(m)
	case actionSpecChosenMsg:
		return r.chooseSpec(m)
	case actionParamsMsg:
		r.dialog = nil
		return r.startPlanning(m.Spec, m.Params, m.Selection)
	case ProposalMsg:
		return r.openProposal(m)
	case planConfirmedMsg:
		return r.confirmPlan(m)
	case runBeginMsg:
		return r.routeRun(m.ID, m)
	case runEventsForMsg:
		return r.routeRun(m.ID, m.Msg)
	case RunEventsMsg:
		if d := r.activeRunDialog(); d != nil {
			return r.routeRun(d.id, m)
		}
		return nil
	case hideRunMsg:
		return r.hideRun(m.ID)
	case runOpenMsg:
		return r.showRun(m.ID)
	case runResultsRequestMsg:
		return r.openRunResults(m.ID)
	case RunListRequestedMsg:
		return r.openRunList()
	case OpenDialogMsg:
		r.dialog = m.Dialog
		return nil
	case RunCommandMsg:
		if r.cmds == nil {
			return nil
		}
		return r.cmds.Exec(r.env, m.Line)
	case OpenQueryMsg:
		return r.openQuery(m)
	case mux.OpMsg:
		return r.handleOp(m)
	case sshCommandMsg:
		return r.sshFromCommand()
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
	case autosaveMsg:
		next := autosaveTick()
		if !r.dirty || r.sessionPath == "" {
			return next
		}
		return tea.Batch(r.saveCmd(), next)
	case HealthMsg:
		r.health[m.Backend] = m.Health.State
		return nil
	case TargetsChangedMsg:
		r.applyTargets(m.Set)
		return Flash(FlashInfo, "%s", targetsFlash(m.Set))
	case saltTargetsMsg:
		return r.applySaltTargets(m)
	case saltPingMsg:
		return r.openSaltJob(m)
	case TargetsViewRequestedMsg:
		return r.pushFocused(r.targetsView())
	case MarkedTargetsRequestMsg:
		if _, c := r.mux.Focused(); c == nil {
			return Flash(FlashWarn, "no focused view to read marked rows from")
		}
		return r.toFocused(m)
	case WindowChangedMsg:
		if r.env != nil && r.env.Session != nil {
			r.env.Session.Window = m.Window
		}
		r.routeMux(msg)
		return Flash(FlashInfo, "window: %s", m.Window.String())
	case readOnlyChangedMsg:
		return r.applyReadOnly(m.On)
	case InvestigationChangedMsg:
		return r.applyInvestigation(m.Meta)
	case alertResolvedMsg:
		return r.openAlert(m)
	case InvestigationAppendedMsg:
		return r.refreshInvestigation()
	case TimelineRequestedMsg:
		return r.openTimeline()
	case InvestigationResolveRequestMsg:
		return r.openResolvePrompt()
	case HelpRequestedMsg:
		return r.openHelp()
	case syncBroadcastMsg:
		return r.broadcastLine(m.Line)
	case syncModeMsg:
		next := r.waitSyncMode()
		if cmd := r.applySyncMode(m.On); cmd != nil {
			return tea.Batch(cmd, next)
		}
		return next
	case syncIdleMsg:
		return r.checkSyncIdle()
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

// applyInvestigation stores the session's investigation and reports the change.
// The status line follows SessionState.Investigation, so nothing else has to be
// told.
func (r *Root) applyInvestigation(meta *investigation.Meta) tea.Cmd {
	if r.env != nil && r.env.Session != nil {
		r.env.Session.Investigation = meta
	}
	var flash tea.Cmd
	switch {
	case meta == nil:
		flash = Flash(FlashInfo, "investigation closed")
	case meta.Status == "resolved":
		flash = Flash(FlashOK, "investigation %s resolved", meta.ID)
	default:
		flash = Flash(FlashOK, "investigation %s", meta.ID)
	}
	if refresh := r.refreshInvestigation(); refresh != nil {
		return tea.Batch(flash, refresh)
	}
	return flash
}

// refreshInvestigation tells every open timeline that the record changed. It is
// the only place the refresh message is sent.
func (r *Root) refreshInvestigation() tea.Cmd {
	if r.mux == nil {
		return nil
	}
	var cmds []tea.Cmd
	for _, c := range r.mux.Contents() {
		vp, ok := c.(*ViewPane)
		if !ok {
			// Terminals and other contents have no timeline to refresh.
			continue
		}
		if _, cmd := vp.Update(InvestigationRefreshMsg{}); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	switch len(cmds) {
	case 0:
		return nil
	case 1:
		return cmds[0]
	}
	return tea.Batch(cmds...)
}

// openTimeline pushes the investigation timeline onto the focused pane.
func (r *Root) openTimeline() tea.Cmd {
	v, err := r.timelineView()
	if err != nil {
		return Flash(FlashError, "%v", err)
	}
	return r.pushFocused(v)
}

// timelineView builds the timeline through the factory registered under
// KindTimeline; package tui may not import internal/tui/views itself.
func (r *Root) timelineView() (View, error) {
	if r.env == nil || r.env.Factories == nil {
		return nil, fmt.Errorf("no view factories configured")
	}
	f := r.env.Factories[KindTimeline]
	if f == nil {
		return nil, fmt.Errorf("timeline view unavailable")
	}
	v, err := f(r.env, resource.Query{Kind: KindTimeline})
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, fmt.Errorf("timeline view unavailable")
	}
	return v, nil
}

// openNotePrompt asks for one free-text line and appends it as a note.
func (r *Root) openNotePrompt() tea.Cmd {
	if r.env == nil || r.env.Recorder == nil {
		return Flash(FlashWarn, "no investigation log configured")
	}
	if _, open := r.env.Recorder.Current(); !open {
		return Flash(FlashWarn, "no investigation open")
	}
	env := r.env
	r.dialog = NewPromptDialog(themeOf(env), "note", "note:", "", func(text string) tea.Cmd {
		return env.AppendInvestigation(investigation.EntryNote, strings.TrimSpace(text), "")
	})
	return nil
}

// openResolvePrompt asks for the one-line root cause and resolves the record.
func (r *Root) openResolvePrompt() tea.Cmd {
	inv, ok := invLog(r.env)
	if !ok {
		return Flash(FlashWarn, "no investigation log configured")
	}
	if _, open := r.env.Recorder.Current(); !open {
		return Flash(FlashWarn, "no investigation open")
	}
	r.dialog = NewPromptDialog(themeOf(r.env), "resolve investigation", "root cause:", "", func(text string) tea.Cmd {
		return func() tea.Msg {
			if err := inv.Resolve(text); err != nil {
				return FlashMsg{Level: FlashError, Text: err.Error()}
			}
			meta, open := inv.Current()
			if !open {
				return InvestigationChangedMsg{Meta: nil}
			}
			return InvestigationChangedMsg{Meta: &meta}
		}
	})
	return nil
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
	if sessionOp(m.Op) {
		r.dirty = true
	}
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
	case mux.OpSSH:
		return r.sshOp(m.Arg)
	case mux.OpSync:
		return r.syncOp(m.Arg)
	case mux.OpSessionList:
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
	var cmds []tea.Cmd
	if r.env != nil {
		// Copy mode is how a selection from anywhere becomes a finding.
		if f := r.env.RecordFinding(text); f != nil {
			cmds = append(cmds, f)
		}
	}
	cmds = append(cmds, func() tea.Msg { return copyDoneMsg{lines: lines, err: c.Copy(text)} })
	return tea.Batch(cmds...)
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
	// Raw synchronized input shows its badge for as long as the mode lasts.
	d.Sync = r.mux.SyncCount()
	if r.env != nil && r.env.Session != nil {
		d.ReadOnly = r.env.Session.ReadOnly
		d.Targets = len(r.env.Session.Targets.Hosts)
		if !r.env.Session.Window.IsZero() {
			d.Window = r.env.Session.Window.String()
		}
		if inv := r.env.Session.Investigation; inv != nil {
			d.Investigation = inv.ID
		}
	}
	// A rollout that was hidden with `h` keeps going; the status line says so
	// in the context slot (task 061).
	if badge := r.rolloutBadge(); badge != "" {
		d.Context = badge
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
