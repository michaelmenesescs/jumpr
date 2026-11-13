package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"jumper/internal/action"
	"jumper/internal/backend"
	"jumper/internal/backend/chef"
	"jumper/internal/backend/elastic"
	"jumper/internal/backend/fake"
	"jumper/internal/backend/grafana"
	"jumper/internal/backend/k8s"
	"jumper/internal/backend/linux"
	"jumper/internal/backend/prom"
	"jumper/internal/backend/salt"
	"jumper/internal/bus"
	"jumper/internal/bus/generic"
	"jumper/internal/clip"
	"jumper/internal/config"
	"jumper/internal/errs"
	"jumper/internal/interact"
	"jumper/internal/inventory"
	"jumper/internal/investigation"
	"jumper/internal/plugin"
	"jumper/internal/redact"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/safe"
	"jumper/internal/transport"
	"jumper/internal/transport/httpx"
	"jumper/internal/transport/ssh"
	"jumper/internal/transport/sysssh"
	"jumper/internal/tui"
	"jumper/internal/tui/theme"
	"jumper/internal/tui/views"
)

// PrometheusBackend builds the Prometheus backend from cfg's `prometheus:`
// section (SPEC §8.3).
//
// One backend serves every configured server: the kinds are global, so two
// backends would collide in the registry. Each entry names a connection profile
// in connections.yaml through `connection`; an entry without one gets a plain
// client, which is what an unauthenticated in-cluster Prometheus needs. It
// returns nil when no server is configured, so callers can skip registration.
//
// The wiring lives here rather than in app.go because app.buildRegistry is
// still the M0 version; the task that owns app.go calls this function to add
// the backend to the registry.
func PrometheusBackend(cfg config.Config, profiles map[string]*httpx.Profile, deps httpx.Deps, log *slog.Logger) (*prom.Backend, error) {
	if len(cfg.Prometheus) == 0 {
		return nil, nil
	}
	servers := make([]prom.Server, 0, len(cfg.Prometheus))
	seen := make(map[string]bool, len(cfg.Prometheus))

	for i, entry := range cfg.Prometheus {
		name := entry.Name
		if seen[name] {
			return nil, errs.New(errs.Config, "app.PrometheusBackend",
				fmt.Sprintf("prometheus[%d]: %q is configured twice", i, name))
		}
		seen[name] = true

		profile, err := profileFor("prometheus", name, entry.Connection, profiles)
		if err != nil {
			return nil, err
		}
		hc, err := profile.Client(deps)
		if err != nil {
			return nil, errs.Wrap(errs.Config, "app.PrometheusBackend", err).WithHost(name)
		}
		for _, warning := range hc.Warnings() {
			if log != nil {
				log.Warn("prometheus client warning", "server", name, "warning", warning)
			}
		}

		client, err := prom.NewClient(prom.ClientOptions{
			URL:    entry.URL,
			Name:   name,
			Client: hc,
			Warn:   warnFunc("prometheus", log, name),
		})
		if err != nil {
			return nil, err
		}
		servers = append(servers, prom.Server{Name: name, Client: client})
	}
	return prom.New(prom.Options{Servers: servers, Log: log}), nil
}

// GrafanaHooks are the app-supplied effects a Grafana dashboard needs. `o` on a
// dashboard or a panel calls Open with the browser URL; when Open is nil the
// backend's default applies and the URL is copied to Clipboard instead
// (BUILD-PLAN §6.1 rule 6).
type GrafanaHooks struct {
	Open      func(url string) error
	Clipboard clip.Clipboard
}

// GrafanaBackend builds the Grafana backend from cfg's `grafana:` section
// (SPEC §8.4).
//
// One backend serves every configured server: the kinds are global, so two
// backends would collide in the registry. Each entry names a connection profile
// in connections.yaml through `connection`; an entry without one gets a plain
// client. It returns nil when no server is configured, so callers can skip
// registration.
//
// The wiring lives here rather than in app.go because app.buildRegistry is
// still the M0 version; the task that owns app.go calls this function to add
// the backend to the registry.
func GrafanaBackend(cfg config.Config, profiles map[string]*httpx.Profile, deps httpx.Deps, hooks GrafanaHooks, log *slog.Logger) (*grafana.Backend, error) {
	if len(cfg.Grafana) == 0 {
		return nil, nil
	}
	servers := make([]grafana.Server, 0, len(cfg.Grafana))
	seen := make(map[string]bool, len(cfg.Grafana))

	for i, entry := range cfg.Grafana {
		name := entry.Name
		if seen[name] {
			return nil, errs.New(errs.Config, "app.GrafanaBackend",
				fmt.Sprintf("grafana[%d]: %q is configured twice", i, name))
		}
		seen[name] = true

		profile, err := profileFor("grafana", name, entry.Connection, profiles)
		if err != nil {
			return nil, err
		}
		hc, err := profile.Client(deps)
		if err != nil {
			return nil, errs.Wrap(errs.Config, "app.GrafanaBackend", err).WithHost(name)
		}
		for _, warning := range hc.Warnings() {
			if log != nil {
				log.Warn("grafana client warning", "server", name, "warning", warning)
			}
		}

		client, err := grafana.NewClient(grafana.ClientOptions{
			URL:    entry.URL,
			Name:   name,
			Client: hc,
			Warn:   warnFunc("grafana", log, name),
		})
		if err != nil {
			return nil, err
		}
		servers = append(servers, grafana.Server{Name: name, Client: client})
	}
	return grafana.New(grafana.Options{
		Servers:   servers,
		Log:       log,
		Open:      hooks.Open,
		Clipboard: hooks.Clipboard,
	}), nil
}

