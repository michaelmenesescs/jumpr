package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/action"
	"jumper/internal/bus"
	"jumper/internal/errs"
	"jumper/internal/inventory"
	"jumper/internal/investigation"
	"jumper/internal/resource"
	"jumper/internal/server"
	"jumper/internal/targets"
	"jumper/internal/tui"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
	"jumper/internal/tui/views"
)

// The preamble and postamble are what the session writes to a fresh terminal
// itself. Bubble Tea cannot do it: its output is a switchable pipe, not a
// terminal, so it never enters the alt screen or hides the cursor on the user's
// screen (task 078 pitfall; from M5 on the client's raw mode does the rest).
const (
	// enterScreen: alt screen, hidden cursor, bracketed paste, cleared screen.
	enterScreen = "\x1b[?1049h\x1b[?25l\x1b[?2004h\x1b[2J\x1b[H"
	// leaveScreen undoes enterScreen: bracketed paste off, cursor shown, main
	// screen back.
	leaveScreen = "\x1b[?2004l\x1b[?25h\x1b[?1049l"
)

// Info describes a session the way the CLI's `ls` shows it (BUILD-PLAN §6.2
// SessionInfo has the same shape on the wire).
type Info struct {
	Name     string
	Windows  int
	Attached bool
	Created  time.Time
	Activity time.Time
}

// Session is one terminal's worth of state: its own investigation log, bus,
// environment, command registry, mux model and — for its whole life — one
// tea.Program whose input and output are switchable pipes. Work in progress
// keeps running while no client is attached; attaching again only re-points the
// pipes and forces a full repaint (BUILD-PLAN §6).
type Session struct {
	name   string
	core   *Core
	ctx    context.Context
	cancel context.CancelFunc

	env     *tui.Env
	cmds    *tui.Commands
	root    *tui.Root
	bus     bus.Bus
	log     *investigation.FileLog
	path    string
	started bool

	in      *SwitchReader
	out     *SwitchWriter
	program *tea.Program
	sink    int           // the uiBridge registration of program.Send
	ready   chan struct{} // closed by the model's first Init
	done    chan struct{} // closed when the program has ended

	mu        sync.Mutex
	attached  bool
	clientOut io.Writer
	onDetach  func(reason string)
	created   time.Time
	activity  time.Time

	// switchTo asks the server to hand this client to another session; skips
	// counts the prompts refused while nobody was attached (task 082).
	switchTo func(name string) error
	skips    *promptSkips
}

// NewSession builds a session on top of c, named name, and starts its
// tea.Program. The program reads from and writes to the session's switchable
// pipes, so it is already rendering — into nothing — before a client attaches.
// The caller must Kill or let the user quit, and Close the core.
func (c *Core) NewSession(name string) (*Session, error) {
	return c.newSession(name, true)
}

// newSession is NewSession with the program start under the caller's control:
// the composition tests and doctor drive the root model themselves and must not
// have a renderer touching it at the same time.
func (c *Core) newSession(name string, start bool) (*Session, error) {
	if c == nil {
		return nil, errs.New(errs.Internal, "app.NewSession", "no core")
	}
	if name == "" {
		name = "main"
	}
	ctx, cancel := context.WithCancel(c.ctx)
	s := &Session{name: name, core: c, ctx: ctx, cancel: cancel, created: time.Now(), activity: time.Now()}
	s.in = NewSwitchReader(s.onInputError)
	s.out = NewSwitchWriter(s.onOutputError)

	env, cmds, root, err := c.buildSession(s)
	if err != nil {
		cancel()
		s.in.Close()
		return nil, err
	}
	s.env, s.cmds, s.root = env, cmds, root

	// Session persistence: <cache>/sessions/<name>.json, restored on start
	// unless --fresh was given. A file that cannot be read or a pane that falls
	// back to its default content warns at startup; neither stops the session.
	if c.paths.CacheDir != "" {
		s.path = filepath.Join(c.paths.CacheDir, "sessions", name+".json")
		root.SetSessionPath(s.path)
		if !c.fresh {
			restoreSession(root, s.path)
		}
	}
	inTmux := c.env["TMUX"] != ""
	root.SetClipboard(c.Clipboard(s.out, inTmux))
	// `d` and `:detach` are left flashing the task-022 hint here: a standalone
	// run has no session server to come back to, so it installs no hook. The
	// daemon calls SetDetachHook to unplug the terminal instead (requirement 7).

	if !start {
		s.ready = make(chan struct{})
		s.done = make(chan struct{})
		return s, nil
	}

	s.started = true
	s.ready = make(chan struct{})
	s.done = make(chan struct{})
	model := &sessionModel{
		inner: root,
		env:   env,
		ttl:   c.cfg.Inventory.CacheTTL.Duration(),
		ready: s.ready,
		touch: s.touch,
	}
	s.program = tea.NewProgram(model,
		tea.WithInput(s.in),
		tea.WithOutput(s.out),
		tea.WithAltScreen(),
		tea.WithoutSignalHandler(),
		tea.WithContext(ctx),
	)
	// Prompts and warnings produced outside the UI reach this program from
	// here on. Messages queued while the core and the model were being built
	// are delivered by Flush once the event loop runs (see Attach).
	s.sink = c.bridge.AddSink(s.program.Send)
	go func() {
		defer close(s.done)
		defer c.bridge.RemoveSink(s.sink)
		_, err := s.program.Run()
		if err != nil && !errors.Is(err, tea.ErrProgramKilled) && !errors.Is(err, tea.ErrProgramPanic) {
			c.log.Debug("session program ended", "session", name, "err", err)
		}
	}()
	return s, nil
}

