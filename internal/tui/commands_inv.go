package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/action"
	"jumpr/internal/backend/linux"
	"jumpr/internal/investigation"
	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/tui/keymap"
)

// KindTimeline is the synthetic view kind of the investigation timeline. No
// backend serves it: internal/tui/views registers the factory under this name
// and the root pushes it for `:timeline` and `T`.
const KindTimeline resource.KindName = "timeline"

// Messages of the investigation lifecycle. The root owns all of them; views
// only emit them.

// InvestigationAppendedMsg reports that one entry reached the record. The root
// answers by refreshing every open timeline.
type InvestigationAppendedMsg struct{}

// InvestigationRefreshMsg tells a timeline view that the record changed and it
// should reload. The root sends it after every append.
type InvestigationRefreshMsg struct{}

// TimelineRequestedMsg asks the root to push the timeline view.
type TimelineRequestedMsg struct{}

// InvestigationResolveRequestMsg asks the root to prompt for the one-line root
// cause and resolve the open investigation.
type InvestigationResolveRequestMsg struct{}

// knowledgeRememberMsg asks every view pane to offer filing the resolved root
// cause in a knowledge note. The resolve command emits it; the pane knows the
// unit and the alert of what it is showing.
type knowledgeRememberMsg struct {
	Cause  string
	Alerts []string
}

// Investigations are appended and read through the log the env carries.

// invLifecycle is the part of the recorder the commands need beyond the frozen
// investigation.Log interface (BUILD-PLAN §4.10): creating, resuming, closing
// and resolving a record. internal/investigation.FileLog implements it.
type invLifecycle interface {
	investigation.Log
	Open(title string, window resource.TimeWindow, targets []resource.HostID, alerts []string) (investigation.Meta, error)
	Resume(id string) (investigation.Meta, error)
	Close()
	Resolve(rootCause string) error
}

// invLog returns env's recorder when it can manage the investigation lifecycle.
func invLog(env *Env) (invLifecycle, bool) {
	if env == nil || env.Recorder == nil {
		return nil, false
	}
	l, ok := env.Recorder.(invLifecycle)
	return l, ok
}

// invLister is the part of the recorder the list view needs.
type invLister interface {
	List() ([]investigation.Meta, error)
}

// AppendInvestigation returns a command that writes one entry. It returns nil
// when no recorder is configured; the write itself never happens in Update.
func (e *Env) AppendInvestigation(kind investigation.EntryKind, text, output string) tea.Cmd {
	if e == nil || e.Recorder == nil {
		return nil
	}
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	entry := investigation.Entry{Time: now(), Kind: kind, Actor: action.Human, Text: text, Output: output}
	inv := e.Recorder
	return func() tea.Msg {
		if err := inv.Append(entry); err != nil {
			return FlashMsg{Level: FlashError, Text: "investigation: " + err.Error()}
		}
		return InvestigationAppendedMsg{}
	}
}

// RecordFinding appends text as a `finding` and flashes the result. It refuses
// when no investigation is open instead of writing to the audit file, because a
// finding without an investigation has no home.
func (e *Env) RecordFinding(text string) tea.Cmd {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if e == nil || e.Recorder == nil {
		return Flash(FlashWarn, "no investigation log configured")
	}
	if _, open := e.Recorder.Current(); !open {
		return Flash(FlashWarn, "no investigation open")
	}
	first, rest := splitSelection(text)
	cmd := e.AppendInvestigation(investigation.EntryFinding, first, rest)
	if cmd == nil {
		return Flash(FlashWarn, "no investigation log configured")
	}
	return tea.Batch(cmd, Flash(FlashOK, "finding recorded"))
}

// splitSelection turns a copied selection into an entry: the first line is the
// bullet's text, the rest is attached output.
func splitSelection(text string) (first, rest string) {
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		return strings.TrimSpace(text[:i]), strings.TrimSpace(text[i+1:])
	}
	return text, ""
}

