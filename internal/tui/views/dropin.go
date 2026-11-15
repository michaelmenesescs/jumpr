package views

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/action"
	"jumpr/internal/backend/linux"
	"jumpr/internal/investigation"
	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/tui"
	"jumpr/internal/tui/theme"
)

// dropInBanner is the first line of the preview and the whole point of the
// screen: jumpr shows the change, the user applies it.
const dropInBanner = "preview only — jumpr does not write files on nodes"

// dropInDefault is the draft a unit without an override starts from.
const dropInDefault = "[Service]\n"

// dropInClipboard is what the preview's `y` copies into;
// internal/clip.Clipboard satisfies it. Writing to the clipboard is I/O, so it
// only ever runs inside a tea.Cmd the view returns.
type dropInClipboard interface {
	Copy(text string) error
}

// dropInMode is which half of the screen is shown.
type dropInMode int

const (
	dropInEdit dropInMode = iota
	dropInPreview
)

// DropIn is the drop-in override editor of one systemd unit: a text area
// pre-filled from the unit's existing override.conf, and on ctrl+s the preview
// of what that drop-in would change, the warnings, and the commands that apply
// it by hand. Nothing is fetched, planned or written: the cat output is the one
// the unit detail already loaded (SPEC §8.1.1, "preview only in v1").
type DropIn struct {
	env  *tui.Env
	host resource.HostID
	unit string
	cat  string
	path string
	// existing is true when the cat output already carries the drop-in.
	existing bool

	ta    textarea.Model
	mode  dropInMode
	pv    linux.DropInPreview
	view  *Text
	clip  dropInClipboard
	blink tea.Cmd

	w, h int
}

var _ tui.View = (*DropIn)(nil)

// NewDropIn builds the drop-in editor of one unit over the `systemctl cat`
// output the unit detail already loaded. An existing override.conf pre-fills
// the editor; otherwise it starts from an empty [Service] section.
func NewDropIn(env *tui.Env, host resource.HostID, unit, cat string) *DropIn {
	d := &DropIn{
		env:  env,
		host: resource.NormalizeHostID(string(host)),
		unit: strings.TrimSpace(unit),
		cat:  cat,
	}
	d.path = linux.DropInPath(d.unit)

	ta := textarea.New()
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetWidth(dropInWidth)
	ta.SetHeight(dropInHeight)
	body, ok := linux.DropInBody(cat, d.path)
	d.existing = ok && strings.TrimSpace(body) != ""
	if d.existing {
		body = strings.TrimRight(body, "\n") + "\n"
	} else {
		body = dropInDefault
	}
	ta.SetValue(body)
	d.blink = ta.Focus()
	d.ta = ta
	return d
}

// Editor sizing before the first Render.
const (
	dropInWidth  = 60
	dropInHeight = 12
)

// SetClipboard installs where the preview yanks the apply-by-hand block.
// Without one `y` flashes a warning, like the root's own yank does.
func (d *DropIn) SetClipboard(c dropInClipboard) { d.clip = c }

// Value is the draft as it stands in the editor.
func (d *DropIn) Value() string { return d.ta.Value() }

// SetValue replaces the draft. The editor is pre-filled by NewDropIn; this is
// for a caller that starts from something else.
func (d *DropIn) SetValue(text string) { d.ta.SetValue(text) }

// Previewing reports whether the diff is shown instead of the editor.
func (d *DropIn) Previewing() bool { return d.mode == dropInPreview }

// Preview is the last preview built by ctrl+s.
func (d *DropIn) Preview() linux.DropInPreview { return d.pv }

// Init starts the editor's cursor blink.
func (d *DropIn) Init() tea.Cmd { return d.blink }

