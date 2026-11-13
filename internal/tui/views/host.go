package views

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend"
	"jumper/internal/backend/elastic"
	"jumper/internal/backend/k8s"
	"jumper/internal/backend/linux"
	"jumper/internal/backend/prom"
	"jumper/internal/errs"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/tui"
	"jumper/internal/tui/keymap"
	"jumper/internal/tui/theme"
)

// hostLoadTimeout bounds the overview Get a host view runs. BUILD-PLAN §8: exec
// 30 s; the transport also carries its own deadline.
const hostLoadTimeout = 30 * time.Second

// hostJournalMax is the most journal entries one batch carries. The list is
// capped at hostJournalLimit entries by the query anyway.
const hostJournalMax = 50

// hostMetricSparkWidth is the sparkline width of one metric column.
const hostMetricSparkWidth = 16

// hostMetricSource is a backend that carries the configured host metric
// columns (metrics.host_columns) and can answer their PromQL.
type hostMetricSource interface {
	HostColumns() []prom.HostColumn
	Queriers() []prom.NamedQuerier
}

// hostNodeSource is a backend that knows the Kubernetes node behind an
// inventory host. The overview's KUBERNETES line comes from it, and so does the
// node ↔ host pivot (task 111): the same backend answers both, so a host that
// is a node shows its cluster state.
type hostNodeSource interface {
	NodeStatusOf(ctx context.Context, host resource.HostID) (k8s.NodeStatus, bool, error)
}

// ruleReader is the part of a Prometheus client that can read rule
// expressions; *prom.Client implements it.
type ruleReader interface {
	Rules(ctx context.Context) ([]prom.RuleGroup, error)
}

// Journal arguments of the overview's error section: priority ≤ 3 (err and
// worse), the eight most recent entries of the session window.
const (
	hostJournalPriority = "3"
	hostJournalLimit    = "8"
)

// Layout constants.
const (
	hostLabelWidth = 10 // "PRESSURE  "
	hostBarWidth   = 10 // pressure and memory bars
	hostPressHint  = "press u"
	hostWideWidth  = 100 // MEMORY appears at or above this width
)

// hostMsg is the result of the overview Get, tagged with the load's Seq so a
// stale refresh is ignored.
type hostMsg struct {
	Seq     int
	Res     resource.Resource
	Err     error
	Elapsed time.Duration
}

// hostJournalMsg is one batch of the overview's journal listing, tagged with
// the journal stream's Seq.
type hostJournalMsg struct {
	Seq     int
	Results []backend.Result
	Closed  bool
}

// hostMetricsMsg is the metrics part: the configured columns, this host's
// values over the window and, when an investigation is open, the alert's own
// rule expression.
type hostMetricsMsg struct {
	Seq    int
	Cols   []prom.HostColumn
	Values map[resource.HostID]map[string]prom.HostValue
	Alert  string
	Err    error
}

// hostNodeMsg is the Kubernetes part: the node this host is, when it is one.
type hostNodeMsg struct {
	Seq    int
	Status k8s.NodeStatus
	Node   bool
	Err    error
}

// Host is the one-screen summary of a single host: system facts, pressure,
// load and memory, failed units and the recent journal errors. It loads two
// independent parts — the overview row and the journal — and renders each as
// soon as it arrives.
type Host struct {
	env  *tui.Env
	b    backend.Backend
	host resource.HostID
	keys *keymap.Map

	window resource.TimeWindow

	// overview part
	hostSeq    int
	hostCancel context.CancelFunc
	row        resource.Resource
	hostErr    error
	hostDone   bool
	elapsed    time.Duration

	// journal part
	journalSeq    int
	journalCancel context.CancelFunc
	journalCh     <-chan backend.Result
	journalRows   []resource.Resource
	journalErr    error
	journalDone   bool

	// metrics part (Prometheus configured)
	metricsSeq    int
	metricsCancel context.CancelFunc
	metricCols    []prom.HostColumn
	metricValues  map[resource.HostID]map[string]prom.HostValue
	metricAlert   string
	metricErr     error
	metricDone    bool

	// Kubernetes part (a k8s backend registered and this host is a node)
	nodeSeq    int
	nodeCancel context.CancelFunc
	nodeStatus k8s.NodeStatus
	nodeIsNode bool
	nodeErr    error
	nodeDone   bool

	w, h int
}

