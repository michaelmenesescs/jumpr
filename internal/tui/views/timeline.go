package views

import (
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/investigation"
	"jumper/internal/tui"
)

// timelineLinesMsg is the loaded content of the record file.
type timelineLinesMsg struct {
	Lines []string
	Err   error
}

// Timeline is the investigation timeline: a Text view over the record file
// itself, so what the user sees is the Markdown that will be in git. It reloads
// whenever the root reports an append.
type Timeline struct {
	env  *tui.Env
	text *Text
	path string // the last file read, "" when nothing is open
}

var _ tui.View = (*Timeline)(nil)

// NewTimeline builds the timeline for the session.
func NewTimeline(env *tui.Env) *Timeline {
	t := &Timeline{env: env}
	t.text = NewText(env, "timeline", t.placeholder())
	return t
}

// Init loads the record.
func (t *Timeline) Init() tea.Cmd { return t.reload() }

// Update reloads on the root's refresh message and owns `o` and `R`; every
// other message belongs to the text view underneath.
func (t *Timeline) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	switch m := msg.(type) {
	case tui.InvestigationRefreshMsg:
		return t, t.reload()
	case timelineLinesMsg:
		if m.Err != nil {
			t.text.SetLines(append(t.placeholder(), "", "cannot read the record: "+m.Err.Error()))
			return t, nil
		}
		t.text.SetLines(m.Lines)
		return t, nil
	case tea.KeyMsg:
		key := m.String()
		switch {
		case key == "o":
			if t.path == "" {
				return t, tui.Flash(tui.FlashWarn, "no investigation open")
			}
			return t, tui.Flash(tui.FlashInfo, "%s", t.path)
		case key == "R":
			return t, func() tea.Msg { return tui.InvestigationResolveRequestMsg{} }
		}
	}
	v, cmd := t.text.Update(msg)
	if next, ok := v.(*Text); ok {
		t.text = next
	}
	return t, cmd
}

// reload reads the record in a command: the file read never happens in Update.
func (t *Timeline) reload() tea.Cmd {
	path := t.target()
	t.path = path
	missing := t.placeholder()
	return func() tea.Msg {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			return timelineLinesMsg{Lines: strings.Split(string(data), "\n")}
		case os.IsNotExist(err):
			// Nothing written yet (or no investigation at all): the hint text
			// is the content.
			return timelineLinesMsg{Lines: missing}
		default:
			return timelineLinesMsg{Err: err}
		}
	}
}

// target is the file the timeline shows: the open investigation, or the day's
// audit file when none is open.
func (t *Timeline) target() string {
	if t.env == nil {
		return ""
	}
	if meta := t.openMeta(); meta != nil && meta.Path != "" {
		return meta.Path
	}
	if l, ok := t.env.Recorder.(interface{ AuditPath(time.Time) string }); ok {
		now := time.Now
		if t.env.Now != nil {
			now = t.env.Now
		}
		return l.AuditPath(now())
	}
	return ""
}

// openMeta is the session's investigation, if there is one.
func (t *Timeline) openMeta() *investigation.Meta {
	if t.env == nil || t.env.Session == nil {
		return nil
	}
	return t.env.Session.Investigation
}

// placeholder is what the view shows before the first read and when there is no
// investigation to show.
func (t *Timeline) placeholder() []string {
	if meta := t.openMeta(); meta != nil {
		return []string{
			"record: " + meta.Path,
			"",
			"no entries yet — n adds a note, F records the selected line as a finding",
		}
	}
	lines := []string{
		"no investigation is open",
		"",
		":inv open <title>   start one with the session's window and targets",
		":inv                list the stored investigations and resume one",
		"n                   add a note to the open investigation",
		"",
		"until then only actions, denials and synchronized input are recorded, in the audit file:",
		"  " + t.auditPath(),
	}
	return lines
}

// auditPath is where an audit entry would land today, or a note when the
// recorder cannot say.
func (t *Timeline) auditPath() string {
	if t.env == nil {
		return "(no investigation root configured)"
	}
	if l, ok := t.env.Recorder.(interface{ AuditPath(time.Time) string }); ok {
		now := time.Now
		if t.env.Now != nil {
			now = t.env.Now
		}
		if p := l.AuditPath(now()); p != "" {
			return p
		}
	}
	return "(no investigation root configured)"
}

// Render delegates to the text view.
func (t *Timeline) Render(w, h int) string { return t.text.Render(w, h) }

// Crumb is "timeline".
func (t *Timeline) Crumb() string { return "timeline" }

// Hints adds the timeline keys to the text view's.
func (t *Timeline) Hints() []tui.KeyHint {
	hints := t.text.Hints()
	hints = append(hints,
		tui.KeyHint{Key: "o", Text: "path"},
		tui.KeyHint{Key: "R", Text: "resolve"},
	)
	return hints
}

// Subject is the open investigation's hosts.
func (t *Timeline) Subject() tui.Subject {
	if meta := t.openMeta(); meta != nil {
		return tui.Subject{Hosts: meta.Targets}
	}
	return tui.Subject{}
}

// Capturing is true while the text view's search owns the keyboard.
func (t *Timeline) Capturing() bool { return t.text.Capturing() }

// CopyLines is the plain text of the record.
func (t *Timeline) CopyLines() []string { return t.text.CopyLines() }