// Update drives the text area in edit mode and the preview in preview mode.
// ctrl+s builds the preview (and records the note), `y` yanks the block, `esc`
// goes back to the editor.
func (d *DropIn) Update(msg tea.Msg) (tui.View, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return d, d.forward(msg)
	}
	switch d.mode {
	case dropInEdit:
		switch k.String() {
		case "ctrl+s":
			return d, d.open()
		case "esc":
			return d, tui.Pop()
		}
		ta, cmd := d.ta.Update(k)
		d.ta = ta
		return d, cmd
	default:
		switch k.String() {
		case "y":
			return d, d.yank()
		case "esc":
			d.mode = dropInEdit
			return d, nil
		case "ctrl+s":
			return d, d.open()
		}
		return d, d.forward(msg)
	}
}

// forward hands a non-key message (a window size, a paste) to the shown view.
func (d *DropIn) forward(msg tea.Msg) tea.Cmd {
	if d.mode == dropInPreview && d.view != nil {
		v, cmd := d.view.Update(msg)
		if t, ok := v.(*Text); ok {
			d.view = t
		}
		return cmd
	}
	if d.mode == dropInEdit {
		ta, cmd := d.ta.Update(msg)
		d.ta = ta
		return cmd
	}
	return nil
}

// open builds the preview from the draft and shows it. The preview is pure, but
// recording it in an open investigation writes a file, so that part runs in the
// returned command.
func (d *DropIn) open() tea.Cmd {
	d.pv = linux.PreviewDropIn(d.unit, d.cat, d.ta.Value())
	d.mode = dropInPreview
	d.view = NewText(d.env, "drop-in "+d.unit, d.previewLines())
	return tea.Batch(d.view.Init(), d.note())
}

// note records the preview as one note entry of the open investigation: the
// headline plus the draft and the diff attached, so the file says what was
// proposed without ever having run.
func (d *DropIn) note() tea.Cmd {
	if d.env == nil || d.env.Recorder == nil {
		return nil
	}
	rec, red, now := d.env.Recorder, d.env.Redact, d.now()
	p := d.pv
	return func() tea.Msg {
		if _, open := rec.Current(); !open {
			return nil
		}
		text := fmt.Sprintf("drop-in draft for %s on %s (preview only)", p.Unit, d.host)
		output := p.Text
		if p.Diff != "" {
			output += "\n\n" + p.Diff
		}
		if red != nil {
			text = red.String(text)
			output = red.String(output)
		}
		_ = rec.Append(investigation.Entry{
			Time:   now,
			Kind:   investigation.EntryNote,
			Actor:  action.Human,
			Text:   text,
			Output: output,
		})
		return nil
	}
}

// yank copies the apply-by-hand block. The clipboard write is I/O, so it runs in
// the returned command.
func (d *DropIn) yank() tea.Cmd {
	if d.mode != dropInPreview || strings.TrimSpace(d.pv.Apply) == "" {
		return nil
	}
	c := d.clip
	if c == nil {
		return tui.Flash(tui.FlashWarn, "no clipboard configured")
	}
	block := d.pv.Apply
	return func() tea.Msg {
		if err := c.Copy(block); err != nil {
			return tui.FlashMsg{Level: tui.FlashWarn, Text: "copy: " + err.Error()}
		}
		return tui.FlashMsg{Level: tui.FlashOK, Text: "apply-by-hand block copied"}
	}
}

// previewLines is the preview text view: the banner, the drop-in path, the
// warnings, the diff and the block to paste.
func (d *DropIn) previewLines() []string {
	st := d.style()
	lines := []string{st.Warn.Render(dropInBanner), ""}

	head := st.Dim.Render("drop-in") + "  " + st.Accent.Render(d.path)
	if !d.existing {
		head += "  " + st.Dim.Render("(new)")
	}
	lines = append(lines, head, "")

	lines = append(lines, st.Dim.Render("warnings"))
	if len(d.pv.Warnings) == 0 {
		lines = append(lines, "  "+st.Faint.Render("none"))
	}
	for _, w := range d.pv.Warnings {
		lines = append(lines, "  "+st.Warn.Render(w))
	}
	lines = append(lines, "")

	lines = append(lines, st.Dim.Render("diff · systemctl cat now → with the drop-in"))
	if strings.TrimSpace(d.pv.Diff) == "" {
		lines = append(lines, st.Faint.Render("  no changes: the drop-in repeats what the unit files already say"))
	} else {
		for _, line := range strings.Split(strings.TrimRight(d.pv.Diff, "\n"), "\n") {
			lines = append(lines, d.diffLine(line))
		}
	}
	lines = append(lines, "")

	return append(lines, strings.Split(strings.TrimRight(d.pv.Apply, "\n"), "\n")...)
}

