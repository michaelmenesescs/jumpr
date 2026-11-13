package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/action"
	"jumper/internal/backend"
	"jumper/internal/backend/fake"
	"jumper/internal/backend/linux"
	"jumper/internal/backend/salt"
	"jumper/internal/bus"
	"jumper/internal/bus/generic"
	"jumper/internal/resource"
	"jumper/internal/targets"
	"jumper/internal/transport"
	"jumper/internal/transport/transporttest"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
)

// saltTestJID is the job id the fake master hands back.
const saltTestJID = "20261112120000000000"

// saltFake is a Salt backend for the command tests: it answers every read the
// `:salt` family makes and records what the plan was asked for.
type saltFake struct {
	preview    []string
	previewErr error
	minionHost map[string]resource.HostID
	hostMinion map[resource.HostID]string
	compiled   salt.Compiled
	unmapped   []resource.HostID
	pingJID    string
	pingErr    error
	health     backend.HealthState

	gotPreview salt.Target
	gotPing    []string
	gotRequest action.Request
}

var (
	_ backend.Backend = (*saltFake)(nil)
	_ SaltOps         = (*saltFake)(nil)
	_ action.Planner  = (*saltFake)(nil)
)

func (f *saltFake) Name() string { return saltBackendName }

func (f *saltFake) Kinds() []resource.KindSpec {
	return []resource.KindSpec{
		{
			Name:    salt.KindMinion,
			Aliases: []string{"minions", "minion"},
			Title:   "Salt minions",
			Scope:   resource.ScopeGlobal,
			Columns: []resource.Column{{ID: "minion", Title: "MINION", Width: 24}},
		},
		{
			Name:    salt.KindKey,
			Aliases: []string{"keys"},
			Title:   "Salt keys",
			Scope:   resource.ScopeGlobal,
			Columns: []resource.Column{{ID: "id", Title: "ID", Width: 24}},
		},
		{
			Name:    salt.KindJobs,
			Aliases: []string{"jobs", "job"},
			Title:   "Salt jobs",
			Scope:   resource.ScopeGlobal,
			Columns: []resource.Column{{ID: "jid", Title: "JID", Width: 22}},
		},
	}
}

func (f *saltFake) List(context.Context, resource.Query) (<-chan backend.Result, error) {
	ch := make(chan backend.Result, 1)
	ch <- backend.Result{Done: 1, Total: 1}
	close(ch)
	return ch, nil
}

func (f *saltFake) Get(_ context.Context, ref resource.Ref) (resource.Resource, error) {
	return resource.Resource{Ref: ref}, nil
}

func (f *saltFake) Links(resource.Resource) []resource.Link { return nil }

func (f *saltFake) Health(context.Context) backend.Health {
	return backend.Health{State: f.health, Checked: time.Now()}
}

func (f *saltFake) PreviewTarget(_ context.Context, t salt.Target) ([]string, error) {
	f.gotPreview = t
	if f.previewErr != nil {
		return nil, f.previewErr
	}
	return append([]string(nil), f.preview...), nil
}

func (f *saltFake) MinionHost(id string) (resource.HostID, bool) {
	h, ok := f.minionHost[id]
	return h, ok
}

func (f *saltFake) HostMinion(host resource.HostID) (string, bool) {
	id, ok := f.hostMinion[host]
	return id, ok
}

func (f *saltFake) CompileSet([]resource.HostID, resource.Selector) (salt.Compiled, []resource.HostID) {
	return f.compiled, f.unmapped
}

func (f *saltFake) TestPing(_ context.Context, minions []string) (string, error) {
	f.gotPing = append([]string(nil), minions...)
	if f.pingErr != nil {
		return "", f.pingErr
	}
	return f.pingJID, nil
}

// Actions is the write half of the Salt backend; the command tests only plan.
func (f *saltFake) Actions([]resource.Resource) []action.Spec { return nil }