// buildSession builds the per-session environment, the bus, the command
// registry and the root model over the core's shared services.
func (c *Core) buildSession(s *Session) (*tui.Env, *tui.Commands, *tui.Root, error) {
	cfg := c.cfg
	prefix, err := tui.PrefixKey(cfg.Mux.Prefix, cfg.Mux.PrefixInTmux, c.env["TMUX"] != "")
	if err != nil {
		return nil, nil, nil, err
	}

	// The investigation log is the session's own: two sessions never append to
	// the same open investigation.
	s.log = investigation.NewLog(c.paths.KnowledgeRoot, c.Redactor, time.Now)

	// A nil *inventoryRuntime in an interface field would be a typed nil the
	// target resolver calls into, so the field stays empty in --demo.
	var inv inventory.Inventory
	if c.Inventory != nil {
		inv = c.Inventory
	}

	env := &tui.Env{
		Registry:  c.Registry,
		Inventory: inv,
		Sets:      targets.NewStore(c.paths.ConfigDir),
		Session:   &tui.SessionState{Name: s.name, ReadOnly: c.readOnly},
		Exec:      c.Exec,
		Shells:    c.Shells,
		Theme:     theme.Dark(),
		Glyphs:    c.Glyphs,
		Keys:      keymap.Default(),
		Now:       time.Now,
		Factories: make(map[resource.KindName]tui.Factory),
		Log:       c.log,
		Redact:    c.Redactor,
		Recorder:  s.log,
		Planners:  c.Planners,
		Execs:     c.Execs,
		Salt:      c.Salt,
		ResultsView: func(env *tui.Env, title string, results []action.StepResult) tui.View {
			return views.NewFanoutFromResults(env, title, results)
		},
	}
	for kind, f := range views.Factories() {
		env.Factories[kind] = f
	}

	// The bus's root context is the session's: a rollout outlives the dialog
	// that started it and survives a detach, but not the end of the session.
	s.bus = bus.New(bus.Options{
		Policy:   c.Policy,
		Recorder: s.log,
		Redact:   c.Redactor,
		ReadOnly: func() bool { return env.Session.ReadOnly },
		Rollout:  rolloutConfig(cfg.Rollout),
		Now:      time.Now,
		Log:      c.log,
		Context:  s.ctx,
	})
	names := make([]string, 0, len(c.Planners))
	for name := range c.Planners {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := s.bus.Register(name, c.Planners[name]); err != nil {
			c.bridge.Warning("action bus: " + err.Error())
		}
	}
	env.Bus = s.bus

	cmds := tui.NewCommands()
	tui.RegisterBuiltins(cmds)
	if err := tui.RegisterMuxCommands(cmds); err != nil {
		return nil, nil, nil, err
	}
	if err := tui.RegisterSessionCommands(cmds); err != nil {
		return nil, nil, nil, err
	}
	if err := tui.RegisterHealthCommands(cmds); err != nil {
		return nil, nil, nil, err
	}
	// `:run` and `:probe` write through the bus or the chosen transport; the
	// Salt family adds targets, the dry run, module runs and ping.
	if err := tui.RegisterRunCommands(cmds, s.bus); err != nil {
		return nil, nil, nil, err
	}
	if err := tui.RegisterProbeCommands(cmds, fanoutOpener); err != nil {
		return nil, nil, nil, err
	}
	if err := tui.RegisterSaltCommands(cmds, s.bus); err != nil {
		return nil, nil, nil, err
	}

	// The session opens on the host list when the inventory has hosts. With
	// none it still opens there and the warning says how to add some.
	root := tui.NewRoot(env, cmds, tableFactory, resource.Query{Kind: linuxHostKind}, prefix)
	return env, cmds, root, nil
}

