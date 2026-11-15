package tui

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

	"jumpr/internal/action"
	"jumpr/internal/bus"
	"jumpr/internal/inventory"
	"jumpr/internal/investigation"
	"jumpr/internal/mux"
	"jumpr/internal/mux/termpane"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
)

// Task 064: the broadcast line, raw synchronized input and the shell guard
// rails. The tests drive the root model the way the Bubble Tea runtime does —
// every command in its own goroutine, every message back through Update — so
// the flows they exercise are the flows a terminal gets.

// syncOpener is a shell opener whose shells only record what is written to
// them. Read blocks until the test is over, so no pane ever sees remote output.
type syncOpener struct {
	mu     sync.Mutex
	writes []shellWrite
	done   chan struct{}
	once   sync.Once
}

// shellWrite is one write a shell received, in arrival order.
type shellWrite struct {
	Host string
	Data string
}

func newSyncOpener() *syncOpener { return &syncOpener{done: make(chan struct{})} }

// OpenShell returns a recording shell for host.
func (o *syncOpener) OpenShell(ctx context.Context, host resource.HostID, req transport.PTYRequest) (transport.Shell, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &syncShell{opener: o, host: host}, nil
}

// finish releases every blocked Read.
func (o *syncOpener) finish() { o.once.Do(func() { close(o.done) }) }

func (o *syncOpener) record(host resource.HostID, b []byte) {
	o.mu.Lock()
	o.writes = append(o.writes, shellWrite{Host: string(host), Data: string(b)})
	o.mu.Unlock()
}

// all is every write in arrival order.
func (o *syncOpener) all() []shellWrite {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]shellWrite(nil), o.writes...)
}

// written is everything host received, concatenated.
func (o *syncOpener) written(host string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var b strings.Builder
	for _, w := range o.writes {
		if w.Host == host {
			b.WriteString(w.Data)
		}
	}
	return b.String()
}

// syncShell is one recording shell.
type syncShell struct {
	opener *syncOpener
	host   resource.HostID
}

func (s *syncShell) Read(p []byte) (int, error) {
	<-s.opener.done
	return 0, io.EOF
}

func (s *syncShell) Write(p []byte) (int, error) {
	s.opener.record(s.host, p)
	return len(p), nil
}

func (s *syncShell) Resize(cols, rows int) error { return nil }
func (s *syncShell) Close() error                { return nil }
func (s *syncShell) Wait() error                 { return nil }

// syncRecorder is the investigation log and the bus recorder at once: it keeps
// the entries the guard rails write and the rollouts the bus ran.
type syncRecorder struct {
	mu      sync.Mutex
	entries []investigation.Entry
	actions []bus.Proposal
	denied  []bus.Proposal
}

var (
	_ investigation.Log = (*syncRecorder)(nil)
	_ bus.Recorder      = (*syncRecorder)(nil)
)

func (r *syncRecorder) Append(e investigation.Entry) error {
	r.mu.Lock()
	r.entries = append(r.entries, e)
	r.mu.Unlock()
	return nil
}

func (r *syncRecorder) Current() (investigation.Meta, bool) { return investigation.Meta{}, false }

func (r *syncRecorder) RecordAction(p bus.Proposal, s bus.Summary, results []action.StepResult) error {
	r.mu.Lock()
	r.actions = append(r.actions, p)
	r.mu.Unlock()
	return nil
}

func (r *syncRecorder) RecordDenied(p bus.Proposal) error {
	r.mu.Lock()
	r.denied = append(r.denied, p)
	r.mu.Unlock()
	return nil
}

// inputs are the texts of the `input` entries, in order.
func (r *syncRecorder) inputs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.entries {
		if e.Kind == investigation.EntryInput {
			out = append(out, e.Text)
		}
	}
	return out
}

// recordedSpecs are the spec IDs of the rollouts that ran, in order.
func (r *syncRecorder) recordedSpecs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.actions))
	for _, p := range r.actions {
		out = append(out, p.Plan.Spec.ID)
	}
	return out
}

// deniedSpecs are the spec IDs the bus denied, in order.
func (r *syncRecorder) deniedSpecs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.denied))
	for _, p := range r.denied {
		out = append(out, p.Plan.Spec.ID)
	}
	return out
}

// testClock is the injected clock, so the idle timer can be reached without
// waiting five minutes.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// ackMsg asks the harness to confirm that everything queued before it was
// processed.
type ackMsg struct{ done chan struct{} }

