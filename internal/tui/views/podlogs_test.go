package views

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"jumpr/internal/backend/k8s"
	"jumpr/internal/errs"
	"jumpr/internal/investigation"
	"jumpr/internal/redact"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
)

// podLogsCall is one recorded log request.
type podLogsCall struct {
	ns, pod string
	o       k8s.LogOptions
}

// workloadCall is one recorded workload log request.
type workloadCall struct {
	kind     resource.KindName
	ns, name string
	o        k8s.LogOptions
}

// fakePodLogs is a scripted podLogsSource.
type fakePodLogs struct {
	info    k8s.PodInfo
	infoErr error
	lines   []k8s.LogLine
	logErr  error
	// hold keeps a follow stream open until its context is canceled.
	hold bool

	mu            sync.Mutex
	calls         []podLogsCall
	workloadCalls []workloadCall
	// canceled receives one value per cancelled stream, so a test can see a
	// follow was stopped.
	canceled chan struct{}
}

func (f *fakePodLogs) PodInfo(context.Context, string, string) (k8s.PodInfo, error) {
	return f.info, f.infoErr
}

func (f *fakePodLogs) Logs(ctx context.Context, ns, pod string, o k8s.LogOptions) (<-chan k8s.LogLine, error) {
	f.mu.Lock()
	f.calls = append(f.calls, podLogsCall{ns: ns, pod: pod, o: o})
	err := f.logErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.stream(ctx, o)
}

func (f *fakePodLogs) WorkloadLogs(ctx context.Context, kind resource.KindName, ns, name string, o k8s.LogOptions) (<-chan k8s.LogLine, error) {
	f.mu.Lock()
	f.workloadCalls = append(f.workloadCalls, workloadCall{kind: kind, ns: ns, name: name, o: o})
	err := f.logErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.stream(ctx, o)
}

// stream buffers the scripted lines; a follow stream stays open until ctx ends.
func (f *fakePodLogs) stream(ctx context.Context, o k8s.LogOptions) (<-chan k8s.LogLine, error) {
	ch := make(chan k8s.LogLine, len(f.lines)+1)
	for _, l := range f.lines {
		ch <- l
	}
	if o.Follow && f.hold {
		go func() {
			<-ctx.Done()
			if f.canceled != nil {
				f.canceled <- struct{}{}
			}
			close(ch)
		}()
		return ch, nil
	}
	close(ch)
	return ch, nil
}

// lastCall is the most recent log request.
func (f *fakePodLogs) lastCall(t *testing.T) podLogsCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("no log request was made")
	}
	return f.calls[len(f.calls)-1]
}

// lastWorkload is the most recent workload request.
func (f *fakePodLogs) lastWorkload(t *testing.T) workloadCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.workloadCalls) == 0 {
		t.Fatal("no workload log request was made")
	}
	return f.workloadCalls[len(f.workloadCalls)-1]
}

// podLogsEnv is the environment of the screen: an open investigation and a
// recorder when wanted.
func podLogsEnv(rec investigation.Log) *tui.Env {
	env := testEnv()
	env.Recorder = rec
	return env
}

// newPodLogs builds a pod screen over f for ns/pod.
func newPodLogs(t *testing.T, env *tui.Env, f *fakePodLogs, pod string) *PodLogs {
	t.Helper()
	return NewPodLogs(env, f, resource.Query{
		Kind: "pod",
		Args: map[string]string{"namespace": "prod", "pod": pod},
	})
}

// podLogsUpdate sends any message and returns the screen Update produced.
func podLogsUpdate(t *testing.T, e *PodLogs, msg tea.Msg) (*PodLogs, tea.Cmd) {
	t.Helper()
	view, cmd := e.Update(msg)
	next, ok := view.(*PodLogs)
	if !ok {
		t.Fatalf("Update returned %T, want *PodLogs", view)
	}
	return next, cmd
}

