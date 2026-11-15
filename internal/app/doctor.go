package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh/agent"

	"jumpr/internal/backend"
	"jumpr/internal/config"
	"jumpr/internal/errs"
	"jumpr/internal/inventory"
	"jumpr/internal/redact"
	"jumpr/internal/resource"
	"jumpr/internal/shq"
	"jumpr/internal/transport"
	"jumpr/internal/transport/httpx"
)

// doctorLabelWidth is the column width of the check name; statusWidth fits
// "needs-privilege", the longest status a check reports.
const (
	doctorStatusWidth = 15
	doctorLabelWidth  = 12
)

// Doctor runs one diagnostic subcommand and returns the process exit code. It
// writes every line to out, so nothing in this package touches a real terminal.
//
// With no arguments it runs the full report (`jumpr doctor`); the positional
// subcommands probe one thing in detail:
//
//	jumpr doctor ssh <host>
func Doctor(ctx context.Context, o Options, args []string, out io.Writer) int {
	if out == nil {
		out = io.Discard
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if len(args) == 0 {
		return doctorAll(ctx, o, out)
	}
	if args[0] != "ssh" || len(args) != 2 {
		fmt.Fprintf(out, "usage: jumpr doctor [--config-dir DIR]\n"+
			"       jumpr doctor ssh <host> [--config-dir DIR]\n")
		return 2
	}
	return doctorSSH(ctx, o, args[1], out)
}

// doctorReport writes one aligned status line per check.
type doctorReport struct {
	out   io.Writer
	red   *redact.Redactor
	fails int
}

// status writes a line whose status is not just ok/FAIL.
func (r *doctorReport) status(status, label, detail string) {
	if status == "FAIL" {
		r.fails++
	}
	fmt.Fprintf(r.out, "%-*s  %-*s  %s\n", doctorStatusWidth, status, doctorLabelWidth, label, detail)
}

// ok writes a passing check.
func (r *doctorReport) ok(label, detail string) { r.status("ok", label, detail) }

// fail writes a failing check naming the error kind and its message, redacted.
func (r *doctorReport) fail(label string, err error) {
	msg := ""
	if err != nil {
		msg = r.redact(err.Error())
	}
	if k := errs.KindOf(err); k != "" {
		msg = string(k) + ": " + msg
	}
	r.status("FAIL", label, msg)
}

// failMsg writes a failing check with a plain detail (no error value).
func (r *doctorReport) failMsg(label, msg string) { r.status("FAIL", label, r.redact(msg)) }

// failDetail writes a failing check about detail, naming the error kind.
func (r *doctorReport) failDetail(label, detail string, err error) {
	msg := r.redact(err.Error())
	if k := errs.KindOf(err); k != "" {
		msg = string(k) + ": " + msg
	}
	r.status("FAIL", label, detail+": "+msg)
}

// redact strips secrets from text before it is shown (BUILD-PLAN §7).
func (r *doctorReport) redact(s string) string {
	if r.red == nil {
		return s
	}
	return r.red.String(s)
}

// doctorHooks are the operations of the full report that touch the outside
// world. A nil hook runs the real implementation; tests fill every hook so the
// whole report runs without SSH, the network or an agent.
type doctorHooks struct {
	// AgentKeys lists the keys of the SSH agent socket the client would use.
	AgentKeys func(socket string) (int, error)
	// Connect dials and authenticates to one host and releases it.
	Connect func(ctx context.Context, host resource.HostID) error
	// Exec runs one read-only command on a host through the SSH transport.
	Exec func(ctx context.Context, host resource.HostID, script string) (transport.ExecResult, error)
	// Health asks every registered backend for its health.
	Health func(ctx context.Context) []doctorHealth
	// Reach makes one request to url through the named connection profile.
	Reach func(ctx context.Context, profile, url string) error
}

// doctorHealth is one backend's answer to Health.
type doctorHealth struct {
	Backend string
	Health  backend.Health
}

// doctorAll is `jumpr doctor` with the real outside world attached.
func doctorAll(ctx context.Context, o Options, out io.Writer) int {
	return doctorAllWith(ctx, o, out, doctorHooks{})
}

// doctorAllWith runs the full report. It checks the configuration, the SSH
// setup, the agent, the bastions and one sample host per inventory source, the
// backends and their connection profiles, the knowledge root, the session
// socket directory and the terminal. Every line is ok, warn or FAIL; the
// return code is 1 when any check FAILed.
func doctorAllWith(ctx context.Context, o Options, out io.Writer, hooks doctorHooks) int {
	if out == nil {
		out = io.Discard
	}
	if ctx == nil {
		ctx = context.Background()
	}
	red, err := redact.New(nil)
	if err != nil {
		fmt.Fprintf(out, "FAIL  doctor  %v\n", err)
		return 1
	}
	d := &doctorAllRun{
		report: &doctorReport{out: out, red: red},
		opts:   o,
		paths:  config.ResolvePaths(o.Home, o.ConfigDir),
		hooks:  hooks,
	}
	return d.run(ctx)
}

// doctorAllRun carries the state of one full report.
type doctorAllRun struct {
	report *doctorReport
	opts   Options
	paths  config.Paths
	hooks  doctorHooks

	cfg   config.Config
	b     *built
	hosts []inventory.Host
}

// run is the report itself, in the order SPEC §14 describes.
func (d *doctorAllRun) run(ctx context.Context) int {
	cfgFile := filepath.Join(d.paths.ConfigDir, "config.yaml")
	cfg, err := config.Load(cfgFile, d.opts.Home)
	if err != nil {
		// Load still returns the defaults, so the checks that do not need a
		// valid file can use them.
		d.cfg = cfg
		d.report.fail("config", err)
		// The remaining checks that do not read the configuration still run:
		// they are what a first run gets wrong.
		d.checkSocket()
		d.checkTerminal()
		return 1
	}
	d.cfg = cfg
	// The session socket lives in the cache directory. jumpr creates it 0700
	// on first start, so create it here too: checkSocket then reports the real
	// mode instead of a directory this run had not made yet (the inventory
	// cache writer would create it 0755).
	if d.paths.CacheDir != "" {
		_ = os.MkdirAll(d.paths.CacheDir, 0o700)
	}
	d.checkConfigFile(cfgFile)
	d.checkSSHConfig()

	if d.hooks.AgentKeys == nil {
		d.hooks.AgentKeys = agentKeyCount
	}
	d.checkAgent()

	b, err := build(cfg, d.opts, d.paths)
	if err != nil {
		d.report.fail("backends", err)
		d.checkKnowledge()
		d.checkSocket()
		d.checkTerminal()
		return 1
	}
	d.b = b
	defer func() { _ = b.Close() }()
	d.fillHooks()

	d.checkInventory(ctx)
	d.checkBastions(ctx)
	d.checkSamples(ctx)
	d.checkBackends(ctx)
	d.checkConnections(ctx)
	d.checkKnowledge()
	d.checkSocket()
	d.checkTerminal()

	if d.report.fails > 0 {
		return 1
	}
	return 0
}

// fillHooks attaches the real implementations to every hook the caller left
// empty. They read the composition root's pool, transport, inventory and
// registry, so the report proves the same wiring the TUI uses.
func (d *doctorAllRun) fillHooks() {
	if d.hooks.Connect == nil && d.b.SSH != nil && d.b.SSH.Pool != nil {
		pool := d.b.SSH.Pool
		d.hooks.Connect = func(ctx context.Context, host resource.HostID) error {
			lease, err := pool.Client(ctx, host)
			if err != nil {
				return err
			}
			lease.Release()
			return nil
		}
	}
	if d.hooks.Exec == nil {
		ex := d.executor()
		if ex != nil {
			d.hooks.Exec = func(ctx context.Context, host resource.HostID, script string) (transport.ExecResult, error) {
				return ex.Exec(ctx, host, transport.Command{Script: script})
			}
		}
	}
	if d.hooks.Health == nil && d.b.Registry != nil {
		reg := d.b.Registry
		d.hooks.Health = func(ctx context.Context) []doctorHealth {
			backends := reg.Backends()
			out := make([]doctorHealth, 0, len(backends))
			for _, be := range backends {
				out = append(out, doctorHealth{Backend: be.Name(), Health: be.Health(ctx)})
			}
			return out
		}
	}
}

// executor is the SSH executor the TUI would use: the system-ssh aware set
// when ssh.transports is configured, else the native transport.
func (d *doctorAllRun) executor() transport.Executor {
	if d.b == nil {
		return nil
	}
	if ex := d.b.Execs["ssh"]; ex != nil {
		return ex
	}
	if d.b.SSH != nil {
		return d.b.SSH.Transport
	}
	return nil
}

// checkConfigFile reports whether config.yaml exists. A missing file is not an
// error (defaults apply) but it is what a first run needs to know.
func (d *doctorAllRun) checkConfigFile(path string) {
	switch _, err := os.Stat(path); {
	case err == nil:
		d.report.ok("config", path)
	case errors.Is(err, fs.ErrNotExist):
		d.report.status("warn", "config", "no config.yaml in "+d.paths.ConfigDir+
			"; defaults in use — run `jumpr init`")
	default:
		d.report.failDetail("config", path, err)
	}
}

// checkSSHConfig proves every configured ssh_config file is readable. A
// missing file is a warning (the inventory falls back to other sources), an
// unreadable one is a failure.
func (d *doctorAllRun) checkSSHConfig() {
	files := d.cfg.SSH.ConfigFiles
	if len(files) == 0 {
		d.report.status("warn", "ssh config", "no ssh.config_files configured")
		return
	}
	readable := make([]string, 0, len(files))
	for _, f := range files {
		fh, err := os.Open(f)
		switch {
		case err == nil:
			_ = fh.Close()
			readable = append(readable, f)
		case errors.Is(err, fs.ErrNotExist):
			d.report.status("warn", "ssh config", "no file at "+f)
		default:
			d.report.failDetail("ssh config", f, err)
		}
	}
	if len(readable) > 0 {
		d.report.ok("ssh config", strings.Join(readable, ", "))
	}
}

// checkAgent counts the keys the SSH agent would offer. No agent socket is a
// warning: key files still work.
func (d *doctorAllRun) checkAgent() {
	socket := d.env("SSH_AUTH_SOCK")
	if socket == "" {
		d.report.status("warn", "agent", "no SSH_AUTH_SOCK; only key files are used")
		return
	}
	start := time.Now()
	n, err := d.hooks.AgentKeys(socket)
	if err != nil {
		d.report.failDetail("agent", socket, err)
		return
	}
	switch n {
	case 0:
		d.report.status("warn", "agent", "0 keys in "+socket+" — run `ssh-add`")
	case 1:
		d.report.ok("agent", "1 key ("+ms(time.Since(start))+")")
	default:
		d.report.ok("agent", fmt.Sprintf("%d keys (%s)", n, ms(time.Since(start))))
	}
}

// agentKeyCount is the real agent check: connect to the socket and list keys.
func agentKeyCount(socket string) (int, error) {
	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return 0, errs.Wrap(errs.Unreachable, "doctor.agent", err)
	}
	defer func() { _ = conn.Close() }()
	keys, err := agent.NewClient(conn).List()
	if err != nil {
		return 0, errs.Wrap(errs.Auth, "doctor.agent", err)
	}
	return len(keys), nil
}

