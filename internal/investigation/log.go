package investigation

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"jumpr/internal/action"
	"jumpr/internal/bus"
	"jumpr/internal/errs"
	"jumpr/internal/group"
	"jumpr/internal/redact"
	"jumpr/internal/resource"
)

// FileLog is the recorder over a Markdown investigation root: it appends to the
// investigation that is currently open, or to the day's audit file when none
// is (SPEC §10). It implements Log and bus.Recorder and is safe for concurrent
// use: every write takes the mutex and lands through the locked, atomic
// append of file.go.
type FileLog struct {
	root string
	red  *redact.Redactor
	now  func() time.Time

	mu  sync.Mutex
	cur *Meta // nil = no investigation open
}

var _ Log = (*FileLog)(nil)
var _ bus.Recorder = (*FileLog)(nil)

// NewLog builds the recorder for root. now defaults to time.Now.
func NewLog(root string, red *redact.Redactor, now func() time.Time) *FileLog {
	if now == nil {
		now = time.Now
	}
	return &FileLog{root: root, red: red, now: now}
}

// Root is the directory investigations and the audit file live under.
func (l *FileLog) Root() string {
	if l == nil {
		return ""
	}
	return l.root
}

// AuditPath is the audit file a day's entries go to.
func (l *FileLog) AuditPath(day time.Time) string {
	if l == nil || l.root == "" {
		return ""
	}
	return filepath.Join(l.root, dirAudit, day.Format("2006-01-02")+".md")
}

// List returns every investigation under the root, newest first.
func (l *FileLog) List() ([]Meta, error) {
	if l == nil {
		return nil, nil
	}
	return List(l.root)
}