var _ tui.View = (*Host)(nil)

// NewHost builds the overview of one host. b is the Linux backend (any backend
// that answers the host-overview kind works) and env supplies the theme, keys,
// clock and the session window.
func NewHost(env *tui.Env, b backend.Backend, host resource.HostID) *Host {
	keys := keymap.Default()
	if env != nil && env.Keys != nil {
		keys = env.Keys
	}
	h := &Host{
		env:  env,
		b:    b,
		host: resource.NormalizeHostID(string(host)),
		keys: keys,
	}
	if env != nil && env.Session != nil {
		h.window = env.Session.Window
	}
	return h
}

// Init starts both parts in parallel. Neither waits for the other.
func (h *Host) Init() tea.Cmd {
	return tea.Batch(h.loadHost(), h.loadJournal(), h.loadMetrics(), h.loadNode())
}

// now is the clock handed in by the environment.
func (h *Host) now() time.Time {
	if h.env != nil && h.env.Now != nil {
		return h.env.Now()
	}
	return time.Now()
}

// style returns the theme, falling back to the default palette.
func (h *Host) style() *theme.Theme {
	if h.env != nil && h.env.Theme != nil {
		return h.env.Theme
	}
	return theme.Dark()
}

// loadHost runs the overview Get in a tea.Cmd. A Get is one round trip, so it
// gets its own cancellable context.
func (h *Host) loadHost() tea.Cmd {
	if h.hostCancel != nil {
		h.hostCancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), hostLoadTimeout)
	h.hostCancel = cancel

	h.hostSeq++
	seq := h.hostSeq
	h.row = resource.Resource{}
	h.hostErr = nil
	h.hostDone = false
	h.elapsed = 0

	b, host, started := h.b, h.host, h.now()
	return func() tea.Msg {
		res, err := b.Get(ctx, resource.Ref{
			Backend: "linux",
			Kind:    linux.KindHostOverview,
			Host:    host,
			Name:    string(host),
		})
		return hostMsg{Seq: seq, Res: res, Err: err, Elapsed: h.now().Sub(started)}
	}
}

// loadJournal starts the journal listing for the current window. List returns
// as soon as the work is started; the batches arrive through NextBatch.
func (h *Host) loadJournal() tea.Cmd {
	if h.journalCancel != nil {
		h.journalCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.journalCancel = cancel

	h.journalSeq++
	h.journalCh = nil
	h.journalRows = nil
	h.journalErr = nil
	h.journalDone = false

	q := resource.Query{
		Kind:   linux.KindJournal,
		Hosts:  []resource.HostID{h.host},
		Args:   map[string]string{"priority": hostJournalPriority, "limit": hostJournalLimit},
		Window: h.window,
	}
	ch, err := h.b.List(ctx, q)
	if err != nil {
		h.journalDone = true
		h.journalErr = err
		return nil
	}
	h.journalCh = ch
	return hostJournalBatches(h.journalSeq, ch)
}

// hostJournalBatches asks for the next batch of one journal stream.
func hostJournalBatches(seq int, ch <-chan backend.Result) tea.Cmd {
	return func() tea.Msg {
		results, closed := tui.NextBatch(ch, hostJournalMax)
		return hostJournalMsg{Seq: seq, Results: results, Closed: closed}
	}
}

// loadMetrics starts the metrics part. It is a no-op unless a registered
// backend carries configured host columns, which is what makes the section
// appear only when a Prometheus is configured (metrics.host_columns defaults to
// none).
func (h *Host) loadMetrics() tea.Cmd {
	if h.metricsCancel != nil {
		h.metricsCancel()
		h.metricsCancel = nil
	}
	h.metricCols = nil
	h.metricValues = nil
	h.metricAlert = ""
	h.metricErr = nil
	h.metricDone = true

	src := h.metricSource()
	if src == nil {
		return nil
	}
	cols := src.HostColumns()
	if len(cols) == 0 {
		return nil
	}
	queriers := src.Queriers()
	if len(queriers) == 0 {
		h.metricCols = cols
		h.metricErr = errs.New(errs.Config, "views.host.metrics", "no prometheus server configured")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), hostLoadTimeout)
	h.metricsCancel = cancel
	h.metricsSeq++
	seq := h.metricsSeq
	h.metricDone = false

	host := h.host
	from, to := h.metricWindow()
	var alerts []string
	if h.env != nil && h.env.Session != nil && h.env.Session.Investigation != nil {
		alerts = append([]string(nil), h.env.Session.Investigation.Alerts...)
	}
	q := queriers[0].Querier
	return func() tea.Msg {
		m := hostMetricsMsg{Seq: seq, Cols: cols}
		values, err := prom.QueryHostColumns(ctx, q, cols, []resource.HostID{host}, from, to)
		if err != nil {
			m.Err = err
			return m
		}
		m.Values = values
		m.Alert = alertExpression(ctx, q, alerts)
		return m
	}
}