// podLogsPress sends one key.
func podLogsPress(t *testing.T, e *PodLogs, msg tea.KeyMsg) (*PodLogs, tea.Cmd) {
	t.Helper()
	return podLogsUpdate(t, e, msg)
}

// pumpPodLogs runs commands until the view settles: a closed stream, an error
// or a batch that asked for nothing more.
func pumpPodLogs(t *testing.T, e *PodLogs, cmd tea.Cmd) *PodLogs {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg := c()
		if msg == nil {
			continue
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}
		var next tea.Cmd
		e, next = podLogsUpdate(t, e, msg)
		queue = append(queue, next)
	}
	return e
}

// startPodLogs runs the start chain until the stream is open and returns the
// pending next-batch command unexecuted, so a follow does not block the test.
func startPodLogs(t *testing.T, e *PodLogs, cmd tea.Cmd) (*PodLogs, tea.Cmd) {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg := c()
		if msg == nil {
			continue
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}
		var next tea.Cmd
		e, next = podLogsUpdate(t, e, msg)
		if e.ch != nil {
			return e, next
		}
		queue = append(queue, next)
	}
	t.Fatal("the stream never started")
	return e, nil
}

// logLine is one scripted line.
func logLine(pod, container, text string) k8s.LogLine {
	return k8s.LogLine{
		Time:      fixedNow.Add(-time.Minute),
		Pod:       pod,
		Container: container,
		Text:      text,
	}
}

// TestPodLogsLoadsAndColours checks a load appends the lines, the banner names
// the target and the level word is coloured.
func TestPodLogsLoadsAndColours(t *testing.T) {
	theme.SetProfile("256")
	defer theme.SetProfile("none")
	env := &tui.Env{Theme: theme.Dark(), Keys: keymap.Default(), Now: func() time.Time { return fixedNow }}
	f := &fakePodLogs{
		info: k8s.PodInfo{Containers: []string{"app"}},
		lines: []k8s.LogLine{
			logLine("web-1", "app", "ERROR boom: upstream timed out"),
			logLine("web-1", "app", "INFO listening on :8080"),
		},
	}
	e := newPodLogs(t, env, f, "web-1")
	e = pumpPodLogs(t, e, e.Init())

	if e.lines != 2 {
		t.Fatalf("lines = %d, want 2", e.lines)
	}
	if len(e.Text.lines) != 2 {
		t.Fatalf("text lines = %d, want 2", len(e.Text.lines))
	}
	if plain := ansi.Strip(e.Text.lines[0]); plain != "ERROR boom: upstream timed out" {
		t.Errorf("first line = %q", plain)
	}
	if !strings.Contains(e.Text.lines[0], "\x1b[") {
		t.Error("the ERROR word is not coloured")
	}
	if got := ansi.Strip(e.Text.lines[1]); got != "INFO listening on :8080" {
		t.Errorf("second line = %q", got)
	}
	if !strings.Contains(e.Text.lines[1], "\x1b[") {
		t.Error("the INFO word is not coloured")
	}

	out := ansi.Strip(e.Render(100, 8))
	for _, want := range []string{"logs prod/web-1", "container app", "2 lines"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	if e.Crumb() != "logs prod/web-1" {
		t.Errorf("crumb = %q", e.Crumb())
	}
	if e.Subject().Ref.Name != "web-1" || e.Subject().Ref.Namespace != "prod" {
		t.Errorf("subject = %+v", e.Subject())
	}
}

// TestPodLogsCrashLoopSelectsPrevious checks a crash-looping pod reads previous
// logs, and that `p` can turn that off.
func TestPodLogsCrashLoopSelectsPrevious(t *testing.T) {
	f := &fakePodLogs{
		info:  k8s.PodInfo{Containers: []string{"app"}, CrashLoop: true},
		lines: []k8s.LogLine{logLine("web-1", "app", "FATAL crash")},
	}
	e := newPodLogs(t, podLogsEnv(nil), f, "web-1")
	e = pumpPodLogs(t, e, e.Init())

	if !e.o.Previous {
		t.Error("previous was not selected for a crash-looping pod")
	}
	if call := f.lastCall(t); !call.o.Previous {
		t.Error("the request did not ask for previous logs")
	}
	if !strings.Contains(e.Render(100, 6), "previous") {
		t.Error("the banner does not say previous")
	}

	e, cmd := podLogsPress(t, e, runeKey("p"))
	e = pumpPodLogs(t, e, cmd)
	if e.o.Previous {
		t.Error("p did not turn previous off")
	}
	if call := f.lastCall(t); call.o.Previous {
		t.Error("the request still asks for previous logs")
	}
}

// TestPodLogsFollowStartsStream checks `w` reopens the stream with follow and
// Close cancels it.
func TestPodLogsFollowStartsStream(t *testing.T) {
	f := &fakePodLogs{
		info:     k8s.PodInfo{Containers: []string{"app"}},
		lines:    []k8s.LogLine{logLine("web-1", "app", "INFO first")},
		hold:     true,
		canceled: make(chan struct{}, 4),
	}
	e := newPodLogs(t, podLogsEnv(nil), f, "web-1")
	e = pumpPodLogs(t, e, e.Init())

	e, cmd := podLogsPress(t, e, runeKey("w"))
	e, _ = startPodLogs(t, e, cmd)

	if !e.o.Follow {
		t.Fatal("w did not turn follow on")
	}
	if !e.Text.follow {
		t.Error("the text view does not pin to the bottom while following")
	}
	if call := f.lastCall(t); !call.o.Follow {
		t.Error("the request did not ask to follow")
	}

	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-f.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel the follow stream")
	}

	// The next batch of the canceled stream is ignored.
	if _, cmd := podLogsUpdate(t, e, podLogsBatchMsg{seq: 0, closed: true}); cmd != nil {
		t.Error("a batch from an old stream produced a command")
	}
}

