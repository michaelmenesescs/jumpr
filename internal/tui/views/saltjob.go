package views

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/action"
	"jumpr/internal/backend/linux"
	"jumpr/internal/backend/salt"
	"jumpr/internal/errs"
	"jumpr/internal/group"
	"jumpr/internal/investigation"
	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/targets"
	"jumpr/internal/tui"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
)

// KindSaltJobDetail is the factory key of the job detail screen. It is a view
// kind, not a backend kind: the salt backend serves salt-job.
const KindSaltJobDetail resource.KindName = "salt-job-detail"

const (
	// saltJobPollInterval is how often a running job is re-read.
	saltJobPollInterval = 2 * time.Second
	// saltJobAutoRefresh is the root's table refresh interval while any job
	// is running.
	saltJobAutoRefresh = 5 * time.Second
	// saltJobLookupTimeout bounds one jobs.list_job + jobs.active round trip.
	saltJobLookupTimeout = 15 * time.Second
	// The journal pivot window: one minute before the job started and five
	// minutes after it finished.
	saltJobJournalBefore = time.Minute
	saltJobJournalAfter  = 5 * time.Minute
)

// Layout of one group row in the detail view.
const (
	saltJobHostsWidth = 7  // "HOSTS"
	saltJobShareWidth = 6  // "SHARE"
	saltJobLabelWidth = 10 // "no return"
	saltJobRCWidth    = 3  // "RC"
)

// opNoReturn is the error op of a minion that never answered a finished job.
const opNoReturn = "views.saltjob.no-return"

// SaltJobBackend is the part of the Salt backend the job detail needs. It is
// an interface so the view is testable without a master.
type SaltJobBackend interface {
	Lookup(ctx context.Context, jid string) (salt.JobInfo, error)
	Active(ctx context.Context) (map[string]salt.ActiveJob, error)
	MinionHost(id string) (resource.HostID, bool)
}

// saltJobLoadedMsg is one jobs.list_job answer, tagged with the load Seq so a
// superseded poll is dropped.
type saltJobLoadedMsg struct {
	Seq    int
	Info   salt.JobInfo
	Active bool
	Err    error
}

// saltJobPollMsg asks for another jobs.list_job read.
type saltJobPollMsg struct{ Seq int }

// SaltJob is the grouped detail of one Salt job: the banner counts identical,
// differing and missing returns, each group lists its minions, and a member
// pivots to its host or journal. Update only builds commands; the master is
// read in a tea.Cmd goroutine.
type SaltJob struct {
	env  *tui.Env
	b    SaltJobBackend
	jid  string
	keys *keymap.Map
	// glyphs is the active glyph set: every rune the view draws comes from it.
	glyphs render.GlyphSet

	// base is the pane's context; every load derives from it, so closing the
	// pane stops the polling.
	base context.Context

	seq    int
	cancel context.CancelFunc

	// poll is the re-poll interval of a running job; tests shorten it.
	poll time.Duration

	info    salt.JobInfo
	loaded  bool
	active  bool
	loadErr error

	grouper *group.Grouper
	sum     group.Summary

	cursor       int // group row (groups + error kinds)
	groupCursor  int // the group row the member list was opened from
	memberCursor int
	members      bool // the cursor is inside a group's member list

	// marks are the marked groups, keyed by group key so a re-poll keeps them.
	marks map[string]bool

	// recordView writes the investigation entry once, on the first load.
	recordView func(salt.JobInfo)
	recordOnce sync.Once

	w, h int
}

var _ tui.View = (*SaltJob)(nil)

// NewSaltJob builds the detail view of one Salt job.
func NewSaltJob(env *tui.Env, b SaltJobBackend, jid string) *SaltJob {
	keys := keymap.Default()
	if env != nil && env.Keys != nil {
		keys = env.Keys
	}
	glyphs, _ := render.Glyphs("unicode")
	if env != nil && env.Glyphs.Name != "" {
		glyphs = env.Glyphs
	}
	j := &SaltJob{
		env:    env,
		b:      b,
		jid:    strings.TrimSpace(jid),
		keys:   keys,
		glyphs: glyphs,
		poll:   saltJobPollInterval,
	}
	j.recordView = j.record
	return j
}