// metricSource finds the registered backend that carries host columns.
func (h *Host) metricSource() hostMetricSource {
	if h.env == nil || h.env.Registry == nil {
		return nil
	}
	for _, b := range h.env.Registry.Backends() {
		if src, ok := b.(hostMetricSource); ok && len(src.HostColumns()) > 0 {
			return src
		}
	}
	return nil
}

// metricWindow is the session window; a zero window means the last hour, the
// same default a series list uses.
func (h *Host) metricWindow() (time.Time, time.Time) {
	now := h.now()
	if h.window.IsZero() {
		return now.Add(-time.Hour), now
	}
	return h.window.Bounds(now)
}

// loadNode starts the Kubernetes part: the node this host is, when it is one.
// It is a no-op unless a registered backend knows the cluster, which is what
// makes the KUBERNETES line appear only with a k8s backend.
func (h *Host) loadNode() tea.Cmd {
	if h.nodeCancel != nil {
		h.nodeCancel()
		h.nodeCancel = nil
	}
	h.nodeStatus = k8s.NodeStatus{}
	h.nodeIsNode = false
	h.nodeErr = nil
	h.nodeDone = true

	src := h.nodeSource()
	if src == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), hostLoadTimeout)
	h.nodeCancel = cancel
	h.nodeSeq++
	seq := h.nodeSeq
	h.nodeDone = false

	host := h.host
	return func() tea.Msg {
		status, node, err := src.NodeStatusOf(ctx, host)
		return hostNodeMsg{Seq: seq, Status: status, Node: node, Err: err}
	}
}

// nodeSource finds the registered backend that knows Kubernetes nodes.
func (h *Host) nodeSource() hostNodeSource {
	if h.env == nil || h.env.Registry == nil {
		return nil
	}
	for _, b := range h.env.Registry.Backends() {
		if src, ok := b.(hostNodeSource); ok {
			return src
		}
	}
	return nil
}

// alertExpression returns the rule expression of the first open
// investigation's alert that the rule list knows, or "".
func alertExpression(ctx context.Context, q prom.Querier, alerts []string) string {
	if len(alerts) == 0 {
		return ""
	}
	reader, ok := q.(ruleReader)
	if !ok {
		return ""
	}
	groups, err := reader.Rules(ctx)
	if err != nil {
		return ""
	}
	want := make(map[string]bool, len(alerts))
	for _, a := range alerts {
		if a = strings.TrimSpace(a); a != "" {
			want[a] = true
		}
	}
	for _, g := range groups {
		for _, r := range g.Rules {
			if want[r.Name] && strings.TrimSpace(r.Query) != "" {
				return strings.TrimSpace(r.Query)
			}
		}
	}
	return ""
}

