package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/action"
	"jumper/internal/backend"
	"jumper/internal/backend/fake"
	"jumper/internal/backend/linux"
	"jumper/internal/backend/salt"
	"jumper/internal/bus"
	"jumper/internal/bus/generic"
	"jumper/internal/config"
	"jumper/internal/interact"
	"jumper/internal/inventory"
	"jumper/internal/investigation"
	"jumper/internal/redact"
	"jumper/internal/resource"
	"jumper/internal/targets"
	"jumper/internal/transport"
	"jumper/internal/transport/ssh"
	"jumper/internal/tui"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
	"jumper/internal/tui/views"
)

// built is one assembled run: the model, the environment every view reads and
// the resources the caller has to release. internal/app builds it, runs it and
// then Close()s it.
type built struct {
	Root     *tui.Root
	Env      *tui.Env
	Registry *backend.Registry
	Logger   *slog.Logger
	Paths    config.Paths
	// Inventory is the merged host store; nil in --demo.
	Inventory *inventoryRuntime
	// Prompter is what the SSH handshakes ask through (host keys, passphrases);
	// nil in --demo.
	Prompter interact.Prompter
	// Session is the per-session state every pane shares. It is created before
	// the bus so the bus's ReadOnly callback can read the same value.
	Session *tui.SessionState

	// actionBus is the only write path (BUILD-PLAN §4.5); nil in --demo.
	actionBus bus.Bus
	// planners are the write-capable backends, keyed by registration name.
	planners map[string]action.Planner
	// execs are the transports `--via` may name, keyed by "ssh"/"salt".
	execs map[string]transport.Executor
	// saltOpts is what the Salt commands read from the configuration.
	saltOpts tui.SaltOptions
	// closers release what build started and Close does not own otherwise.
	closers []func()

	ssh    *sshStack
	bridge *uiBridge
	// m3 is the M3 layer: policy, redactor, record, knowledge index and the
	// action bus; nil in --demo, where nothing can write.
	m3 *m3Runtime

	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

// build assembles a run from cfg, in order: logger → prompter → SSH stack →
// inventory sources and store → Linux backend → registry → Env → commands →
// root. Nothing is dialled and no inventory source is loaded on this path: the
// first frame comes from the cache (or an empty list) and data streams in
// afterwards. The caller must Close the result.
func build(cfg config.Config, o Options, paths config.Paths) (*built, error) {
	log := newLogger(paths.CacheDir, o.Debug)
	b := &built{Logger: log, Paths: paths, bridge: newUIBridge()}
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	b.Session = &tui.SessionState{Name: "main", ReadOnly: o.ReadOnly}

	if o.Demo {
		if err := b.buildDemo(cfg, o); err != nil {
			_ = b.Close()
			return nil, err
		}
		return b, nil
	}
	if err := b.buildReal(ctx, cfg, o, log); err != nil {
		_ = b.Close()
		return nil, err
	}
	return b, nil
}

// buildDemo assembles the fake backend run: no SSH, no inventory, no prompts.
func (b *built) buildDemo(cfg config.Config, o Options) error {
	reg := backend.NewRegistry()
	if err := reg.Register(fake.New(fake.Options{})); err != nil {
		return fmt.Errorf("register fake backend: %w", err)
	}
	b.Registry = reg
	return b.finish(cfg, o, nil, nil, nil)
}

// buildReal assembles the SSH + Linux (+ Salt) run.
func (b *built) buildReal(ctx context.Context, cfg config.Config, o Options, log *slog.Logger) error {
	b.Prompter = buildPrompter(cfg, o, b.bridge)

	stack, err := buildSSHWith(cfg, o, log, b.Prompter, newAgentSocket(o))
	if err != nil {
		return err
	}
	b.ssh = stack

	// buildExecSet honours ssh.transports: hosts matching a rule go through the
	// system ssh client instead of the native pool (SPEC §7.1).
	ex, err := buildExecSet(cfg, o, stack, b.Paths, log)
	if err != nil {
		return err
	}

	// The Salt runner drives salt, salt-run and salt-key on the master over
	// the same SSH transport. No master means no Salt at all.
	runner := saltRunner(cfg, ex, log)

	sources, warns := inventorySources(cfg, stack.Resolver, o.Home, runner)
	for _, w := range warns {
		b.bridge.Warning(w)
	}

	store := inventory.New(sources, inventoryRewrites(cfg), inventoryCachePath(b.Paths), nil)
	b.Inventory = &inventoryRuntime{Store: store}
	if err := b.Inventory.LoadCache(); err != nil {
		b.bridge.Warning("inventory cache: " + err.Error())
	}
	// The first frame must show hosts, and the sources wired here are local
	// (ssh_config, static): with no cache they are read once before the UI
	// starts. Anything slower runs in the background, as the ticker in Run
	// does for every later refresh.
	if len(store.Hosts()) == 0 && len(sources) > 0 && allLocal(sources) {
		b.refreshInventorySync(ctx)
	} else {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.refreshInventory(ctx)
		}()
	}

	reg := backend.NewRegistry()

	// The Salt backend is built first because the Linux backend asks it for
	// the extra pivots of a host row (host → minion) through LinkProvider.
	var (
		saltBackend *salt.Backend
		links       backend.LinkProvider
	)
	if runner != nil {
		saltBackend = salt.New(salt.Options{
			Runner:    runner,
			Inventory: store,
			Master:    resource.HostID(strings.TrimSpace(cfg.Salt.Master)),
			Log:       log,
		})
		links = saltBackend
		b.saltOpts = tui.SaltOptions{SuggestOver: cfg.Salt.SuggestOver, Configured: true}
	}

	lb := linux.New(linux.Options{
		Exec:        ex,
		Stream:      ex,
		Inventory:   b.Inventory,
		Concurrency: cfg.SSH.Concurrency,
		Timeout:     cfg.SSH.ExecTimeout.Duration(),
		Log:         log,
		Links:       links,
	})
	if err := reg.Register(lb); err != nil {
		return fmt.Errorf("register linux backend: %w", err)
	}
	b.Registry = reg

	b.execs = map[string]transport.Executor{"ssh": ex}
	if saltBackend != nil {
		if err := reg.Register(saltBackend); err != nil {
			return fmt.Errorf("register salt backend: %w", err)
		}
		saltExec := salt.NewExecutor(runner, saltBackend, salt.ExecOptions{
			Chunk:   cfg.Salt.ListChunk,
			Timeout: cfg.Salt.Timeout.Duration(),
		})
		b.execs["salt"] = saltExec
		b.closers = append(b.closers, saltExec.Close)
	}

	m3, err := buildM3(ctx, cfg, b.Paths, o.Home, b.Session, []namedPlanner{
		{name: "linux", planner: lb},
		{name: "generic", planner: generic.New(b.execs, "ssh")},
		{name: "salt", planner: saltBackend},
	}, log)
	if err != nil {
		return err
	}
	b.m3 = m3
	b.actionBus = m3.Bus
	b.planners = m3.Planners

	if len(sources) == 0 {
		b.bridge.Warning("no inventory sources: add inventory.sources (ssh_config or static) to config.yaml, or run with --demo")
	} else if len(store.Hosts()) == 0 {
		b.bridge.Warning("inventory has no hosts: check inventory.sources (ssh_config aliases, static file path), or run with --demo")
	}
	return b.finish(cfg, o, b.Inventory, ex, ex)
}