// RecordView returns the command appending the automatic `view` entry for
// opening q, when opening it is an event worth recording and an investigation
// is open. Plain table navigation records nothing.
func (e *Env) RecordView(q resource.Query) tea.Cmd {
	if e == nil || e.Recorder == nil {
		return nil
	}
	entry := AutoViewEntry(q)
	if entry == nil {
		return nil
	}
	if _, open := e.Recorder.Current(); !open {
		return nil
	}
	return e.AppendInvestigation(entry.Kind, entry.Text, entry.Output)
}

// AutoViewEntry is the timeline entry that opening q records, or nil when
// opening q is not an event: only a host overview counts.
func AutoViewEntry(q resource.Query) *investigation.Entry {
	if q.Kind != linux.KindHostOverview {
		return nil
	}
	var host resource.HostID
	switch {
	case len(q.Hosts) > 0:
		host = resource.NormalizeHostID(string(q.Hosts[0]))
	case strings.TrimSpace(q.Args["host"]) != "":
		host = resource.NormalizeHostID(q.Args["host"])
	}
	if host == "" {
		return nil
	}
	return &investigation.Entry{Kind: investigation.EntryView, Text: "host overview " + string(host)}
}

// InvestigationTargets is the host list of the open investigation, for the
// `@investigation` selector. Nil when none is open.
func InvestigationTargets(env *Env) []resource.HostID {
	if env == nil || env.Session == nil || env.Session.Investigation == nil {
		return nil
	}
	return env.Session.Investigation.Targets
}

// RegisterInvCommands registers the investigation commands on c. The root calls
// it; registering twice is an error from Commands.Register, which callers may
// ignore.
func RegisterInvCommands(c *Commands) error {
	if c == nil {
		return nil
	}
	return c.Register(invCommand())
}

// invCommand is the `:inv` family plus `:note` and `:timeline`.
func invCommand() Command {
	return Command{
		Name:  "inv",
		Usage: ":inv [open <title>|close|resolve|resume <id>]",
		Help:  "open, resume, resolve or close an investigation",
		Run:   runInv,
		Complete: func(env *Env, args string) []string {
			word := partialWord(args)
			fields := strings.Fields(args)
			if len(fields) == 0 {
				return filterPrefix([]string{"close", "open", "resolve", "resume"}, word)
			}
			if fields[0] == "resume" {
				return filterPrefix(investigationIDs(env), word)
			}
			return nil
		},
	}
}

// investigationIDs lists the ids of the stored investigations for completion.
func investigationIDs(env *Env) []string {
	lister, ok := invListerOf(env)
	if !ok {
		return nil
	}
	metas, err := lister.List()
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(metas))
	for _, m := range metas {
		ids = append(ids, m.ID)
	}
	return ids
}

// invListerOf returns env's recorder when it can list the stored records.
func invListerOf(env *Env) (invLister, bool) {
	if env == nil || env.Recorder == nil {
		return nil, false
	}
	l, ok := env.Recorder.(invLister)
	return l, ok
}

// runInv dispatches the `:inv` argument text. No branch blocks: the file work
// runs inside the returned command.
func runInv(env *Env, args string) tea.Cmd {
	args = strings.TrimSpace(args)
	if args == "" {
		return Push(&invListView{env: env})
	}

	word, rest := splitFirstWord(args)
	rest = strings.TrimSpace(rest)
	switch word {
	case "open":
		return openInvestigation(env, rest)
	case "resume":
		return resumeInvestigation(env, rest)
	case "close":
		return closeInvestigation(env)
	case "resolve":
		return resolveInvestigation(env, rest)
	}
	return Flash(FlashError, "usage: :inv [open <title>|close|resolve|resume <id>]")
}

