package app

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend/salt"
	"jumper/internal/bus"
	"jumper/internal/config"
	"jumper/internal/inventory"
	"jumper/internal/resource"
	"jumper/internal/transport"
	"jumper/internal/transport/ssh"
	"jumper/internal/tui"
	"jumper/internal/tui/views"
)

// built is one assembled run for the callers that drive the model themselves:
// `jumper doctor`, the composition tests and the session tests. It is a Core
// plus a Session that has not started its tea.Program, so nothing renders while
// the caller calls Update and View on the root. Run does not use it: it builds
// the same Core and a session whose program runs from the first line.
type built struct {
	*Core
	*Session

	// Root and Env are the session's model and environment, spelled out because
	// the callers of build drive the model themselves.
	Root *tui.Root
	Env  *tui.Env
}

// build assembles a run from cfg, in order: logger → prompter → SSH stack →
// inventory sources and store → Linux backend → registry → Env → commands →
// root. Nothing is dialled and no inventory source is loaded on this path: the
// first frame comes from the cache (or an empty list) and data streams in
// afterwards. The caller must Close the result.
func build(cfg config.Config, o Options, paths config.Paths) (*built, error) {
	log := newLogger(paths.CacheDir, o.Debug)
	core, err := newCore(context.Background(), cfg, paths, o.Env, o, log)
	if err != nil {
		return nil, err
	}
	sess, err := core.newSession("main", false)
	if err != nil {
		_ = core.Close()
		return nil, err
	}
	return &built{Core: core, Session: sess, Root: sess.root, Env: sess.env}, nil
}

// Close detaches the session and releases the core.
func (b *built) Close() error {
	if b == nil {
		return nil
	}
	var err error
	if b.Session != nil {
		err = b.Session.close()
	}
	if cerr := b.Core.Close(); err == nil {
		err = cerr
	}
	return err
}

// fanoutOpener builds the grouped fan-out view a `:probe` run opens. It is a
// closure around views.NewFanout because package tui injects the opener.
func fanoutOpener(env *tui.Env, title string, cmd transport.Command, hosts []resource.HostID, normalize func(string) string) tui.View {
	return views.NewFanout(env, title, cmd, hosts, normalize)
}

// firstLoadTimeout bounds the one refresh build does before the UI starts.
// Only local sources (ssh_config, static) are wired, so this is a file read,
// not a network call.
const firstLoadTimeout = 30 * time.Second

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

// uiBridge carries messages from non-UI goroutines to the programs of the
// sessions. One core serves every session of the process, so it fans out to
// each program that registered a sink; a prompt or a warning is shown by
// whichever sessions are running.
//
// Before any program has started (a source that fails while the model is still
// being built) messages are queued and delivered by Flush, in order, once the
// program's event loop is running: program.Send blocks before that.
type uiBridge struct {
	mu     sync.Mutex
	next   int
	sinks  map[int]func(tea.Msg)
	queued []tea.Msg
	warns  []string
}

// newUIBridge returns an empty bridge.
func newUIBridge() *uiBridge { return &uiBridge{sinks: make(map[int]func(tea.Msg))} }

// AddSink registers a program's Send and returns the id RemoveSink takes.
func (u *uiBridge) AddSink(send func(tea.Msg)) int {
	if send == nil {
		return 0
	}
	u.mu.Lock()
	u.next++
	id := u.next
	u.sinks[id] = send
	u.mu.Unlock()
	return id
}

// RemoveSink unregisters a program that has ended.
func (u *uiBridge) RemoveSink(id int) {
	if u == nil || id == 0 {
		return
	}
	u.mu.Lock()
	delete(u.sinks, id)
	u.mu.Unlock()
}

// Flush delivers what was queued before a program was running. It is called
// once the program's first Init has run, which is the first moment Send is
// safe.
func (u *uiBridge) Flush() {
	u.mu.Lock()
	queued := u.queued
	u.queued = nil
	sends := u.sinksLocked()
	u.mu.Unlock()
	for _, send := range sends {
		for _, msg := range queued {
			send(msg)
		}
	}
}

// sinksLocked copies the registered sinks. The caller holds u.mu.
func (u *uiBridge) sinksLocked() []func(tea.Msg) {
	if len(u.sinks) == 0 {
		return nil
	}
	out := make([]func(tea.Msg), 0, len(u.sinks))
	for _, send := range u.sinks {
		out = append(out, send)
	}
	return out
}

// Send delivers msg to every program, or queues it until one is running.
func (u *uiBridge) Send(msg tea.Msg) {
	u.mu.Lock()
	sends := u.sinksLocked()
	if len(sends) == 0 {
		if len(u.queued) < 32 {
			u.queued = append(u.queued, msg)
		}
		u.mu.Unlock()
		return
	}
	u.mu.Unlock()
	for _, send := range sends {
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