// Update handles the two streams, the session window and the keys. Neither
// part waits for the other: each renders as soon as its first message arrives.
func (h *Host) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	switch m := msg.(type) {
	case hostMsg:
		if m.Seq != h.hostSeq {
			return h, nil
		}
		h.row, h.hostErr = m.Res, m.Err
		h.elapsed, h.hostDone = m.Elapsed, true
		if h.hostCancel != nil {
			h.hostCancel()
			h.hostCancel = nil
		}
		return h, nil
	case hostJournalMsg:
		if m.Seq != h.journalSeq {
			return h, nil
		}
		for _, r := range m.Results {
			if r.Err != nil {
				if h.journalErr == nil {
					h.journalErr = r.Err
				}
				continue
			}
			h.journalRows = append(h.journalRows, r.Items...)
		}
		if m.Closed {
			h.journalDone = true
			if h.journalCancel != nil {
				h.journalCancel()
				h.journalCancel = nil
			}
			return h, nil
		}
		return h, hostJournalBatches(m.Seq, h.journalCh)
	case hostMetricsMsg:
		if m.Seq != h.metricsSeq {
			return h, nil
		}
		h.metricCols, h.metricValues, h.metricAlert, h.metricErr = m.Cols, m.Values, m.Alert, m.Err
		h.metricDone = true
		if h.metricsCancel != nil {
			h.metricsCancel()
			h.metricsCancel = nil
		}
		return h, nil
	case hostNodeMsg:
		if m.Seq != h.nodeSeq {
			return h, nil
		}
		h.nodeStatus, h.nodeIsNode, h.nodeErr = m.Status, m.Node, m.Err
		h.nodeDone = true
		if h.nodeCancel != nil {
			h.nodeCancel()
			h.nodeCancel = nil
		}
		return h, nil
	case tui.WindowChangedMsg:
		h.window = m.Window
		return h, tea.Batch(h.loadJournal(), h.loadMetrics(), h.loadNode())
	case tea.KeyMsg:
		return h, h.handleKey(m)
	}
	return h, nil
}

// handleKey routes one key press. The five pivots open a query for this host;
// r reloads both parts and esc goes back.
func (h *Host) handleKey(m tea.KeyMsg) tea.Cmd {
	key := m.String()
	switch {
	case key == "u":
		return tui.OpenQuery(h.queryFor(linux.KindUnit))
	case key == "j":
		return tui.OpenQuery(h.queryFor(linux.KindJournal))
	case key == "p":
		return tui.OpenQuery(h.queryFor(linux.KindProcess))
	case key == "s":
		return tui.OpenQuery(h.queryFor(linux.KindSocket))
	case key == "l":
		// Central logs for this host (task 122). The elastic backend refuses
		// the query when no log source is configured, which is what the view
		// shows instead of an empty table.
		return tui.OpenQuery(h.queryFor(elastic.KindLog))
	case h.keys.Refresh.Matches(key):
		return tea.Batch(h.loadHost(), h.loadJournal(), h.loadMetrics(), h.loadNode())
	case h.keys.Back.Matches(key):
		return tui.Pop()
	}
	return nil
}

// queryFor is the pivot query of one kind on this host.
func (h *Host) queryFor(kind resource.KindName) resource.Query {
	return resource.Query{
		Kind:   kind,
		Hosts:  []resource.HostID{h.host},
		Window: h.window,
	}
}

// Close cancels both loads. The view stack calls it when the view is popped.
func (h *Host) Close() error {
	if h.hostCancel != nil {
		h.hostCancel()
		h.hostCancel = nil
	}
	if h.journalCancel != nil {
		h.journalCancel()
		h.journalCancel = nil
	}
	if h.metricsCancel != nil {
		h.metricsCancel()
		h.metricsCancel = nil
	}
	if h.nodeCancel != nil {
		h.nodeCancel()
		h.nodeCancel = nil
	}
	return nil
}