// doMsg runs a closure in the model's goroutine, so a test never reads a field
// Update may be writing.
type doMsg struct {
	fn   func()
	done chan struct{}
}

// syncHarness runs a root model the way the Bubble Tea runtime does.
type syncHarness struct {
	root  *Root
	op    *syncOpener
	rec   *syncRecorder
	clock *testClock
	bus   bus.Bus

	in   chan tea.Msg
	stop chan struct{}
	done chan struct{}
}

// newSyncHarness builds a root with panes terminal panes already connected.
func newSyncHarness(t *testing.T, panes int) *syncHarness {
	t.Helper()
	theme.SetProfile("none")
	op := newSyncOpener()
	rec := &syncRecorder{}
	clock := newTestClock()
	policy := bus.DefaultPolicy()
	b := bus.New(bus.Options{
		Policy:   policy,
		Rollout:  actionRollout(100),
		Recorder: rec,
		Now:      clock.Now,
	})
	h := newSyncHarnessFor(t, op, rec, clock, policy, b)
	for i := 1; i <= panes; i++ {
		h.addPane(t, fmt.Sprintf("web-%02d", i))
	}
	h.until(t, "terminal panes connected", func() bool {
		return len(h.root.connectedTerminals()) == panes
	})
	return h
}

// newSyncHarnessFor wires a root over the given opener, recorder, clock and
// bus. It is the common part of every harness in this file.
func newSyncHarnessFor(t *testing.T, op *syncOpener, rec *syncRecorder, clock *testClock, policy *bus.Policy, b bus.Bus) *syncHarness {
	t.Helper()
	env := &Env{
		Session:   &SessionState{Name: "main"},
		Theme:     theme.Dark(),
		Keys:      keymap.Default(),
		Now:       clock.Now,
		Shells:    op,
		Bus:       b,
		Recorder:  rec,
		Factories: map[resource.KindName]Factory{},
	}
	cmds := NewCommands()
	RegisterBuiltins(cmds)
	if err := RegisterMuxCommands(cmds); err != nil {
		t.Fatalf("register mux commands: %v", err)
	}
	root := NewRoot(env, cmds, nil, resource.Query{Kind: "host"}, "ctrl+a")
	root.SetPolicy(policy)
	h := &syncHarness{root: root, op: op, rec: rec, clock: clock, bus: b}
	h.start(t)
	t.Cleanup(h.shutdown)
	return h
}

// start launches the model's message loop and its first commands.
func (h *syncHarness) start(t *testing.T) {
	h.in = make(chan tea.Msg, 512)
	h.stop = make(chan struct{})
	h.done = make(chan struct{})
	go h.loop()
	h.launch(h.root.Init())
	h.send(t, tea.WindowSizeMsg{Width: 120, Height: 36})
}

// shutdown stops the loop and the panes. It runs from t.Cleanup, after the
// test's own assertions.
func (h *syncHarness) shutdown() {
	select {
	case <-h.stop:
		return
	default:
	}
	h.do(h.root.mux.CloseAll)
	close(h.stop)
	<-h.done
	h.op.finish()
}

func (h *syncHarness) loop() {
	defer close(h.done)
	for {
		select {
		case <-h.stop:
			return
		case msg := <-h.in:
			h.dispatch(msg)
		}
	}
}

func (h *syncHarness) dispatch(msg tea.Msg) {
	switch m := msg.(type) {
	case nil:
	case ackMsg:
		close(m.done)
	case doMsg:
		m.fn()
		close(m.done)
	case tea.QuitMsg:
		// The test decides when the model stops.
	default:
		_, cmd := h.root.Update(msg)
		h.launch(cmd)
	}
}

func (h *syncHarness) launch(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() { h.deliver(cmd()) }()
}

func (h *syncHarness) deliver(msg tea.Msg) {
	switch m := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range m {
			h.launch(c)
		}
	default:
		select {
		case h.in <- msg:
		case <-h.stop:
		}
	}
}

// push queues one message.
func (h *syncHarness) push(t *testing.T, msg tea.Msg) {
	t.Helper()
	select {
	case h.in <- msg:
	case <-time.After(5 * time.Second):
		t.Fatal("sync harness is stuck")
	}
}

// send queues a message and waits until the model processed it.
func (h *syncHarness) send(t *testing.T, msg tea.Msg) {
	t.Helper()
	h.push(t, msg)
	h.settle(t)
}