// checkInventory loads every configured source once and reports what it found.
func (d *doctorAllRun) checkInventory(ctx context.Context) {
	if d.b.Inventory == nil {
		d.report.status("warn", "inventory", "no inventory sources configured")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	failed := d.b.Inventory.Refresh(ctx)
	d.hosts = d.b.Inventory.Hosts()

	names := make([]string, 0, len(failed))
	for name := range failed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		d.report.failDetail("inventory", name, failed[name])
	}
	if len(names) == 0 {
		d.report.ok("inventory", fmt.Sprintf("%d hosts", len(d.hosts)))
	}
}

// bastionNames lists the aliases some host uses as a jump, sorted.
func (d *doctorAllRun) bastionNames() []string {
	if d.b == nil || d.b.SSH == nil || d.b.SSH.Resolver == nil {
		return nil
	}
	r := d.b.SSH.Resolver
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
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// checkBastions connects to every bastion the inventory knows about. A bastion
// is reachable by name but is never a fleet target, so it may well be absent
// from the inventory; that is reported, not failed.
func (d *doctorAllRun) checkBastions(ctx context.Context) {
	names := d.bastionNames()
	if len(names) == 0 {
		d.report.ok("bastion", "none configured")
		return
	}
	for _, name := range names {
		if _, ok := d.host(name); !ok {
			d.report.status("warn", "bastion", name+": not in the inventory; not checked")
			continue
		}
		if d.hooks.Connect == nil {
			d.report.status("warn", "bastion", name+": no SSH pool to dial with")
			continue
		}
		start := time.Now()
		cctx, cancel := context.WithTimeout(ctx, d.connectTimeout())
		err := d.hooks.Connect(cctx, resource.HostID(name))
		cancel()
		if err != nil {
			d.report.failDetail("bastion", name, err)
			continue
		}
		d.report.ok("bastion", name+": connected in "+ms(time.Since(start)))
	}
}

// checkSamples runs echo and `sudo -n true` on one sample host per inventory
// source. A source with no hosts is a warning: the fleet is not visible.
func (d *doctorAllRun) checkSamples(ctx context.Context) {
	if len(d.cfg.Inventory.Sources) == 0 {
		d.report.status("warn", "sample", "no inventory sources configured")
		return
	}
	for _, src := range d.cfg.Inventory.Sources {
		name := inventorySourceName(src.Type)
		host, ok := d.sampleFor(name)
		if !ok {
			d.report.status("warn", "sample", name+": no hosts from this source")
			continue
		}
		d.checkSample(ctx, name, host)
	}
}

// checkSample runs the two probes that prove a source's hosts are usable.
func (d *doctorAllRun) checkSample(ctx context.Context, source string, host inventory.Host) {
	label := source + " " + string(host.ID)
	if d.hooks.Exec == nil {
		d.report.status("warn", "sample", label+": no SSH transport to probe with")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, d.execTimeout())
	res, err := d.hooks.Exec(cctx, host.ID, "echo ok")
	cancel()
	if err != nil {
		d.report.failDetail("sample", label+": echo", err)
	} else if got := strings.TrimSpace(string(res.Stdout)); got != "ok" || res.RC != 0 {
		d.report.failMsg("sample", fmt.Sprintf("%s: echo: unexpected output %q (rc %d)", label, got, res.RC))
	} else {
		d.report.ok("sample", label+": echo ok in "+ms(res.Duration))
	}

	cctx, cancel = context.WithTimeout(ctx, d.execTimeout())
	res, err = d.hooks.Exec(cctx, host.ID, "sudo -n true")
	cancel()
	switch {
	case err == nil && res.RC == 0:
		d.report.ok("sample", label+": sudo -n true ok")
	case err == nil && errs.KindOf(shq.Classify(res.RC, string(res.Stderr), true)) == errs.Sudo:
		d.report.status("needs-privilege", "sample", label+": sudo -n true: passwordless sudo is not available")
	case err != nil:
		d.report.failDetail("sample", label+": sudo -n true", err)
	case res.RC != 0:
		d.report.status("warn", "sample",
			fmt.Sprintf("%s: sudo -n true: rc %d: %s", label, res.RC, strings.TrimSpace(string(res.Stderr))))
	default:
		d.report.status("warn", "sample", label+": sudo -n true: unexpected result")
	}
}

// checkBackends reports the health of every registered backend.
func (d *doctorAllRun) checkBackends(ctx context.Context) {
	if d.hooks.Health == nil {
		d.report.status("warn", "backend", "no backends registered")
		return
	}
	hs := d.hooks.Health(ctx)
	if len(hs) == 0 {
		d.report.status("warn", "backend", "no backends registered")
		return
	}
	for _, h := range hs {
		detail := h.Backend + ": " + h.Health.State.String()
		if h.Health.Detail != "" {
			detail += " (" + h.Health.Detail + ")"
		}
		switch h.Health.State {
		case backend.HealthOK:
			d.report.ok("backend", detail)
		case backend.HealthDown:
			d.report.failMsg("backend", detail)
		default:
			d.report.status("warn", "backend", detail)
		}
	}
}

// connRef is one backend's reference to a connection profile.
type connRef struct {
	Backend string
	Profile string
	URL     string
}

// connectionRefs lists the connection profiles the configured HTTP backends
// name, in config order.
func connectionRefs(cfg config.Config) []connRef {
	var out []connRef
	add := func(section, name, profile, url string) {
		if strings.TrimSpace(profile) == "" {
			return
		}
		out = append(out, connRef{Backend: section + "/" + name, Profile: strings.TrimSpace(profile), URL: url})
	}
	for _, p := range cfg.Prometheus {
		add("prometheus", p.Name, p.Connection, p.URL)
	}
	for _, a := range cfg.Alertmanager {
		add("alertmanager", a.Name, a.Connection, a.URL)
	}
	for _, g := range cfg.Grafana {
		add("grafana", g.Name, g.Connection, g.URL)
	}
	for _, e := range cfg.Elasticsearch {
		add("elasticsearch", e.Name, e.Connection, e.URL)
	}
	return out
}

// checkConnections proves every connection profile a backend names exists and
// can reach that backend's URL. Unreferenced profiles are not dialled.
func (d *doctorAllRun) checkConnections(ctx context.Context) {
	refs := connectionRefs(d.cfg)
	if len(refs) == 0 {
		d.report.ok("connection", "no backend uses a connection profile")
		return
	}
	path := d.paths.ConnectionsPath()
	profiles, err := config.LoadConnections(path, d.opts.Home)
	if err != nil {
		d.report.failDetail("connection", path, err)
		return
	}
	if d.hooks.Reach == nil {
		red := d.report.red
		env := d.opts.Env
		home := d.opts.Home
		d.hooks.Reach = func(ctx context.Context, profile, url string) error {
			p, ok := profiles[profile]
			if !ok {
				return errs.New(errs.Config, "doctor.connection",
					fmt.Sprintf("no profile %q in %s", profile, path))
			}
			c, err := p.Client(httpx.Deps{Env: env, Redactor: red, Home: home})
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return errs.Wrap(errs.Config, "doctor.connection", err)
			}
			resp, err := c.Do(ctx, req)
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			if resp.StatusCode >= 500 {
				return errs.New(errs.Remote, "doctor.connection",
					fmt.Sprintf("HTTP %d from %s", resp.StatusCode, url))
			}
			return nil
		}
	}
	for _, ref := range refs {
		if _, ok := profiles[ref.Profile]; !ok {
			d.report.failMsg("connection",
				fmt.Sprintf("%s: no profile %q in %s", ref.Backend, ref.Profile, path))
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		start := time.Now()
		err := d.hooks.Reach(cctx, ref.Profile, ref.URL)
		cancel()
		if err != nil {
			d.report.failDetail("connection", ref.Backend+" via "+ref.Profile, err)
			continue
		}
		d.report.ok("connection", fmt.Sprintf("%s via %s in %s",
			ref.Backend, ref.Profile, ms(time.Since(start))))
	}
}

// checkKnowledge proves the knowledge root is writable; investigations, notes
// and the audit log are written there.
func (d *doctorAllRun) checkKnowledge() {
	root := d.cfg.Knowledge.Root
	if root == "" {
		d.report.status("warn", "knowledge", "knowledge.root is empty; using the default")
		root = filepath.Join(d.opts.Home, "jumpr")
	}
	created := false
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		created = true
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		d.report.failDetail("knowledge", root, err)
		return
	}
	f, err := os.CreateTemp(root, ".jumpr-doctor-*")
	if err != nil {
		d.report.failDetail("knowledge", root, err)
		return
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	if created {
		d.report.ok("knowledge", root+" writable (created)")
		return
	}
	d.report.ok("knowledge", root+" writable")
}

// macOS caps a unix socket path at 103 bytes (BUILD-PLAN §6.2).
const maxSocketPath = 103

// checkSocket checks the session socket's directory: it must be private, or
// the server would expose a terminal to every local user.
func (d *doctorAllRun) checkSocket() {
	socket := d.env("JUMPR_SOCKET")
	if socket == "" {
		socket = filepath.Join(d.paths.CacheDir, "jumpr.sock")
	}
	if len(socket) > maxSocketPath {
		d.report.failMsg("socket dir",
			fmt.Sprintf("%s is %d bytes; a unix socket path may be at most %d", socket, len(socket), maxSocketPath))
		return
	}
	dir := filepath.Dir(socket)
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		d.report.status("warn", "socket dir", dir+" missing; created 0700 on first start")
		return
	case err != nil:
		d.report.failDetail("socket dir", dir, err)
		return
	case !info.IsDir():
		d.report.failMsg("socket dir", dir+" is not a directory")
		return
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		d.report.failMsg("socket dir",
			fmt.Sprintf("%s is %04o; run `chmod 700 %s` (other users must not reach the socket)", dir, mode, dir))
		return
	}
	d.report.ok("socket dir", fmt.Sprintf("%s (%04o)", dir, mode))
}