// ElasticBackend builds the Elasticsearch backend from cfg's `elasticsearch:`
// section (SPEC §8.5).
//
// One backend serves every configured cluster: the log kind is global, so two
// backends would collide in the registry. Each entry names a connection profile
// in connections.yaml through `connection`; an entry without one gets a plain
// client. Each cluster carries its saved log sources (index pattern and the
// time, host, message and level fields). It returns nil when no cluster is
// configured, so callers can skip registration. A redactor cleans every
// document the backend hands out.
//
// The wiring lives here rather than in app.go because app.buildRegistry is
// still the M0 version; the task that owns app.go calls this function to add
// the backend to the registry.
func ElasticBackend(cfg config.Config, profiles map[string]*httpx.Profile, deps httpx.Deps, redactText func(string) string, log *slog.Logger) (*elastic.Backend, error) {
	if len(cfg.Elasticsearch) == 0 {
		return nil, nil
	}
	clusters := make([]elastic.Cluster, 0, len(cfg.Elasticsearch))
	seen := make(map[string]bool, len(cfg.Elasticsearch))

	for i, entry := range cfg.Elasticsearch {
		name := entry.Name
		if seen[name] {
			return nil, errs.New(errs.Config, "app.ElasticBackend",
				fmt.Sprintf("elasticsearch[%d]: %q is configured twice", i, name))
		}
		seen[name] = true

		profile, err := profileFor("elasticsearch", name, entry.Connection, profiles)
		if err != nil {
			return nil, err
		}
		hc, err := profile.Client(deps)
		if err != nil {
			return nil, errs.Wrap(errs.Config, "app.ElasticBackend", err).WithHost(name)
		}
		for _, warning := range hc.Warnings() {
			if log != nil {
				log.Warn("elasticsearch client warning", "cluster", name, "warning", warning)
			}
		}

		client, err := elastic.NewClient(elastic.ClientOptions{
			URL:    entry.URL,
			Name:   name,
			Client: hc,
			Warn:   warnFunc("elasticsearch", log, name),
		})
		if err != nil {
			return nil, err
		}
		sources := make([]elastic.LogSource, 0, len(entry.Sources))
		for _, src := range entry.Sources {
			sources = append(sources, elastic.LogSource{
				Name:         src.Name,
				Index:        src.Index,
				TimeField:    src.TimeField,
				HostField:    src.HostField,
				MessageField: src.MessageField,
				LevelField:   src.LevelField,
			})
		}
		clusters = append(clusters, elastic.Cluster{Name: name, Client: client, Sources: sources})
	}
	return elastic.New(elastic.Options{Clusters: clusters, Log: log, Redact: redactText}), nil
}

// KubernetesBackend builds the Kubernetes backend from cfg's `kubernetes:`
// section (SPEC §8.2).
//
// kubeconfig is an explicit path; empty means the standard rules: $KUBECONFIG
// (kubeconfigEnv — the caller passes the value in, because only internal/app
// may read the environment, BUILD-PLAN §6.1) and then ~/.kube/config. protected
// lists kubernetes.contexts_protected, whose contexts make the session
// read-only when selected (SPEC §16). The backend is built even without a
// kubeconfig: it then reports HealthUnconfigured, as a grey dot, rather than
// leaving the k8s kinds unregistered.
//
// The wiring lives here rather than in app.go because app.buildRegistry is
// still the M0 version; the task that owns app.go calls this function to add
// the backend to the registry and RegisterK8sCommands to the command registry.
func KubernetesBackend(cfg config.Config, home, kubeconfigEnv string, log *slog.Logger) (*k8s.Backend, error) {
	return k8s.New(k8s.Options{
		Kubeconfig:    cfg.Kubernetes.Kubeconfig,
		KubeconfigEnv: kubeconfigEnv,
		Home:          home,
		Protected:     cfg.Kubernetes.ContextsProtected,
		Log:           log,
	})
}

// KubernetesDynamicBackend builds the Kubernetes backend that serves every
// listable API resource of the cluster as a kind (task 112), not only the typed
// kinds KubernetesBackend knows. It is the same client, the same contexts and
// the same health probe; the difference is the discovery cache and the Table
// API behind the generic kinds.
//
// One discovery pass runs here, bounded, so the discovered kinds exist before
// the registry keys them: Registry.Register fixes the kind → backend map at
// registration, and a kind discovered later would resolve to nothing. A cluster
// that cannot be reached is not an error — the typed kinds still work and
// Health reports the problem.
//
// The caller registers the backend and the planner KubernetesPatchPlanner
// returns; the edit flow is k8s.apply-patch, because there are no local editor
// panes in v1 (task 112 requirement 2).
func KubernetesDynamicBackend(cfg config.Config, home, kubeconfigEnv string, log *slog.Logger) (*k8s.Dynamic, error) {
	b, err := k8s.NewDynamic(k8s.Options{
		Kubeconfig:    cfg.Kubernetes.Kubeconfig,
		KubeconfigEnv: kubeconfigEnv,
		Home:          home,
		Protected:     cfg.Kubernetes.ContextsProtected,
		Log:           log,
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), discoveryBudget)
	defer cancel()
	if err := b.RefreshKinds(ctx); err != nil && log != nil {
		log.Warn("kubernetes discovery", "error", err)
	}
	return b, nil
}