// Plan records the request `:salt run` made and returns one step per target.
func (f *saltFake) Plan(_ context.Context, req action.Request) (action.Plan, error) {
	f.gotRequest = req
	fun := req.Params[salt.ParamFun]
	steps := make([]action.Step, 0, len(req.Targets))
	for _, t := range req.Targets {
		steps = append(steps, action.Step{
			Target:  t,
			Display: "salt -L " + string(t.Host) + " " + fun,
		})
	}
	return action.Plan{
		Spec:     action.Spec{ID: req.SpecID, Title: "run function", Risk: action.RiskDestructive},
		Request:  req,
		Title:    "run " + fun,
		Steps:    steps,
		Via:      "salt:master.example.internal",
		Warnings: []string{"runs as root via the Salt master"},
	}, nil
}

// namedExecutor renames a transport, so the generic planner reports the
// transport name the plan dialog shows.
type namedExecutor struct {
	transport.Executor
	name string
}

func (n namedExecutor) Name() string { return n.name }

// saltHarness is a root with `:run` and `:salt` registered over a fake master.
type saltHarness struct {
	env  *Env
	cmds *Commands
	salt *saltFake
	b    bus.Bus
	root *Root
}

// newSaltHarness builds the harness for the given session targets.
func newSaltHarness(t *testing.T, hosts []resource.HostID) *saltHarness {
	t.Helper()
	theme.SetProfile("none")

	sshFake := transporttest.New().On("", transporttest.Reply{Stdout: "ok\n"})
	execs := map[string]transport.Executor{
		"ssh":           sshFake,
		saltBackendName: namedExecutor{Executor: sshFake, name: saltBackendName},
	}

	f := &saltFake{
		health:     backend.HealthOK,
		pingJID:    saltTestJID,
		minionHost: make(map[string]resource.HostID),
		hostMinion: make(map[resource.HostID]string),
	}
	reg := backend.NewRegistry()
	if err := reg.Register(f); err != nil {
		t.Fatalf("register salt fake: %v", err)
	}
	if err := reg.Register(fake.New(fake.Options{})); err != nil {
		t.Fatalf("register fake backend: %v", err)
	}

	b := bus.New(bus.Options{
		Now: time.Now,
		Rollout: bus.RolloutConfig{
			Canary:                  1,
			BatchPercent:            100,
			BatchMax:                50,
			FailureThresholdPercent: 5,
			TypedConfirmOver:        20,
		},
	})
	if err := b.Register("generic", generic.New(execs, "ssh")); err != nil {
		t.Fatalf("register generic planner: %v", err)
	}
	if err := b.Register(saltBackendName, f); err != nil {
		t.Fatalf("register salt planner: %v", err)
	}

	env := &Env{
		Registry:  reg,
		Session:   &SessionState{Name: "main", Targets: targets.FromHosts("test", hosts)},
		Exec:      sshFake,
		Execs:     execs,
		Salt:      SaltOptions{SuggestOver: 1000, Configured: true},
		Theme:     theme.Dark(),
		Keys:      keymap.Default(),
		Now:       time.Now,
		Factories: map[resource.KindName]Factory{},
	}
	cmds := NewCommands()
	RegisterBuiltins(cmds)
	if err := RegisterRunCommands(cmds, b); err != nil {
		t.Fatalf("register :run: %v", err)
	}
	if err := RegisterSaltCommands(cmds, b); err != nil {
		t.Fatalf("register :salt: %v", err)
	}
	return &saltHarness{
		env:  env,
		cmds: cmds,
		salt: f,
		b:    b,
		root: NewRoot(env, cmds, nil, resource.Query{Kind: "host"}, "ctrl+a"),
	}
}

// mapMinion makes minion id belong to the host of the same name.
func (h *saltHarness) mapMinion(id string) {
	host := resource.HostID(id)
	h.salt.minionHost[id] = host
	h.salt.hostMinion[host] = id
}

// webHosts returns n hosts named web-01…web-nn.
func webHosts(n int) []resource.HostID {
	hosts := make([]resource.HostID, n)
	for i := range hosts {
		hosts[i] = resource.HostID(fmt.Sprintf("web-%02d", i+1))
	}
	return hosts
}