// checkTerminal reports what jumpr can tell about the terminal it will draw
// in: TERM, the colour depth and whether it runs inside tmux.
func (d *doctorAllRun) checkTerminal() {
	term := d.env("TERM")
	switch term {
	case "":
		d.report.status("warn", "term", "TERM is not set; colours and keys may be wrong")
	case "dumb":
		d.report.status("warn", "term", "TERM=dumb: the full-screen UI needs a real terminal")
	default:
		d.report.ok("term", term)
	}

	colorTerm := strings.ToLower(d.env("COLORTERM"))
	t := strings.ToLower(term)
	switch {
	case colorTerm == "truecolor" || colorTerm == "24bit":
		d.report.ok("colour", "24-bit (truecolor)")
	case strings.Contains(t, "256color"):
		d.report.ok("colour", "256 colours")
	case term == "" || t == "dumb":
		d.report.status("warn", "colour", "unknown; set TERM to a colour terminal (e.g. xterm-256color)")
	default:
		d.report.ok("colour", "16 colours")
	}

	if d.env("TMUX") != "" {
		d.report.ok("tmux", "inside tmux; mux.prefix_in_tmux ("+d.cfg.Mux.PrefixInTmux+") applies")
		return
	}
	d.report.ok("tmux", "not inside tmux; mux.prefix ("+d.cfg.Mux.Prefix+") applies")
}