// facts returns the typed detail of the loaded host row.
func (h *Host) facts() (linux.HostFacts, bool) {
	if h.row.Raw == nil {
		return linux.HostFacts{}, false
	}
	return linux.HostFactsOf(h.row)
}

// shortHost is the host name without its domain.
func (h *Host) shortHost() string {
	name := string(h.host)
	if i := strings.IndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	return name
}

// firstFailedUnit is the name of the first failed unit, or "".
func (h *Host) firstFailedUnit() string {
	f, ok := h.facts()
	if !ok || len(f.FailedUnits) == 0 {
		return ""
	}
	return f.FailedUnits[0].Name
}

// sudoDenied reports whether a section failed because `sudo -n` was not
// permitted; the backend records it as a "sudo" degraded label.
func (h *Host) sudoDenied() bool {
	return strings.Contains(h.row.Labels["_degraded"], ": "+string(errs.Sudo))
}

// sectionLabel styles a section label ("SYSTEM", "PRESSURE", ...).
func (h *Host) sectionLabel(label string) string {
	return h.style().Header.Render(render.PadRight(label, hostLabelWidth))
}

// statusLine is the first line: host, fetch time and sudo state.
func (h *Host) statusLine() string {
	st := h.style()
	dim := " " + st.Dim.Render("·") + " "
	parts := []string{st.Bold.Render(string(h.host))}

	switch {
	case h.hostErr != nil:
		parts = append(parts, st.Crit.Render("fetch failed: "+errorLabel(h.hostErr)))
	case h.hostDone:
		parts = append(parts, "fetched in "+fmt.Sprintf("%.1fs", h.elapsed.Seconds()))
	default:
		parts = append(parts, st.Dim.Render("loading…"))
	}
	if h.sudoDenied() {
		parts = append(parts, st.Warn.Render("sudo -n ✗ needs-privilege"))
	} else {
		parts = append(parts, st.OK.Render("sudo -n ✓"))
	}
	return strings.Join(parts, dim)
}

// systemLine is the SYSTEM line: os, kernel, uptime, systemd state and the
// failed-unit count.
func (h *Host) systemLine() string {
	f, ok := h.facts()
	if !ok {
		return h.sectionErrLine("SYSTEM", errs.NotAvailable)
	}
	st := h.style()
	var parts []string
	if os := h.osName(); os != "" {
		parts = append(parts, "os "+os)
	}
	if kernel := h.kernelName(); kernel != "" {
		parts = append(parts, "kernel "+kernel)
	}
	if d, ok := h.row.Fields["uptime"].(time.Duration); ok && d > 0 {
		parts = append(parts, "uptime "+render.Duration(d))
	}
	state, _ := h.row.Fields["state"].(string)
	if state != "" {
		style := st.Base
		if state != "running" {
			style = st.Crit
		}
		parts = append(parts, "systemd "+style.Render(state))
	}
	if n := len(f.FailedUnits); n > 0 {
		noun := "unit"
		if n != 1 {
			noun = "units"
		}
		parts = append(parts, st.Crit.Render(fmt.Sprintf("%d failed %s", n, noun)))
	}
	if len(parts) == 0 {
		parts = append(parts, st.Faint.Render("—"))
	}
	return h.sectionLabel("SYSTEM") + strings.Join(parts, " "+st.Dim.Render("·")+" ")
}

// osName is the distribution name without the "(Blue Onyx)" style suffix.
func (h *Host) osName() string {
	name, _ := h.row.Fields["os"].(string)
	if i := strings.Index(name, " ("); i > 0 {
		name = name[:i]
	}
	return strings.TrimSpace(name)
}