// Init starts the first read.
func (j *SaltJob) Init() tea.Cmd { return j.load() }

// SetContext makes every load derive from ctx, so closing the pane stops the
// polling.
func (j *SaltJob) SetContext(ctx context.Context) {
	if ctx != nil {
		j.base = ctx
	}
}

// AutoRefresh is the root's tick interval for this view: a running job is
// re-read every few seconds.
func (j *SaltJob) AutoRefresh() time.Duration {
	if j.active {
		return saltJobAutoRefresh
	}
	return 0
}

// Close cancels a load in flight. The stack calls it when the view is popped.
func (j *SaltJob) Close() error {
	if j.cancel != nil {
		j.cancel()
		j.cancel = nil
	}
	return nil
}

// now returns the clock the environment handed in.
func (j *SaltJob) now() time.Time {
	if j.env != nil && j.env.Now != nil {
		return j.env.Now()
	}
	return time.Now()
}

// baseCtx is the context every load derives from.
func (j *SaltJob) baseCtx() context.Context {
	if j.base != nil {
		return j.base
	}
	return context.Background()
}

// load reads the job and whether it is still running, in one tea.Cmd.
func (j *SaltJob) load() tea.Cmd {
	seq := j.seq
	b, jid := j.b, j.jid
	record := j.recordView
	parent := j.baseCtx()

	ctx, cancel := context.WithCancel(parent)
	j.cancel = cancel

	return func() tea.Msg {
		cctx, ccancel := context.WithTimeout(ctx, saltJobLookupTimeout)
		defer ccancel()
		info, err := b.Lookup(cctx, jid)
		if err != nil {
			return saltJobLoadedMsg{Seq: seq, Err: err}
		}
		// A pillar.* job's arguments and returns stay hidden (task 093): the
		// master keeps them, jumpr does not show or record them.
		info = salt.HideJobInfo(info)
		if info.JID == "" {
			info.JID = jid
		}
		active := false
		if running, aerr := b.Active(cctx); aerr == nil {
			_, active = running[info.JID]
		}
		if record != nil {
			record(info)
		}
		return saltJobLoadedMsg{Seq: seq, Info: info, Active: active}
	}
}

// pollCmd asks for the next read after the poll interval.
func (j *SaltJob) pollCmd() tea.Cmd {
	seq := j.seq
	d := j.poll
	return tea.Tick(d, func(time.Time) tea.Msg { return saltJobPollMsg{Seq: seq} })
}

// record appends the `view` entry of an investigation, once per view.
func (j *SaltJob) record(info salt.JobInfo) {
	if j.env == nil || j.env.Recorder == nil {
		return
	}
	rec := j.env.Recorder
	j.recordOnce.Do(func() {
		if _, open := rec.Current(); !open {
			return
		}
		text := fmt.Sprintf("salt job %s %s", info.JID, info.Function)
		if j.env.Redact != nil {
			text = j.env.Redact.String(text)
		}
		_ = rec.Append(investigation.Entry{
			Time:  j.now(),
			Kind:  investigation.EntryView,
			Actor: action.Human,
			Text:  text,
		})
	})
}

// Update applies a load, schedules the next poll of a running job and routes
// keys. Stale answers are ignored.
func (j *SaltJob) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	switch m := msg.(type) {
	case saltJobLoadedMsg:
		if m.Seq != j.seq {
			return j, nil
		}
		return j, j.apply(m)
	case saltJobPollMsg:
		if m.Seq != j.seq {
			return j, nil
		}
		return j, j.load()
	case tui.MarkedTargetsRequestMsg:
		return j, j.markedTargetsCmd()
	case tea.KeyMsg:
		return j, j.handleKey(m)
	}
	return j, nil
}

// apply folds one answer in and keeps polling while the job is running.
func (j *SaltJob) apply(m saltJobLoadedMsg) tea.Cmd {
	if m.Err != nil {
		j.loadErr = m.Err
		return nil
	}
	j.info = m.Info
	j.active = m.Active
	j.loadErr = nil
	j.loaded = true
	j.rebuild()
	j.clampCursor()

	if j.active {
		return j.pollCmd()
	}
	return nil
}