// settle waits until everything queued so far was processed.
func (h *syncHarness) settle(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	select {
	case h.in <- ackMsg{done: done}:
	case <-time.After(5 * time.Second):
		t.Fatal("sync harness is stuck")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sync harness did not settle")
	}
}

// do runs fn in the model's goroutine, after everything queued so far.
func (h *syncHarness) do(fn func()) {
	done := make(chan struct{})
	select {
	case h.in <- doMsg{fn: fn, done: done}:
	case <-time.After(5 * time.Second):
		panic("sync harness is stuck")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		panic("sync harness is stuck")
	}
}

// until waits for cond, evaluated in the model's goroutine, to hold.
func (h *syncHarness) until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ok := false
		h.do(func() { ok = cond() })
		if ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// addPane adds a connected terminal pane for host.
func (h *syncHarness) addPane(t *testing.T, host string) {
	t.Helper()
	h.do(func() {
		// connectTerm tells the pane factory this pane is a fresh connection,
		// not one restored from a session file.
		h.root.connectTerm = true
		_, cmd := h.root.mux.AddPane(mux.ContentState{Type: "term", Host: host}, "h")
		h.root.connectTerm = false
		h.launch(cmd)
	})
	h.settle(t)
}

// key sends one key press by name.
func (h *syncHarness) key(t *testing.T, name string) {
	t.Helper()
	h.send(t, syncKeyMsg(name))
}