// TestPodLogsCycleContainer checks `c` moves to the pod's next container.
func TestPodLogsCycleContainer(t *testing.T) {
	f := &fakePodLogs{
		info:  k8s.PodInfo{Containers: []string{"app", "sidecar"}},
		lines: []k8s.LogLine{logLine("web-1", "app", "INFO one")},
	}
	e := newPodLogs(t, podLogsEnv(nil), f, "web-1")
	e = pumpPodLogs(t, e, e.Init())
	if call := f.lastCall(t); call.o.Container != "" {
		t.Errorf("first request container = %q, want the backend default", call.o.Container)
	}
	if !strings.Contains(e.Render(100, 6), "container app") {
		t.Error("the banner does not name the first container")
	}

	e, cmd := podLogsPress(t, e, runeKey("c"))
	e = pumpPodLogs(t, e, cmd)
	if e.o.Container != "sidecar" {
		t.Errorf("container = %q, want sidecar", e.o.Container)
	}
	if call := f.lastCall(t); call.o.Container != "sidecar" {
		t.Errorf("request container = %q, want sidecar", call.o.Container)
	}
	if !strings.Contains(e.Render(100, 6), "container sidecar") {
		t.Error("the banner does not name the chosen container")
	}
}

// TestPodLogsFilterAndFinding checks `/` filters through the text view and `F`
// records the visible finding.
func TestPodLogsFilterAndFinding(t *testing.T) {
	rec := &fakeRecorder{open: true}
	f := &fakePodLogs{
		info: k8s.PodInfo{Containers: []string{"app"}},
		lines: []k8s.LogLine{
			logLine("web-1", "app", "ERROR boom happened"),
			logLine("web-1", "app", "INFO all quiet"),
		},
	}
	e := newPodLogs(t, podLogsEnv(rec), f, "web-1")
	e = pumpPodLogs(t, e, e.Init())

	e, _ = podLogsPress(t, e, runeKey("/"))
	for _, r := range "boom" {
		e, _ = podLogsPress(t, e, runeKey(string(r)))
	}
	e, _ = podLogsPress(t, e, tea.KeyMsg{Type: tea.KeyEnter})

	if e.Text.search != "boom" {
		t.Fatalf("search = %q, want boom", e.Text.search)
	}
	if len(e.Text.matches) != 1 {
		t.Fatalf("matches = %v, want one", e.Text.matches)
	}

	if e.Capturing() {
		t.Fatal("the screen is still capturing after enter")
	}
	e, cmd := podLogsPress(t, e, runeKey("F"))
	e = pumpPodLogs(t, e, cmd)

	var findings []investigation.Entry
	for _, entry := range rec.seen() {
		if entry.Kind == investigation.EntryFinding {
			findings = append(findings, entry)
		}
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(findings))
	}
	if !strings.Contains(findings[0].Text, "boom") {
		t.Errorf("finding = %q, want the visible line", findings[0].Text)
	}
}

