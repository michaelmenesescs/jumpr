package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	teatest "github.com/charmbracelet/x/exp/teatest"

	"jumper/internal/action"
	"jumper/internal/bus"
	"jumper/internal/bus/generic"
	"jumper/internal/investigation"
	"jumper/internal/resource"
	"jumper/internal/targets"
	"jumper/internal/transport"
	"jumper/internal/transport/transporttest"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
)

// runHarness is a root model with `:run` registered over a scripted transport
// and a real bus.
type runHarness struct {
	env  *Env
	cmds *Commands
	root *Root
	b    bus.Bus
	fake *transporttest.Fake
	rec  *auditRecorder
}

// auditRecorder writes through internal/investigation directly: with no
// investigation open the audit file is the record (SPEC §10).
type auditRecorder struct{ root string }

func (a *auditRecorder) RecordAction(p bus.Proposal, s bus.Summary, results []action.StepResult) error {
	text := fmt.Sprintf("%s → ok %d/%d", p.Plan.Title, s.OK, s.OK+s.Failed)
	output := ""
	if len(results) > 0 {
		output = results[0].Output
	}
	return investigation.AppendAudit(a.root, investigation.Entry{
		Time:   time.Now(),
		Kind:   investigation.EntryAction,
		Actor:  action.Human,
		Text:   text,
		Output: output,
	}, nil, time.Now())
}

func (a *auditRecorder) RecordDenied(p bus.Proposal) error {
	rule := ""
	if p.Denied != nil {
		rule = p.Denied.Rule
	}
	return investigation.AppendAudit(a.root, investigation.Entry{
		Time:  time.Now(),
		Kind:  investigation.EntryDenied,
		Actor: action.Human,
		Text:  "denied: " + rule,
	}, nil, time.Now())
}

// newRunHarness builds the harness. withRecorder adds the audit recorder.
func newRunHarness(t *testing.T, hosts []resource.HostID, withRecorder bool) *runHarness {
	t.Helper()
	theme.SetProfile("none")
	fake := transporttest.New().On("", transporttest.Reply{Stdout: "hi\n"})
	planner := generic.New(map[string]transport.Executor{"ssh": fake}, "ssh")

	opts := bus.Options{
		Rollout: bus.RolloutConfig{
			Canary:                  1,
			BatchPercent:            100,
			BatchMax:                50,
			FailureThresholdPercent: 5,
			TypedConfirmOver:        20,
		},
	}
	var rec *auditRecorder
	if withRecorder {
		rec = &auditRecorder{root: t.TempDir()}
		opts.Recorder = rec
	}
	b := bus.New(opts)
	if err := b.Register("generic", planner); err != nil {
		t.Fatalf("register planner: %v", err)
	}

	env := &Env{
		Session: &SessionState{
			Name:    "main",
			Targets: targets.FromHosts("test", hosts),
		},
		Theme: theme.Dark(),
		Keys:  keymap.Default(),
		Now:   time.Now,
	}
	cmds := NewCommands()
	RegisterBuiltins(cmds)
	if err := RegisterRunCommands(cmds, b); err != nil {
		t.Fatalf("register :run: %v", err)
	}
	root := NewRoot(env, cmds, nil, resource.Query{Kind: "host"}, "ctrl+a")
	return &runHarness{env: env, cmds: cmds, root: root, b: b, fake: fake, rec: rec}
}

// start launches the harness at 120×36.
func (h *runHarness) start(t *testing.T) *teatest.TestModel {
	t.Helper()
	return teatest.NewTestModel(t, h.root, teatest.WithInitialTermSize(120, 36))
}

// line dispatches one `:` command line through the registry and sends what it
// produced to the running model.
func (h *runHarness) line(t *testing.T, tm *teatest.TestModel, line string) {
	t.Helper()
	cmd := h.cmds.Exec(h.env, line)
	if cmd == nil {
		t.Fatalf(":%s produced no command", line)
	}
	msg := cmd()
	if msg == nil {
		t.Fatalf(":%s produced no message", line)
	}
	tm.Send(msg)
}

// key sends one key press.
func (h *runHarness) key(t *testing.T, tm *teatest.TestModel, key string) {
	t.Helper()
	if len(key) == 1 {
		tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		return
	}
	t.Fatalf("unsupported key %q", key)
}