// exec dispatches one `:` line and returns the single message it produced. A
// command that produces a batch fails the test: the tests assert on the one
// message they expect.
func (h *saltHarness) exec(t *testing.T, line string) tea.Msg {
	t.Helper()
	cmd := h.cmds.Exec(h.env, line)
	if cmd == nil {
		t.Fatalf(":%s produced no command", line)
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if msg == nil {
			t.Fatalf(":%s produced no message", line)
		}
		if _, ok := msg.(tea.BatchMsg); ok {
			t.Fatalf(":%s produced a batch; the test wants one message", line)
		}
		return msg
	case <-time.After(10 * time.Second):
		t.Fatalf(":%s timed out", line)
	}
	return nil
}

// TestSaltKindsResolveAliases checks that `:minions`, `:keys` and `:jobs` open
// the Salt kinds through the aliases the real backend declares, and that
// `:jobs --arg fun=state.` reaches the query.
func TestSaltKindsResolveAliases(t *testing.T) {
	theme.SetProfile("none")
	reg := backend.NewRegistry()
	// The real backend registers the real alias table; it needs no master to
	// list its kinds.
	if err := reg.Register(salt.New(salt.Options{})); err != nil {
		t.Fatalf("register salt backend: %v", err)
	}
	env := &Env{
		Registry: reg,
		Session:  &SessionState{Name: "main"},
		Theme:    theme.Dark(),
		Keys:     keymap.Default(),
		Now:      time.Now,
	}
	cmds := NewCommands()

	cases := []struct {
		line string
		kind resource.KindName
		args map[string]string
	}{
		{"minions", salt.KindMinion, nil},
		{"keys", salt.KindKey, nil},
		{"jobs", salt.KindJobs, nil},
		{"jobs --arg fun=state.", salt.KindJobs, map[string]string{"fun": "state."}},
	}
	for _, c := range cases {
		t.Run(c.line, func(t *testing.T) {
			cmd := cmds.Exec(env, c.line)
			if cmd == nil {
				t.Fatalf(":%s produced no command", c.line)
			}
			msg, ok := cmd().(OpenQueryMsg)
			if !ok {
				t.Fatalf(":%s = %T, want an OpenQueryMsg", c.line, msg)
			}
			if msg.Query.Kind != c.kind {
				t.Fatalf(":%s opened kind %q, want %q", c.line, msg.Query.Kind, c.kind)
			}
			for k, v := range c.args {
				if msg.Query.Args[k] != v {
					t.Fatalf(":%s arg %q = %q, want %q", c.line, k, msg.Query.Args[k], v)
				}
			}
		})
	}
}

// TestSaltNativeTargetSetsTargets is `:salt -C 'role:web'`: the preview's
// minions are mapped onto inventory hosts, the session targets become those
// hosts, the flash counts what was mapped and the host list opens on them.
func TestSaltNativeTargetSetsTargets(t *testing.T) {
	hosts := webHosts(12)
	h := newSaltHarness(t, hosts)
	for _, id := range []string{"web-01", "web-02", "web-03", "web-04", "web-05", "web-06",
		"web-07", "web-08", "web-09", "web-10", "web-11", "web-12"} {
		h.mapMinion(id)
	}
	// Two minions Salt reports but the inventory does not hold.
	h.salt.preview = append(append([]string(nil), "web-01", "web-02", "web-03", "web-04",
		"web-05", "web-06", "web-07", "web-08", "web-09", "web-10", "web-11", "web-12"), "ghost-1", "ghost-2")

	msg := h.exec(t, "salt -C 'role:web'")
	m, ok := msg.(saltTargetsMsg)
	if !ok {
		t.Fatalf(":salt -C = %T, want a saltTargetsMsg", msg)
	}
	if got, want := h.salt.gotPreview, (salt.Target{Type: "compound", Expr: "role:web"}); got != want {
		t.Fatalf("preview target = %+v, want %+v", got, want)
	}
	if len(m.Set.Hosts) != 12 {
		t.Fatalf("target set = %d hosts, want 12 (%v)", len(m.Set.Hosts), m.Set.Hosts)
	}
	if m.Set.Label != "-C role:web" {
		t.Fatalf("target label = %q, want the native expression", m.Set.Label)
	}
	const wantFlash = "14 minions → 12 hosts, 2 unmapped"
	if m.Text != wantFlash {
		t.Fatalf("flash = %q, want %q", m.Text, wantFlash)
	}
	if m.Query.Kind != "host" {
		t.Fatalf("opened kind = %q, want the host list", m.Query.Kind)
	}

	h.root.Update(m)
	if got := h.env.Session.Targets.Hosts; len(got) != 12 {
		t.Fatalf("session targets = %d hosts, want 12", len(got))
	}
	if label := h.env.Session.Targets.Label; label != "-C role:web" {
		t.Fatalf("session target label = %q", label)
	}
}