// TestPodLogsRecordsJournalEntry checks one `logs <ns>/<pod> (N lines)` entry,
// written once per opened screen.
func TestPodLogsRecordsJournalEntry(t *testing.T) {
	rec := &fakeRecorder{open: true}
	f := &fakePodLogs{
		info: k8s.PodInfo{Containers: []string{"app"}},
		lines: []k8s.LogLine{
			logLine("web-1", "app", "INFO one"),
			logLine("web-1", "app", "WARN two"),
		},
	}
	e := newPodLogs(t, podLogsEnv(rec), f, "web-1")
	e = pumpPodLogs(t, e, e.Init())

	var journals []investigation.Entry
	for _, entry := range rec.seen() {
		if entry.Kind == investigation.EntryJournal {
			journals = append(journals, entry)
		}
	}
	if len(journals) != 1 {
		t.Fatalf("journal entries = %d, want 1: %+v", len(journals), rec.seen())
	}
	if want := "logs prod/web-1 (2 lines)"; journals[0].Text != want {
		t.Errorf("entry = %q, want %q", journals[0].Text, want)
	}

	// A reload does not record a second entry.
	e = pumpPodLogs(t, e, e.restart())
	n := 0
	for _, entry := range rec.seen() {
		if entry.Kind == investigation.EntryJournal {
			n++
		}
	}
	if n != 1 {
		t.Errorf("journal entries = %d after a reload, want 1", n)
	}
}

// TestPodLogsWorkloadMergesPods checks a deployment row merges its pods and
// prefixes every line with the pod name.
func TestPodLogsWorkloadMergesPods(t *testing.T) {
	f := &fakePodLogs{
		lines: []k8s.LogLine{
			logLine("web-1", "app", "INFO from one"),
			logLine("web-2", "app", "INFO from two"),
		},
	}
	e := NewPodLogs(testEnv(), f, resource.Query{
		Kind: "deployment",
		Args: map[string]string{"namespace": "prod", "workload": "web"},
	})
	e = pumpPodLogs(t, e, e.Init())

	if call := f.lastWorkload(t); call.kind != "deployment" || call.ns != "prod" || call.name != "web" {
		t.Fatalf("workload request = %+v", call)
	}
	out := ansi.Strip(strings.Join(e.Text.lines, "\n"))
	for _, want := range []string{"[web-1] INFO from one", "[web-2] INFO from two"} {
		if !strings.Contains(out, want) {
			t.Errorf("lines missing %q:\n%s", want, out)
		}
	}
	if e.Crumb() != "logs prod/deployment/web" {
		t.Errorf("crumb = %q", e.Crumb())
	}
	if e.o.AllContainers {
		t.Error("a workload read must not merge all containers by default")
	}
}