// env reads one variable from the caller's environment, or from the process
// when the caller passed none (cmd/jumpr always does; the tests do not).
func (d *doctorAllRun) env(key string) string {
	if d.opts.Env != nil {
		return d.opts.Env[key]
	}
	return os.Getenv(key)
}

// host resolves a name or alias against the loaded inventory.
func (d *doctorAllRun) host(name string) (inventory.Host, bool) {
	for _, h := range d.hosts {
		if string(h.ID) == strings.ToLower(strings.TrimSpace(name)) {
			return h, true
		}
		for _, a := range h.Aliases {
			if strings.EqualFold(a, name) {
				return h, true
			}
		}
	}
	return inventory.Host{}, false
}

// sampleFor returns the first host a source contributed.
func (d *doctorAllRun) sampleFor(source string) (inventory.Host, bool) {
	for _, h := range d.hosts {
		for _, s := range h.Sources {
			if s == source {
				return h, true
			}
		}
	}
	return inventory.Host{}, false
}

// inventorySourceName is the name a source type contributes under: the
// ssh_config source is "ssh", every other type its own name.
func inventorySourceName(typ string) string {
	if typ == "ssh_config" {
		return "ssh"
	}
	return typ
}

// connectTimeout and execTimeout are the configured SSH timeouts, with the
// BUILD-PLAN defaults when the configuration is empty.
func (d *doctorAllRun) connectTimeout() time.Duration {
	if v := d.cfg.SSH.ConnectTimeout.Duration(); v > 0 {
		return v
	}
	return 10 * time.Second
}