// TestSaltNativeTargetUnmappedKeepsRunning checks that no mapped host is a
// warning, not an error: nothing is set and the flash says why.
func TestSaltNativeTargetUnmappedKeepsRunning(t *testing.T) {
	h := newSaltHarness(t, webHosts(2))
	h.salt.preview = []string{"ghost-1", "ghost-2"}

	msg := h.exec(t, "salt web-*")
	m, ok := msg.(FlashMsg)
	if !ok {
		t.Fatalf(":salt web-* = %T, want a FlashMsg", msg)
	}
	if m.Level != FlashWarn || !strings.Contains(m.Text, "no target set") {
		t.Fatalf("flash = %+v, want a warning about no target set", m)
	}
	if len(h.env.Session.Targets.Hosts) != 2 {
		t.Fatalf("session targets changed to %v", h.env.Session.Targets.Hosts)
	}
}

// TestSaltTargetExplain is `:salt target`: the compile explanation and the CLI
// string of every compiled target.
func TestSaltTargetExplain(t *testing.T) {
	hosts := append(webHosts(3), "db-01")
	h := newSaltHarness(t, hosts)
	h.salt.compiled = salt.Compiled{
		Targets: []salt.Target{{Type: "compound", Expr: "G@role:web"}},
		Native:  true,
		Minions: []string{"web-01", "web-02", "web-03"},
		Explain: "compound: G@role:web",
	}
	h.salt.unmapped = []resource.HostID{"db-01"}

	msg := h.exec(t, "salt target")
	push, ok := msg.(PushViewMsg)
	if !ok {
		t.Fatalf(":salt target = %T, want a PushViewMsg", msg)
	}
	page, ok := push.View.(*textPage)
	if !ok {
		t.Fatalf("view = %T, want a text page", push.View)
	}
	text := strings.Join(page.lines, "\n")
	for _, want := range []string{
		"hosts: 4 (minions: 3)",
		"not minions: 1 hosts",
		"compiled: compound: G@role:web",
		"cli: -C G@role:web",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("`:salt target` has no %q:\n%s", want, text)
		}
	}
}

// TestSaltStateOpensDryRun checks that `:salt state [sls]` opens the dry-run
// view for the session targets and passes the sls through.
func TestSaltStateOpensDryRun(t *testing.T) {
	h := newSaltHarness(t, webHosts(2))
	var got resource.Query
	h.env.Factories[saltStateKind] = func(env *Env, q resource.Query) (View, error) {
		got = q
		return newTextPage(env, "salt state", nil), nil
	}

	msg := h.exec(t, "salt state nginx")
	if _, ok := msg.(PushViewMsg); !ok {
		t.Fatalf(":salt state nginx = %T, want a PushViewMsg", msg)
	}
	if got.Kind != saltStateKind || got.Args["sls"] != "nginx" {
		t.Fatalf("state view query = %+v, want the salt-state kind and sls nginx", got)
	}

	// With no targets there is nothing to dry run.
	h.env.Session.Targets = targets.Set{}
	flash, ok := h.exec(t, "salt state nginx").(FlashMsg)
	if !ok || flash.Level != FlashError || !strings.Contains(flash.Text, "no targets") {
		t.Fatalf(":salt state without targets = %#v, want a no-targets error", flash)
	}
}