// saltRunner builds the master's command runner, or nil when salt.master is
// unset (Salt unconfigured, BUILD-PLAN §11).
func saltRunner(cfg config.Config, ex transport.Executor, log *slog.Logger) *salt.Runner {
	master := strings.TrimSpace(cfg.Salt.Master)
	if master == "" {
		return nil
	}
	return salt.NewRunner(ex, resource.HostID(master), salt.RunnerOptions{
		Sudo:    cfg.Salt.Sudo,
		Timeout: cfg.Salt.Timeout.Duration(),
		Log:     log,
	})
}

// wireBus builds the action bus and registers every planner: the generic `:run`
// one over the transports, and Salt's when a master is configured.
func (b *built) wireBus(ctx context.Context, cfg config.Config, saltBackend *salt.Backend, log *slog.Logger) {
	busOpts := bus.Options{
		Now:     time.Now,
		Log:     log,
		Context: ctx,
		Rollout: bus.RolloutConfig{
			Canary:                  cfg.Rollout.Canary,
			BatchPercent:            cfg.Rollout.BatchPercent,
			BatchMax:                cfg.Rollout.BatchMax,
			FailureThresholdPercent: cfg.Rollout.FailureThresholdPercent,
			TypedConfirmOver:        cfg.Rollout.TypedConfirmOver,
		},
	}
	b.actionBus = bus.New(busOpts)
	b.planners = make(map[string]action.Planner, 2)
	// One planner instance per name: the bus and the actions menu must see the
	// same one.
	runPlanner := generic.New(b.execs, "ssh")
	if err := b.actionBus.Register("generic", runPlanner); err != nil {
		b.bridge.Warning("action bus: " + err.Error())
	} else {
		b.planners["generic"] = runPlanner
	}
	if saltBackend != nil {
		if err := b.actionBus.Register("salt", saltBackend); err != nil {
			b.bridge.Warning("action bus: " + err.Error())
		} else {
			b.planners["salt"] = saltBackend
		}
	}
}