func (d *doctorAllRun) execTimeout() time.Duration {
	if v := d.cfg.SSH.ExecTimeout.Duration(); v > 0 {
		return v
	}
	return 30 * time.Second
}

// doctorSSH proves that SSH works against one host: the resolved chain, the
// dial, the host key, a round trip, sudo and the remote OS. It exits 0 when the
// dial and the echo round trip succeed, 1 otherwise.
func doctorSSH(ctx context.Context, o Options, alias string, out io.Writer) int {
	red, err := redact.New(nil)
	if err != nil {
		fmt.Fprintf(out, "FAIL  doctor         %v\n", err)
		return 1
	}
	r := &doctorReport{out: out, red: red}

	paths := config.ResolvePaths(o.Home, o.ConfigDir)
	log := newLogger(paths.CacheDir, o.Debug)

	cfg, err := config.Load(filepath.Join(paths.ConfigDir, "config.yaml"), o.Home)
	if err != nil {
		r.fail("config", err)
		return 1
	}
	stack, err := buildSSH(cfg, o, log)
	if err != nil {
		r.fail("config", err)
		return 1
	}
	defer func() { _ = stack.Close() }()

	chain, err := stack.Resolver.Chain(alias)
	if err != nil {
		r.fail("resolved", err)
		return 1
	}
	spec := chain[len(chain)-1]
	host := resource.HostID(alias)
	if len(chain) > 1 {
		hops := make([]string, 0, len(chain)-1)
		for _, h := range chain[:len(chain)-1] {
			hops = append(hops, h.Alias)
		}
		r.ok("resolved", fmt.Sprintf("%s via %s", alias, strings.Join(hops, " -> ")))
	} else {
		r.ok("resolved", alias)
	}

	start := time.Now()
	lease, err := stack.Pool.Client(ctx, host)
	elapsed := time.Since(start)
	if err != nil {
		r.fail("dial + auth", err)
		r.failMsg("host key", "not checked: the dial failed")
		r.failMsg("echo ok", "not run: the dial failed")
		r.failMsg("sudo -n true", "not run: the dial failed")
		r.failMsg("uname -sr", "not run: the dial failed")
		r.failMsg("os-release", "not run: the dial failed")
		return 1
	}
	lease.Release()
	r.ok("dial + auth", ms(elapsed))
	r.ok("host key", "accepted ("+strictMode(spec.StrictHostKeyChecking)+")")

	echoOK := doctorEcho(ctx, stack.Transport, host, r)
	doctorSudo(ctx, stack.Transport, host, r)
	doctorRun(ctx, stack.Transport, host, r, "uname -sr", "uname -sr")
	doctorRun(ctx, stack.Transport, host, r, "os-release",
		`. /etc/os-release 2>/dev/null && printf '%s\n' "$PRETTY_NAME"`)

	if !echoOK {
		return 1
	}
	return 0
}