// discoveryBudget bounds the one discovery pass KubernetesDynamicBackend makes
// before the registry is built: long enough for a slow API server, short enough
// that a cluster that is down does not hold up the session.
const discoveryBudget = 15 * time.Second

// KubernetesPatchPlanner is the planner of the v1 edit flow: a JSON merge patch
// previewed with a server-side dry run and applied through the bus. It is
// registered under its own name and claims only k8s.apply-patch.
func KubernetesPatchPlanner(b *k8s.Dynamic) action.Planner {
	if b == nil {
		return nil
	}
	return b.PatchPlanner()
}

// Alertmanager builds the Alertmanager backend and its silence planner from
// cfg's `alertmanager:` section (SPEC §8.3), over one set of clients.
//
// The backend's health dot and refs are named "am"; the planner is the one the
// bus registers, and every silence it plans is signed with creator, the local
// user name the caller passes in (app.defaultUser). An entry names a connection
// profile in connections.yaml through `connection`; without one it gets a plain
// client. Both results are nil when no server is configured.
func Alertmanager(cfg config.Config, profiles map[string]*httpx.Profile, deps httpx.Deps, creator string, log *slog.Logger) (*prom.AlertmanagerBackend, *prom.AlertmanagerPlanner, error) {
	if len(cfg.Alertmanager) == 0 {
		return nil, nil, nil
	}
	servers := make([]prom.AlertmanagerServer, 0, len(cfg.Alertmanager))
	seen := make(map[string]bool, len(cfg.Alertmanager))

	for i, entry := range cfg.Alertmanager {
		name := entry.Name
		if seen[name] {
			return nil, nil, errs.New(errs.Config, "app.AlertmanagerBackend",
				fmt.Sprintf("alertmanager[%d]: %q is configured twice", i, name))
		}
		seen[name] = true

		profile, err := profileFor("alertmanager", name, entry.Connection, profiles)
		if err != nil {
			return nil, nil, err
		}
		hc, err := profile.Client(deps)
		if err != nil {
			return nil, nil, errs.Wrap(errs.Config, "app.AlertmanagerBackend", err).WithHost(name)
		}
		for _, warning := range hc.Warnings() {
			if log != nil {
				log.Warn("alertmanager client warning", "server", name, "warning", warning)
			}
		}

		client, err := prom.NewAlertmanagerClient(prom.AlertmanagerClientOptions{
			URL:    entry.URL,
			Name:   name,
			Client: hc,
		})
		if err != nil {
			return nil, nil, err
		}
		servers = append(servers, prom.AlertmanagerServer{Name: name, Client: client})
	}
	amBackend := prom.NewAlertmanager(prom.AlertmanagerOptions{Servers: servers, Log: log})
	planner := prom.NewAlertmanagerPlanner(prom.AlertmanagerPlannerOptions{
		Servers: servers,
		Creator: creator,
	})
	return amBackend, planner, nil
}

// ChefBackend builds the Chef backend from cfg's `chef:` section (SPEC §8.7).
//
// Everything it reads is one `knife … -F json` line run on the configured
// workstation through ex: the Chef server is never contacted directly and
// nothing is installed on a node. It returns nil when no workstation is
// configured, so callers can skip registration.
func ChefBackend(cfg config.Config, ex transport.Executor, red *redact.Redactor, log *slog.Logger) (*chef.Backend, error) {
	workstation := strings.TrimSpace(cfg.Chef.Workstation)
	if workstation == "" {
		return nil, nil
	}
	if ex == nil {
		return nil, errs.New(errs.Config, "app.ChefBackend", "chef.workstation is set but there is no ssh transport")
	}
	host := resource.HostID(workstation)
	runner := chef.NewRunner(ex, host, chef.RunnerOptions{
		Knife:   cfg.Chef.Knife,
		Sudo:    cfg.Chef.Sudo,
		Timeout: cfg.Chef.Timeout.Duration(),
		Log:     log,
	})
	return chef.New(chef.Options{
		Runner:      runner,
		Workstation: host,
		Redactor:    red,
		Log:         log,
	}), nil
}

// profileFor returns the connection profile an entry names, or a plain profile
// for an entry without one. A name that is not in connections.yaml is an
// errs.Config error naming the server: silently falling back to no auth would
// turn a typo into a diagnosis. product is the config section the entry came
// from, so the error says which file to fix.