// finish builds the Env, the command registry and the root model. ex and shells
// are nil in --demo.
func (b *built) finish(cfg config.Config, o Options, inv inventory.Inventory, ex transport.Executor, shells transport.ShellOpener) error {
	theme.SetProfile(cfg.UI.Color)
	glyphs, err := resolveGlyphs(cfg.UI.Glyphs, os.Getenv("JUMPER_GLYPHS"))
	if err != nil {
		return err
	}
	prefix, err := tui.PrefixKey(cfg.Mux.Prefix, cfg.Mux.PrefixInTmux, o.Env["TMUX"] != "")
	if err != nil {
		return err
	}
	session := b.Session
	if session == nil {
		session = &tui.SessionState{Name: "main", ReadOnly: o.ReadOnly}
	}
	// A policy that says readonly: true shows the RO badge from the first
	// frame: the bus refuses writes either way, and the status line must not
	// claim otherwise.
	if b.m3 != nil && b.m3.Policy.ReadOnly() {
		session.ReadOnly = true
	}

	env := &tui.Env{
		Registry:  b.Registry,
		Inventory: inv,
		Sets:      targets.NewStore(b.Paths.ConfigDir),
		Session:   session,
		Exec:      ex,
		Shells:    shells,
		Theme:     theme.Dark(),
		Glyphs:    glyphs,
		Keys:      keymap.Default(),
		Now:       time.Now,
		Factories: make(map[resource.KindName]tui.Factory),
		Log:       b.Logger,
		// The write path and what `--via` may name (M3/M4).
		Bus:      b.actionBus,
		Planners: b.planners,
		Execs:    b.execs,
		Salt:     b.saltOpts,
		ResultsView: func(env *tui.Env, title string, results []action.StepResult) tui.View {
			return views.NewFanoutFromResults(env, title, results)
		},
	}
	for kind, f := range views.Factories() {
		env.Factories[kind] = f
	}
	if b.m3 != nil {
		env.Policy = b.m3.Policy
		env.Redact = b.m3.Redact
		env.Recorder = b.m3.Log
		env.Bus = b.m3.Bus
		env.Planners = b.m3.Planners
		env.Knowledge = b.m3.Index
		env.ResultsView = func(e *tui.Env, title string, results []action.StepResult) tui.View {
			return views.NewFanoutFromResults(e, title, results)
		}
	}
	b.Env = env

	cmds := tui.NewCommands()
	tui.RegisterBuiltins(cmds)
	if err := tui.RegisterMuxCommands(cmds); err != nil {
		return err
	}
	if err := tui.RegisterHealthCommands(cmds); err != nil {
		return err
	}
	// `:run` and `:probe` write through the bus or the chosen transport; the
	// Salt family adds targets, the dry run, module runs and ping.
	if err := tui.RegisterPolicyCommands(cmds); err != nil {
		return err
	}
	if err := tui.RegisterRunCommands(cmds, b.actionBus); err != nil {
		return err
	}
	if err := tui.RegisterProbeCommands(cmds, fanoutOpener); err != nil {
		return err
	}
	if err := tui.RegisterSaltCommands(cmds, b.actionBus); err != nil {
		return err
	}

	// Requirement 3: the session opens on the host list when the inventory has
	// hosts. With none it still opens there and the warning above says how to
	// add some; there is no other view to start in.
	b.Root = tui.NewRoot(env, cmds, tableFactory, resource.Query{Kind: linux.KindHost}, prefix)
	if b.m3 != nil {
		// The shell guard rails and the sync mode ask the same policy the bus
		// applies, so a rule cannot be enforced in one place and not the other.
		b.Root.SetPolicy(b.m3.Policy)
	}
	return nil
}