// openInvestigation opens a new investigation with the session's window and
// targets.
func openInvestigation(env *Env, title string) tea.Cmd {
	title = strings.TrimSpace(title)
	if title == "" {
		return Flash(FlashError, "usage: :inv open <title>")
	}
	inv, ok := invLog(env)
	if !ok {
		return Flash(FlashWarn, "no investigation log configured")
	}
	var window resource.TimeWindow
	var hosts []resource.HostID
	if env.Session != nil {
		window = env.Session.Window
		hosts = append([]resource.HostID(nil), env.Session.Targets.Hosts...)
	}
	return func() tea.Msg {
		meta, err := inv.Open(title, window, hosts, nil)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: err.Error()}
		}
		return InvestigationChangedMsg{Meta: &meta}
	}
}

// resumeInvestigation makes an existing investigation current.
func resumeInvestigation(env *Env, id string) tea.Cmd {
	id = strings.TrimSpace(id)
	if id == "" {
		return Flash(FlashError, "usage: :inv resume <id>")
	}
	inv, ok := invLog(env)
	if !ok {
		return Flash(FlashWarn, "no investigation log configured")
	}
	return func() tea.Msg {
		meta, err := inv.Resume(id)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: err.Error()}
		}
		return InvestigationChangedMsg{Meta: &meta}
	}
}

// closeInvestigation forgets the current investigation. Nothing is written.
func closeInvestigation(env *Env) tea.Cmd {
	inv, ok := invLog(env)
	if !ok {
		return Flash(FlashWarn, "no investigation log configured")
	}
	return func() tea.Msg {
		inv.Close()
		return InvestigationChangedMsg{Meta: nil}
	}
}

// resolveInvestigation resolves the record with the given root cause, or asks
// for one when none was typed. A resolved record offers its root cause to the
// knowledge base afterwards.
func resolveInvestigation(env *Env, rootCause string) tea.Cmd {
	rootCause = strings.TrimSpace(rootCause)
	inv, ok := invLog(env)
	if !ok {
		return Flash(FlashWarn, "no investigation log configured")
	}
	if rootCause == "" {
		if _, open := env.Recorder.Current(); !open {
			return Flash(FlashWarn, "no investigation open")
		}
		d := NewPromptDialog(themeOf(env), "resolve investigation", "root cause:", "", func(text string) tea.Cmd {
			return resolveWith(env, inv, text)
		})
		return func() tea.Msg { return OpenDialogMsg{Dialog: d} }
	}
	return resolveWith(env, inv, rootCause)
}

// resolveWith writes the resolution and, in the same batch, asks the panes to
// offer filing the root cause in a knowledge note.
func resolveWith(env *Env, inv invLifecycle, rootCause string) tea.Cmd {
	rootCause = strings.TrimSpace(rootCause)
	if rootCause == "" {
		return nil
	}
	return tea.Batch(
		func() tea.Msg {
			if err := inv.Resolve(rootCause); err != nil {
				return FlashMsg{Level: FlashError, Text: err.Error()}
			}
			meta, ok := inv.Current()
			if !ok {
				return InvestigationChangedMsg{Meta: nil}
			}
			return InvestigationChangedMsg{Meta: &meta}
		},
		func() tea.Msg {
			meta, ok := inv.Current()
			if !ok {
				return nil
			}
			return knowledgeRememberMsg{Cause: rootCause, Alerts: meta.Alerts}
		},
	)
}

// kbCommand is `:kb`: the knowledge base itself.
func kbCommand() Command {
	return Command{
		Name:  "kb",
		Usage: ":kb [new host|service|alert|runbook <name>|reindex]",
		Help:  "create a knowledge note, or reindex the knowledge base",
		Run:   runKB,
		Complete: func(_ *Env, args string) []string {
			word := partialWord(args)
			fields := strings.Fields(args)
			if len(fields) == 0 {
				return filterPrefix([]string{investigation.NoteKindAlert, investigation.NoteKindHost,
					investigation.NoteKindRunbook, investigation.NoteKindService, "new", "reindex"}, word)
			}
			if len(fields) == 1 && fields[0] == "new" {
				return filterPrefix([]string{investigation.NoteKindAlert, investigation.NoteKindHost,
					investigation.NoteKindRunbook, investigation.NoteKindService}, word)
			}
			return nil
		},
	}
}