// TestSaltRunProposesModule is `:salt run <fun> [args…] [k=v…]`: it plans a
// salt.module.run through the bus with the words kept as typed.
func TestSaltRunProposesModule(t *testing.T) {
	h := newSaltHarness(t, webHosts(2))
	h.mapMinion("web-01")
	h.mapMinion("web-02")

	msg := h.exec(t, "salt run cmd.run uptime test=True")
	dialog, ok := msg.(OpenDialogMsg)
	if !ok {
		t.Fatalf(":salt run = %T, want the plan dialog", msg)
	}
	req := h.salt.gotRequest
	if req.SpecID != salt.SpecModuleRun {
		t.Fatalf("planned spec = %q, want %q", req.SpecID, salt.SpecModuleRun)
	}
	if got := req.Params[salt.ParamFun]; got != "cmd.run" {
		t.Fatalf("fun = %q, want cmd.run", got)
	}
	if got := req.Params[salt.ParamArgs]; got != "uptime" {
		t.Fatalf("args = %q, want uptime", got)
	}
	if got := req.Params[salt.ParamKwargs]; got != "test=True" {
		t.Fatalf("kwargs = %q, want test=True", got)
	}
	if len(req.Targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(req.Targets))
	}
	if view := dialog.Dialog.View(120, 30); !strings.Contains(view, "targets: 2 hosts") {
		t.Fatalf("plan dialog does not describe the targets:\n%s", view)
	}
	// The Salt planner's own plan carries the root warning.
	if !strings.Contains(dialog.Dialog.View(120, 30), "runs as root") {
		t.Fatalf("plan dialog has no root warning:\n%s", dialog.Dialog.View(120, 30))
	}
}

// TestSaltRunRejectsBadArgs checks the grammar: a missing function never
// reaches the bus.
func TestSaltRunRejectsBadArgs(t *testing.T) {
	h := newSaltHarness(t, webHosts(1))
	msg := h.exec(t, "salt run")
	flash, ok := msg.(FlashMsg)
	if !ok || flash.Level != FlashError || !strings.Contains(flash.Text, "needs a function") {
		t.Fatalf(":salt run = %#v, want a usage error", msg)
	}
	if h.salt.gotRequest.SpecID != "" {
		t.Fatalf("a bad :salt run reached the bus as %q", h.salt.gotRequest.SpecID)
	}
}

// TestSaltPingStartsJob is `:salt ping`: one test.ping job on the minions of
// the session targets, with the unmapped hosts counted instead of sent.
func TestSaltPingStartsJob(t *testing.T) {
	hosts := append(webHosts(2), "db-01")
	h := newSaltHarness(t, hosts)
	h.mapMinion("web-01")
	h.mapMinion("web-02")

	msg := h.exec(t, "salt ping")
	m, ok := msg.(saltPingMsg)
	if !ok {
		t.Fatalf(":salt ping = %T, want a saltPingMsg", msg)
	}
	if m.JID != saltTestJID {
		t.Fatalf("jid = %q, want %q", m.JID, saltTestJID)
	}
	if want := []string{"web-01", "web-02"}; !equalStrings(h.salt.gotPing, want) {
		t.Fatalf("pinged %v, want %v", h.salt.gotPing, want)
	}
	if m.Minions != 2 || m.Unmapped != 1 {
		t.Fatalf("ping counts = %d minions, %d unmapped; want 2 and 1", m.Minions, m.Unmapped)
	}
	if !m.First {
		t.Fatal("the first Salt use of the session is not marked as such")
	}
	if !h.env.Session.saltUsed {
		t.Fatal("the session did not record the Salt use")
	}
}

// TestRunViaSalt is `:run --via salt`: the plan dialog names the transport, the
// generic root warning, and the job-cache warning on the first use only.
func TestRunViaSalt(t *testing.T) {
	h := newSaltHarness(t, webHosts(3))

	msg := h.exec(t, "run --via salt --sudo uptime")
	open, ok := msg.(OpenDialogMsg)
	if !ok {
		t.Fatalf(":run --via salt = %T, want the plan dialog", msg)
	}
	view := open.Dialog.View(200, 40)
	for _, want := range []string{"via: salt", "runs as root on every target", saltJobCacheWarning} {
		if !strings.Contains(view, want) {
			t.Fatalf("first salt plan has no %q:\n%s", want, view)
		}
	}

	// The second use in the same session keeps the root warning and drops the
	// job-cache one.
	msg = h.exec(t, "run --via salt uptime")
	open, ok = msg.(OpenDialogMsg)
	if !ok {
		t.Fatalf("second :run --via salt = %T, want the plan dialog", msg)
	}
	view = open.Dialog.View(200, 40)
	if !strings.Contains(view, "runs as root on every target") {
		t.Fatalf("second salt plan has no root warning:\n%s", view)
	}
	if strings.Contains(view, saltJobCacheWarning) {
		t.Fatalf("the job-cache warning is shown twice:\n%s", view)
	}
}