// diffLine colours one line of the unified diff.
func (d *DropIn) diffLine(line string) string {
	st := d.style()
	switch {
	case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		return st.Dim.Render(line)
	case strings.HasPrefix(line, "@@"):
		return st.Accent.Render(line)
	case strings.HasPrefix(line, "+"):
		return st.OK.Render(line)
	case strings.HasPrefix(line, "-"):
		return st.Crit.Render(line)
	}
	return line
}

// style returns the theme, falling back to the default palette.
func (d *DropIn) style() *theme.Theme {
	if d.env != nil && d.env.Theme != nil {
		return d.env.Theme
	}
	return theme.Dark()
}

// now is the clock handed in by the environment.
func (d *DropIn) now() time.Time {
	if d.env != nil && d.env.Now != nil {
		return d.env.Now()
	}
	return time.Now()
}

// Render returns exactly h lines of exactly w cells: the editor with its header
// and footer, or the preview.
func (d *DropIn) Render(w, h int) string {
	d.w, d.h = w, h
	if w <= 0 || h <= 0 {
		return ""
	}
	if d.mode == dropInPreview && d.view != nil {
		return d.view.Render(w, h)
	}
	st := d.style()
	d.ta.SetWidth(w)
	if h > 2 {
		d.ta.SetHeight(h - 2)
	}
	head := st.Dim.Render(render.Truncate("drop-in · "+d.unit+" · "+d.path, w))
	foot := st.Dim.Render(render.Truncate("ctrl+s preview · esc close", w))
	return render.Fit(strings.Join([]string{head, d.ta.View(), foot}, "\n"), w, h)
}

// Crumb is the unit this draft belongs to.
func (d *DropIn) Crumb() string { return "drop-in " + d.unit }

// Hints are the keys of the shown half of the screen.
func (d *DropIn) Hints() []tui.KeyHint {
	if d.mode == dropInPreview {
		return []tui.KeyHint{
			{Key: "y", Text: "copy the block"},
			{Key: "ctrl+s", Text: "rebuild"},
			{Key: "esc", Text: "back to the editor"},
		}
	}
	return []tui.KeyHint{
		{Key: "ctrl+s", Text: "preview"},
		{Key: "esc", Text: "close"},
	}
}

// Subject is this unit on this host.
func (d *DropIn) Subject() tui.Subject {
	return tui.Subject{
		Hosts: []resource.HostID{d.host},
		Unit:  d.unit,
		Kind:  linux.KindUnitDetail,
		Ref:   resource.Ref{Backend: "linux", Kind: linux.KindUnitDetail, Host: d.host, Name: d.unit},
	}
}

// Capturing is always true: the editor owns the keyboard, and the preview needs
// `y` and `esc` for itself.
func (d *DropIn) Capturing() bool { return true }

// CopyLines is the plain text of the shown half of the screen.
func (d *DropIn) CopyLines() []string {
	if d.mode == dropInPreview && d.view != nil {
		return d.view.CopyLines()
	}
	return strings.Split(d.ta.Value(), "\n")
}

// openDropIn opens the drop-in editor of this unit from the cat tab. Nothing is
// fetched: the draft starts from the `systemctl cat` output already loaded, so
// the editor is the same screen whether or not the node is reachable now.
func (u *Unit) openDropIn() tea.Cmd {
	if !u.done || u.err != nil {
		return tui.Flash(tui.FlashWarn, "still loading %s", u.name)
	}
	return tui.Push(NewDropIn(u.env, u.host, u.name, u.info.Cat))
}