// runWaitFor waits until the output produced from now on contains every want.
func runWaitFor(t *testing.T, tm *teatest.TestModel, want ...string) {
	t.Helper()
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		for _, w := range want {
			if !strings.Contains(string(b), w) {
				return false
			}
		}
		return true
	}, teatest.WithDuration(5*time.Second))
}

// runFinish quits and returns the final screen.
func runFinish(t *testing.T, tm *teatest.TestModel) string {
	t.Helper()
	tm.Quit()
	m := tm.FinalModel(t, teatest.WithFinalTimeout(3*time.Second))
	root, ok := m.(*Root)
	if !ok {
		t.Fatalf("final model is %T", m)
	}
	return root.View()
}

// auditText returns everything written to the audit directory.
func (h *runHarness) auditText(t *testing.T) string {
	t.Helper()
	if h.rec == nil {
		t.Fatal("harness has no recorder")
	}
	paths, err := filepath.Glob(filepath.Join(h.rec.root, "audit", "*.md"))
	if err != nil {
		t.Fatalf("glob audit: %v", err)
	}
	var b strings.Builder
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		b.Write(data)
	}
	return b.String()
}

func TestRunCommandParsesFlags(t *testing.T) {
	cases := []struct {
		name string
		args string
		want runArgs
	}{
		{"sudo before the command", "--sudo ss -tlnp | wc -l", runArgs{command: "ss -tlnp | wc -l", sudo: true}},
		{"no flags", "ss -s", runArgs{command: "ss -s"}},
		{"no rollout", "--no-rollout uptime", runArgs{command: "uptime", noRollout: true}},
		{"via salt", "--via salt --sudo systemctl restart nginx", runArgs{command: "systemctl restart nginx", sudo: true, via: "salt"}},
		{"flags in any order", "--no-rollout --sudo --via ssh df -h", runArgs{command: "df -h", sudo: true, via: "ssh", noRollout: true}},
		{"end of flags marker", "-- --version", runArgs{command: "--version"}},
		{"inner spacing kept", "  printf 'a  b'  ", runArgs{command: "printf 'a  b'"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseRunArgs(c.args)
			if err != nil {
				t.Fatalf("parseRunArgs(%q): %v", c.args, err)
			}
			if got != c.want {
				t.Fatalf("parseRunArgs(%q) = %+v, want %+v", c.args, got, c.want)
			}
		})
	}

	bad := []string{"", "--sudo", "--via", "--via bogus ls", "--bogus ls", "--sudo --no-rollout"}
	for _, args := range bad {
		t.Run("rejects "+args, func(t *testing.T) {
			if _, err := parseRunArgs(args); err == nil {
				t.Fatalf("parseRunArgs(%q) succeeded, want an error", args)
			}
		})
	}
}

func TestRunCommandQuotedWholeCommand(t *testing.T) {
	cases := []struct{ args, want string }{
		{`'ss -s'`, "ss -s"},
		{`"ss -s"`, "ss -s"},
		{`'grep "x" /var/log/syslog'`, `grep "x" /var/log/syslog`},
		{`grep "x" f`, `grep "x" f`},
		{`'unbalanced`, `'unbalanced`},
		{`--sudo 'ss -tlnp | wc -l'`, "ss -tlnp | wc -l"},
	}
	for _, c := range cases {
		t.Run(c.args, func(t *testing.T) {
			got, err := parseRunArgs(c.args)
			if err != nil {
				t.Fatalf("parseRunArgs(%q): %v", c.args, err)
			}
			if got.command != c.want {
				t.Fatalf("command = %q, want %q", got.command, c.want)
			}
		})
	}
}

func TestRunCommandNoTargetsFlashes(t *testing.T) {
	h := newRunHarness(t, nil, false)
	cmd := h.cmds.Exec(h.env, "run 'echo hi'")
	if cmd == nil {
		t.Fatal(":run produced no command")
	}
	msg := cmd()
	flash, ok := msg.(FlashMsg)
	if !ok {
		t.Fatalf("message = %T, want FlashMsg", msg)
	}
	if flash.Level != FlashError || !strings.Contains(flash.Text, "no targets") {
		t.Fatalf("flash = %+v, want a no-targets error", flash)
	}
	if calls := h.fake.Calls(); len(calls) != 0 {
		t.Fatalf("executor ran %d commands without targets", len(calls))
	}
}