// TestSuggestSaltHint checks the plan hint: a set larger than
// salt.suggest_over suggests the Salt transport, but only when Salt is healthy
// and --via was not given.
func TestSuggestSaltHint(t *testing.T) {
	cases := []struct {
		name   string
		opts   SaltOptions
		target int
		want   string
	}{
		{"over the threshold", SaltOptions{SuggestOver: 1000, Configured: true}, 1512, "1,512 targets: consider --via salt"},
		{"at the threshold", SaltOptions{SuggestOver: 1000, Configured: true}, 1000, ""},
		{"below the threshold", SaltOptions{SuggestOver: 1000, Configured: true}, 25, ""},
		{"salt not configured", SaltOptions{}, 5000, ""},
		{"no threshold", SaltOptions{Configured: true}, 5000, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := &Env{Salt: c.opts}
			if got := saltSuggestHint(env, c.target); got != c.want {
				t.Fatalf("saltSuggestHint(%d) = %q, want %q", c.target, got, c.want)
			}
		})
	}

	t.Run("shown in the plan", func(t *testing.T) {
		h := newSaltHarness(t, webHosts(25))
		h.env.Salt = SaltOptions{SuggestOver: 10, Configured: true}

		msg := h.exec(t, "run uptime")
		open, ok := msg.(OpenDialogMsg)
		if !ok {
			t.Fatalf(":run = %T, want the plan dialog", msg)
		}
		if view := open.Dialog.View(200, 40); !strings.Contains(view, "25 targets: consider --via salt") {
			t.Fatalf("plan has no salt hint:\n%s", view)
		}
	})

	t.Run("silent when salt is down", func(t *testing.T) {
		h := newSaltHarness(t, webHosts(25))
		h.env.Salt = SaltOptions{SuggestOver: 10, Configured: true}
		h.salt.health = backend.HealthDown

		msg := h.exec(t, "run uptime")
		open, ok := msg.(OpenDialogMsg)
		if !ok {
			t.Fatalf(":run = %T, want the plan dialog", msg)
		}
		if view := open.Dialog.View(200, 40); strings.Contains(view, "consider --via salt") {
			t.Fatalf("a down Salt master still suggests --via salt:\n%s", view)
		}
	})

	t.Run("never over --via ssh", func(t *testing.T) {
		h := newSaltHarness(t, webHosts(25))
		h.env.Salt = SaltOptions{SuggestOver: 10, Configured: true}

		msg := h.exec(t, "run --via ssh uptime")
		open, ok := msg.(OpenDialogMsg)
		if !ok {
			t.Fatalf(":run --via ssh = %T, want the plan dialog", msg)
		}
		if view := open.Dialog.View(200, 40); strings.Contains(view, "consider --via salt") {
			t.Fatalf("an explicit --via ssh still suggests salt:\n%s", view)
		}
	})
}

