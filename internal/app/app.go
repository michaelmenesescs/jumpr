// Package app is the composition root: it loads configuration, builds the
// per-process core (transports, inventory, backends, policy), then one session
// with its own bus, environment, root model and Bubble Tea program, and runs it
// until the user quits (BUILD-PLAN §6). It is the only package besides
// cmd/jumper that may touch os.Stdin/Stdout/Stderr (BUILD-PLAN §6.1); it only
// reads the ones handed to Run.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
	// prompter is built, which shows a dialog.
	Prompter interact.Prompter

	// Size is the terminal size to attach with. cmd/jumper reads it from the
	// terminal with golang.org/x/term; nil means the default 80×24 (a run over
	// pipes has no terminal to ask). Bubble Tea cannot detect it any more: the
	// session's output is a switchable pipe, not a TTY.
	Size *Size
	// Resizes carries later size changes for a run that cannot see SIGWINCH.
	// Each value becomes one tea.WindowSizeMsg; the pump stops with ctx.
	Resizes <-chan Size
	// Ready, when non-nil, is closed once the first client is attached and the
	// program has taken its first size. Tests wait on it instead of sleeping.
	Ready chan<- struct{}
}

// Size is a terminal size in cells.
type Size struct{ Cols, Rows int }

// defaultSize is what a run over pipes starts with when the caller has no
// terminal to measure.
var defaultSize = Size{Cols: 80, Rows: 24}

// Run loads configuration, builds the core and one session, attaches the given
// reader and writer, and waits until the session ends: the user quit with `:q`,
// closed the last pane, or ctx was canceled. A panic anywhere below is
// recovered, written to <cache>/crash/ and returned as an errs.Internal error.
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

	// One core per process; every session shares it (BUILD-PLAN §6).
	core, err := newCore(ctx, cfg, paths, o.Env, o, logger)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := core.Close(); cerr != nil {
			logger.Warn("ssh pool not closed cleanly", "err", cerr)
		}
	}()

	sess, err := core.NewSession("main")
	if err != nil {
		return err
	}
	defer func() {
		if cerr := sess.close(); cerr != nil {
			logger.Warn("session not closed cleanly", "err", cerr)
		}
	}()

	// Standalone mode is a session with one client that is always attached.
	size := defaultSize
	if o.Size != nil {
		size = *o.Size
	}
	if err := sess.Attach(o.In, o.Out, size, o.Env); err != nil {
		return err
	}
	if o.Resizes != nil {
		go pumpSizes(ctx, sess, o.Resizes)
	}
	if o.Ready != nil {
		close(o.Ready)
	}

	select {
	case <-sess.Done():
	case <-ctx.Done():
		sess.Kill()
		<-sess.Done()
	}

	// Save once more on quit: the autosave tick is debounced, so the last change
	// before a quit would otherwise be lost.
	if err := sess.save(); err != nil {
		logger.Warn("session not saved", "path", sess.path, "err", err)
	}
	return nil
}

// pumpSizes turns the Options.Resizes channel into tea.WindowSizeMsg values
// until ctx or the channel ends. It is I/O-free and never inside Update, so the
// UI cannot block on it.
func pumpSizes(ctx context.Context, sess *Session, ch <-chan Size) {
	for {
		select {
		case <-ctx.Done():
			return
		case size, ok := <-ch:
			if !ok {
				return
			}
			sess.Resize(size)
		}
	}
}

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