// profileFor returns the connection profile an entry names, or a plain profile
// for an entry without one. A name that is not in connections.yaml is an
// errs.Config error naming the server: silently falling back to no auth would
// turn a typo into a diagnosis. product is the config section the entry came
// from, so the error says which file to fix.
func profileFor(product, server, connection string, profiles map[string]*httpx.Profile) (*httpx.Profile, error) {
	if connection == "" {
		return &httpx.Profile{Name: product + " " + server}, nil
	}
	if p, ok := profiles[connection]; ok && p != nil {
		return p, nil
	}
	return nil, errs.New(errs.Config, "app."+product+"Backend",
		fmt.Sprintf("%s %q: connection %q is not in connections.yaml", product, server, connection))
}

// warnFunc turns a client warning into a log line.
func warnFunc(product string, log *slog.Logger, server string) func(string) {
	if log == nil {
		return nil
	}
	return func(msg string) {
		log.Warn(product, "server", server, "warning", msg)
	}
}

// execSet is what the app runs remote work through: one executor and one
// streamer for the backends, one shell opener for the panes, and the pool
// holding for `:health`. The three faces are the same value on purpose, so a
// routed host is routed for commands, streams and shells alike.
type execSet struct {
	transport.Executor
	transport.Streamer
	transport.ShellOpener

	pool *ssh.Pool
}

// PoolStats reports what the connection pool currently holds.
func (e *execSet) PoolStats() tui.PoolStats {
	if e == nil || e.pool == nil {
		return tui.PoolStats{}
	}
	st := e.pool.Stats()
	return tui.PoolStats{Bastions: st.Bastions, Targets: st.Targets, Leases: st.Leases}
}

// buildExecSet chooses the transports cfg asks for: the native SSH transport
// for every host, or — when ssh.transports routes patterns away — a
// transport.Router that sends those hosts through the system ssh client
// (SPEC §7.1). Nothing is dialled here.
func buildExecSet(cfg config.Config, o Options, stack *sshStack, paths config.Paths, log *slog.Logger) (*execSet, error) {
	if stack == nil || stack.Transport == nil {
		return nil, errs.New(errs.Config, "ssh.build", "no ssh transport")
	}
	if len(cfg.SSH.Transports) == 0 {
		return &execSet{
			Executor:    &runtimeTransport{Transport: stack.Transport, pool: stack.Pool},
			Streamer:    stack.Transport,
			ShellOpener: stack.Transport,
			pool:        stack.Pool,
		}, nil
	}

	// The fallback runs the local ssh with the same ssh_config the native
	// client reads (its first configured file: OpenSSH takes one -F) and the
	// control sockets next to the rest of the cache.
	files := cfg.SSH.ConfigFiles
	cfgFile := ""
	if len(files) > 0 {
		cfgFile = config.ExpandHome(files[0], o.Home)
	}
	fallback := sysssh.New(sysssh.Options{
		ConfigFile:     cfgFile,
		CacheDir:       paths.CacheDir,
		ConnectTimeout: cfg.SSH.ConnectTimeout.Duration(),
		ExecTimeout:    cfg.SSH.ExecTimeout.Duration(),
		MaxOutput:      int(cfg.SSH.MaxOutput),
		Log:            log,
	})

	rules := make([]transport.RouteRule, 0, len(cfg.SSH.Transports))
	for _, tr := range cfg.SSH.Transports {
		rules = append(rules, transport.RouteRule{Match: tr.Match, Transport: tr.Transport})
	}
	router, err := transport.NewRouter(stack.Transport, fallback, rules)
	if err != nil {
		return nil, err
	}
	if log != nil {
		log.Debug("ssh.transports configured", "rules", len(rules))
	}
	return &execSet{Executor: router, Streamer: router, ShellOpener: router, pool: stack.Pool}, nil
}

// Core is the part of a run that lives for the whole process (BUILD-PLAN §6):
// the SSH resolver, pool and transport, the inventory, the backends and their
// registry, the Salt runner and executor, the guard-rail policy, the redactor,
// the knowledge index, the agent-socket holder and the clipboard factory.
//
// One Core is built per process by NewCore and closed once. Everything that
// belongs to one session — its investigation log, its bus, its environment,
// its root model and its one tea.Program — is in Session, which is built on top
// of a Core by Core.NewSession.
type Core struct {
	cfg   config.Config
	paths config.Paths
	home  string
	log   *slog.Logger
	env   map[string]string
	fresh bool
	demo  bool
	// readOnly is what `--readonly` asked for: every session the core builds
	// starts with the bus's read-only switch on.
	readOnly bool
	bridge   *uiBridge

	// Prompter answers host-key and passphrase questions for every session's
	// SSH handshakes. It is the caller's when Options carried one.
	Prompter interact.Prompter
	// Glyphs is the glyph set of the process, resolved once from the config
	// and JUMPER_GLYPHS.
	Glyphs render.GlyphSet

	// SSH is the transport stack; nil in --demo.
	SSH *sshStack
	// Inventory is the merged host store; nil in --demo.
	Inventory *inventoryRuntime
	// Registry holds every backend this process registered.
	Registry *backend.Registry
	// Exec is the transport reads and fan-outs go through; nil in --demo.
	Exec transport.Executor
	// Shells opens interactive shells for terminal panes; nil in --demo.
	Shells transport.ShellOpener
	// Execs are the transports `--via` may name, keyed by "ssh"/"salt".
	Execs map[string]transport.Executor
	// Planners are the write-capable backends, keyed by registration name.
	Planners map[string]action.Planner
	// Salt is what the Salt commands read from the configuration.
	Salt tui.SaltOptions
	// SaltRunner is the master's command runner; nil when salt.master is unset.
	SaltRunner *salt.Runner
	// Policy is the guard rail every session's bus enforces.
	Policy *bus.Policy
	// Redactor cleans text before it is stored or shown (§7).
	Redactor *redact.Redactor
	// Knowledge indexes the knowledge root; nil when it could not be built.
	Knowledge *investigation.Index
	// Clipboard builds a session's clipboard around its switchable output.
	Clipboard func(out io.Writer, inTmux bool) clip.Clipboard
	// k8s is true when the Kubernetes backend registered, so the session
	// registers `:ctx`/`:ns`/`:pf` only when there is something to point at.
	k8s bool
	// plugins is the YAML plugin manager, nil when no plugin loaded.
	plugins *plugin.Manager
	// Agent is the SSH_AUTH_SOCK holder every dial reads; a new attach
	// replaces its path.
	Agent *agentSocket

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// internalErrors counts the panics this process recovered (task 126
	// pitfall: a recovered panic is never silent).
	internalErrors atomic.Int64
	// closers release what build started and Close does not own otherwise.
	closers   []func()
	closeOnce sync.Once
	closeErr  error
}