// fanoutOpener builds the grouped fan-out view a `:probe` run opens. It is a
// closure around views.NewFanout because package tui injects the opener.
func fanoutOpener(env *tui.Env, title string, cmd transport.Command, hosts []resource.HostID, normalize func(string) string) tui.View {
	return views.NewFanout(env, title, cmd, hosts, normalize)
}

// namedPlanner is one planner and the name the bus registers it under. The
// slice order decides which planner wins a spec ID that two of them claim.
type namedPlanner struct {
	name    string
	planner action.Planner
}

// m3Runtime is the M3 assembly as the rest of the app sees it: the guard rails,
// the redactor, the record, the knowledge index and the action bus. Nothing
// here touches the UI, so a lab test can build the same layer without a
// terminal.
type m3Runtime struct {
	Policy   *bus.Policy
	Redact   *redact.Redactor
	Log      *investigation.FileLog
	Index    *investigation.Index
	Bus      bus.Bus
	Planners map[string]action.Planner
}

// buildM3 assembles the M3 layer, in the order the pieces depend on each other:
// policy (the file is the source of the extra redaction patterns) → redactor →
// investigation log under knowledge.root → knowledge index → bus → planners.
// ctx is the session context and becomes the bus's root context, so a rollout
// outlives the pane that started it. A missing policy file means the built-in
// policy, and a knowledge tree that cannot be read leaves Index nil instead of
// stopping the start.
func buildM3(ctx context.Context, cfg config.Config, paths config.Paths, home string, session *tui.SessionState, planners []namedPlanner, logger *slog.Logger) (*m3Runtime, error) {
	policy, err := bus.LoadPolicy(filepath.Join(paths.ConfigDir, "policy.yaml"))
	if err != nil {
		return nil, err
	}
	red, err := redact.New(policy.RedactPatterns())
	if err != nil {
		return nil, err
	}
	root := config.ExpandHome(cfg.Knowledge.Root, home)
	rec := investigation.NewLog(root, red, nil)
	index, ixErr := investigation.BuildIndex(root)
	if ixErr != nil {
		if logger != nil {
			logger.Warn("knowledge index not built", "root", root, "err", ixErr)
		}
		index = nil
	}

	b := bus.New(bus.Options{
		Policy:   policy,
		Rollout:  rolloutConfig(cfg.Rollout),
		Recorder: rec,
		Redact:   red,
		ReadOnly: func() bool { return sessionReadOnly(session) },
		Context:  ctx,
		Log:      logger,
	})
	rt := &m3Runtime{
		Policy:   policy,
		Redact:   red,
		Log:      rec,
		Index:    index,
		Bus:      b,
		Planners: make(map[string]action.Planner, len(planners)),
	}
	for _, np := range planners {
		if np.planner == nil || np.name == "" {
			continue
		}
		if err := b.Register(np.name, np.planner); err != nil {
			return nil, err
		}
		rt.Planners[np.name] = np.planner
	}
	return rt, nil
}

// rolloutConfig mirrors config's rollout section into the bus's own type.
func rolloutConfig(r config.RolloutConfig) bus.RolloutConfig {
	return bus.RolloutConfig{
		Canary:                  r.Canary,
		BatchPercent:            r.BatchPercent,
		BatchMax:                r.BatchMax,
		FailureThresholdPercent: r.FailureThresholdPercent,
		TypedConfirmOver:        r.TypedConfirmOver,
	}
}

// sessionReadOnly reports the session-level read-only switch the bus consults.
func sessionReadOnly(s *tui.SessionState) bool {
	return s != nil && s.ReadOnly
}

// Close stops the background inventory work and shuts the connection pool down.
// It is safe to call more than once.
func (b *built) Close() error {
	if b == nil {
		return nil
	}
	b.closeOnce.Do(func() {
		if b.cancel != nil {
			b.cancel()
		}
		b.wg.Wait()
		for _, close := range b.closers {
			close()
		}
		if b.ssh != nil {
			b.closeErr = b.ssh.Close()
		}
	})
	return b.closeErr
}

