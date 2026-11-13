// Package app is the composition root: it loads configuration, builds the
// backends, the shared tui.Env, the command registry and the root model, then
// runs the Bubble Tea program. It is the only package besides cmd/jumper that
// may touch os.Stdin/Stdout/Stderr (BUILD-PLAN §6.1); it only reads the ones
// handed to Run.
package app

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend"
	"jumper/internal/backend/fake"
	"jumper/internal/config"
	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/tui"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
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
	Version   string
	Env       map[string]string
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
	theme.SetProfile(cfg.UI.Color)

	reg, err := buildRegistry(o)
	if err != nil {
		return err
	}

	prefix, err := tui.PrefixKey(cfg.Mux.Prefix, cfg.Mux.PrefixInTmux, o.Env["TMUX"] != "")
	if err != nil {
		return err
	}

	env := &tui.Env{
		Registry:  reg,
		Session:   &tui.SessionState{Name: "main"},
		Theme:     theme.Dark(),
		Keys:      keymap.Default(),
		Now:       time.Now,
		Factories: make(map[resource.KindName]tui.Factory),
		Log:       logger,
	}
	for kind, f := range views.Factories() {
		env.Factories[kind] = f
	}
	cmds := tui.NewCommands()
	tui.RegisterBuiltins(cmds)
	if err := tui.RegisterMuxCommands(cmds); err != nil {
		return err
	}

	root := tui.NewRoot(env, cmds, tableFactory, resource.Query{Kind: "host"}, prefix)

	p := tea.NewProgram(root,
		tea.WithInput(o.In),
		tea.WithOutput(o.Out),
		tea.WithAltScreen(),
		tea.WithContext(ctx),
	)
	if _, err := p.Run(); err != nil {
		return errs.Wrap(errs.Internal, "app.Run", err)
	}
	return nil
}

// buildRegistry registers the backends for this run. In M0 only the fake
// backend exists; real backends arrive with the SSH/Linux work (M2), which is
// where the configured case grows its checks.
func buildRegistry(o Options) (*backend.Registry, error) {
	reg := backend.NewRegistry()
	if !o.Demo {
		return nil, errs.New(errs.Config, "app.Run", "no backends configured; try --demo")
	}
	if err := reg.Register(fake.New(fake.Options{})); err != nil {
		return nil, fmt.Errorf("register fake backend: %w", err)
	}
	return reg, nil
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