// noteInternalError counts one recovered panic and flashes it in every running
// session. The log line was already written where the panic was recovered; this
// is what makes it visible: a flash now and the counter `:health` reads.
func (c *Core) noteInternalError(name string, err error) {
	if c == nil {
		return
	}
	c.internalErrors.Add(1)
	if err == nil || c.bridge == nil {
		return
	}
	c.bridge.Warning(fmt.Sprintf("internal error in %s: %s (recovered)", name, err))
}

// InternalErrors reports how many panics were recovered since the process
// started. It is the number `:health` shows as "internal errors".
func (c *Core) InternalErrors() int {
	if c == nil {
		return 0
	}
	return int(c.internalErrors.Load())
}

// panicHook is the reporter safe.GoHook and safe.DoHook call: a panic in a
// goroutine jumper started is counted and flashed, never dropped.
func (c *Core) panicHook() func(safe.Panic) {
	return func(p safe.Panic) { c.noteInternalError(p.Name, p.Err()) }
}

// NewCore builds the per-process core from cfg. baseEnv is the caller's
// environment: it seeds the agent-socket holder and supplies HOME when the
// caller has no Option for it. Nothing is dialled here and no inventory source
// is loaded on this path; the caller must Close the result.
func NewCore(ctx context.Context, cfg config.Config, paths config.Paths, baseEnv map[string]string, log *slog.Logger) (*Core, error) {
	home := baseEnv["HOME"]
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return newCore(ctx, cfg, paths, baseEnv, Options{Home: home, Env: baseEnv}, log)
}