// Attach hands the running program to the bridge: flashes queued before the
// program existed, and every prompt the SSH dialogs raise, are delivered from
// now on.
func (b *built) Attach(p *tea.Program) {
	if b == nil || p == nil {
		return
	}
	b.bridge.Attach(p.Send)
}

// Warnings returns the warning lines queued so far. The tests use it to assert
// what the user would have been told; the UI shows them as flashes.
func (b *built) Warnings() []string {
	if b == nil {
		return nil
	}
	return b.bridge.Warnings()
}

// refreshInventory loads every source once. The store keeps the previous hosts
// of a source that fails, so the caller only has to report the failures.
func (b *built) refreshInventory(ctx context.Context) {
	if b.Inventory == nil {
		return
	}
	failed := b.Inventory.Refresh(ctx)
	for _, line := range sortedSourceErrors(failed) {
		b.bridge.Warning("inventory: " + line)
	}
}

// firstLoadTimeout bounds the one refresh build does before the UI starts.
// Only local sources (ssh_config, static) are wired, so this is a file read,
// not a network call.
const firstLoadTimeout = 30 * time.Second

// refreshInventorySync is refreshInventory for the first start with no cache.
func (b *built) refreshInventorySync(ctx context.Context) {
	if b.Inventory == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, firstLoadTimeout)
	defer cancel()
	b.refreshInventory(ctx)
}

// buildPrompter returns what the SSH stack asks for host keys and passphrases:
// the caller's Prompter when it has one, else the TUI's, which shows a modal
// dialog through the program bridge.
func buildPrompter(cfg config.Config, o Options, bridge *uiBridge) interact.Prompter {
	if o.Prompter != nil {
		return o.Prompter
	}
	return tui.NewPrompter(bridge.Send, cfg.Interact.Timeout.Duration())
}

// inventorySources builds the sources cfg declares. A type this build cannot
// serve yet is skipped with a warning instead of failing the start. runner is
// the Salt master's runner; a salt source without one is skipped.
func inventorySources(cfg config.Config, resolver *ssh.Resolver, home string, runner *salt.Runner) ([]inventory.Source, []string) {
	var out []inventory.Source
	var warns []string
	for _, src := range cfg.Inventory.Sources {
		switch src.Type {
		case "ssh_config":
			if resolver == nil {
				warns = append(warns, "inventory: ssh_config source without an ssh_config resolver; skipped")
				continue
			}
			out = append(out, &inventory.SSHConfigSource{
				Resolver: resolver,
				// Bastions are reachable by name but never fleet targets.
				ExcludePatterns: jumpAliases(resolver),
			})
		case "static":
			out = append(out, inventory.NewStaticSource(config.ExpandHome(src.Path, home)))
		case "salt":
			if runner == nil {
				warns = append(warns, "inventory: salt source needs salt.master; skipped")
				continue
			}
			out = append(out, salt.NewSource(runner))
		default:
			warns = append(warns, fmt.Sprintf("inventory: source type %q is not wired yet; skipped", src.Type))
		}
	}
	return out, warns
}

// allLocal reports whether every source reads a local file, so the first
// refresh is safe to run before the UI starts. The Salt source asks the master
// over SSH, so it never is.
func allLocal(sources []inventory.Source) bool {
	for _, s := range sources {
		if s.Name() == "salt" {
			return false
		}
	}
	return true
}

// inventoryRewrites compiles the identity rewrites of the config. Validation has
// already rejected any expression that does not compile.
func inventoryRewrites(cfg config.Config) []inventory.Rewrite {
	if len(cfg.Inventory.Identity.Rewrites) == 0 {
		return nil
	}
	out := make([]inventory.Rewrite, 0, len(cfg.Inventory.Identity.Rewrites))
	for _, rw := range cfg.Inventory.Identity.Rewrites {
		re, err := regexp.Compile(rw.From)
		if err != nil {
			continue
		}
		out = append(out, inventory.Rewrite{From: re, To: rw.To})
	}
	return out
}