// kernelName shortens a release to its first two dash-separated parts:
// 5.14.0-427.el9.x86_64 becomes 5.14.0-427.
func (h *Host) kernelName() string {
	name, _ := h.row.Fields["kernel"].(string)
	if name == "" {
		return ""
	}
	if i := strings.IndexByte(name, '-'); i >= 0 {
		rest := name[i+1:]
		if j := strings.IndexByte(rest, '-'); j >= 0 {
			return name[:i+1+j]
		}
		if j := strings.IndexByte(rest, '.'); j >= 0 {
			return name[:i+1+j]
		}
	}
	return name
}

// pressureLine is the PRESSURE line: three labelled bars for cpu, memory and
// io, coloured Warn above 2 % and Crit above 10 %.
func (h *Host) pressureLine() string {
	f, ok := h.facts()
	if !ok || !f.HavePSI {
		return h.sectionErrLine("PRESSURE", errs.NotAvailable)
	}
	segs := []string{
		h.pressureSegment("cpu", f.PSI.CPU),
		h.pressureSegment("mem", f.PSI.Mem),
		h.pressureSegment("io", f.PSI.IO),
	}
	return h.sectionLabel("PRESSURE") + strings.Join(segs, "   ")
}

// pressureSegment renders one "name bar percent" pressure segment.
func (h *Host) pressureSegment(name string, line linux.PSILine) string {
	st := h.style()
	if !line.Present {
		return st.Dim.Render(name+" ") + st.Faint.Render(render.Bar(0, hostBarWidth)+" —")
	}
	style := st.Base
	switch {
	case line.SomeAvg10 > 10:
		style = st.Crit
	case line.SomeAvg10 > 2:
		style = st.Warn
	}
	value := render.Bar(line.SomeAvg10/100, hostBarWidth) + " " + render.Percent(line.SomeAvg10)
	return st.Dim.Render(name+" ") + style.Render(value)
}

// metricsLine is the METRICS section: one "TITLE value spark" segment per
// configured column. It is empty when no columns are configured.
func (h *Host) metricsLine(w int) string {
	if len(h.metricCols) == 0 {
		return ""
	}
	st := h.style()
	if h.metricErr != nil {
		return h.sectionLabel("METRICS") + st.Crit.Render(errorLabel(h.metricErr))
	}
	values := h.metricValues[h.host]
	segs := make([]string, 0, len(h.metricCols))
	for _, col := range h.metricCols {
		title := strings.ToUpper(strings.TrimSpace(col.Title))
		if title == "" {
			title = strings.ToUpper(col.ID)
		}
		v, ok := values[col.ID]
		if !ok {
			segs = append(segs, st.Dim.Render(title+" ")+st.Faint.Render("—"))
			continue
		}
		sparkWidth := hostMetricSparkWidth
		if w > 0 && w < 80 {
			sparkWidth = 8
		}
		text := fmt.Sprintf("%.2f", v.Value)
		if spark := render.Spark(v.Spark, sparkWidth); spark != "" {
			text += " " + spark
		}
		segs = append(segs, st.Dim.Render(title+" ")+st.Base.Render(text))
	}
	if len(segs) == 0 {
		return ""
	}
	return h.sectionLabel("METRICS") + strings.Join(segs, "   ")
}

// alertExprLine is the ALERT section: the open investigation's alert and the
// rule expression it fires on, when Prometheus' rules know it.
func (h *Host) alertExprLine(w int) string {
	if strings.TrimSpace(h.metricAlert) == "" || h.env == nil || h.env.Session == nil ||
		h.env.Session.Investigation == nil {
		return ""
	}
	st := h.style()
	name := ""
	if alerts := h.env.Session.Investigation.Alerts; len(alerts) > 0 {
		name = alerts[0]
	}
	line := h.sectionLabel("ALERT")
	if name != "" {
		line += st.Accent.Render(name) + "  "
	}
	return render.Truncate(line+st.Dim.Render(h.metricAlert), w)
}