// Attach points the session at a terminal: it detaches whatever client is
// attached now (asking it to go away with reason "replaced"), follows the new
// client's agent socket and tmux state, writes the alt-screen preamble, swaps
// both pipes and forces a full repaint with a WindowSizeMsg.
func (s *Session) Attach(in io.Reader, out io.Writer, size Size, env map[string]string) error {
	if s == nil {
		return errs.New(errs.Internal, "app.Attach", "no session")
	}
	if in == nil || out == nil {
		return errs.New(errs.Config, "app.Attach", "attach needs a reader and a writer")
	}
	// One client per session: a second attach replaces the first (§6).
	s.Detach("replaced")

	if s.core.Agent != nil {
		s.core.Agent.Set(env["SSH_AUTH_SOCK"])
	}
	inTmux := env["TMUX"] != ""
	prefix, err := tui.PrefixKey(s.core.cfg.Mux.Prefix, s.core.cfg.Mux.PrefixInTmux, inTmux)
	if err != nil {
		return err
	}

	if _, err := io.WriteString(out, enterScreen); err != nil {
		return errs.Wrap(errs.Internal, "app.Attach", err)
	}
	s.out.Set(out)
	s.in.Set(in)

	s.mu.Lock()
	s.attached = true
	s.clientOut = out
	s.activity = time.Now()
	s.mu.Unlock()
	s.setPrompter(true)

	if s.program == nil {
		return nil
	}
	s.waitReady()
	// Warnings and prompts produced before the program ran are delivered now
	// (Send blocks before the first Init, and the root's Init is what closed
	// ready).
	s.core.bridge.Flush()
	// The prefix follows the client (a tmux client gets prefix-in-tmux) and is
	// handed to the event loop as a message: Attach is not the UI goroutine.
	s.program.Send(tui.PrefixMsg{Key: prefix})
	// Bubble Tea v1 diffs against its last frame and the new terminal is
	// blank, so the size is injected even when it did not change: that is what
	// makes it paint everything again.
	s.program.Send(tea.WindowSizeMsg{Width: size.Cols, Height: size.Rows})
	// What happened while nobody was looking becomes one line, and the
	// counters start again (task 082 requirements 6 and 8).
	hostKeys, passphrases := 0, 0
	if s.skips != nil {
		hostKeys, passphrases = s.skips.take()
	}
	s.program.Send(tui.AttachMsg{HostKeySkipped: hostKeys, PassphraseSkipped: passphrases})
	return nil
}

// Detach unplugs the terminal: the pipes are cleared, the old terminal gets the
// postamble that undoes Attach's preamble, prompts wait for a client again and
// the client's onDetach callback is called with reason. Everything the session
// was doing keeps running.
func (s *Session) Detach(reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	out := s.clientOut
	was := s.attached
	cb := s.onDetach
	s.clientOut = nil
	s.attached = false
	s.activity = time.Now()
	s.mu.Unlock()

	s.in.Clear()
	s.out.Clear()
	if out != nil {
		_, _ = io.WriteString(out, leaveScreen)
	}
	s.setPrompter(false)
	if was && cb != nil {
		cb(reason)
	}
	// From now on what happens is counted for the next attach's summary. It is
	// sent last: the client's EXIT must not wait on the model.
	if s.program != nil {
		s.program.Send(tui.DetachMsg{})
	}
}