// doctorEcho runs the round trip that decides the exit code.
func doctorEcho(ctx context.Context, ex transport.Executor, host resource.HostID, r *doctorReport) bool {
	res, err := ex.Exec(ctx, host, transport.Command{Script: "echo ok"})
	if err != nil {
		r.fail("echo ok", err)
		return false
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "ok" || res.RC != 0 {
		r.failMsg("echo ok", fmt.Sprintf("unexpected output %q (rc %d)", got, res.RC))
		return false
	}
	r.ok("echo ok", ms(res.Duration))
	return true
}

// doctorSudo reports whether `sudo -n true` works. A password requirement is
// reported as needs-privilege, not as a failure: the transport works, the login
// simply is not passwordlessly privileged.
func doctorSudo(ctx context.Context, ex transport.Executor, host resource.HostID, r *doctorReport) {
	res, err := ex.Exec(ctx, host, transport.Command{Script: "sudo -n true"})
	if err == nil && res.RC == 0 {
		r.ok("sudo -n true", "ok")
		return
	}
	cause := err
	if cause == nil {
		// The command ran and failed: classify its stderr, which is only done
		// for sudo failures.
		cause = shq.Classify(res.RC, string(res.Stderr), true)
	}
	if errs.KindOf(cause) == errs.Sudo {
		r.status("needs-privilege", "sudo -n true", "passwordless sudo is not available")
		return
	}
	if err != nil {
		r.fail("sudo -n true", err)
		return
	}
	r.failMsg("sudo -n true", fmt.Sprintf("rc %d: %s", res.RC, strings.TrimSpace(string(res.Stderr))))
}

// doctorRun runs one read-only probe and prints its first output line under
// label. It never changes the exit code.
func doctorRun(ctx context.Context, ex transport.Executor, host resource.HostID, r *doctorReport, label, script string) {
	res, err := ex.Exec(ctx, host, transport.Command{Script: script})
	if err != nil {
		r.fail(label, err)
		return
	}
	if res.RC != 0 {
		r.failMsg(label, fmt.Sprintf("rc %d: %s", res.RC, strings.TrimSpace(string(res.Stderr))))
		return
	}
	line := strings.TrimSpace(string(res.Stdout))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if line == "" {
		line = "(empty)"
	}
	r.ok(label, line)
}

// strictMode names the effective host-key checking mode of the resolved host.
func strictMode(mode string) string {
	if mode = strings.ToLower(strings.TrimSpace(mode)); mode == "" {
		return "ask"
	}
	return mode
}

// ms formats a duration for the report: milliseconds below a second, tenths
// above.
func ms(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}