// newCore is NewCore plus the caller's options: --demo, the prompter, --fresh
// and the home directory Run resolved. Nothing is dialled here either.
func newCore(ctx context.Context, cfg config.Config, paths config.Paths, baseEnv map[string]string, o Options, log *slog.Logger) (*Core, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	c := &Core{
		cfg:      cfg,
		paths:    paths,
		home:     o.Home,
		log:      log,
		env:      baseEnv,
		fresh:    o.Fresh,
		demo:     o.Demo,
		readOnly: o.ReadOnly,
		bridge:   newUIBridge(),
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.Agent = newAgentSocket(o)
	c.Prompter = o.Prompter

	theme.SetProfile(cfg.UI.Color)
	glyphs, err := resolveGlyphs(cfg.UI.Glyphs, os.Getenv("JUMPER_GLYPHS"))
	if err != nil {
		c.cancel()
		return nil, err
	}
	c.Glyphs = glyphs

	c.Policy = loadPolicy(paths, log)
	red, err := buildRedactor(c.Policy)
	if err != nil {
		c.cancel()
		return nil, err
	}
	c.Redactor = red
	c.Knowledge = buildKnowledge(paths, log)
	c.Clipboard = clipboardFactory()

	if o.Demo {
		if err := c.buildDemo(); err != nil {
			_ = c.Close()
			return nil, err
		}
		return c, nil
	}
	if err := c.buildReal(o); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
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

// buildDemo assembles the fake backend core: no SSH, no inventory, no prompts.
func (c *Core) buildDemo() error {
	reg := backend.NewRegistry()
	if err := reg.Register(fake.New(fake.Options{})); err != nil {
		return errs.Wrap(errs.Internal, "app.build", fmt.Errorf("register fake backend: %w", err))
	}
	c.Registry = reg
	c.Execs = map[string]transport.Executor{}
	c.Planners = map[string]action.Planner{}
	return nil
}

// buildReal assembles the SSH + Linux (+ Salt) core.
func (c *Core) buildReal(o Options) error {
	cfg, log := c.cfg, c.log
	if c.Prompter == nil {
		c.Prompter = tui.NewPrompter(c.bridge.Send, cfg.Interact.Timeout.Duration())
	}

	stack, err := buildSSHWith(cfg, o, log, c.Prompter, c.Agent)
	if err != nil {
		return err
	}
	c.SSH = stack

	// buildExecSet honours ssh.transports: hosts matching a rule go through the
	// system ssh client instead of the native pool (SPEC §7.1).
	ex, err := buildExecSet(cfg, o, stack, c.paths, log)
	if err != nil {
		return err
	}
	// Every remote command leaves one redacted line in the debug log (§15).
	ex = ex.logCommands(log, c.Redactor)
	c.Exec = ex
	c.Shells = ex

	// The Salt runner drives salt, salt-run and salt-key on the master over
	// the same SSH transport. No master means no Salt at all.
	c.SaltRunner = saltRunner(cfg, ex, log)

	sources, warns := inventorySources(cfg, stack.Resolver, o.Home, c.SaltRunner)
	for _, w := range warns {
		c.bridge.Warning(w)
	}

	store := inventory.New(sources, inventoryRewrites(cfg), inventoryCachePath(c.paths), nil)
	c.Inventory = &inventoryRuntime{Store: store}
	if err := c.Inventory.LoadCache(); err != nil {
		c.bridge.Warning("inventory cache: " + err.Error())
	}
	// The first frame must show hosts, and the sources wired here are local
	// (ssh_config, static): with no cache they are read once before the UI
	// starts. Anything slower runs in the background, as the ticker in Run
	// does for every later refresh.
	if len(store.Hosts()) == 0 && len(sources) > 0 && allLocal(sources) {
		c.refreshInventorySync(c.ctx)
	} else {
		c.wg.Add(1)
		safe.GoHook(c.log, "inventory refresh", c.panicHook(), func() {
			defer c.wg.Done()
			c.refreshInventory(c.ctx)
		})
	}

	reg := backend.NewRegistry()

	// The Salt backend is built first because the Linux backend asks it for
	// the extra pivots of a host row (host → minion) through LinkProvider.
	var (
		saltBackend *salt.Backend
		links       backend.LinkProvider
	)
	if c.SaltRunner != nil {
		saltBackend = salt.New(salt.Options{
			Runner:    c.SaltRunner,
			Inventory: store,
			Master:    resource.HostID(strings.TrimSpace(cfg.Salt.Master)),
			Log:       log,
		})
		links = saltBackend
		c.Salt = tui.SaltOptions{SuggestOver: cfg.Salt.SuggestOver, Configured: true}
	}

	lb := linux.New(linux.Options{
		Exec:        ex,
		Stream:      ex,
		Inventory:   c.Inventory,
		Concurrency: cfg.SSH.Concurrency,
		Timeout:     cfg.SSH.ExecTimeout.Duration(),
		Log:         log,
		Links:       links,
	})
	if err := reg.Register(lb); err != nil {
		return errs.Wrap(errs.Internal, "app.build", fmt.Errorf("register linux backend: %w", err))
	}
	c.Registry = reg

	c.Execs = map[string]transport.Executor{"ssh": ex}
	if saltBackend != nil {
		if err := reg.Register(saltBackend); err != nil {
			return errs.Wrap(errs.Internal, "app.build", fmt.Errorf("register salt backend: %w", err))
		}
		saltExec := salt.NewExecutor(c.SaltRunner, saltBackend, salt.ExecOptions{
			Chunk:   cfg.Salt.ListChunk,
			Timeout: cfg.Salt.Timeout.Duration(),
		})
		c.Execs["salt"] = saltExec
		c.closers = append(c.closers, saltExec.Close)
	}

	c.Planners = make(map[string]action.Planner, 2)
	// One planner instance per name: every session's bus and the actions menu
	// must see the same one.
	c.Planners["generic"] = generic.New(c.Execs, "ssh")
	if saltBackend != nil {
		c.Planners["salt"] = saltBackend
	}

	// M8–M11: register every optional backend whose config section is set.
	// Before this existed the builders were written and tested but nothing
	// called them, so the binary could only ever reach fake/linux/salt
	// (task 159; found by running the binary against a real k3s cluster).
	c.wireExtraBackends()

	if len(sources) == 0 {
		c.bridge.Warning("no inventory sources: add inventory.sources (ssh_config or static) to config.yaml, or run with --demo")
	} else if len(store.Hosts()) == 0 {
		c.bridge.Warning("inventory has no hosts: check inventory.sources (ssh_config aliases, static file path), or run with --demo")
	}
	return nil
}

// Close stops the background inventory work, releases the Salt executor and
// shuts the connection pool down. It is safe to call more than once.
func (c *Core) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
		c.wg.Wait()
		for _, close := range c.closers {
			close()
		}
		if c.SSH != nil {
			c.closeErr = c.SSH.Close()
		}
	})
	return c.closeErr
}

// Warnings returns the warning lines the core queued so far: an inventory
// source that failed, a session file that could not be restored. The UI shows
// them as flashes; the tests assert what the user would have been told.
func (c *Core) Warnings() []string {
	if c == nil {
		return nil
	}
	return c.bridge.Warnings()
}

