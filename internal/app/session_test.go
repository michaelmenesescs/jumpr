package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"jumper/internal/action"
	"jumper/internal/bus"
	"jumper/internal/config"
	"jumper/internal/interact"
	"jumper/internal/resource"
	"jumper/internal/transport"
	"jumper/internal/transport/transporttest"
	"jumper/internal/tui"
	"jumper/internal/tui/theme"
)

// testCore builds a core over a static inventory of names and, when ex is not
// nil, the executor reads and fan-outs go through. Nothing is dialled: the
// static source is a file and the executor is a fake.
func testCore(t *testing.T, names []string, ex transport.Executor) *Core {
	t.Helper()
	theme.SetProfile("none")
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	cfgDir := filepath.Join(dir, "config")
	for _, d := range []string{home, cfgDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	var inv strings.Builder
	inv.WriteString("hosts:\n")
	for i, name := range names {
		fmt.Fprintf(&inv, "  - name: %s\n    addr: 127.0.0.1\n    labels:\n      role: web\n      rack: r%d\n", name, i+1)
	}
	invPath := filepath.Join(dir, "hosts.yaml")
	writeFile(t, invPath, inv.String())
	writeFile(t, filepath.Join(cfgDir, "config.yaml"), fmt.Sprintf(
		"inventory:\n  cache_ttl: 1h\n  sources:\n    - type: static\n      path: %q\n", invPath))

	cfg, err := config.Load(filepath.Join(cfgDir, "config.yaml"), home)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	// A canary of one host is what makes a rollout pause for confirmation.
	cfg.Rollout.Canary = 1

	paths := config.ResolvePaths(home, cfgDir)
	env := map[string]string{"USER": "tester"}
	core, err := newCore(context.Background(), cfg, paths, env,
		Options{Home: home, ConfigDir: cfgDir, Env: env}, nil)
	if err != nil {
		t.Fatalf("core: %v", err)
	}
	if ex != nil {
		core.Exec = ex
		core.Execs = map[string]transport.Executor{"ssh": ex}
	}
	t.Cleanup(func() { _ = core.Close() })
	return core
}

// testSession starts a session with its program running.
func testSession(t *testing.T, core *Core) *Session {
	t.Helper()
	sess, err := core.NewSession("test")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	t.Cleanup(func() { _ = sess.close() })
	return sess
}

// testClient is one attached terminal: a pipe to type into and a buffer to read
// frames from.
type testClient struct {
	in  *io.PipeWriter
	out *syncBuffer
}

// attach attaches a fresh client to sess and waits until the first frame is on
// its terminal.
func attach(t *testing.T, sess *Session, waitFor string) *testClient {
	t.Helper()
	pr, pw := io.Pipe()
	c := &testClient{in: pw, out: &syncBuffer{}}
	if err := sess.Attach(pr, c.out, Size{Cols: 120, Rows: 36}, map[string]string{"TERM": "xterm-256color"}); err != nil {
		pw.Close()
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { pw.Close() })
	if waitFor != "" {
		waitForText(t, c.out, waitFor)
	}
	return c
}

// TestSessionAttachDetachReattach is the point of M5 in one process: frames arrive on
// the first terminal, the session is detached while a slow fan-out is still
// running, that work finishes with nobody looking, and the second terminal gets
// the alt-screen preamble plus a full repaint that shows the finished run.
func TestSessionAttachDetachReattach(t *testing.T) {
	ex := transporttest.New().OnFunc(func(resource.HostID, transport.Command) (transporttest.Reply, bool) {
		return transporttest.Reply{Stdout: "ALL CLEAR\n", Delay: 250 * time.Millisecond}, true
	})
	core := testCore(t, []string{"web-01", "web-02", "web-03"}, ex)
	sess := testSession(t, core)

	var mu sync.Mutex
	var reasons []string
	sess.SetOnDetach(func(reason string) {
		mu.Lock()
		reasons = append(reasons, reason)
		mu.Unlock()
	})

	a := attach(t, sess, "web-01")
	typeKeys(t, a.in, ":targets role=web\r")
	waitForText(t, a.out, "targets: 3 hosts")
	typeKeys(t, a.in, ":probe uptime\r")
	// Let the fan-out start before the terminal goes away.
	time.Sleep(100 * time.Millisecond)

	sess.Detach("detached")
	if !strings.Contains(a.out.String(), leaveScreen) {
		t.Fatal("the detached terminal did not receive the postamble")
	}
	if sess.Info().Attached {
		t.Fatal("session still reports an attached client")
	}

	// The work keeps running with nobody attached: every host is asked.
	deadline := time.Now().Add(10 * time.Second)
	for len(ex.Calls()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(ex.Calls()); got < 3 {
		t.Fatalf("%d hosts probed while detached, want 3: work stopped with the terminal", got)
	}

	b := attach(t, sess, "")
	if !strings.HasPrefix(b.out.String(), enterScreen) {
		t.Fatalf("the new terminal did not start with the alt-screen preamble: %q", b.out.String()[:min(40, b.out.Len())])
	}
	// A full repaint, not a diff: the finished grouping is on the new screen.
	waitForText(t, b.out, "3 identical")
	// The same program kept running through all of it.
	select {
	case <-sess.Done():
		t.Fatal("the session ended when the first terminal went away")
	default:
	}

	mu.Lock()
	got := append([]string(nil), reasons...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "detached" {
		t.Fatalf("onDetach reasons = %v, want [detached]", got)
	}
}

// min is the smaller of a and b.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestSessionSecondAttachReplacesFirst is BUILD-PLAN §6: one client per session, and a
// second attach detaches the first with reason "replaced".
func TestSessionSecondAttachReplacesFirst(t *testing.T) {
	core := testCore(t, []string{"web-01"}, nil)
	sess := testSession(t, core)

	var mu sync.Mutex
	var reasons []string
	sess.SetOnDetach(func(reason string) {
		mu.Lock()
		reasons = append(reasons, reason)
		mu.Unlock()
	})

	first := attach(t, sess, "web-01")
	second := attach(t, sess, "")

	if !strings.Contains(first.out.String(), leaveScreen) {
		t.Fatal("the replaced client did not receive the postamble")
	}
	mu.Lock()
	got := append([]string(nil), reasons...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "replaced" {
		t.Fatalf("onDetach reasons = %v, want [replaced]", got)
	}
	if !sess.Info().Attached {
		t.Fatal("the session has no client after the second attach")
	}

	// The new terminal is the one the session listens to: a command typed
	// there reaches it, and the program never ended.
	select {
	case <-sess.Done():
		t.Fatal("the session ended when the first terminal was replaced")
	default:
	}
	off := second.out.Len()
	typeKeys(t, second.in, ":hosts\r")
	deadline := time.Now().Add(10 * time.Second)
	for second.out.Len() == off && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if second.out.Len() == off {
		t.Fatal("the attached terminal painted nothing after a command")
	}
}

// TestSessionResizeAfterAttach is requirement 6: a resize reaches the running program
// and the frames after it are drawn at the new width.
func TestSessionResizeAfterAttach(t *testing.T) {
	core := testCore(t, []string{"web-01"}, nil)
	sess := testSession(t, core)
	c := attach(t, sess, "web-01")

	off := c.out.Len()
	sess.Resize(Size{Cols: 80, Rows: 24})
	waitForFrame(t, c.out, off, "an 80-column status line", func(lines []string) bool {
		for _, l := range lines {
			if strings.HasPrefix(l, "jumper") && ansi.StringWidth(l) == 80 {
				return true
			}
		}
		return false
	})
}

// TestSessionStandaloneNoDetachHook is requirement 7: a plain session is
// standalone — nobody can attach back to it — so the root keeps the task-022
// flash and `d` does not throw the terminal away while Run waits for Done.
func TestSessionStandaloneNoDetachHook(t *testing.T) {
	core := testCore(t, []string{"web-01"}, nil)
	sess := testSession(t, core)
	if sess.root.OnDetach != nil {
		t.Fatal("a standalone session installed a detach hook")
	}
}

// TestSessionPrefixDetachCallsHook is requirement 7's other half: the hook runs
// through a tea.Cmd, so `d` unplugs the terminal and the session keeps running.
func TestSessionPrefixDetachCallsHook(t *testing.T) {
	core := testCore(t, []string{"web-01"}, nil)
	sess := testSession(t, core)

	var mu sync.Mutex
	var why []string
	sess.SetOnDetach(func(reason string) {
		mu.Lock()
		why = append(why, reason)
		mu.Unlock()
	})
	var calls int
	sess.SetDetachHook(func() {
		mu.Lock()
		calls++
		mu.Unlock()
		sess.Detach("detached")
	})

	c := attach(t, sess, "web-01")
	typeKeys(t, c.in, "\x01d")

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n > 0 && !sess.Info().Attached {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	n, reasons := calls, append([]string(nil), why...)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("detach hook called %d times, want 1", n)
	}
	if len(reasons) != 1 || reasons[0] != "detached" {
		t.Fatalf("onDetach reasons = %v, want [detached]", reasons)
	}
	if !strings.Contains(c.out.String(), leaveScreen) {
		t.Fatal("the terminal did not receive the postamble")
	}
	select {
	case <-sess.Done():
		t.Fatal("the session ended on a detach")
	default:
	}
}

// TestSessionPromptReachesClient guards the bridge wiring: a question asked from
// outside the UI (an SSH handshake) has to reach the program's dialog, or the
// answer would never be asked for and every dial would time out.
func TestSessionPromptReachesClient(t *testing.T) {
	core := testCore(t, []string{"web-01"}, nil)
	sess := testSession(t, core)
	c := attach(t, sess, "web-01")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		_, _ = core.Prompter.Ask(ctx, interact.Prompt{
			Kind:    interact.Confirm,
			Title:   "bridge sink question",
			Body:    "is a prompt delivered?",
			Choices: []string{"yes", "no"},
		})
	}()
	waitForText(t, c.out, "bridge sink question")
}

// TestUIBridgeQueuesUntilFlush is what makes an early warning visible: the
// bridge holds it until a program's event loop is running, then fans out.
func TestUIBridgeQueuesUntilFlush(t *testing.T) {
	b := newUIBridge()
	var mu sync.Mutex
	var got []string
	sink := func(msg tea.Msg) {
		mu.Lock()
		got = append(got, fmt.Sprint(msg))
		mu.Unlock()
	}

	b.Send("early")
	mu.Lock()
	if len(got) != 0 {
		mu.Unlock()
		t.Fatalf("delivered %v with no sink", got)
	}
	mu.Unlock()

	id := b.AddSink(sink)
	b.Flush()
	b.Send("late")
	b.RemoveSink(id)
	b.Send("nobody")

	b.AddSink(sink)
	b.Flush()
	mu.Lock()
	defer mu.Unlock()
	want := []string{"early", "late", "nobody"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("bridge delivered %v, want %v", got, want)
	}
}

// TestSessionDoneOnQuit is requirement 6: `:q` ends the program and Done is
// closed for whoever is waiting on the session.
func TestSessionDoneOnQuit(t *testing.T) {
	core := testCore(t, []string{"web-01"}, nil)
	sess := testSession(t, core)
	c := attach(t, sess, "web-01")

	typeKeys(t, c.in, ":q\r")
	select {
	case <-sess.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done was not closed after :q")
	}
}

// fakePlanner is a write-capable backend that runs instantly, so a rollout's
// canary and batches only depend on the bus.
type fakePlanner struct{ spec action.Spec }

func (p *fakePlanner) Actions([]resource.Resource) []action.Spec {
	return []action.Spec{p.spec}
}

func (p *fakePlanner) Plan(_ context.Context, req action.Request) (action.Plan, error) {
	steps := make([]action.Step, 0, len(req.Targets))
	for _, tgt := range req.Targets {
		tgt := tgt
		steps = append(steps, action.Step{
			Target:  tgt,
			Display: "fake write on " + string(tgt.Host),
			Run: func(context.Context) action.StepResult {
				return action.StepResult{Target: tgt, RC: 0, Output: "ok"}
			},
		})
	}
	return action.Plan{Spec: p.spec, Request: req, Title: "fake write", Steps: steps}, nil
}

// TestSessionPausedRolloutStaysPausedAcrossDetach: a rollout stops at its canary and
// waits for a human. Detaching the terminal must not cancel it, abort it or
// finish it — it is still paused when the client comes back, and continues from
// there.
func TestSessionPausedRolloutStaysPausedAcrossDetach(t *testing.T) {
	theme.SetProfile("none")
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	for _, d := range []string{home, filepath.Join(home, ".config", "jumper")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	cfg := config.Default()
	cfg.Rollout.Canary = 1
	paths := config.ResolvePaths(home, "")
	env := map[string]string{"USER": "tester"}
	core, err := newCore(context.Background(), cfg, paths, env,
		Options{Demo: true, Home: home, Env: env}, nil)
	if err != nil {
		t.Fatalf("core: %v", err)
	}
	t.Cleanup(func() { _ = core.Close() })
	core.Planners["fake"] = &fakePlanner{spec: action.Spec{ID: "fake.write", Title: "write", Risk: action.RiskNormal}}

	sess := testSession(t, core)
	attach(t, sess, "jumper")

	proposal, err := sess.env.Bus.Propose(context.Background(), action.Request{
		SpecID: "fake.write",
		Targets: []action.Target{
			{Host: "web-01"}, {Host: "web-02"}, {Host: "web-03"}, {Host: "web-04"},
		},
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if proposal.Denied != nil {
		t.Fatalf("policy denied the fake plan: %s", proposal.Denied.Reason)
	}
	run, err := sess.env.Bus.Execute(context.Background(), proposal, bus.Confirmation{ProposalID: proposal.ID})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	events := make(chan bus.Event, 32)
	go func() {
		defer close(events)
		for ev := range run.Events() {
			events <- ev
		}
	}()

	// The canary finishes and the rollout waits for a human.
	waitForPhase(t, events, bus.PhasePaused)

	sess.Detach("detached")
	select {
	case ev := <-events:
		t.Fatalf("the paused rollout emitted %v after the detach", ev.Phase)
	case <-time.After(300 * time.Millisecond):
	}

	// It is still the same run and it finishes when the human says so.
	run.Continue()
	waitForPhase(t, events, bus.PhaseDone)
	select {
	case _, open := <-events:
		if open {
			// Token through the closed channel: the run reported everything.
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run's event channel did not close after Done")
	}
}

// waitForPhase waits until phase appears in events.
func waitForPhase(t *testing.T, events <-chan bus.Event, phase bus.Phase) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("the run ended before %s", phase)
			}
			if ev.Phase == phase {
				return
			}
		case <-deadline:
			t.Fatalf("the run never reached %s", phase)
		}
	}
}

// TestSessionTitleOnAttach is requirement 4's third clause: the attach preamble
// names the session in the terminal's title bar, ESC ] 2 ; jumper: <session> BEL,
// after the alt-screen preamble that existing tests already match.
func TestSessionTitleOnAttach(t *testing.T) {
	core := testCore(t, []string{"web-01"}, nil)
	sess := testSession(t, core)
	c := attach(t, sess, "web-01")

	want := "\x1b]2;jumper: " + sess.Name() + "\a"
	if !strings.Contains(c.out.String(), want) {
		t.Fatalf("attach output has no title sequence %q", want)
	}
	if !strings.Contains(c.out.String(), enterScreen) {
		t.Fatal("attach output has no alt-screen preamble")
	}
}

// TestTmuxPrefixOnAttach is requirement 4's first clause: a client started
// inside tmux gets mux.prefix_in_tmux, and the status line shows it.
func TestTmuxPrefixOnAttach(t *testing.T) {
	core := testCore(t, []string{"web-01"}, nil)
	sess := testSession(t, core)

	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	out := &syncBuffer{}
	if err := sess.Attach(pr, out, Size{Cols: 120, Rows: 36}, map[string]string{
		"TERM": "xterm-256color",
		"TMUX": "/tmp/tmux-501/default,1,0",
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	waitForText(t, out, "web-01")
	// Default config: prefix C-a, prefix_in_tmux C-b.
	want := tui.PrefixDisplay("ctrl+" + strings.TrimPrefix(core.cfg.Mux.PrefixInTmux, "C-"))
	if want == "" {
		t.Fatal("no prefix display")
	}
	waitForText(t, out, want)
}

// TestTmuxClipboardPassthrough is requirement 4's second clause: a session
// started inside tmux writes OSC 52 in the tmux passthrough form as well as
// trying pbcopy. An empty PATH makes pbcopy fail, so the OSC 52 fallback is the
// one that has to land.
func TestTmuxClipboardPassthrough(t *testing.T) {
	t.Setenv("PATH", "")
	out := &syncBuffer{}
	c := clipboardFactory()(out, true)
	if err := c.Copy("hi"); err != nil {
		t.Fatalf("copy: %v", err)
	}
	want := "\x1bPtmux;\x1b\x1b]52;c;aGk=\a\x1b\\"
	if got := out.String(); !strings.Contains(got, want) {
		t.Fatalf("clipboard wrote %q, want the tmux passthrough %q", got, want)
	}
}

// TestSessionEnvConstant is requirement 5: the marker a local pane exports is
// defined here, and InSession is the check the nested-attach refusal uses.
func TestSessionEnvConstant(t *testing.T) {
	if SessionEnvVar != "SITREP" {
		t.Fatalf("SessionEnvVar = %q, want SITREP", SessionEnvVar)
	}
	if InSession(nil) || InSession(map[string]string{"OTHER": "main"}) {
		t.Fatal("InSession accepted an environment without the marker")
	}
	if !InSession(map[string]string{SessionEnvVar: "main"}) {
		t.Fatal("InSession ignored the marker")
	}
}