// inventoryCachePath is where the merged host list is cached for an instant
// first frame.
func inventoryCachePath(paths config.Paths) string {
	if paths.CacheDir == "" {
		return ""
	}
	return paths.CacheDir + string(os.PathSeparator) + "inventory.json"
}

// jumpAliases lists the aliases other hosts use as a jump. They stay resolvable
// for `:ssh bastion`, but `path.Match` against these exact names keeps them out
// of the inventory, so a fleet probe never targets a bastion.
func jumpAliases(r *ssh.Resolver) []string {
	if r == nil {
		return nil
	}
	seen := make(map[string]bool)
	for _, alias := range r.Hosts() {
		spec, err := r.Resolve(alias)
		if err != nil {
			continue
		}
		for _, j := range spec.Jumps {
			if j = strings.TrimSpace(j); j != "" {
				seen[j] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for alias := range seen {
		out = append(out, alias)
	}
	sort.Strings(out)
	return out
}

// sortedSourceErrors renders per-source errors as sorted "source: error" lines.
func sortedSourceErrors(failed map[string]error) []string {
	names := make([]string, 0, len(failed))
	for name := range failed {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, name+": "+failed[name].Error())
	}
	return out
}

// inventoryRuntime is the Store the TUI gets as Env.Inventory. It remembers the
// last error of every source so `:health` can list them without refreshing, and
// otherwise behaves exactly like the store.
type inventoryRuntime struct {
	*inventory.Store

	mu   sync.Mutex
	errs map[string]error
}

// Refresh loads every source and records what failed.
func (r *inventoryRuntime) Refresh(ctx context.Context) map[string]error {
	failed := r.Store.Refresh(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(failed) == 0 {
		r.errs = nil
		return failed
	}
	if r.errs == nil {
		r.errs = make(map[string]error, len(failed))
	}
	for name, err := range failed {
		r.errs[name] = err
	}
	for name := range r.errs {
		if _, failed := failed[name]; !failed {
			delete(r.errs, name)
		}
	}
	return failed
}

// SourceErrors returns a copy of the last per-source errors.
func (r *inventoryRuntime) SourceErrors() map[string]error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.errs) == 0 {
		return nil
	}
	out := make(map[string]error, len(r.errs))
	for name, err := range r.errs {
		out[name] = err
	}
	return out
}

// runtimeTransport is the SSH transport as the TUI sees it: everything the
// transport does, plus the pool's holdings for `:health`.
type runtimeTransport struct {
	*ssh.Transport
	pool *ssh.Pool
}

// PoolStats reports what the connection pool currently holds.
func (t *runtimeTransport) PoolStats() tui.PoolStats {
	if t == nil || t.pool == nil {
		return tui.PoolStats{}
	}
	st := t.pool.Stats()
	return tui.PoolStats{Bastions: st.Bastions, Targets: st.Targets, Leases: st.Leases}
}

// uiBridge carries messages from non-UI goroutines to the program. Before the
// program exists (a source that fails while the model is still being built)
// messages are queued: they are delivered by Attach, in order.
type uiBridge struct {
	mu     sync.Mutex
	send   func(tea.Msg)
	queued []tea.Msg
	warns  []string
}

// newUIBridge returns an empty bridge.
func newUIBridge() *uiBridge { return &uiBridge{} }

// Send delivers msg to the program, or queues it until Attach.
func (u *uiBridge) Send(msg tea.Msg) {
	u.mu.Lock()
	if u.send == nil {
		if len(u.queued) < 32 {
			u.queued = append(u.queued, msg)
		}
		u.mu.Unlock()
		return
	}
	send := u.send
	u.mu.Unlock()
	send(msg)
}

// Attach installs the program's Send and flushes what was queued.
func (u *uiBridge) Attach(send func(tea.Msg)) {
	u.mu.Lock()
	u.send = send
	queued := u.queued
	u.queued = nil
	u.mu.Unlock()
	for _, msg := range queued {
		send(msg)
	}
}

// Warning flashes a warning line and remembers its text.
func (u *uiBridge) Warning(text string) {
	if text == "" {
		return
	}
	u.mu.Lock()
	u.warns = append(u.warns, text)
	u.mu.Unlock()
	u.Send(tui.FlashMsg{Level: tui.FlashWarn, Text: text})
}

// Warnings returns the warning texts queued so far.
func (u *uiBridge) Warnings() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.warns...)
}