// Resize tells the program the client's new size. Detached, it is a harmless
// no-op once the program has ended.
func (s *Session) Resize(size Size) {
	if s == nil || s.program == nil {
		return
	}
	s.waitReady()
	s.program.Send(tea.WindowSizeMsg{Width: size.Cols, Height: size.Rows})
}

// Done is closed when the program ended: the user quit with `:q` or the last
// pane was closed.
func (s *Session) Done() <-chan struct{} {
	if s == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return s.done
}

// Kill stops the session now, without the final repaint.
func (s *Session) Kill() {
	if s == nil {
		return
	}
	if s.program != nil {
		s.program.Kill()
	}
	s.in.Close()
	if s.cancel != nil {
		s.cancel()
	}
}

// SetOnDetach installs the callback Detach calls with its reason: "replaced"
// when a second attach takes over, "detached" when the user pressed the prefix
// `d` or typed `:detach`, and whatever the caller passes when it detaches.
func (s *Session) SetOnDetach(fn func(reason string)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.onDetach = fn
	s.mu.Unlock()
}

// SetDetachHook makes the prefix `d` and `:detach` unplug the terminal instead
// of flashing the task-022 hint. A standalone run leaves the hook unset,
// because there is nothing to attach back to; the session server sets it for
// every session a client can leave and rejoin. A running program gets it as a
// message: this is called from the attach path, not the UI goroutine.
func (s *Session) SetDetachHook(fn func()) {
	if s == nil || s.root == nil {
		return
	}
	if s.program != nil {
		s.program.Send(tui.DetachHookMsg{Hook: fn})
		return
	}
	s.root.OnDetach = fn
}

// Info reports the session's name, window count, whether a client is attached
// and when it was created and last active.
func (s *Session) Info() Info {
	if s == nil {
		return Info{}
	}
	s.mu.Lock()
	info := Info{
		Name:     s.name,
		Attached: s.attached,
		Created:  s.created,
		Activity: s.activity,
	}
	s.mu.Unlock()
	if s.root != nil {
		info.Windows = len(s.root.Snapshot().Windows)
	}
	return info
}

// save writes the session file now, so the last change before a quit is not
// lost to the autosave debounce.
func (s *Session) save() error {
	if s == nil || s.root == nil {
		return nil
	}
	s.mu.Lock()
	path := s.path
	s.mu.Unlock()
	if path == "" {
		return nil
	}
	return s.root.SaveSession()
}

// Name reports the session's name.
func (s *Session) Name() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.name
}

// SetSwitchHook installs what SwitchTo calls: the session server's request to
// hand this client to another session (task 082 requirement 3).
func (s *Session) SetSwitchHook(fn func(name string) error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.switchTo = fn
	s.mu.Unlock()
}

// SwitchTo asks the server to move this client to the session named name. The
// server detaches from the current session and attaches the other, with the
// same terminal and without the client reconnecting. It is the request the
// root's sessions dialog raises through its callback.
func (s *Session) SwitchTo(name string) error {
	if s == nil {
		return errs.New(errs.Internal, "app.SwitchTo", "no session")
	}
	s.mu.Lock()
	fn := s.switchTo
	s.mu.Unlock()
	if fn == nil {
		return errs.New(errs.Config, "app.SwitchTo", "no session server to switch in")
	}
	return fn(name)
}

// SetPromptSkips installs the counter of prompts the session server refused
// while nobody was attached. Attach reads it to say what was skipped.
func (s *Session) SetPromptSkips(skips *promptSkips) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.skips = skips
	s.mu.Unlock()
}

// SetSessionFuncs installs the sessions dialog's server side on the root. The
// server binds it right after the session is created, while the program is
// already running, so it travels as a message.
func (s *Session) SetSessionFuncs(f tui.SessionFuncs) {
	if s == nil || s.root == nil {
		return
	}
	if s.program != nil {
		s.program.Send(tui.SessionControlMsg{Funcs: f})
		return
	}
	s.root.SetSessionFuncs(f)
}

