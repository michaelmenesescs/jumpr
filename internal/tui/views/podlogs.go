package views

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"jumpr/internal/backend/k8s"
	"jumpr/internal/errs"
	"jumpr/internal/investigation"
	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
)

// Arguments a pod logs query carries (SPEC §8.2).
const (
	podLogsArgNamespace = "namespace"
	podLogsArgPod       = "pod"
	podLogsArgWorkload  = "workload"
	podLogsArgName      = "name"
)

// podLogsMaxBatch is the most lines one tui.NextBatch call carries.
const podLogsMaxBatch = 200

// podLogLevel is the level regex of the log screen: the matched word is the
// one that gets the severity colour.
var podLogLevel = regexp.MustCompile(`\b(ERROR|FATAL|WARN|INFO|DEBUG)\b`)

// podLogsSource is what the log screen needs from the Kubernetes backend.
// *k8s.Backend implements it.
type podLogsSource interface {
	// Logs streams one pod's logs.
	Logs(ctx context.Context, ns, pod string, o k8s.LogOptions) (<-chan k8s.LogLine, error)
	// WorkloadLogs merges the logs of the pods of one workload row.
	WorkloadLogs(ctx context.Context, kind resource.KindName, ns, name string, o k8s.LogOptions) (<-chan k8s.LogLine, error)
	// PodInfo reports the containers and the crash-loop state of a pod.
	PodInfo(ctx context.Context, ns, pod string) (k8s.PodInfo, error)
}

// podLogsInfoMsg is the pod lookup that precedes the first stream.
type podLogsInfoMsg struct {
	seq  int
	info k8s.PodInfo
	err  error
}

// podLogsStartMsg is one started stream, or the error that kept it from
// starting.
type podLogsStartMsg struct {
	seq int
	ch  <-chan k8s.LogLine
	err error
}

// podLogsBatchMsg is one batch of lines, tagged with its stream's Seq.
type podLogsBatchMsg struct {
	seq    int
	lines  []k8s.LogLine
	closed bool
}

// PodLogs is the pod log screen (SPEC §8.2): one pod's logs, or the merged
// logs of a deployment/statefulset/daemonset's pods. It is built on the text
// view, so scrolling, search, wrap and follow behave exactly as everywhere
// else; it adds `w` follow (a real stream), `p` previous logs, `c` the
// container, level colouring and `F` findings.
type PodLogs struct {
	*Text

	env *tui.Env
	src podLogsSource
	// ns and pod select one pod; kind and workload select a workload row.
	ns       string
	pod      string
	kind     resource.KindName
	workload string
	window   resource.TimeWindow

	o k8s.LogOptions
	// containers is the pod's container list, loaded before the first stream.
	containers []string
	// containerIdx is the container the banner names when o.Container is
	// empty (the pod's first).
	containerIdx int
	crashLoop    bool
	// prevTouched is set by `p`: after that the crash-loop default no longer
	// switches previous on.
	prevTouched bool

	seq     int
	ctx     context.Context
	cancel  context.CancelFunc
	ch      <-chan k8s.LogLine
	loading bool
	err     error
	lines   int
	// recorded guards the one investigation entry per opened screen.
	recorded bool

	theme *theme.Theme
}

var _ tui.View = (*PodLogs)(nil)

// NewPodLogs builds the log screen over src. q.Args["pod"] selects one pod;
// otherwise q.Kind (deployment, statefulset, daemonset) and
// q.Args["workload"] select the pods of a workload. q.Args["namespace"] and
// q.Window narrow the read.
func NewPodLogs(env *tui.Env, src podLogsSource, q resource.Query) *PodLogs {
	ns := strings.TrimSpace(q.Args[podLogsArgNamespace])
	if ns == "" {
		ns = "default"
	}
	pod := strings.TrimSpace(q.Args[podLogsArgPod])
	workload := strings.TrimSpace(q.Args[podLogsArgWorkload])
	if workload == "" && pod == "" {
		workload = strings.TrimSpace(q.Args[podLogsArgName])
	}
	e := &PodLogs{
		env:      env,
		src:      src,
		ns:       ns,
		pod:      pod,
		kind:     canonicalWorkloadKind(q.Kind),
		workload: workload,
		window:   q.Window,
		o:        k8s.LogOptions{TailLines: k8s.DefaultTailLines},
	}
	if env != nil && env.Theme != nil {
		e.theme = env.Theme
	} else {
		e.theme = theme.Dark()
	}
	e.Text = NewText(env, e.Crumb(), nil)
	e.Text.follow = false
	return e
}