// rebuild regroups the returns of the current snapshot.
func (j *SaltJob) rebuild() {
	names := j.minionNames()
	g := group.NewGrouper(len(names), nil)
	for _, m := range names {
		mr, ok := j.info.Result[m]
		if !ok {
			// A running job has simply not reported that minion yet; only
			// a finished job can say "no return".
			if j.active {
				continue
			}
			g.AddError(resource.HostID(m), errs.New(errs.Timeout, opNoReturn, "no return"))
			continue
		}
		g.Add(resource.HostID(m), renderJobReturn(j.info.Function, mr.Return), mr.Retcode)
	}
	j.sum = g.Summary()
}

// minionNames is the expected minion set: the union of what the master says
// it targeted and what actually returned, sorted.
func (j *SaltJob) minionNames() []string {
	seen := make(map[string]bool, len(j.info.Minions)+len(j.info.Result))
	out := make([]string, 0, len(j.info.Minions)+len(j.info.Result))
	for _, m := range j.info.Minions {
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	for m := range j.info.Result {
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// dur is the job duration known so far: the elapsed time while it runs, zero
// once the master no longer reports it.
func (j *SaltJob) dur() time.Duration {
	if !j.active || j.info.Start.IsZero() {
		return 0
	}
	if d := j.now().Sub(j.info.Start); d > 0 {
		return d
	}
	return 0
}

// renderJobReturn renders one minion's return the way the grouping wants to
// see it: command output for cmd.run/cmd.run_all, the normalized state summary
// for state functions, compact JSON otherwise.
func renderJobReturn(fun string, raw json.RawMessage) string {
	switch {
	case fun == "cmd.run":
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return strings.TrimRight(s, "\n")
		}
	case fun == "cmd.run_all":
		var w struct {
			Stdout  string `json:"stdout"`
			Stderr  string `json:"stderr"`
			Retcode int    `json:"retcode"`
		}
		if err := json.Unmarshal(raw, &w); err == nil {
			out := strings.TrimRight(w.Stdout, "\n")
			if err := strings.TrimRight(w.Stderr, "\n"); err != "" {
				if out != "" {
					out += "\n"
				}
				out += err
			}
			if w.Retcode != 0 {
				if out != "" {
					out += "\n"
				}
				out += fmt.Sprintf("rc=%d", w.Retcode)
			}
			return out
		}
	case strings.HasPrefix(fun, "state."):
		if run, err := salt.ParseStateReturn(raw); err == nil {
			return run.Normalized()
		}
	}
	return stableJSON(raw)
}

// stableJSON renders raw as compact JSON with sorted keys. Anything that is
// not JSON is passed through untrimmed.
func stableJSON(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return string(trimmed)
	}
	b, err := json.Marshal(v) // maps marshal with sorted keys
	if err != nil {
		return string(trimmed)
	}
	return string(b)
}

// jobArg renders one argument of the job: a string without its quotes,
// anything else as compact JSON.
func jobArg(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return stableJSON(raw)
}

// arguments is the redacted argument list of the job.
func (j *SaltJob) arguments() string {
	parts := make([]string, 0, len(j.info.Arguments))
	for _, raw := range j.info.Arguments {
		if a := jobArg(raw); a != "" {
			parts = append(parts, a)
		}
	}
	text := strings.Join(parts, " ")
	if j.env != nil && j.env.Redact != nil {
		text = j.env.Redact.String(text)
	}
	return text
}

// compactJobTarget renders the job target for the header.
func compactJobTarget(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		switch {
		case len(list) == 0:
			return ""
		case len(list) <= 2:
			return strings.Join(list, ",")
		default:
			return fmt.Sprintf("%s (+%d)", strings.Join(list[:2], ","), len(list)-2)
		}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

// headerLines is the two-line header: the function with its redacted
// arguments, then target, user, start and duration.
func (j *SaltJob) headerLines() []string {
	line1 := strings.TrimSpace(j.info.Function)
	if args := j.arguments(); args != "" {
		if line1 != "" {
			line1 += " "
		}
		line1 += args
	}
	if line1 == "" {
		line1 = "job " + j.jid
	}
	var parts []string
	if target := compactJobTarget(j.info.Target); target != "" {
		parts = append(parts, "target "+target)
	}
	if j.info.User != "" {
		parts = append(parts, "user "+j.info.User)
	}
	if !j.info.Start.IsZero() {
		parts = append(parts, "start "+j.info.Start.Format("15:04:05"))
	}
	switch {
	case j.active:
		parts = append(parts, "running "+render.Duration(j.dur()))
		if j.sum.Total > 0 {
			parts = append(parts, fmt.Sprintf("%d/%d returned", j.sum.Done, j.sum.Total))
		}
	case j.loaded:
		parts = append(parts, "done")
	}
	return []string{line1, strings.Join(parts, " · ")}
}

// handleKey routes one key press. In a member list `j` opens that host's
// journal, which preempts the j/down binding.
func (j *SaltJob) handleKey(m tea.KeyMsg) tea.Cmd {
	key := m.String()
	if j.members && key == "j" {
		return j.openJournal()
	}

	switch {
	case j.keys.Back.Matches(key):
		if j.members {
			j.members = false
			j.cursor = j.groupCursor
			return nil
		}
		return tui.Pop()
	case j.keys.Up.Matches(key):
		j.moveCursor(-1)
		return nil
	case j.keys.Down.Matches(key):
		j.moveCursor(1)
		return nil
	case j.keys.PageUp.Matches(key):
		j.moveCursor(-max(1, j.h-4))
		return nil
	case j.keys.PageDown.Matches(key):
		j.moveCursor(max(1, j.h-4))
		return nil
	case j.keys.Top.Matches(key):
		j.setCursor(0)
		return nil
	case j.keys.Bottom.Matches(key):
		j.setCursor(j.rowCount() - 1)
		return nil
	case j.keys.Open.Matches(key):
		return j.open()
	case j.keys.Refresh.Matches(key):
		return j.load()
	case j.keys.Diff.Matches(key):
		return j.diffCmd()
	}

	switch key {
	case "o":
		return j.openOutput()
	case " ", "space":
		j.toggleMark()
		return nil
	}
	return nil
}

// rowCount is the number of rows in the current mode.
func (j *SaltJob) rowCount() int {
	if j.members {
		if g, ok := j.selectedGroup(); ok {
			return len(g.Hosts)
		}
		return 0
	}
	return len(j.sum.Groups) + len(j.errKinds())
}

// cursorPos is the cursor of the current mode.
func (j *SaltJob) cursorPos() int {
	if j.members {
		return j.memberCursor
	}
	return j.cursor
}

// setCursor moves the cursor of the current mode.
func (j *SaltJob) setCursor(n int) {
	if j.members {
		j.memberCursor = n
	} else {
		j.cursor = n
	}
	j.clampCursor()
}

// moveCursor moves the cursor of the current mode by delta.
func (j *SaltJob) moveCursor(delta int) { j.setCursor(j.cursorPos() + delta) }

// clampCursor keeps the cursor inside the rows of the current mode.
func (j *SaltJob) clampCursor() {
	last := j.rowCount() - 1
	if last < 0 {
		j.cursor, j.memberCursor = 0, 0
		return
	}
	if j.cursorPos() > last {
		j.setCursor(last)
		return
	}
	if j.cursorPos() < 0 {
		j.setCursor(0)
	}
}

// errKinds returns the error kinds of the summary, sorted by kind name.
func (j *SaltJob) errKinds() []errs.Kind {
	names := make([]string, 0, len(j.sum.Errors))
	for k := range j.sum.Errors {
		names = append(names, string(k))
	}
	sort.Strings(names)
	out := make([]errs.Kind, len(names))
	for i, n := range names {
		out[i] = errs.Kind(n)
	}
	return out
}

// selectedGroup returns the group under the cursor, when the cursor is on one.
func (j *SaltJob) selectedGroup() (group.Group, bool) {
	if j.cursor < 0 || j.cursor >= len(j.sum.Groups) {
		return group.Group{}, false
	}
	return j.sum.Groups[j.cursor], true
}

// selectedErrKind returns the error bucket under the cursor, when it is on one.
func (j *SaltJob) selectedErrKind() (errs.Kind, bool) {
	if j.members {
		return "", false
	}
	kinds := j.errKinds()
	i := j.cursor - len(j.sum.Groups)
	if i < 0 || i >= len(kinds) {
		return "", false
	}
	return kinds[i], true
}

// open drills in: a group lists its minions, a member opens its host.
func (j *SaltJob) open() tea.Cmd {
	if j.members {
		return j.openHost()
	}
	if _, ok := j.selectedGroup(); ok {
		j.groupCursor = j.cursor
		j.members = true
		j.memberCursor = 0
		return nil
	}
	if k, ok := j.selectedErrKind(); ok {
		return tui.Flash(tui.FlashWarn, "no return for this minion: %s", k)
	}
	return tui.Flash(tui.FlashInfo, "no rows")
}

// memberMinion is the minion under the cursor of the member list.
func (j *SaltJob) memberMinion() string {
	g, ok := j.selectedGroup()
	if !ok || j.memberCursor < 0 || j.memberCursor >= len(g.Hosts) {
		return ""
	}
	return string(g.Hosts[j.memberCursor])
}

// memberHost resolves a minion onto its inventory host.
func (j *SaltJob) memberHost(minion string) (resource.HostID, bool) {
	if minion == "" || j.b == nil {
		return "", false
	}
	return j.b.MinionHost(minion)
}

// openHost pivots from a member to its host overview.
func (j *SaltJob) openHost() tea.Cmd {
	minion := j.memberMinion()
	host, ok := j.memberHost(minion)
	if !ok {
		return tui.Flash(tui.FlashWarn, "minion %s is not a known host", minion)
	}
	return tui.OpenQuery(resource.Query{Kind: linux.KindHostOverview, Hosts: []resource.HostID{host}})
}

// openJournal pivots from a member to its journal, windowed around the job.
func (j *SaltJob) openJournal() tea.Cmd {
	minion := j.memberMinion()
	host, ok := j.memberHost(minion)
	if !ok {
		return tui.Flash(tui.FlashWarn, "minion %s is not a known host", minion)
	}
	return tui.OpenQuery(resource.Query{
		Kind:   linux.KindJournal,
		Hosts:  []resource.HostID{host},
		Window: j.journalWindow(),
	})
}

// journalWindow is [start − 1m, start + duration + 5m].
func (j *SaltJob) journalWindow() resource.TimeWindow {
	start := j.info.Start
	if start.IsZero() {
		start = j.now()
	}
	return resource.TimeWindow{
		From:  start.Add(-saltJobJournalBefore),
		To:    start.Add(j.dur()).Add(saltJobJournalAfter),
		Label: "job",
	}
}

// diffCmd opens the diff of the selected group against the majority.
func (j *SaltJob) diffCmd() tea.Cmd {
	if j.members {
		return tui.Flash(tui.FlashInfo, "go back to the groups to diff")
	}
	g, ok := j.selectedGroup()
	if !ok {
		return tui.Flash(tui.FlashInfo, "select a group to diff")
	}
	majority, hasMajority := j.sum.Majority()
	if !hasMajority {
		return tui.Flash(tui.FlashInfo, "no groups yet")
	}
	if j.cursor == 0 {
		return tui.Flash(tui.FlashWarn, "the majority group is selected; pick a differing group")
	}
	diff := group.UnifiedDiff("majority", majority.Output,
		fmt.Sprintf("%d minions", len(g.Hosts)), g.Output)
	if strings.TrimSpace(diff) == "" {
		return tui.Flash(tui.FlashInfo, "the group has the same output as the majority")
	}
	var t *theme.Theme
	if j.env != nil {
		t = j.env.Theme
	}
	return func() tea.Msg {
		return tui.OpenDialogMsg{Dialog: tui.NewDiffDialog(t, "diff against majority", diff)}
	}
}

// openOutput shows the selected group's full output.
func (j *SaltJob) openOutput() tea.Cmd {
	g, ok := j.selectedGroup()
	if !ok {
		return tui.Flash(tui.FlashInfo, "select a group to show its output")
	}
	lines := outputLines(g.Output)
	if len(lines) == 0 {
		lines = []string{"(no output)"}
	}
	crumb := fmt.Sprintf("output · %d minions · rc %d", len(g.Hosts), g.RC)
	return tui.Push(NewText(j.env, crumb, lines))
}

// toggleMark marks or unmarks the group under the cursor.
func (j *SaltJob) toggleMark() {
	if j.members {
		return
	}
	g, ok := j.selectedGroup()
	if !ok {
		return
	}
	if j.marks == nil {
		j.marks = make(map[string]bool)
	}
	if j.marks[g.Key] {
		delete(j.marks, g.Key)
		return
	}
	j.marks[g.Key] = true
}

// MarkedHosts returns the hosts of the marked groups, sorted and de-duplicated.
func (j *SaltJob) MarkedHosts() []resource.HostID {
	if len(j.marks) == 0 {
		return nil
	}
	seen := make(map[resource.HostID]bool)
	var out []resource.HostID
	for _, g := range j.sum.Groups {
		if !j.marks[g.Key] {
			continue
		}
		for _, m := range g.Hosts {
			host := m
			if h, ok := j.memberHost(string(m)); ok {
				host = h
			}
			if host == "" || seen[host] {
				continue
			}
			seen[host] = true
			out = append(out, host)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}

// markedTargetsCmd answers `:targets marked`.
func (j *SaltJob) markedTargetsCmd() tea.Cmd {
	hosts := j.MarkedHosts()
	if len(hosts) == 0 {
		return tui.Flash(tui.FlashInfo, "no marked groups")
	}
	return func() tea.Msg {
		return tui.TargetsChangedMsg{Set: targets.FromHosts(fmt.Sprintf("%d marked", len(hosts)), hosts)}
	}
}

// Summary returns the current grouping.
func (j *SaltJob) Summary() group.Summary { return j.sum }

// Render returns exactly h lines of exactly w cells.
func (j *SaltJob) Render(w, h int) string {
	j.w, j.h = w, h
	if w <= 0 || h <= 0 {
		return ""
	}
	lines := []string{j.banner()}
	if j.members {
		lines = append(lines, j.memberTitle(w), j.memberHeader(w))
	} else {
		hdr := j.headerLines()
		lines = append(lines, j.headerLine(hdr[0], w), j.headerLine(hdr[1], w), j.groupHeader(w))
	}
	if visible := h - len(lines); visible > 0 {
		if j.members {
			lines = append(lines, j.memberRows(w, visible)...)
		} else {
			lines = append(lines, j.groupRows(w, visible)...)
		}
	}
	return render.FitWith(j.glyphs, strings.Join(lines, "\n"), w, h)
}

// banner is the summary line: the headline, then the error if a refresh
// failed. A missing minion reads "no return" here, not "errors".
func (j *SaltJob) banner() string {
	var text string
	switch {
	case j.loaded:
		text = j.headline()
		if j.loadErr != nil {
			text += " · refresh failed"
		}
	case j.loadErr != nil:
		text = "error: " + j.loadErr.Error()
	default:
		text = "loading job " + j.jid + " " + string(j.glyphs.Ellipsis)
	}
	if j.env != nil && j.env.Theme != nil {
		text = j.env.Theme.Dim.Render(text)
	}
	return text
}

// headline is the grouped summary. The timeout bucket is the "no return"
// group of this view.
func (j *SaltJob) headline() string {
	text := j.sum.Headline()
	if len(j.sum.Errors) == 1 && len(j.sum.Errors[errs.Timeout]) > 0 {
		text = strings.TrimSuffix(text, " errors") + " no return"
	}
	if j.active {
		text += " · " + render.Duration(j.dur())
	}
	return text
}

// headerLine renders one header line, dimmed and truncated.
func (j *SaltJob) headerLine(s string, w int) string {
	s = render.Truncate(s, w)
	if j.env != nil && j.env.Theme != nil {
		s = j.env.Theme.Dim.Render(s)
	}
	return render.PadRight(s, w)
}

// groupHeader is the column title row of the group list.
func (j *SaltJob) groupHeader(w int) string {
	line := render.PadLeft("MINIONS", saltJobHostsWidth) + " " +
		render.PadLeft("SHARE", saltJobShareWidth) + " " +
		render.PadRight("GROUP", saltJobLabelWidth) + " " +
		render.PadLeft("RC", saltJobRCWidth) + " " +
		"OUTPUT"
	line = render.Truncate(line, w)
	if j.env != nil && j.env.Theme != nil {
		line = j.env.Theme.Header.Render(line)
	}
	return render.PadRight(line, w)
}

// groupRows renders the visible group and error rows.
func (j *SaltJob) groupRows(w, visible int) []string {
	start := 0
	if j.cursor >= visible {
		start = j.cursor - visible + 1
	}
	out := make([]string, 0, visible)
	for i := start; i < start+visible && i < j.rowCount(); i++ {
		out = append(out, j.rowLine(i, w))
	}
	return out
}

// rowLine renders one group or one error bucket.
func (j *SaltJob) rowLine(i, w int) string {
	groups := j.sum.Groups
	if i < len(groups) {
		return j.groupLine(i, groups[i], w)
	}
	kinds := j.errKinds()
	idx := i - len(groups)
	if idx < 0 || idx >= len(kinds) {
		return render.PadRight("", w)
	}
	k := kinds[idx]
	list := j.sum.Errors[k]
	label := string(k)
	if k == errs.Timeout {
		label = "no return"
	}
	line := " " + " " + render.PadLeft(fmt.Sprintf("%d", len(list)), saltJobHostsWidth) + " " +
		render.PadLeft(render.Percent(100*shareOf(len(list), j.sum.Total)), saltJobShareWidth) + " " +
		render.PadRight(label, saltJobLabelWidth) + " " +
		render.PadLeft("", saltJobRCWidth) + " " + "(no return)"
	line = render.PadRight(render.Truncate(line, w), w)
	if j.env != nil && j.env.Theme != nil {
		if i == j.cursor {
			return j.env.Theme.Selected.Render(line)
		}
		return j.env.Theme.Crit.Render(line)
	}
	return line
}

// groupLine renders one group: its size, share, majority/differs label, exit
// code and the first output line.
func (j *SaltJob) groupLine(i int, g group.Group, w int) string {
	label := "differs"
	if i == 0 {
		label = "majority"
	}
	out := firstOutputLine(g.Output)
	if n := len(outputLines(g.Output)); n > 1 {
		out += fmt.Sprintf(" (+%d lines)", n-1)
	}
	line := j.markCell(g) + " " +
		render.PadLeft(fmt.Sprintf("%d", len(g.Hosts)), saltJobHostsWidth) + " " +
		render.PadLeft(render.Percent(shareOf(len(g.Hosts), j.sum.Total)), saltJobShareWidth) + " " +
		render.PadRight(label, saltJobLabelWidth) + " " +
		render.PadLeft(fmt.Sprintf("%d", g.RC), saltJobRCWidth) + " " +
		out
	line = render.PadRight(render.Truncate(line, w), w)
	if j.env != nil && j.env.Theme != nil {
		if i == j.cursor && !j.members {
			return j.env.Theme.Selected.Render(line)
		}
		return j.env.Theme.Base.Render(line)
	}
	return line
}

// shareOf is the percentage of total that n is, 100 when total is unknown.
func shareOf(n, total int) float64 {
	if total <= 0 {
		if n > 0 {
			return 100
		}
		return 0
	}
	return 100 * float64(n) / float64(total)
}

// markCell is the one-cell gutter: the mark glyph for a marked group.
func (j *SaltJob) markCell(g group.Group) string {
	if j.marks[g.Key] {
		if j.glyphs.ASCII() {
			return ">"
		}
		return "▸"
	}
	return " "
}

// memberTitle names the group the member list belongs to.
func (j *SaltJob) memberTitle(w int) string {
	g, ok := j.selectedGroup()
	if !ok {
		return render.PadRight("", w)
	}
	text := fmt.Sprintf("minions of a group of %d · rc %d · %s", len(g.Hosts), g.RC, firstOutputLine(g.Output))
	text = render.Truncate(text, w)
	if j.env != nil && j.env.Theme != nil {
		text = j.env.Theme.Dim.Render(text)
	}
	return render.PadRight(text, w)
}

// memberHeader is the column title row of the member list.
func (j *SaltJob) memberHeader(w int) string {
	line := render.PadRight("MINION", membersHostWidth) + " OUTPUT"
	line = render.Truncate(line, w)
	if j.env != nil && j.env.Theme != nil {
		line = j.env.Theme.Header.Render(line)
	}
	return render.PadRight(line, w)
}

// memberRows renders the visible minions of the selected group.
func (j *SaltJob) memberRows(w, visible int) []string {
	g, ok := j.selectedGroup()
	if !ok {
		return nil
	}
	start := 0
	if j.memberCursor >= visible {
		start = j.memberCursor - visible + 1
	}
	out := firstOutputLine(g.Output)
	lines := make([]string, 0, visible)
	for i := start; i < start+visible && i < len(g.Hosts); i++ {
		line := render.PadRight(render.Truncate(string(g.Hosts[i]), membersHostWidth), membersHostWidth) +
			" " + out
		line = render.PadRight(render.Truncate(line, w), w)
		if j.env != nil && j.env.Theme != nil {
			if i == j.memberCursor {
				line = j.env.Theme.Selected.Render(line)
			} else {
				line = j.env.Theme.Base.Render(line)
			}
		}
		lines = append(lines, line)
	}
	return lines
}

// Crumb is "job <jid>".
func (j *SaltJob) Crumb() string { return "job " + j.jid }

// Hints are the keys the view answers.
func (j *SaltJob) Hints() []tui.KeyHint {
	if j.members {
		return []tui.KeyHint{
			{Key: firstKey(j.keys.Open), Text: "host"},
			{Key: "j", Text: "journal"},
			{Key: firstKey(j.keys.Back), Text: "groups"},
		}
	}
	return []tui.KeyHint{
		{Key: firstKey(j.keys.Open), Text: "minions"},
		{Key: firstKey(j.keys.Diff), Text: "diff"},
		{Key: "o", Text: "output"},
		{Key: "space", Text: "mark"},
		{Key: firstKey(j.keys.Refresh), Text: "refresh"},
		{Key: firstKey(j.keys.Back), Text: "back"},
	}
}

// Subject is the job and every minion it touched.
func (j *SaltJob) Subject() tui.Subject {
	names := j.minionNames()
	hosts := make([]resource.HostID, 0, len(names))
	for _, m := range names {
		hosts = append(hosts, resource.HostID(m))
	}
	return tui.Subject{
		Hosts: hosts,
		Kind:  salt.KindJobs,
		Ref:   resource.Ref{Backend: "salt", Kind: salt.KindJobs, Name: j.jid},
	}
}

// Capturing is always false: the detail has no input of its own.
func (j *SaltJob) Capturing() bool { return false }

// CommandLine is the `:` command line that reopens this screen.
func (j *SaltJob) CommandLine() string {
	return "salt-job-detail --arg jid=" + j.jid
}

// CopyLines returns the plain text of the view.
func (j *SaltJob) CopyLines() []string {
	lines := []string{j.headline()}
	hdr := j.headerLines()
	lines = append(lines, hdr...)
	for i, g := range j.sum.Groups {
		label := "differs"
		if i == 0 {
			label = "majority"
		}
		lines = append(lines, fmt.Sprintf("%s  %d minions  rc %d", label, len(g.Hosts), g.RC))
		hosts := make([]string, len(g.Hosts))
		for k, h := range g.Hosts {
			hosts[k] = string(h)
		}
		lines = append(lines, strings.Join(hosts, ", "))
		lines = append(lines, outputLines(g.Output)...)
	}
	for _, k := range j.errKinds() {
		label := string(k)
		if k == errs.Timeout {
			label = "no return"
		}
		list := j.sum.Errors[k]
		hosts := make([]string, len(list))
		for i, he := range list {
			hosts[i] = string(he.Host)
		}
		lines = append(lines, fmt.Sprintf("%s  %d minions", label, len(list)))
		lines = append(lines, strings.Join(hosts, ", "))
	}
	return lines
}