// Rename renames the session and its autosave file
// (<cache>/sessions/<name>.json), and points later saves at the new file
// (task 082 requirement 4). The root follows with SetSessionName when it hears
// the rename finished.
func (s *Session) Rename(name string) error {
	if s == nil {
		return errs.New(errs.Internal, "app.Rename", "no session")
	}
	name = strings.TrimSpace(name)
	if !server.ValidSessionName(name) {
		return errs.New(errs.Config, "app.Rename",
			fmt.Sprintf("invalid session name %q", name))
	}
	s.mu.Lock()
	old, oldPath := s.name, s.path
	newPath := ""
	if oldPath != "" {
		newPath = filepath.Join(filepath.Dir(oldPath), name+".json")
	}
	s.name, s.path = name, newPath
	s.mu.Unlock()
	if name == old {
		return nil
	}
	if oldPath != "" && oldPath != newPath {
		if err := os.Rename(oldPath, newPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.mu.Lock()
			s.name, s.path = old, oldPath
			s.mu.Unlock()
			return errs.Wrap(errs.Config, "app.Rename", err)
		}
	}
	if s.root != nil {
		// Guarded by the root: the rename runs in a tea.Cmd, not in Update.
		s.root.SetSessionPath(newPath)
	}
	return nil
}

// close detaches any client, stops the program and closes the input pipe.
func (s *Session) close() error {
	if s == nil {
		return nil
	}
	s.Detach("shutdown")
	s.Kill()
	if s.core != nil && s.core.bridge != nil {
		s.core.bridge.RemoveSink(s.sink)
	}
	return nil
}

// touch records activity: an attach, a detach, a resize or any message the
// program handled.
func (s *Session) touch() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.activity = time.Now()
	s.mu.Unlock()
}

// onInputError is what the reader reports when a client's input fails: the
// client is gone, so the session detaches. It runs on the reader's goroutine,
// never in Update.
func (s *Session) onInputError(err error) {
	if s == nil {
		return
	}
	s.core.log.Debug("session input ended", "session", s.name, "err", err)
	s.Detach("detached")
}

// onOutputError is what the writer reports when a client's terminal stops
// accepting bytes: the same detach, reported once.
func (s *Session) onOutputError(err error) {
	if s == nil {
		return
	}
	s.core.log.Debug("session output ended", "session", s.name, "err", err)
	s.Detach("detached")
}

// waitReady blocks until the program's first Init has run. program.Send blocks
// before that, and a session that never started has no program to send to.
func (s *Session) waitReady() {
	if s.ready == nil {
		return
	}
	select {
	case <-s.ready:
	case <-s.done:
	case <-time.After(readyTimeout):
	}
}

// readyTimeout bounds how long Attach waits for the program to come up. It is
// a backstop against a model whose Init never returns, not a normal path.
const readyTimeout = 10 * time.Second

// setPrompter tells the prompter whether a client can answer. While detached a
// question waits for interact.timeout and then fails with errs.Interaction.
func (s *Session) setPrompter(on bool) {
	if s.core == nil || s.core.Prompter == nil {
		return
	}
	if p, ok := s.core.Prompter.(interface{ SetAttached(bool) }); ok {
		p.SetAttached(on)
	}
}

// sessionModel is the session's root model plus what belongs to the process:
// the inventory refresh tick, the ready signal and the activity clock. The
// wrapper changes nothing the root sees.
type sessionModel struct {
	inner tea.Model
	env   *tui.Env
	ttl   time.Duration
	ready chan struct{}
	touch func()
}

var _ tea.Model = (*sessionModel)(nil)

// Init starts the root and closes ready once the program is actually running:
// a command runs in the event loop, which is the first moment program.Send is
// safe.
func (m *sessionModel) Init() tea.Cmd {
	first := m.inner.Init()
	ready := m.ready
	return tea.Batch(first, func() tea.Msg {
		close(ready)
		return nil
	})
}

// Update turns a tick into a refresh and re-arms the next one; every other
// message is the root's.
func (m *sessionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.touch != nil {
		m.touch()
	}
	if _, ok := msg.(tui.InventoryTickMsg); ok {
		return m, tea.Batch(tui.InventoryRefreshCmd(m.env), tui.InventoryTick(m.ttl))
	}
	model, cmd := m.inner.Update(msg)
	if model != nil {
		m.inner = model
	}
	return m, cmd
}

// View renders the root.
func (m *sessionModel) View() string { return m.inner.View() }

// linuxHostKind is the kind the session opens on.
const linuxHostKind = resource.KindName("host")