// canonicalWorkloadKind maps kubectl's aliases onto the backend's kind names.
func canonicalWorkloadKind(k resource.KindName) resource.KindName {
	switch k {
	case "deploy":
		return "deployment"
	case "sts":
		return "statefulset"
	case "ds":
		return "daemonset"
	}
	return k
}

// Init starts the first load.
func (e *PodLogs) Init() tea.Cmd { return e.restart() }

// now is the clock handed in by the environment.
func (e *PodLogs) now() time.Time {
	if e.env != nil && e.env.Now != nil {
		return e.env.Now()
	}
	return time.Now()
}

// restart cancels the running stream and starts a new one with the current
// options. In pod mode it looks the pod up first, so the container list and
// the crash-loop state are fresh.
func (e *PodLogs) restart() tea.Cmd {
	e.stop()
	if e.src == nil {
		e.loading = false
		e.err = errs.New(errs.Config, "views.logs", "no kubernetes backend")
		e.Text.AppendError(string(errs.KindOf(e.err)))
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.ctx = ctx
	e.cancel = cancel
	e.seq++
	seq := e.seq
	e.loading = true
	e.err = nil

	if e.pod == "" {
		return e.startStream()
	}
	src, ns, pod := e.src, e.ns, e.pod
	return func() tea.Msg {
		info, err := src.PodInfo(ctx, ns, pod)
		return podLogsInfoMsg{seq: seq, info: info, err: err}
	}
}

// stop cancels the stream of the previous Seq and forgets its channel.
func (e *PodLogs) stop() {
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}
	e.ctx = nil
	e.ch = nil
}

// startStream opens the stream for the current target and options.
func (e *PodLogs) startStream() tea.Cmd {
	ctx, src, seq := e.ctx, e.src, e.seq
	if ctx == nil || src == nil {
		return nil
	}
	o := e.options()
	ns, pod, kind, workload := e.ns, e.pod, e.kind, e.workload
	return func() tea.Msg {
		if pod != "" {
			ch, err := src.Logs(ctx, ns, pod, o)
			return podLogsStartMsg{seq: seq, ch: ch, err: err}
		}
		ch, err := src.WorkloadLogs(ctx, kind, ns, workload, o)
		return podLogsStartMsg{seq: seq, ch: ch, err: err}
	}
}

// options is the request: the user's choices plus the session window.
func (e *PodLogs) options() k8s.LogOptions {
	o := e.o
	if o.TailLines <= 0 {
		o.TailLines = k8s.DefaultTailLines
	}
	if !e.window.IsZero() {
		from, _ := e.window.Bounds(e.now())
		o.SinceTime = from
	}
	return o
}

// nextBatch asks for the next batch of the running stream.
func (e *PodLogs) nextBatch() tea.Cmd {
	ch, seq := e.ch, e.seq
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		lines, closed := tui.NextBatch(ch, podLogsMaxBatch)
		return podLogsBatchMsg{seq: seq, lines: lines, closed: closed}
	}
}

// Update handles the stream messages, the session window and the keys that
// are not the text view's.
func (e *PodLogs) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	switch m := msg.(type) {
	case podLogsInfoMsg:
		return e, e.gotInfo(m)
	case podLogsStartMsg:
		return e, e.started(m)
	case podLogsBatchMsg:
		return e, e.batch(m)
	case tui.WindowChangedMsg:
		e.window = m.Window
		return e, e.restart()
	case tea.KeyMsg:
		return e, e.handleKey(m)
	}
	return e, nil
}

// gotInfo stores the pod lookup and starts the stream. A crash-looping pod
// switches previous on, which is the only useful thing to read when the
// current container has no logs yet.
func (e *PodLogs) gotInfo(m podLogsInfoMsg) tea.Cmd {
	if m.seq != e.seq {
		return nil
	}
	if m.err != nil {
		e.loading = false
		e.err = m.err
		e.Text.AppendError(string(errs.KindOf(m.err)))
		return tui.Flash(tui.FlashError, "logs: %v", m.err)
	}
	e.containers = m.info.Containers
	e.crashLoop = m.info.CrashLoop
	if e.containerIdx >= len(e.containers) {
		e.containerIdx = 0
	}
	if m.info.CrashLoop && !e.prevTouched && !e.o.Previous {
		e.o.Previous = true
		e.o.Follow = false
		e.Text.follow = false
	}
	return e.startStream()
}

// started stores a stream and asks for its first batch.
func (e *PodLogs) started(m podLogsStartMsg) tea.Cmd {
	if m.seq != e.seq {
		return nil
	}
	if m.err != nil {
		e.loading = false
		e.err = m.err
		e.Text.AppendError(string(errs.KindOf(m.err)))
		return tui.Flash(tui.FlashError, "logs: %v", m.err)
	}
	e.ch = m.ch
	return e.nextBatch()
}