// Open creates a new investigation, appends its `open` entry and makes it
// current.
func (l *FileLog) Open(title string, window resource.TimeWindow, targets []resource.HostID, alerts []string) (Meta, error) {
	const op = "investigation.log.open"
	title = strings.TrimSpace(title)
	if title == "" {
		return Meta{}, errs.New(errs.Config, op, "an investigation needs a title")
	}
	if l.root == "" {
		return Meta{}, errs.New(errs.Config, op, "no investigation root configured")
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	m := Meta{
		Title:   title,
		Status:  "open",
		Opened:  now,
		Window:  window,
		Targets: append([]resource.HostID(nil), targets...),
		Alerts:  append([]string(nil), alerts...),
	}
	path, err := Create(l.root, m)
	if err != nil {
		return Meta{}, err
	}
	// Create may have suffixed a taken id; the path is the truth. The targets
	// stay ours: the frontmatter folds long host lists, so reading the meta
	// back would lose everything past the fold.
	m.ID = strings.TrimSuffix(filepath.Base(path), ".md")
	m.Path = path
	entry := Entry{Time: now, Kind: EntryOpen, Actor: action.Human, Text: openText(m)}
	if err := AppendEntry(m.Path, entry, l.red); err != nil {
		return Meta{}, err
	}
	l.cur = &m
	return copyMeta(m), nil
}

// openText is the text of the `open` bullet: the quoted title and the target
// count. The window is in the frontmatter, where it belongs.
func openText(m Meta) string {
	text := "`" + strings.ReplaceAll(m.Title, "`", "'") + "`"
	if n := len(m.Targets); n > 0 {
		text += fmt.Sprintf(" on %d %s", n, plural(n, "host"))
	}
	return text
}

// plural returns word for one and word+s otherwise.
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// Resume makes an existing investigation current again.
func (l *FileLog) Resume(id string) (Meta, error) {
	const op = "investigation.log.resume"
	id = strings.TrimSpace(id)
	if id == "" {
		return Meta{}, errs.New(errs.Config, op, "resume needs an investigation id")
	}
	if l.root == "" {
		return Meta{}, errs.New(errs.Config, op, "no investigation root configured")
	}
	meta, err := ReadMeta(filepath.Join(l.root, dirInvestigations, id+".md"))
	if err != nil {
		return Meta{}, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.cur = &meta
	return copyMeta(meta), nil
}

// Close forgets the current investigation. Nothing is written: closing is a
// session decision, resolving is the record.
func (l *FileLog) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cur = nil
}

// Resolve appends the `resolve` entry, writes the root cause under
// ## Resolution and marks the frontmatter resolved.
func (l *FileLog) Resolve(rootCause string) error {
	const op = "investigation.log.resolve"
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.cur == nil {
		return errs.New(errs.Config, op, "no investigation is open")
	}
	rootCause = strings.TrimSpace(rootCause)
	entry := Entry{Time: l.now(), Kind: EntryResolve, Actor: action.Human, Text: rootCause}
	if err := AppendEntry(l.cur.Path, entry, l.red); err != nil {
		return err
	}
	if err := SetStatus(l.cur.Path, "resolved", redText(l.red, rootCause)); err != nil {
		return err
	}
	l.cur.Status = "resolved"
	return nil
}

// Append writes one entry: to the current investigation, or to the audit file
// when none is open. Only action, denied and input entries reach the audit
// file; everything else is dropped silently.
func (l *FileLog) Append(e Entry) error {
	if l == nil || l.root == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appendLocked(e)
}

// appendLocked writes e. l.mu must be held.
func (l *FileLog) appendLocked(e Entry) error {
	if e.Time.IsZero() {
		e.Time = l.now()
	}
	if e.Actor == "" {
		e.Actor = action.Human
	}
	if l.cur != nil {
		return AppendEntry(l.cur.Path, e, l.red)
	}
	if !auditKind(e.Kind) {
		return nil
	}
	return AppendAudit(l.root, e, l.red, e.Time)
}

// auditKind reports whether a kind belongs in the audit file.
func auditKind(k EntryKind) bool {
	switch k {
	case EntryAction, EntryDenied, EntryInput:
		return true
	}
	return false
}

// Current returns the open investigation, or ok=false when none is.
func (l *FileLog) Current() (Meta, bool) {
	if l == nil {
		return Meta{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur == nil {
		return Meta{}, false
	}
	return copyMeta(*l.cur), true
}

// copyMeta returns a copy that does not alias the log's own slices.
func copyMeta(m Meta) Meta {
	m.Targets = append([]resource.HostID(nil), m.Targets...)
	m.Alerts = append([]string(nil), m.Alerts...)
	m.Tags = append([]string(nil), m.Tags...)
	return m
}

// RecordAction appends the `action` bullet for a finished rollout, with the
// plan and the grouped per-host results attached.
func (l *FileLog) RecordAction(p bus.Proposal, s bus.Summary, results []action.StepResult) error {
	if l == nil || l.root == "" {
		return nil
	}
	entry := Entry{
		Time:   l.now(),
		Kind:   EntryAction,
		Actor:  proposalActor(p),
		Text:   actionText(p, s),
		Output: actionOutput(p, results),
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appendLocked(entry)
}

// RecordDenied appends the `denied` bullet naming the rule that refused the
// plan.
func (l *FileLog) RecordDenied(p bus.Proposal) error {
	if l == nil || l.root == "" {
		return nil
	}
	entry := Entry{
		Time:  l.now(),
		Kind:  EntryDenied,
		Actor: proposalActor(p),
		Text:  deniedText(p),
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appendLocked(entry)
}

// proposalActor is who asked for the plan.
func proposalActor(p bus.Proposal) action.Actor {
	if a := p.Plan.Request.Actor; a != "" {
		return a
	}
	return action.Human
}

// planTitle is the one-line description of a plan.
func planTitle(p bus.Proposal) string {
	if t := strings.TrimSpace(p.Plan.Title); t != "" {
		return strings.ReplaceAll(t, "`", "'")
	}
	if t := strings.TrimSpace(p.Plan.Spec.Title); t != "" {
		return strings.ReplaceAll(t, "`", "'")
	}
	return strings.ReplaceAll(strings.TrimSpace(p.Plan.Spec.ID), "`", "'")
}

// actionText renders "`restart nginx.service on 12 hosts` → ok 12/12, 2 failed".
func actionText(p bus.Proposal, s bus.Summary) string {
	total := s.OK + s.Failed + s.Skipped
	text := fmt.Sprintf("`%s` → ok %d/%d", planTitle(p), s.OK, total)
	var rest []string
	if s.Failed > 0 {
		rest = append(rest, fmt.Sprintf("%d failed", s.Failed))
	}
	if s.Skipped > 0 {
		rest = append(rest, fmt.Sprintf("%d skipped", s.Skipped))
	}
	if len(rest) > 0 {
		text += ", " + strings.Join(rest, ", ")
	}
	return text
}

// deniedText renders "`restart nginx.service` denied: <rule>".
func deniedText(p bus.Proposal) string {
	text := "`" + planTitle(p) + "` denied"
	if p.Denied == nil {
		return text
	}
	rule := strings.TrimSpace(p.Denied.Rule)
	if rule == "" {
		rule = strings.TrimSpace(p.Denied.Reason)
	}
	if rule == "" {
		return text
	}
	return text + ": " + rule
}

// outputHostsShown is how many hosts of a group the attachment names.
const outputHostsShown = 3

// outputLinesShown is how many lines of one group's output are attached.
const outputLinesShown = 3

// actionOutput renders the attachment: the plan's distinct step displays, then
// one block per group of identical results, so 500 identical outputs cost one
// block rather than 500.
func actionOutput(p bus.Proposal, results []action.StepResult) string {
	var b strings.Builder

	if displays := distinctDisplays(p.Plan.Steps); len(displays) > 0 {
		b.WriteString("plan:\n")
		for _, d := range displays {
			b.WriteString("  " + d + "\n")
		}
	}
	if len(results) == 0 {
		return b.String()
	}

	g := group.NewGrouper(len(results), nil)
	for _, r := range results {
		if r.Err != nil {
			g.AddError(r.Target.Host, r.Err)
			continue
		}
		g.Add(r.Target.Host, r.Output, r.RC)
	}
	sum := g.Summary()

	b.WriteString("results:\n")
	for _, gr := range sum.Groups {
		fmt.Fprintf(&b, "  rc=%d on %d %s", gr.RC, len(gr.Hosts), plural(len(gr.Hosts), "host"))
		if hosts := hostsShown(gr.Hosts); hosts != "" {
			b.WriteString(" (" + hosts + ")")
		}
		b.WriteString(":\n")
		for _, ln := range firstLines(gr.Output, outputLinesShown) {
			b.WriteString("    " + ln + "\n")
		}
	}

	kinds := make([]string, 0, len(sum.Errors))
	for k := range sum.Errors {
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		list := sum.Errors[errs.Kind(k)]
		hosts := make([]resource.HostID, 0, len(list))
		for _, he := range list {
			hosts = append(hosts, he.Host)
		}
		fmt.Fprintf(&b, "  %d %s failed (%s)\n", len(list), plural(len(list), "host"), k)
		if shown := hostsShown(hosts); shown != "" {
			b.WriteString("    " + shown + "\n")
		}
	}
	return b.String()
}

// distinctDisplays returns the step displays a plan uses, in order and without
// repeats: one plan over 500 hosts usually carries one display per host kind.
func distinctDisplays(steps []action.Step) []string {
	seen := make(map[string]bool, len(steps))
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		d := strings.TrimSpace(s.Display)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// firstLines returns the first n non-empty lines of s, trimmed.
func firstLines(s string, n int) []string {
	out := make([]string, 0, n)
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimRight(ln, "\r"); strings.TrimSpace(ln) == "" {
			continue
		}
		out = append(out, ln)
		if len(out) == n {
			break
		}
	}
	return out
}

// hostsShown names at most outputHostsShown hosts, then the remainder.
func hostsShown(hosts []resource.HostID) string {
	if len(hosts) == 0 {
		return ""
	}
	shown := make([]string, 0, outputHostsShown)
	for i, h := range hosts {
		if i == outputHostsShown {
			break
		}
		shown = append(shown, string(h))
	}
	out := strings.Join(shown, ", ")
	if len(hosts) > len(shown) {
		out += fmt.Sprintf(" +%d more", len(hosts)-len(shown))
	}
	return out
}