// refreshInventory loads every source once. The store keeps the previous hosts
// of a source that fails, so the caller only has to report the failures.
func (c *Core) refreshInventory(ctx context.Context) {
	if c.Inventory == nil {
		return
	}
	failed := c.Inventory.Refresh(ctx)
	for _, line := range sortedSourceErrors(failed) {
		c.bridge.Warning("inventory: " + line)
	}
}

// refreshInventorySync is refreshInventory for the first start with no cache.
func (c *Core) refreshInventorySync(ctx context.Context) {
	if c.Inventory == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, firstLoadTimeout)
	defer cancel()
	c.refreshInventory(ctx)
}

// loadPolicy reads <config>/policy.yaml. A policy that does not parse is a
// warning and the built-in guard rails; a missing file is the built-in ones.
func loadPolicy(paths config.Paths, log *slog.Logger) *bus.Policy {
	if paths.ConfigDir == "" {
		return bus.DefaultPolicy()
	}
	p, err := bus.LoadPolicy(filepath.Join(paths.ConfigDir, "policy.yaml"))
	if err != nil {
		if log != nil {
			log.Warn("policy.yaml ignored", "err", err)
		}
		return bus.DefaultPolicy()
	}
	return p
}

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

// buildRedactor returns the process's redactor: the built-in patterns plus the
// extra ones policy.yaml declares (§7). A pattern that does not compile is an
// errs.Config error the start reports, never a warning: a redactor that
// silently dropped a secret pattern would leak the thing it was written to
// hide.
func buildRedactor(pol *bus.Policy) (*redact.Redactor, error) {
	var extra []string
	if pol != nil {
		extra = pol.RedactPatterns()
	}
	return redact.New(extra)
}

// buildKnowledge indexes the knowledge root once. A missing or unreadable root
// is not an error: the index is simply empty.
func buildKnowledge(paths config.Paths, log *slog.Logger) *investigation.Index {
	if paths.KnowledgeRoot == "" {
		return nil
	}
	ix, err := investigation.BuildIndex(paths.KnowledgeRoot)
	if err != nil {
		if log != nil {
			log.Debug("knowledge index", "root", paths.KnowledgeRoot, "err", err)
		}
		return nil
	}
	return ix
}

// clipboardFactory returns the function that builds one session's clipboard:
// pbcopy, and OSC 52 to the attached terminal as the fallback, which also works
// when the client is tmux.
func clipboardFactory() func(out io.Writer, inTmux bool) clip.Clipboard {
	return func(out io.Writer, inTmux bool) clip.Clipboard {
		pb := clip.NewPbcopy(pbcopyRunner(clipTimeout))
		if out == nil {
			return pb
		}
		return clip.Multi(pb, clip.NewOSC52(out, inTmux))
	}
}

// clipTimeout bounds one pbcopy run.
const clipTimeout = 2 * time.Second

// pbcopyRunner feeds stdin to a local command and gives up after timeout. The
// process is built here, never in internal/clip (BUILD-PLAN §6.1 rule 6).
func pbcopyRunner(timeout time.Duration) func(name string, stdin []byte) error {
	return func(name string, stdin []byte) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, name)
		cmd.Stdin = bytes.NewReader(stdin)
		if err := cmd.Run(); err != nil {
			return errs.Wrap(errs.Internal, "clip."+name, err)
		}
		return nil
	}
}

// sessionReadOnly reports the session-level read-only switch the bus consults.
func sessionReadOnly(s *tui.SessionState) bool {
	return s != nil && s.ReadOnly
}

// PluginDir is where the YAML plugins live: <config-dir>/plugins (SPEC §12).
// `:plugins` lists from it and `S` in a grouped view saves into it.
func PluginDir(paths config.Paths) string {
	return filepath.Join(paths.ConfigDir, plugin.DirName)
}

// PluginManager builds the YAML plugin manager and loads the directory once
// (SPEC §12, task 117). The problems of that first load are returned so the
// caller can warn about them; a directory with no plugins is not an error.
//
// The manager registers one backend per plugin, whose kind is the plugin name,
// so `:<name>` opens it like any built-in kind. The caller registers the
// planner it returns — m.Planner() — with the action bus under the name
// "plugin"; every plugin action then goes through the bus and is recorded with
// the actor plugin:<name>.
//
// The wiring lives here rather than in build.go because build still assembles
// the M0 registry; the task that owns build.go calls this function.
func PluginManager(reg *backend.Registry, ex transport.Executor, inv inventory.Inventory, dir string, timeout time.Duration, log *slog.Logger) (*plugin.Manager, []plugin.Problem, error) {
	opts := plugin.ManagerOptions{
		Dir:      dir,
		Registry: reg,
		Exec:     ex,
		Timeout:  timeout,
		Log:      log,
	}
	if inv != nil {
		opts.Select = func(sel resource.Selector) []resource.HostID {
			hosts := inv.Select(sel)
			out := make([]resource.HostID, 0, len(hosts))
			for _, h := range hosts {
				out = append(out, h.ID)
			}
			return out
		}
		opts.Resolve = func(name string) (resource.HostID, bool) {
			h, ok := inv.Resolve(name)
			if !ok {
				return "", false
			}
			return h.ID, true
		}
	}
	m, err := plugin.NewManager(opts)
	if err != nil {
		return nil, nil, err
	}
	return m, m.Load(), nil
}