// batch appends one batch. A closed stream ends the load and records the
// journal entry; a failed one adds the error kind as its last line.
func (e *PodLogs) batch(m podLogsBatchMsg) tea.Cmd {
	if m.seq != e.seq {
		return nil
	}
	for _, line := range m.lines {
		if line.Err != nil {
			e.err = line.Err
			e.Text.AppendError(string(errs.KindOf(line.Err)))
			continue
		}
		e.lines++
		e.Text.Append(e.lineText(line))
	}
	if m.closed {
		e.loading = false
		e.ch = nil
		return e.record()
	}
	return e.nextBatch()
}

// lineText renders one line: the merge prefixes, then the level colour. The
// text is redacted first (BUILD-PLAN §7): an application that logs a token must
// not put it on screen.
func (e *PodLogs) lineText(l k8s.LogLine) string {
	var b strings.Builder
	if e.workload != "" && l.Pod != "" {
		b.WriteString(e.theme.Dim.Render("[" + l.Pod + "] "))
	}
	if e.o.AllContainers && l.Container != "" {
		b.WriteString(e.theme.Dim.Render("[" + l.Container + "] "))
	}
	b.WriteString(e.levelText(strings.TrimRight(e.redact(l.Text), "\r")))
	return b.String()
}

// redact passes text through the environment's redactor.
func (e *PodLogs) redact(s string) string {
	if e.env != nil && e.env.Redact != nil {
		return e.env.Redact.String(s)
	}
	return s
}

// levelText colours the first level token of a line: FATAL/ERROR crit, WARN
// warn, INFO ok, DEBUG faint. A line without a level is left alone.
func (e *PodLogs) levelText(text string) string {
	loc := podLogLevel.FindStringIndex(text)
	if loc == nil {
		return text
	}
	level := text[loc[0]:loc[1]]
	style := e.theme.Base
	switch level {
	case "FATAL", "ERROR":
		style = e.theme.Crit
	case "WARN":
		style = e.theme.Warn
	case "INFO":
		style = e.theme.OK
	case "DEBUG":
		style = e.theme.Faint
	}
	return text[:loc[0]] + style.Render(level) + text[loc[1]:]
}

// record appends the journal-style entry once, when the stream finishes.
func (e *PodLogs) record() tea.Cmd {
	if e.recorded {
		return nil
	}
	e.recorded = true
	if e.env == nil || e.env.Recorder == nil {
		return nil
	}
	if _, open := e.env.Recorder.Current(); !open {
		return nil
	}
	return e.env.AppendInvestigation(investigation.EntryJournal, e.entryText(), "")
}

// entryText is the investigation entry: `logs <ns>/<pod> (N lines)`.
func (e *PodLogs) entryText() string {
	return fmt.Sprintf("logs %s (%d lines)", e.target(), e.lines)
}

// target names what the screen read.
func (e *PodLogs) target() string {
	if e.pod == "" {
		return fmt.Sprintf("%s/%s/%s", e.ns, e.kind, e.workload)
	}
	return e.ns + "/" + e.pod
}

// handleKey routes one key. While the text view is searching every key is the
// pattern; otherwise `p`, `c`, `w` and `F` are handled here and everything
// else is the text view's.
func (e *PodLogs) handleKey(m tea.KeyMsg) tea.Cmd {
	if e.Text.Capturing() {
		_, cmd := e.Text.Update(m)
		return cmd
	}
	keys := e.keys()
	key := m.String()
	switch {
	case keys.Finding.Matches(key):
		return e.finding()
	case key == "p":
		return e.togglePrevious()
	case key == "c":
		return e.cycleContainer()
	case keys.Follow.Matches(key):
		return e.toggleFollow()
	case keys.Refresh.Matches(key):
		return e.restart()
	}
	_, cmd := e.Text.Update(m)
	return cmd
}

// keys is the session key map, or the default one.
func (e *PodLogs) keys() *keymap.Map {
	if e.env != nil && e.env.Keys != nil {
		return e.env.Keys
	}
	return keymap.Default()
}

// togglePrevious switches between the current and the previous container's
// logs. Previous logs are static, so follow goes off with it.
func (e *PodLogs) togglePrevious() tea.Cmd {
	e.prevTouched = true
	e.o.Previous = !e.o.Previous
	if e.o.Previous {
		e.o.Follow = false
		e.Text.follow = false
	}
	if e.o.Previous {
		return e.restartWith("previous logs")
	}
	return e.restartWith("current logs")
}