// runKB dispatches `:kb`. The file work runs inside the returned command.
func runKB(env *Env, args string) tea.Cmd {
	args = strings.TrimSpace(args)
	if args == "" {
		return Flash(FlashError, "usage: :kb [new <type> <name>|reindex]")
	}
	word, rest := splitFirstWord(args)
	switch word {
	case "new":
		return newKnowledgeNote(env, rest)
	case "reindex":
		return func() tea.Msg { return knowledgeReindexMsg{} }
	}
	return Flash(FlashError, "usage: :kb [new <type> <name>|reindex]")
}

// newKnowledgeNote creates (or opens) a note and shows it. Editing stays in the
// user's editor outside jumpr, so the path is flashed.
func newKnowledgeNote(env *Env, rest string) tea.Cmd {
	kind, name := splitFirstWord(strings.TrimSpace(rest))
	kind = strings.ToLower(strings.TrimSpace(kind))
	name = strings.TrimSpace(name)
	if kind == "" || name == "" {
		return Flash(FlashError, "usage: :kb new <host|service|alert|runbook> <name>")
	}
	root := knowledgeRoot(env)
	if root == "" {
		return Flash(FlashWarn, "no knowledge base configured")
	}
	path, err := investigation.NotePath(root, kind, name)
	if err != nil {
		return Flash(FlashError, "%v", err)
	}
	note := relToRoot(root, path)
	return func() tea.Msg {
		_, created, cerr := investigation.CreateNote(root, kind, name)
		if cerr != nil {
			return FlashMsg{Level: FlashError, Text: cerr.Error()}
		}
		flash := "opened " + note
		if created {
			flash = "created " + note
		}
		return PushViewMsg{View: newNoteView(env, path, flash)}
	}
}

// noteCommand is `:note <text>`: one free-text line in the timeline.
func noteCommand() Command {
	return Command{
		Name:  "note",
		Usage: ":note <text>",
		Help:  "add a note to the investigation",
		Run: func(env *Env, args string) tea.Cmd {
			text := strings.TrimSpace(args)
			if text == "" {
				return Flash(FlashError, "usage: :note <text>")
			}
			if env == nil || env.Recorder == nil {
				return Flash(FlashWarn, "no investigation log configured")
			}
			if _, open := env.Recorder.Current(); !open {
				return Flash(FlashWarn, "no investigation open")
			}
			return env.AppendInvestigation(investigation.EntryNote, text, "")
		},
	}
}

// timelineCommand is `:timeline`: show the record as Markdown.
func timelineCommand() Command {
	return Command{
		Name:  "timeline",
		Usage: ":timeline",
		Help:  "show the investigation timeline",
		Run: func(*Env, string) tea.Cmd {
			return func() tea.Msg { return TimelineRequestedMsg{} }
		},
	}
}

// RegisterInvestigationCommands registers `:inv`, `:note`, `:timeline` and the
// knowledge base command `:kb`.
func RegisterInvestigationCommands(c *Commands) error {
	if c == nil {
		return nil
	}
	if err := RegisterInvCommands(c); err != nil {
		return err
	}
	if err := c.Register(noteCommand()); err != nil {
		return err
	}
	if err := c.Register(timelineCommand()); err != nil {
		return err
	}
	return c.Register(kbCommand())
}

// invListMsg carries the loaded investigation list into the list view.
type invListMsg struct {
	Metas []investigation.Meta
	Err   error
}

// invListView lists the stored investigations; enter resumes the one under the
// cursor through the `:inv resume` line, so there is one code path.
type invListView struct {
	env    *Env
	metas  []investigation.Meta
	cursor int
	err    error
	loaded bool
}

var _ View = (*invListView)(nil)

// Init loads the list. The file read happens in the command, never in Update.
func (v *invListView) Init() tea.Cmd {
	lister, ok := invListerOf(v.env)
	if !ok {
		return nil
	}
	return func() tea.Msg {
		metas, err := lister.List()
		return invListMsg{Metas: metas, Err: err}
	}
}