// PluginFactories returns the views of the grouped plugins: a plugin that
// declares view.group_by_output and runs one command for every host gets the
// fan-out view rather than the table (SPEC §12). Every other plugin keeps the
// generic table, which already renders the host column and the declared
// columns. The task that owns build.go merges this into Env.Factories.
func PluginFactories(m *plugin.Manager) map[resource.KindName]tui.Factory {
	out := make(map[resource.KindName]tui.Factory)
	if m == nil {
		return out
	}
	for _, def := range m.Definitions() {
		b, ok := m.Backend(def.Name)
		if !ok || !b.GroupByOutput() || !b.UniformCommand() {
			// A command that substitutes {{ .Host }} renders differently per
			// target, so it can only be a table.
			continue
		}
		kind, title := b.Kind(), def.Description
		if title == "" {
			title = def.Name
		}
		bk := b
		out[kind] = func(env *tui.Env, q resource.Query) (tui.View, error) {
			hosts, herr := bk.Hosts(q)
			if herr != nil {
				return nil, herr
			}
			cmd, cerr := bk.Command("", q.Args)
			if cerr != nil {
				return nil, cerr
			}
			return views.NewFanout(env, "plugin "+title, cmd, hosts, nil), nil
		}
	}
	return out
}

// wireExtraBackends registers the optional backends — Prometheus, Alertmanager,
// Grafana, Elasticsearch, Chef, Kubernetes and the YAML plugins — whose config
// sections are set. A backend that cannot be built is a startup warning naming
// its section and the app starts without it; an empty section registers nothing.
func (c *Core) wireExtraBackends() {
	cfg, log := c.cfg, c.log
	profiles, err := httpx.LoadProfiles(filepath.Join(c.paths.ConfigDir, "connections.yaml"), c.home)
	if err != nil {
		c.bridge.Warning("connections.yaml: " + err.Error())
	}
	deps := httpx.Deps{Env: c.env, Redactor: c.Redactor}

	warn := func(section string, err error) {
		if err != nil {
			c.bridge.Warning(section + ": " + err.Error())
		}
	}

	if b, berr := PrometheusBackend(cfg, profiles, deps, log); berr != nil {
		warn("prometheus", berr)
	} else if b != nil {
		if rerr := c.Registry.Register(b); rerr != nil {
			warn("prometheus", rerr)
		}
	}

	am, amPlanner, aerr := Alertmanager(cfg, profiles, deps, cfg.Knowledge.Root, log)
	if aerr != nil {
		warn("alertmanager", aerr)
	} else if am != nil {
		if rerr := c.Registry.Register(am); rerr != nil {
			warn("alertmanager", rerr)
		} else if amPlanner != nil {
			c.Planners["alertmanager"] = amPlanner
		}
	}

	if b, gerr := GrafanaBackend(cfg, profiles, deps, GrafanaHooks{}, log); gerr != nil {
		warn("grafana", gerr)
	} else if b != nil {
		if rerr := c.Registry.Register(b); rerr != nil {
			warn("grafana", rerr)
		}
	}

	var redactText func(string) string
	if c.Redactor != nil {
		redactText = c.Redactor.String
	}
	if b, eerr := ElasticBackend(cfg, profiles, deps, redactText, log); eerr != nil {
		warn("elastic", eerr)
	} else if b != nil {
		if rerr := c.Registry.Register(b); rerr != nil {
			warn("elastic", rerr)
		}
	}

	if b, cerr := ChefBackend(cfg, c.Exec, c.Redactor, log); cerr != nil {
		warn("chef", cerr)
	} else if b != nil {
		if rerr := c.Registry.Register(b); rerr != nil {
			warn("chef", rerr)
		}
	}

	// Kubernetes registers even without a kubeconfig: it then reports
	// Unconfigured (a grey health dot) instead of hiding its kinds.
	if b, kerr := KubernetesBackend(cfg, c.home, c.env["KUBECONFIG"], log); kerr != nil {
		warn("kubernetes", kerr)
	} else if b != nil {
		if rerr := c.Registry.Register(b); rerr != nil {
			warn("kubernetes", rerr)
		} else {
			c.k8s = true
			if dyn, derr := KubernetesDynamicBackend(cfg, c.home, c.env["KUBECONFIG"], log); derr != nil {
				warn("kubernetes", derr)
			} else if dyn != nil {
				if rerr := c.Registry.Register(dyn); rerr != nil {
					warn("kubernetes", rerr)
				} else if planner := KubernetesPatchPlanner(dyn); planner != nil {
					c.Planners["k8s"] = planner
				}
			}
		}
	}

	if mgr, problems, perr2 := PluginManager(c.Registry, c.Exec, c.Inventory, PluginDir(c.paths), cfg.SSH.ExecTimeout.Duration(), log); perr2 != nil {
		warn("plugins", perr2)
	} else if mgr != nil {
		c.plugins = mgr
		for _, pb := range problems {
			c.bridge.Warning("plugin " + pb.File + ": " + pb.Msg)
		}
		c.Planners["plugin"] = plugin.NewPlanner(mgr)
	}
}
