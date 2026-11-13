// Package app is the composition root: it loads configuration, builds the
// backends, the shared tui.Env, the command registry and the root model, then
// runs the Bubble Tea program. It is the only package besides cmd/jumper that
// may touch os.Stdin/Stdout/Stderr (BUILD-PLAN §6.1); it only reads the ones
// handed to Run.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/config"
	"jumper/internal/errs"
	"jumper/internal/interact"
	"jumper/internal/mux"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/tui"
	"jumper/internal/tui/views"
)

// Options is everything Run needs from the outside world. Nothing here is read
// from the process environment: cmd/jumper fills it in.
type Options struct {
	In        io.Reader
	Out       io.Writer
	Demo      bool
	ConfigDir string
	Home      string
	Debug     bool
	Fresh     bool
	// ReadOnly starts the session with the read-only switch on, so every write
	// the bus plans is denied until `:readonly off`. policy.yaml's own
	// `readonly: true` also starts it on and cannot be turned off in-session.
	ReadOnly bool
	Version  string
	Env      map[string]string
	// Prompter answers host-key and passphrase questions. Nil means the TUI
	// prompter is built, which shows a dialog; with no program yet the
	// question fails with errs.Interaction.
	Prompter interact.Prompter

	// Size is the terminal size to inject at startup. When it is nil the
	// program is on a real TTY and Bubble Tea detects the size itself.
	Size *Size
	// Resizes carries later size changes for a run that cannot see SIGWINCH.
	// Each value becomes one tea.WindowSizeMsg; the pump stops with ctx.
	Resizes <-chan Size
	// Ready, when non-nil, is closed once the program is running and the
	// first size was sent. Tests wait on it instead of sleeping. With Size
	// nil there is no size to send, so Ready is closed at program start.
	Ready chan<- struct{}
}

type Size struct{ Cols, Rows int }

// startup wraps the root model to inject the configured size. The first
// tea.WindowSizeMsg is produced by a command from Init, so it reaches the model
// as the program starts, long before any input is read; Ready is closed right
// after the model has handled it, which is the first moment a test may act.
type startup struct {
	inner tea.Model
	size  *Size
	ready chan<- struct{}
}

// initialSizeMsg is the injected first size. It is a distinct type so that a
// real tea.WindowSizeMsg (which Bubble Tea may send on a terminal) is never
// mistaken for it.
type initialSizeMsg struct{ cols, rows int }

func (s *startup) Init() tea.Cmd {
	first := s.inner.Init()
	switch {
	case s.size != nil:
		size := *s.size
		return tea.Batch(first, func() tea.Msg {
			return initialSizeMsg{cols: size.Cols, rows: size.Rows}
		})
	case s.ready != nil:
		// No size to inject: the program is running as soon as Init ran.
		ready := s.ready
		s.ready = nil
		return tea.Batch(first, func() tea.Msg {
			close(ready)
			return nil
		})
	default:
		return first
	}
}

func (s *startup) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if init, ok := msg.(initialSizeMsg); ok {
		m, cmd := s.inner.Update(tea.WindowSizeMsg{Width: init.cols, Height: init.rows})
		s.inner = m
		if s.ready != nil {
			close(s.ready)
			s.ready = nil
		}
		return s, cmd
	}
	m, cmd := s.inner.Update(msg)
	s.inner = m
	return s, cmd
}

func (s *startup) View() string { return s.inner.View() }

// pumpSizes turns the Options.Resizes channel into tea.WindowSizeMsg values
// until ctx, the channel or the program ends. It is I/O-free and never inside
// Update, so the UI cannot block on it.
func pumpSizes(ctx context.Context, p *tea.Program, ch <-chan Size, done <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case size, ok := <-ch:
			if !ok {
				return
			}
			p.Send(tea.WindowSizeMsg{Width: size.Cols, Height: size.Rows})
		}
	}
}