// Update handles the load result and the cursor keys.
func (v *invListView) Update(msg tea.Msg) (View, tea.Cmd) {
	switch m := msg.(type) {
	case invListMsg:
		v.loaded = true
		v.metas = m.Metas
		v.err = m.Err
		v.clampCursor()
		return v, nil
	case tea.KeyMsg:
		return v, v.handleKey(m)
	}
	return v, nil
}

// handleKey moves the cursor, resumes on enter and pops on esc.
func (v *invListView) handleKey(m tea.KeyMsg) tea.Cmd {
	keys := keymap.Default()
	if v.env != nil && v.env.Keys != nil {
		keys = v.env.Keys
	}
	key := m.String()
	switch {
	case keys.Up.Matches(key):
		v.cursor--
		v.clampCursor()
	case keys.Down.Matches(key):
		v.cursor++
		v.clampCursor()
	case keys.Top.Matches(key):
		v.cursor = 0
	case keys.Bottom.Matches(key):
		v.cursor = len(v.metas) - 1
		v.clampCursor()
	case keys.Open.Matches(key):
		if v.cursor < 0 || v.cursor >= len(v.metas) {
			return Flash(FlashInfo, "no investigation selected")
		}
		return func() tea.Msg {
			return RunCommandMsg{Line: "inv resume " + v.metas[v.cursor].ID}
		}
	case keys.Back.Matches(key):
		return Pop()
	}
	return nil
}

// clampCursor keeps the cursor inside the list.
func (v *invListView) clampCursor() {
	if len(v.metas) == 0 {
		v.cursor = 0
		return
	}
	if v.cursor < 0 {
		v.cursor = 0
	}
	if v.cursor >= len(v.metas) {
		v.cursor = len(v.metas) - 1
	}
}

// Render draws the list, one line per investigation.
func (v *invListView) Render(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	lines := []string{v.title()}
	switch {
	case !v.loaded:
		lines = append(lines, "", "loading"+string(glyphsFor(v.env).Ellipsis))
	case v.err != nil:
		lines = append(lines, "", v.err.Error())
	case len(v.metas) == 0:
		lines = append(lines, "", "no investigations yet: :inv open <title>")
	default:
		lines = append(lines, "")
		for i, m := range v.metas {
			line := fmt.Sprintf("%-40s %-9s %s", m.ID, m.Status, m.Opened.Format("2006-01-02 15:04"))
			if i == v.cursor {
				if t := themeOf(v.env); t != nil {
					line = t.Selected.Render(line)
				}
			}
			lines = append(lines, " "+line)
		}
	}
	return render.Fit(strings.Join(lines, "\n"), w, h)
}

// title is the header line, styled when a theme is present.
func (v *invListView) title() string {
	const t = "investigations"
	if th := themeOf(v.env); th != nil {
		return th.Header.Render(t)
	}
	return strings.ToUpper(t)
}

// Crumb is the breadcrumb of the list.
func (v *invListView) Crumb() string { return "investigations" }

// Hints lists resume and back.
func (v *invListView) Hints() []KeyHint {
	return []KeyHint{
		{Key: "enter", Text: "resume"},
		{Key: "esc", Text: "back"},
	}
}

// Subject is empty: the list is about no single resource.
func (v *invListView) Subject() Subject { return Subject{} }

// Capturing is false: the list owns no text input.
func (v *invListView) Capturing() bool { return false }

// CopyLines is the plain text of the list.
func (v *invListView) CopyLines() []string {
	lines := []string{"INVESTIGATIONS"}
	for _, m := range v.metas {
		lines = append(lines, fmt.Sprintf("%s  %s  %s", m.ID, m.Status, m.Opened.Format("2006-01-02 15:04")))
	}
	return lines
}

// glyphsFor returns the active glyph set, unicode when none was resolved.
func glyphsFor(env *Env) render.GlyphSet {
	if env != nil && env.Glyphs.Name != "" {
		return env.Glyphs
	}
	set, _ := render.Glyphs("unicode")
	return set
}