// TestPodLogsAllContainersPrefixes checks the container prefix of a merged
// container read.
func TestPodLogsAllContainersPrefixes(t *testing.T) {
	f := &fakePodLogs{
		info:  k8s.PodInfo{Containers: []string{"app", "sidecar"}},
		lines: []k8s.LogLine{logLine("web-1", "sidecar", "INFO proxied")},
	}
	e := newPodLogs(t, podLogsEnv(nil), f, "web-1")
	e.o.AllContainers = true
	e = pumpPodLogs(t, e, e.Init())

	if got := ansi.Strip(e.Text.lines[0]); got != "[sidecar] INFO proxied" {
		t.Errorf("line = %q, want the container prefix", got)
	}
}

// TestPodLogsStreamError checks a refused stream shows its kind instead of
// failing silently.
func TestPodLogsStreamError(t *testing.T) {
	f := &fakePodLogs{
		info:   k8s.PodInfo{Containers: []string{"app"}},
		logErr: errs.New(errs.Auth, "k8s.Logs", "forbidden"),
	}
	e := newPodLogs(t, podLogsEnv(nil), f, "web-1")
	e = pumpPodLogs(t, e, e.Init())

	if e.err == nil {
		t.Fatal("no error was stored")
	}
	if !strings.Contains(ansi.Strip(e.Render(100, 6)), "stream ended: auth") {
		t.Errorf("render does not show the error kind:\n%s", ansi.Strip(e.Render(100, 6)))
	}
}

// TestPodLogsWindowFromSession checks the session window bounds the read.
func TestPodLogsWindowFromSession(t *testing.T) {
	f := &fakePodLogs{
		info:  k8s.PodInfo{Containers: []string{"app"}},
		lines: []k8s.LogLine{logLine("web-1", "app", "INFO one")},
	}
	e := NewPodLogs(testEnv(), f, resource.Query{
		Kind:   "pod",
		Args:   map[string]string{"namespace": "prod", "pod": "web-1"},
		Window: resource.TimeWindow{From: fixedNow.Add(-time.Hour), ToNow: true, Label: "-1h"},
	})
	e = pumpPodLogs(t, e, e.Init())

	if got := f.lastCall(t).o.SinceTime; !got.Equal(fixedNow.Add(-time.Hour)) {
		t.Errorf("sinceTime = %v, want the window's lower bound", got)
	}
}

// TestPodLogsRedactsSecrets checks a token an application logged never reaches
// the screen or the record.
func TestPodLogsRedactsSecrets(t *testing.T) {
	red, err := redact.New(nil)
	if err != nil {
		t.Fatalf("redact.New: %v", err)
	}
	env := podLogsEnv(nil)
	env.Redact = red
	f := &fakePodLogs{
		info:  k8s.PodInfo{Containers: []string{"app"}},
		lines: []k8s.LogLine{logLine("web-1", "app", "INFO calling with token=supersecrettoken123456")},
	}
	e := newPodLogs(t, env, f, "web-1")
	e = pumpPodLogs(t, e, e.Init())

	joined := strings.Join(e.CopyLines(), "\n")
	if strings.Contains(joined, "supersecrettoken123456") {
		t.Errorf("the log screen shows the token:\n%s", joined)
	}
	if !strings.Contains(joined, "redacted") {
		t.Errorf("the line carries no redaction marker:\n%s", joined)
	}
}

// TestPodLogsNoSource checks a screen without a backend says so.
func TestPodLogsNoSource(t *testing.T) {
	e := NewPodLogs(testEnv(), nil, resource.Query{
		Kind: "pod",
		Args: map[string]string{"namespace": "prod", "pod": "web-1"},
	})
	e = pumpPodLogs(t, e, e.Init())
	if e.err == nil {
		t.Fatal("no error without a backend")
	}
	if !strings.Contains(ansi.Strip(e.Render(100, 6)), "stream ended: config") {
		t.Errorf("render does not show the error kind:\n%s", ansi.Strip(e.Render(100, 6)))
	}
}