func TestRunCommandFlowToGroupedResults(t *testing.T) {
	h := newRunHarness(t, []resource.HostID{"web-ewr-001", "web-ewr-002"}, false)
	tm := h.start(t)

	h.line(t, tm, "run 'echo hi'")
	runWaitFor(t, tm, "run command", "targets: 2 hosts", "command: echo hi",
		"rollout: canary 1 → pause → 1 batch of 1")

	h.key(t, tm, "y")
	runWaitFor(t, tm, "canary ok — c continue · esc abort")

	h.key(t, tm, "c")
	runWaitFor(t, tm, "identical", "hi")

	screen := runFinish(t, tm)
	for _, want := range []string{"identical", "hi"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("final screen has no %q:\n%s", want, screen)
		}
	}
	if calls := h.fake.Calls(); len(calls) != 2 {
		t.Fatalf("executor ran %d commands, want 2", len(calls))
	}
}

func TestRunCommandDeniedByPolicy(t *testing.T) {
	h := newRunHarness(t, []resource.HostID{"web-ewr-001"}, true)
	tm := h.start(t)

	h.line(t, tm, "run reboot")
	runWaitFor(t, tm, "denied by policy", "use your change process")

	// The denial has no confirmation control: a y must not start anything.
	h.key(t, tm, "y")
	time.Sleep(200 * time.Millisecond)
	if calls := h.fake.Calls(); len(calls) != 0 {
		t.Fatalf("a denied run executed %d commands", len(calls))
	}
	if audit := h.auditText(t); !strings.Contains(audit, "denied") {
		t.Fatalf("audit has no denial entry:\n%s", audit)
	}
}

func TestRunCommandRecordedInAudit(t *testing.T) {
	h := newRunHarness(t, []resource.HostID{"web-ewr-001"}, true)
	tm := h.start(t)

	h.line(t, tm, "run 'echo hi'")
	runWaitFor(t, tm, "command: echo hi")
	h.key(t, tm, "y")
	runWaitFor(t, tm, "identical", "hi")

	audit := h.auditText(t)
	for _, want := range []string{"run echo hi on 1 host", "ok 1/1", "hi"} {
		if !strings.Contains(audit, want) {
			t.Fatalf("audit has no %q:\n%s", want, audit)
		}
	}
	runFinish(t, tm)
}

func TestRunCommandRerunRepeatsLastLine(t *testing.T) {
	h := newRunHarness(t, []resource.HostID{"web-ewr-001"}, false)

	// Before any run there is nothing to repeat.
	msg := h.cmds.Exec(h.env, "rerun")()
	if flash, ok := msg.(FlashMsg); !ok || !strings.Contains(flash.Text, "no previous") {
		t.Fatalf("rerun before a run = %#v, want a no-previous flash", msg)
	}

	h.cmds.Exec(h.env, "run --sudo 'echo hi'")()
	msg = h.cmds.Exec(h.env, "rerun")()
	dialog, ok := msg.(OpenDialogMsg)
	if !ok {
		t.Fatalf("rerun = %T, want an OpenDialogMsg plan", msg)
	}
	plan, ok := dialog.Dialog.(*runPlanDialog)
	if !ok {
		t.Fatalf("dialog = %T, want the run plan dialog", dialog.Dialog)
	}
	if plan.p.Plan.Steps[0].Display != "sudo -n echo hi" {
		t.Fatalf("rerun planned %q", plan.p.Plan.Steps[0].Display)
	}
}

func TestRunCommandKeepsHistory(t *testing.T) {
	h := newRunHarness(t, []resource.HostID{"web-ewr-001"}, false)
	tm := h.start(t)

	tm.Type(":run 'echo hi'")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	runWaitFor(t, tm, "command: echo hi")

	history := h.root.Snapshot().History
	if len(history) == 0 || history[len(history)-1] != "run 'echo hi'" {
		t.Fatalf("history = %v, want the :run line", history)
	}
	runFinish(t, tm)
}