// kubernetesLine is the KUBERNETES section: the node's ready state, the pods it
// runs and how many of them are not ready (task 111, SPEC W3).
func (h *Host) kubernetesLine() string {
	if !h.nodeIsNode && h.nodeErr == nil {
		return ""
	}
	st := h.style()
	if h.nodeErr != nil {
		return h.sectionLabel("KUBERNETES") + st.Crit.Render(errorLabel(h.nodeErr))
	}
	s := h.nodeStatus
	state := s.Status
	if state == "" {
		state = "Unknown"
	}
	style := st.OK
	if !s.Ready {
		style = st.Crit
	}
	segs := []string{style.Render(state)}
	nouns := "pods"
	if s.Pods == 1 {
		nouns = "pod"
	}
	segs = append(segs, fmt.Sprintf("%d %s", s.Pods, nouns))
	if s.NotReadyPods > 0 {
		segs = append(segs, st.Crit.Render(fmt.Sprintf("%d not ready", s.NotReadyPods)))
	} else {
		segs = append(segs, st.Dim.Render("all ready"))
	}
	return h.sectionLabel("KUBERNETES") + strings.Join(segs, " "+st.Dim.Render("·")+" ")
}

// loadLine is the LOAD line, with the MEMORY block on terminals at least 100
// columns wide (the right column is dropped below that).
func (h *Host) loadLine(w int) string {
	st := h.style()
	f, ok := h.facts()
	left := h.sectionLabel("LOAD")
	switch {
	case !ok || !f.HaveLoad:
		left += st.Faint.Render("—")
	default:
		cpu := f.NProc
		left += fmt.Sprintf("%.2f %.2f %.2f", f.Load.L1, f.Load.L5, f.Load.L15)
		if cpu > 0 {
			left += st.Dim.Render(fmt.Sprintf(" (%d cpu)", cpu))
		}
	}
	if w < hostWideWidth || !ok || !f.HaveMem {
		return left
	}
	used := f.Mem.UsedPercent()
	style := st.Base
	if used > 90 {
		style = st.Crit
	}
	mem := render.Bar(used/100, hostBarWidth) + " " + render.Percent(used) +
		" of " + render.Bytes(f.Mem.TotalKB*1024)
	return render.PadRight(left, 40) + st.Header.Render("MEMORY") + "  " + style.Render(mem)
}

// failedUnits is the loaded failed-unit list.
func (h *Host) failedUnits() []linux.Unit {
	f, ok := h.facts()
	if !ok {
		return nil
	}
	return f.FailedUnits
}

// failedHeader is the FAILED UNITS section header.
func (h *Host) failedHeader() string { return h.style().Header.Render("FAILED UNITS") }

// unitLine renders one failed unit: name, state, and the hint that `u` lists
// every unit.
func (h *Host) unitLine(u linux.Unit, w int) string {
	st := h.style()
	state := u.Active
	if state == "" {
		state = "failed"
	}
	body := "  " + render.PadRight(u.Name, 16) + " " + st.Crit.Render(state)
	if w > len(hostPressHint)+20 {
		body = render.PadRight(body, w-len(hostPressHint)) + st.Dim.Render(hostPressHint)
	}
	return body
}

// journalHeader labels the journal section, including the error kind when the
// listing failed.
func (h *Host) journalHeader() string {
	st := h.style()
	if h.journalErr != nil {
		return st.Header.Render("JOURNAL") + " " + st.Crit.Render(errorLabel(h.journalErr))
	}
	return st.Header.Render("JOURNAL") +
		st.Dim.Render(fmt.Sprintf(" · errors in window (%d shown)", len(h.journalRows)))
}

// journalLine renders one journal entry as "13:41:07 source: message".
func (h *Host) journalLine(row resource.Resource, w int) string {
	st := h.style()
	text := "  "
	if t, ok := row.Fields["time"].(time.Time); ok && !t.IsZero() {
		text += st.Dim.Render(t.Format("15:04:05")) + " "
	}
	if source, _ := row.Fields["source"].(string); source != "" {
		text += st.Accent.Render(source) + ": "
	}
	msg, _ := row.Fields["message"].(string)
	if row.Status == resource.StatusCrit {
		msg = st.Crit.Render(msg)
	}
	return render.Truncate(text+msg, w)
}