// TestProbeViaParsesTransport checks the `:probe --via` grammar without a
// backend: the flag is split off the probe name and validated.
func TestProbeViaParsesTransport(t *testing.T) {
	cases := []struct {
		args    string
		via     string
		name    string
		wantErr bool
	}{
		{"--via salt failed-units", "salt", "failed-units", false},
		{"--via=ssh uptime", "ssh", "uptime", false},
		{"uptime", "", "uptime", false},
		{"--via bogus uptime", "", "", true},
		{"--via", "", "", true},
	}
	for _, c := range cases {
		via, name, err := parseProbeArgs(c.args)
		if c.wantErr {
			if err == nil {
				t.Fatalf("parseProbeArgs(%q) succeeded, want an error", c.args)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseProbeArgs(%q): %v", c.args, err)
		}
		if via != c.via || name != c.name {
			t.Fatalf("parseProbeArgs(%q) = (%q, %q), want (%q, %q)", c.args, via, name, c.via, c.name)
		}
	}
}

// TestHostRowSaltPivots is requirement 4: the Salt backend offers a host row
// its minion and that minion's jobs, and the Linux backend appends them without
// importing Salt. A row that does not carry a minion gets nothing extra.
func TestHostRowSaltPivots(t *testing.T) {
	saltBackend := salt.New(salt.Options{})
	lb := linux.New(linux.Options{Links: saltBackend})

	row := resource.Resource{
		Ref:    resource.Ref{Backend: "linux", Kind: "host", Host: "web-01", Name: "web-01"},
		Labels: map[string]string{"salt.id": "web-01-web"},
	}
	links := lb.Links(row)
	byKey := make(map[string]resource.Link, len(links))
	for _, l := range links {
		if l.Key != "" {
			byKey[l.Key] = l
		}
	}
	minion, ok := byKey["M"]
	if !ok {
		t.Fatalf("host row has no salt minion link: %+v", links)
	}
	if minion.To.Kind != salt.KindMinion || minion.To.Args["minion"] != "web-01-web" {
		t.Fatalf("salt minion link = %+v, want the salt-minion kind for web-01-web", minion)
	}
	jobs, ok := byKey["J"]
	if !ok {
		t.Fatalf("host row has no salt jobs link: %+v", links)
	}
	if jobs.To.Kind != salt.KindJobs || jobs.To.Args["minion"] != "web-01-web" {
		t.Fatalf("salt jobs link = %+v, want the salt-job kind for web-01-web", jobs)
	}

	// A host the grains source did not map stays as it was.
	plain := lb.Links(resource.Resource{
		Ref:    resource.Ref{Backend: "linux", Kind: "host", Host: "db-01", Name: "db-01"},
		Labels: map[string]string{"host": "db-01"},
	})
	for _, l := range plain {
		if l.Key == "M" || l.Key == "J" {
			t.Fatalf("unmapped host row got a salt pivot: %+v", l)
		}
	}
}

// TestProbeViaSaltUsesSaltTransport checks that `:probe --via salt` hands the
// fan-out an environment whose executor is the Salt transport, leaving the
// session's default alone.
func TestProbeViaSaltUsesSaltTransport(t *testing.T) {
	h := newSaltHarness(t, webHosts(1))
	// Not the first Salt use, so the command answers with the one push.
	h.env.Session.saltUsed = true
	var viewEnv *Env
	if err := RegisterProbeCommands(h.cmds, func(env *Env, title string, cmd transport.Command, hosts []resource.HostID, normalize func(string) string) View {
		viewEnv = env
		return newTextPage(env, title, nil)
	}); err != nil {
		t.Fatalf("register :probe: %v", err)
	}

	if msg := h.exec(t, "probe --via salt failed-units"); msg == nil {
		t.Fatal(":probe --via salt produced no message")
	}
	if viewEnv == nil {
		t.Fatal("the fan-out opener was never called")
	}
	if got := viewEnv.Exec.Name(); got != saltBackendName {
		t.Fatalf("fan-out executor = %q, want %q", got, saltBackendName)
	}
	if got := h.env.Exec.Name(); got == saltBackendName {
		t.Fatalf("the session's default executor changed to %q", got)
	}
}

// TestProbeViaSaltWarnsOnce checks the first-use job-cache warning for the
// probe path as well.
func TestProbeViaSaltWarnsOnce(t *testing.T) {
	h := newSaltHarness(t, webHosts(1))
	if err := RegisterProbeCommands(h.cmds, func(env *Env, title string, cmd transport.Command, hosts []resource.HostID, normalize func(string) string) View {
		return newTextPage(env, title, nil)
	}); err != nil {
		t.Fatalf("register :probe: %v", err)
	}

	cmd := h.cmds.Exec(h.env, "probe --via salt failed-units")
	if cmd == nil {
		t.Fatal(":probe --via salt produced no command")
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("first :probe --via salt = %T, want a batch with the warning", msg)
	}
	var texts []string
	for _, c := range batch {
		if m, ok := c().(FlashMsg); ok {
			texts = append(texts, m.Text)
		}
	}
	found := false
	for _, text := range texts {
		if text == saltJobCacheWarning {
			found = true
		}
	}
	if !found {
		t.Fatalf("no job-cache warning among %v", texts)
	}
	if !h.env.Session.saltUsed {
		t.Fatal("the session did not record the Salt use")
	}
}

// equalStrings reports whether two string slices hold the same values in order.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