// Run loads configuration, builds the model and runs the TUI until the user
// quits or ctx is canceled. A panic anywhere below is recovered, written to
// <cache>/crash/ and returned as an errs.Internal error.
func Run(ctx context.Context, o Options) (err error) {
	if o.In == nil {
		o.In = strings.NewReader("")
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	paths := config.ResolvePaths(o.Home, o.ConfigDir)
	logger := newLogger(paths.CacheDir, o.Debug)
	defer recoverCrash(paths.CacheDir, logger, &err)

	cfg, err := config.Load(filepath.Join(paths.ConfigDir, "config.yaml"), o.Home)
	if err != nil {
		return err
	}
	b, err := build(cfg, o, paths)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := b.Close(); cerr != nil {
			logger.Warn("ssh pool not closed cleanly", "err", cerr)
		}
	}()
	root := b.Root

	// Session persistence: <cache>/sessions/<name>.json, restored on start
	// unless --fresh was given. A file that cannot be read or a pane that falls
	// back to its default content warns at startup; neither stops the program.
	sessionPath := filepath.Join(paths.CacheDir, "sessions", "main.json")
	root.SetSessionPath(sessionPath)
	if !o.Fresh {
		restoreSession(root, sessionPath)
	}

	var model tea.Model = root
	if o.Size != nil || o.Ready != nil {
		model = &startup{inner: root, size: o.Size, ready: o.Ready}
	}
	// The inventory tick sits outside the startup wrapper: it forwards every
	// message (including the injected first size) to the wrapped model.
	ticked := newInventoryTicker(model, b.Env, cfg.Inventory.CacheTTL.Duration())
	p := tea.NewProgram(ticked,
		tea.WithInput(o.In),
		tea.WithOutput(o.Out),
		tea.WithAltScreen(),
		tea.WithContext(ctx),
	)
	done := make(chan struct{})
	if o.Resizes != nil {
		go pumpSizes(ctx, p, o.Resizes, done)
	}
	// Prompts and startup warnings queued while the model was being built are
	// delivered from here on.
	b.Attach(p)
	_, runErr := p.Run()
	close(done)
	if runErr != nil {
		return errs.Wrap(errs.Internal, "app.Run", runErr)
	}
	// Save once more on quit: the autosave tick is debounced, so the last change
	// before a quit would otherwise be lost.
	if err := root.SaveSession(); err != nil {
		logger.Warn("session not saved", "path", sessionPath, "err", err)
	}
	return nil
}

// inventoryTicker is the root model plus the inventory refresh tick. The tick
// belongs to app, not to the root: cache_ttl is configuration, and the root
// knows nothing about it. Everything else is handed to the root unchanged.
type inventoryTicker struct {
	root tea.Model
	env  *tui.Env
	ttl  time.Duration
}

var _ tea.Model = (*inventoryTicker)(nil)

// newInventoryTicker wraps root; a non-positive ttl only disables the tick.
func newInventoryTicker(root tea.Model, env *tui.Env, ttl time.Duration) *inventoryTicker {
	return &inventoryTicker{root: root, env: env, ttl: ttl}
}

// Init starts the root and the first tick.
func (t *inventoryTicker) Init() tea.Cmd {
	return tea.Batch(t.root.Init(), tui.InventoryTick(t.ttl))
}

// Update turns a tick into a refresh and re-arms the next one; every other
// message is the root's.
func (t *inventoryTicker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(tui.InventoryTickMsg); ok {
		return t, tea.Batch(tui.InventoryRefreshCmd(t.env), tui.InventoryTick(t.ttl))
	}
	model, cmd := t.root.Update(msg)
	if model != nil {
		t.root = model
	}
	return t, cmd
}

// View renders the root.
func (t *inventoryTicker) View() string { return t.root.View() }

// restoreSession loads the session file and hands it to the root. "No file yet"
// is the normal first start; anything else is reported as a startup warning and
// the fresh session is kept.
func restoreSession(root *tui.Root, path string) {
	f, err := mux.Load(path)
	switch {
	case err == nil:
		if rerr := root.RestoreSession(f); rerr != nil {
			root.AddWarning(fmt.Sprintf("session restored with warnings: %v", rerr))
		}
	case errors.Is(err, os.ErrNotExist):
		// First start: nothing to restore.
	default:
		root.AddWarning(fmt.Sprintf("session not restored: %v", err))
	}
}

// resolveGlyphs resolves the active glyph set once, at startup. A non-empty
// JUMPER_GLYPHS value wins over the config; "auto" and an empty value mean
// unicode. An unknown value in either place is an errs.Config error naming its
// source, never a silent fallback. It is the only place the UI's glyph set is
// read from the environment (BUILD-PLAN §6.1 puts os.Getenv in internal/app).
func resolveGlyphs(cfg, env string) (render.GlyphSet, error) {
	name, source := env, "JUMPER_GLYPHS"
	if name == "" {
		name, source = cfg, "ui.glyphs"
	}
	if name == "" || name == "auto" {
		name = "unicode"
	}
	set, err := render.Glyphs(name)
	if err != nil {
		return render.GlyphSet{}, errs.New(errs.Config, "app.resolveGlyphs",
			fmt.Sprintf("%s: unknown glyph set %q (want auto, unicode, nerd or ascii)", source, name))
	}
	return set, nil
}

// tableFactory is the default view factory: the generic table. It lives here,
// not in internal/tui, because views imports tui and the root may not.
func tableFactory(env *tui.Env, q resource.Query) (tui.View, error) {
	b, spec, ok := env.Registry.Lookup(string(q.Kind))
	if !ok {
		return nil, errs.New(errs.Config, "app.tableFactory",
			fmt.Sprintf("unknown kind %q", q.Kind))
	}
	return views.NewTable(env, b, spec, q), nil
}