// sectionErrLine is a section whose part failed: the label and the error kind.
func (h *Host) sectionErrLine(label string, kind errs.Kind) string {
	return h.sectionLabel(label) + h.style().Crit.Render(string(kind))
}

// errorLabel renders an error kind for a person: sudo becomes
// "needs-privilege (sudo)", everything else keeps its kind name.
func errorLabel(err error) string {
	kind := errs.KindOf(err)
	if kind == errs.Sudo {
		return "needs-privilege (" + string(kind) + ")"
	}
	if kind == "" {
		return "—"
	}
	return string(kind)
}

// Render returns exactly h lines of exactly w cells.
func (h *Host) Render(w, height int) string {
	h.w, h.h = w, height
	if w <= 0 || height <= 0 {
		return ""
	}
	lines := make([]string, 0, height)
	lines = append(lines, h.statusLine())

	if h.hostErr != nil {
		lines = append(lines, h.sectionErrLine("SYSTEM", errs.KindOf(h.hostErr)))
	} else {
		lines = append(lines, h.systemLine())
		lines = append(lines, h.pressureLine())
		lines = append(lines, h.loadLine(w))
		if line := h.metricsLine(w); line != "" {
			lines = append(lines, line)
		}
		if line := h.alertExprLine(w); line != "" {
			lines = append(lines, line)
		}
		if line := h.kubernetesLine(); line != "" {
			lines = append(lines, line)
		}
	}

	// Failed units: keep three lines for the journal section below.
	if units := h.failedUnits(); len(units) > 0 {
		maxRows := height - len(lines) - 3 // header, journal header, one journal row
		if maxRows > len(units) {
			maxRows = len(units)
		}
		if maxRows > 0 {
			lines = append(lines, h.failedHeader())
			for i := 0; i < maxRows; i++ {
				lines = append(lines, h.unitLine(units[i], w))
			}
		}
	}

	// Journal section.
	if rem := height - len(lines); rem > 0 {
		lines = append(lines, h.journalHeader())
		rem--
		switch {
		case len(h.journalRows) > 0:
			for i := 0; i < len(h.journalRows) && i < rem; i++ {
				lines = append(lines, h.journalLine(h.journalRows[i], w))
			}
		case h.journalErr == nil && !h.journalDone && rem > 0:
			// The header already carries an error kind; otherwise say it is
			// still loading.
			lines = append(lines, h.style().Faint.Render("  loading…"))
		}
	}
	return render.Fit(strings.Join(lines, "\n"), w, height)
}

// Crumb is the short host name.
func (h *Host) Crumb() string { return h.shortHost() }

// Hints lists the pivots, refresh and back.
func (h *Host) Hints() []tui.KeyHint {
	return []tui.KeyHint{
		{Key: "u", Text: "units"},
		{Key: "j", Text: "journal"},
		{Key: "p", Text: "processes"},
		{Key: "s", Text: "sockets"},
		{Key: "l", Text: "logs"},
		{Key: firstKey(h.keys.Refresh), Text: "refresh"},
		{Key: firstKey(h.keys.Back), Text: "back"},
	}
}

// Subject is this host, plus its first failed unit when it has one.
func (h *Host) Subject() tui.Subject {
	return tui.Subject{
		Hosts: []resource.HostID{h.host},
		Unit:  h.firstFailedUnit(),
		Kind:  linux.KindHostOverview,
		Ref:   h.row.Ref,
	}
}

// Capturing is always false: the overview has no input of its own.
func (h *Host) Capturing() bool { return false }

// CommandLine is the `:` command line that reopens this screen.
func (h *Host) CommandLine() string {
	return "overview --host " + string(h.host)
}

// CopyLines returns the plain text of the screen.
func (h *Host) CopyLines() []string {
	return strings.Split(h.Render(h.w, h.h), "\n")
}