// text types text one rune at a time.
func (h *syncHarness) text(t *testing.T, s string) {
	t.Helper()
	for _, r := range s {
		h.send(t, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// command runs a `:` command line from a terminal pane, where the prefix is the
// way to the command bar.
func (h *syncHarness) command(t *testing.T, line string) {
	t.Helper()
	h.key(t, "ctrl+a")
	h.key(t, ":")
	h.text(t, line)
	h.key(t, "enter")
	h.settle(t)
}

// screen is the model's view, rendered in the model's goroutine.
func (h *syncHarness) screen() string {
	var out string
	h.do(func() { out = h.root.View() })
	return out
}

// dialog is the dialog on screen, read in the model's goroutine.
func (h *syncHarness) dialog() Dialog {
	var d Dialog
	h.do(func() { d = h.root.dialog })
	return d
}

// syncKeyMsg turns a key name into the message Bubble Tea would send.
func syncKeyMsg(name string) tea.KeyMsg {
	switch name {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+a":
		return tea.KeyMsg{Type: tea.KeyCtrlA}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(name)}
}

// enableRawSync turns raw synchronized input on through the confirmed plan.
func (h *syncHarness) enableRawSync(t *testing.T) {
	t.Helper()
	h.command(t, "sync raw")
	plan := h.planDialog(t)
	if plan.p.Plan.Spec.ID != SpecSyncRaw {
		t.Fatalf("plan spec = %q, want %q", plan.p.Plan.Spec.ID, SpecSyncRaw)
	}
	h.key(t, "y")
	h.until(t, "raw sync on", func() bool { return h.root.mux.SyncOn() })
	h.until(t, "the mode rollout to finish", func() bool {
		d, ok := h.root.dialog.(*runDialog)
		return ok && d.done
	})
	// The record stays on screen until it is dismissed; the mode does not.
	h.key(t, "enter")
	h.until(t, "the run dialog to close", func() bool { return h.root.dialog == nil })
}

// planDialog is the plan dialog on screen.
func (h *syncHarness) planDialog(t *testing.T) *planDialog {
	t.Helper()
	h.until(t, "the plan dialog", func() bool {
		_, ok := h.root.dialog.(*planDialog)
		return ok
	})
	d, ok := h.dialog().(*planDialog)
	if !ok {
		t.Fatalf("dialog = %T, want the plan dialog", h.dialog())
	}
	return d
}

// TestBroadcastLinePlansAndSends checks requirement 2: `prefix B` collects one
// line, the plan goes through the bus with the focused pane as the canary, and
// every shell of the window receives the line and a carriage return.
func TestBroadcastLinePlansAndSends(t *testing.T) {
	h := newSyncHarness(t, 3)

	h.key(t, "ctrl+a")
	h.key(t, "B")
	h.until(t, "the broadcast prompt", func() bool {
		_, ok := h.root.dialog.(*promptDialog)
		return ok
	})
	if v := h.dialog().View(60, 6); !strings.Contains(v, "broadcast to 3 panes") {
		t.Fatalf("prompt does not name the pane count:\n%s", v)
	}

	h.text(t, "uptime")
	h.key(t, "enter")

	plan := h.planDialog(t)
	if plan.p.Plan.Spec.ID != SpecSyncLine {
		t.Fatalf("plan spec = %q, want %q", plan.p.Plan.Spec.ID, SpecSyncLine)
	}
	if len(plan.p.Plan.Steps) != 3 {
		t.Fatalf("plan has %d steps, want one per pane", len(plan.p.Plan.Steps))
	}
	if got := plan.p.Plan.Steps[0].Display; got != "uptime" {
		t.Fatalf("step display = %q, want the line", got)
	}

	h.key(t, "y")
	h.until(t, "the canary pause", func() bool {
		d, ok := h.root.dialog.(*runDialog)
		return ok && d.phase == bus.PhasePaused
	})
	h.key(t, "c")
	h.until(t, "the rollout to finish", func() bool {
		d, ok := h.root.dialog.(*runDialog)
		return ok && d.done
	})

	for _, host := range []string{"web-01", "web-02", "web-03"} {
		if got := h.op.written(host); got != "uptime\r" {
			t.Fatalf("%s received %q, want %q", host, got, "uptime\r")
		}
	}
	writes := h.op.all()
	if len(writes) == 0 || writes[0].Host != "web-03" {
		t.Fatalf("writes = %+v, want the focused pane (web-03) first as the canary", writes)
	}
	if got := h.rec.recordedSpecs(); len(got) != 1 || got[0] != SpecSyncLine {
		t.Fatalf("recorded specs = %v, want [%s]", got, SpecSyncLine)
	}
}

// TestBroadcastDeniedLine checks requirement 2: a denied line never reaches a
// pane, the plan says why, and the denial is recorded.
func TestBroadcastDeniedLine(t *testing.T) {
	h := newSyncHarness(t, 3)

	h.key(t, "ctrl+a")
	h.key(t, "B")
	h.text(t, "systemctl stop sshd")
	h.key(t, "enter")

	plan := h.planDialog(t)
	if plan.p.Denied == nil {
		t.Fatalf("plan was not denied: %+v", plan.p)
	}
	if !strings.Contains(plan.p.Denied.Rule, "sshd") {
		t.Fatalf("denial rule = %q, want the built-in sshd rule", plan.p.Denied.Rule)
	}
	if v := plan.View(70, 20); !strings.Contains(v, "denied by policy") {
		t.Fatalf("plan dialog does not say it is denied:\n%s", v)
	}
	for _, w := range h.op.all() {
		if strings.Contains(w.Data, "systemctl") {
			t.Fatalf("%s received the denied line: %q", w.Host, w.Data)
		}
	}
	if got := h.rec.deniedSpecs(); len(got) != 1 || got[0] != SpecSyncLine {
		t.Fatalf("recorded denials = %v, want [%s]", got, SpecSyncLine)
	}
	if got := h.rec.recordedSpecs(); len(got) != 0 {
		t.Fatalf("a denied line ran a rollout: %v", got)
	}
}

// TestRawSyncRequiresConfirm checks requirement 3: turning the mode on is a
// planned action, the warning says what the deny rules are worth, and the mode
// is only on after the confirmation.
func TestRawSyncRequiresConfirm(t *testing.T) {
	h := newSyncHarness(t, 3)

	h.command(t, "sync raw")
	plan := h.planDialog(t)
	if plan.p.Plan.Spec.ID != SpecSyncRaw {
		t.Fatalf("plan spec = %q, want %q", plan.p.Plan.Spec.ID, SpecSyncRaw)
	}
	if len(plan.p.Plan.Steps) != 1 {
		t.Fatalf("plan has %d steps, want the single mode step", len(plan.p.Plan.Steps))
	}
	var warning string
	for _, w := range plan.p.Plan.Warnings {
		if strings.Contains(w, "seat belt") {
			warning = w
		}
	}
	if warning != syncRawWarning {
		t.Fatalf("plan warnings = %v, want the seat-belt warning", plan.p.Plan.Warnings)
	}
	if v := plan.View(90, 24); !strings.Contains(v, "seat belt, not a boundary") {
		t.Fatalf("plan dialog does not state the limit:\n%s", v)
	}
	if h.rootSyncOn() {
		t.Fatal("the mode is on before the plan was confirmed")
	}

	h.key(t, "y")
	h.until(t, "raw sync on", func() bool { return h.root.mux.SyncOn() })
	if n := h.syncCount(); n != 3 {
		t.Fatalf("sync covers %d panes, want 3", n)
	}
	var status string
	h.do(func() {
		status = RenderStatus(h.root.env.Theme, h.root.statusData(), 120)
	})
	if !strings.Contains(status, "SYNC ×3") {
		t.Fatalf("status line has no SYNC ×3 badge:\n%s", status)
	}
	if got := h.rec.recordedSpecs(); len(got) != 1 || got[0] != SpecSyncRaw {
		t.Fatalf("recorded specs = %v, want [%s]", got, SpecSyncRaw)
	}
}

// rootSyncOn and syncCount read the mux in the model's goroutine.
func (h *syncHarness) rootSyncOn() bool {
	var on bool
	h.do(func() { on = h.root.mux.SyncOn() })
	return on
}

func (h *syncHarness) syncCount() int {
	var n int
	h.do(func() { n = h.root.mux.SyncCount() })
	return n
}

// terminals is how many terminal panes of the active window are connected.
func (h *syncHarness) terminals() int {
	var n int
	h.do(func() { n = len(h.root.connectedTerminals()) })
	return n
}

// opWritten is everything a host's shell received, in order.
func (h *syncHarness) opWritten(host string) string { return h.op.written(host) }

// TestRawSyncFansOutKeys checks requirement 3: every key goes to all panes, and
// the finished line is recorded.
func TestRawSyncFansOutKeys(t *testing.T) {
	h := newSyncHarness(t, 3)
	h.enableRawSync(t)

	h.text(t, "ls")
	h.key(t, "enter")
	h.until(t, "the line to be recorded", func() bool {
		for _, text := range h.rec.inputs() {
			if text == "sync ×3: ls" {
				return true
			}
		}
		return false
	})

	for _, host := range []string{"web-01", "web-02", "web-03"} {
		if got := h.op.written(host); got != "ls\r" {
			t.Fatalf("%s received %q, want %q", host, got, "ls\r")
		}
	}
}

// TestRawSyncBlocksDeniedEnter checks requirement 3: a denied line does not get
// its enter, the panes are cleared with ctrl+u instead, and the rule is shown.
func TestRawSyncBlocksDeniedEnter(t *testing.T) {
	h := newSyncHarness(t, 3)
	h.enableRawSync(t)

	h.text(t, "systemctl stop sshd")
	h.key(t, "enter")
	h.until(t, "the denial flash", func() bool {
		return h.root.flash.Level == FlashError && strings.Contains(h.root.flash.Text, "sshd")
	})

	for _, host := range []string{"web-01", "web-02", "web-03"} {
		got := h.op.written(host)
		if strings.Contains(got, "\r") {
			t.Fatalf("%s received the denied enter: %q", host, got)
		}
		if !strings.HasSuffix(got, "\x15") {
			t.Fatalf("%s was not cleared with ctrl+u: %q", host, got)
		}
	}
	for _, text := range h.rec.inputs() {
		if strings.Contains(text, "systemctl") {
			t.Fatalf("the denied line was recorded: %q", text)
		}
	}
}

// TestRawSyncAutoOff checks requirement 3: five minutes without a key turns the
// mode off.
func TestRawSyncAutoOff(t *testing.T) {
	h := newSyncHarness(t, 3)
	h.enableRawSync(t)

	h.text(t, "l")
	h.clock.Advance(syncIdleTimeout + time.Second)
	h.send(t, syncIdleMsg{})

	if h.rootSyncOn() {
		t.Fatal("the mode is still on after five idle minutes")
	}
	if screen := h.screen(); strings.Contains(screen, "SYNC") {
		t.Fatalf("the status line still shows the badge:\n%s", screen)
	}
	// Keys go back to the focused pane alone.
	h.text(t, "p")
	h.until(t, "the key to reach the focused pane", func() bool {
		return h.opWritten("web-03") == "lp"
	})
	if got := h.opWritten("web-01"); got != "l" {
		t.Fatalf("web-01 received %q after the mode ended, want %q", got, "l")
	}
	if got := h.opWritten("web-02"); got != "l" {
		t.Fatalf("web-02 received %q after the mode ended, want %q", got, "l")
	}
}

// TestRawSyncRefusedOverLimit checks requirement 3: more panes than
// mux.max_sync_panes is refused before anything is planned.
func TestRawSyncRefusedOverLimit(t *testing.T) {
	h := newSyncHarness(t, 3)
	h.do(func() { h.root.SetMaxSyncPanes(2) })

	h.command(t, "sync raw")
	h.until(t, "the refusal flash", func() bool {
		return strings.Contains(h.root.flash.Text, "limited to 2 panes")
	})
	if d := h.dialog(); d != nil {
		t.Fatalf("a refused mode still opened %T", d)
	}
	if h.rootSyncOn() {
		t.Fatal("raw sync is on after the refusal")
	}
	if got := h.rec.recordedSpecs(); len(got) != 0 {
		t.Fatalf("a refused mode ran a rollout: %v", got)
	}
}

// TestShellGuardOpenRecorded checks requirement 1: opening a terminal pane is an
// `input` entry.
func TestShellGuardOpenRecorded(t *testing.T) {
	h := newSyncHarness(t, 0)
	h.command(t, "ssh web-07")

	h.until(t, "the shell entry", func() bool {
		for _, text := range h.rec.inputs() {
			if text == "shell opened on web-07" {
				return true
			}
		}
		return false
	})
	if got := h.terminals(); got != 1 {
		t.Fatalf("connected terminals = %d, want the new pane", got)
	}
}

// TestShellGuardProtectedTitle checks requirement 1: a pane on a protected host
// says so in its title.
func TestShellGuardProtectedTitle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte("protected: [\"role=db\"]\n"), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	policy, err := bus.LoadPolicy(path)
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}

	op := newSyncOpener()
	rec := &syncRecorder{}
	clock := newTestClock()
	b := bus.New(bus.Options{Policy: policy, Rollout: actionRollout(100), Now: clock.Now})
	h := newSyncHarnessFor(t, op, rec, clock, policy, b)
	h.do(func() {
		h.root.env.Inventory = fakeInventory{hosts: map[string]inventory.Host{
			"db-01":  {ID: "db-01", Labels: map[string]string{"host": "db-01", "role": "db", "protected": "true"}},
			"web-01": {ID: "web-01", Labels: map[string]string{"host": "web-01", "role": "web"}},
		}}
	})

	h.command(t, "ssh db-01")
	h.until(t, "the protected pane", func() bool {
		targets := h.root.connectedTerminals()
		return len(targets) == 1 && strings.Contains(h.root.mux.View(), "protected")
	})
	var title string
	h.do(func() { title = h.root.connectedTerminals()[0].Pane.(*termpane.Pane).Title() })
	if !strings.Contains(title, "protected") {
		t.Fatalf("pane title = %q, want it to say protected", title)
	}
}

// TestShellGuardSSHRefusedInReadOnly checks requirement 1: a read-only session does not
// open shells unless policy allows them.
func TestShellGuardSSHRefusedInReadOnly(t *testing.T) {
	h := newSyncHarness(t, 0)
	h.do(func() { h.root.env.Session.ReadOnly = true })

	h.command(t, "ssh web-07")
	h.until(t, "the read-only refusal", func() bool {
		return strings.Contains(h.root.flash.Text, "read-only mode")
	})
	if got := h.terminals(); got != 0 {
		t.Fatalf("a read-only session opened %d shells", got)
	}
	for _, text := range h.rec.inputs() {
		if strings.Contains(text, "shell opened") {
			t.Fatalf("the refused shell was recorded: %q", text)
		}
	}
}

// fakeInventory is an inventory with fixed hosts and labels.
type fakeInventory struct {
	hosts map[string]inventory.Host
}

func (f fakeInventory) Hosts() []inventory.Host {
	out := make([]inventory.Host, 0, len(f.hosts))
	for _, h := range f.hosts {
		out = append(out, h)
	}
	return out
}

func (f fakeInventory) Select(sel resource.Selector) []inventory.Host {
	var out []inventory.Host
	for _, h := range f.hosts {
		if sel.Matches(h.Labels) {
			out = append(out, h)
		}
	}
	return out
}

func (f fakeInventory) Resolve(nameOrAlias string) (inventory.Host, bool) {
	h, ok := f.hosts[nameOrAlias]
	return h, ok
}

func (f fakeInventory) Refresh(ctx context.Context) map[string]error { return nil }

func (f fakeInventory) Age() time.Duration { return 0 }