// toggleFollow starts or stops the live stream.
func (e *PodLogs) toggleFollow() tea.Cmd {
	e.o.Follow = !e.o.Follow
	e.Text.follow = e.o.Follow
	if e.o.Follow {
		e.o.Previous = false
		return e.restartWith("following")
	}
	return e.restartWith("follow off")
}

// cycleContainer moves to the next container of the pod.
func (e *PodLogs) cycleContainer() tea.Cmd {
	switch len(e.containers) {
	case 0:
		return tui.Flash(tui.FlashInfo, "containers not loaded yet")
	case 1:
		return tui.Flash(tui.FlashInfo, "pod has one container: %s", e.containers[0])
	}
	e.containerIdx = (e.containerIdx + 1) % len(e.containers)
	e.o.Container = e.containers[e.containerIdx]
	return e.restartWith("container " + e.o.Container)
}

// restartWith restarts the stream and flashes why.
func (e *PodLogs) restartWith(text string) tea.Cmd {
	cmd := e.restart()
	if cmd == nil {
		return tui.Flash(tui.FlashWarn, "%s", text)
	}
	return tea.Batch(cmd, tui.Flash(tui.FlashInfo, "%s", text))
}

// finding records the first visible line as an investigation finding.
func (e *PodLogs) finding() tea.Cmd {
	if len(e.Text.lines) == 0 {
		return tui.Flash(tui.FlashInfo, "no log lines")
	}
	if e.env == nil {
		return nil
	}
	i := e.Text.top
	if i < 0 || i >= len(e.Text.lines) {
		i = 0
	}
	return e.env.RecordFinding(ansi.Strip(e.Text.lines[i]))
}

// Rendering: a banner line, then the text view body and footer.

// Render returns exactly h lines of exactly w cells.
func (e *PodLogs) Render(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	if h == 1 {
		return e.theme.Dim.Render(render.Truncate(e.banner(), w))
	}
	body := e.Text.Render(w, h-1)
	return render.Fit(e.theme.Dim.Render(render.Truncate(e.banner(), w))+"\n"+body, w, h)
}

// banner is the one-line status: target, container, previous/follow and the
// number of lines read.
func (e *PodLogs) banner() string {
	parts := []string{"logs " + e.target()}
	if c := e.containerName(); c != "" {
		parts = append(parts, "container "+c)
	}
	if e.o.Previous {
		parts = append(parts, "previous")
	}
	if e.o.Follow {
		parts = append(parts, "follow")
	}
	if e.crashLoop {
		parts = append(parts, "crashloop")
	}
	parts = append(parts, render.Int(int64(e.lines))+" lines")
	if e.loading && !e.o.Follow {
		parts = append(parts, "loading")
	}
	if !e.window.IsZero() {
		parts = append(parts, windowText(e.window))
	}
	return strings.Join(parts, " · ")
}

// containerName is the container the screen is reading, or "" when the
// workload's pods each use their own first container.
func (e *PodLogs) containerName() string {
	if e.o.Container != "" {
		return e.o.Container
	}
	if len(e.containers) > 0 {
		return e.containers[0]
	}
	return ""
}

// Close cancels the stream. The view stack calls it on pop and replace, which
// is what stops a follow from holding an API connection forever.
func (e *PodLogs) Close() error {
	e.stop()
	return nil
}

// Crumb is "logs <ns>/<target>".
func (e *PodLogs) Crumb() string { return "logs " + e.target() }

// Hints lists the keys the screen binds.
func (e *PodLogs) Hints() []tui.KeyHint {
	if e.Text.Capturing() {
		return []tui.KeyHint{
			{Key: "enter", Text: "search"},
			{Key: "esc", Text: "cancel"},
		}
	}
	return []tui.KeyHint{
		{Key: firstKey(e.keys().Up) + "/" + firstKey(e.keys().Down), Text: "scroll"},
		{Key: "/", Text: "filter"},
		{Key: "w", Text: "follow"},
		{Key: "p", Text: "previous"},
		{Key: "c", Text: "container"},
		{Key: "F", Text: "finding"},
		{Key: "esc", Text: "back"},
	}
}

// Subject is the pod (or workload) the screen read.
func (e *PodLogs) Subject() tui.Subject {
	name := e.pod
	kind := resource.KindName("pod")
	if name == "" {
		name, kind = e.workload, e.kind
	}
	return tui.Subject{
		Kind: kind,
		Ref: resource.Ref{
			Backend:   "k8s",
			Kind:      kind,
			Namespace: e.ns,
			Name:      name,
		},
	}
}

// Capturing is true while the search pattern owns the keyboard.
func (e *PodLogs) Capturing() bool { return e.Text.Capturing() }

// CopyLines is the plain text of the screen, without colour.
func (e *PodLogs) CopyLines() []string {
	return e.Text.CopyLines()
}
