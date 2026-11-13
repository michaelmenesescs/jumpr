package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/action"
	"jumper/internal/render"
	"jumper/internal/resource"
	"jumper/internal/tui/theme"
)

// The dialogs of the action flow before there is a plan: the actions menu, the
// parameter form and the planning spinner (BUILD-PLAN §4.5, SPEC §11). They are
// pure: nothing here does I/O, every remote round-trip is a tea.Cmd.

// planningTick is how often the planning spinner advances.
const planningTick = 90 * time.Millisecond

// planningFrames is the spinner, in order.
var planningFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// planningTickMsg advances the planning spinner.
type planningTickMsg struct{}

// actionsMenu is the list of specs every planner offers for the selection.
// Destructive specs are marked with a "!" in the gutter.
type actionsMenu struct {
	t     *theme.Theme
	kind  resource.KindName
	specs []action.Spec
	sel   int
	pick  func(action.Spec) tea.Cmd
}

var _ Dialog = (*actionsMenu)(nil)

// newActionsMenu builds the menu over specs. pick runs with the chosen spec on
// enter; nil pick only closes the dialog.
func newActionsMenu(t *theme.Theme, kind resource.KindName, specs []action.Spec, pick func(action.Spec) tea.Cmd) Dialog {
	return &actionsMenu{t: t, kind: kind, specs: specs, pick: pick}
}

// Title is the dialog's border title.
func (d *actionsMenu) Title() string {
	if d.kind == "" {
		return "actions"
	}
	return "actions · " + string(d.kind)
}

// Update moves the selection and picks on enter.
func (d *actionsMenu) Update(msg tea.Msg) (Dialog, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return d, nil
	}
	last := len(d.specs) - 1
	switch k.String() {
	case "esc", "q":
		return d, CloseDialog()
	case "enter":
		if d.pick == nil || d.sel < 0 || d.sel >= len(d.specs) {
			return d, CloseDialog()
		}
		// The pick alone: the root replaces this dialog when it handles the
		// message. A batched CloseDialog would race the pick and could close
		// the dialog the pick just opened.
		spec := d.specs[d.sel]
		return d, d.pick(spec)
	case "up", "k":
		if d.sel > 0 {
			d.sel--
		}
	case "down", "j":
		if d.sel < last {
			d.sel++
		}
	case "home", "g":
		d.sel = 0
	case "end", "G":
		if last >= 0 {
			d.sel = last
		}
	}
	return d, nil
}

// View renders the specs and a footer, exactly maxW cells wide.
func (d *actionsMenu) View(maxW, maxH int) string {
	if maxW < 1 || maxH < 1 {
		return ""
	}
	rows := maxH - 1
	if rows < 1 {
		rows = 1
	}
	start := 0
	if d.sel >= rows {
		start = d.sel - rows + 1
	}
	lines := make([]string, 0, rows+1)
	for i := 0; i < rows; i++ {
		idx := start + i
		if idx >= len(d.specs) {
			lines = append(lines, "")
			continue
		}
		spec := d.specs[idx]
		text := render.Truncate(specLine(spec), maxW)
		if d.t != nil {
			if idx == d.sel {
				text = d.t.Selected.Render(text)
			} else if spec.Risk == action.RiskDestructive {
				text = d.t.Crit.Render(text)
			} else {
				text = d.t.Base.Render(text)
			}
		}
		lines = append(lines, render.PadRight(text, maxW))
	}
	footer := "enter choose  esc close"
	if d.t != nil {
		footer = d.t.Dim.Render(footer)
	}
	lines = append(lines, render.PadRight(footer, maxW))
	return strings.Join(lines, "\n")
}

// specLine is one menu row: the destructive marker, the title and the spec ID.
func specLine(spec action.Spec) string {
	mark := " "
	if spec.Risk == action.RiskDestructive {
		mark = "!"
	}
	title := spec.Title
	if title == "" {
		title = spec.ID
	}
	line := mark + " " + title
	if spec.Title != "" && spec.ID != spec.Title {
		line += "  " + spec.ID
	}
	return line
}

// paramField is one parameter of the form: a text input, plus the index of the
// chosen value when the parameter offers a fixed list.
type paramField struct {
	param  action.Param
	input  textinput.Model
	choice int // -1 = free text
}

// paramForm asks for the parameters of a spec: one text input per param,
// defaults filled in, choices cycling with left and right.
type paramForm struct {
	t      *theme.Theme
	spec   action.Spec
	fields []paramField
	cur    int
	note   string
	submit func(action.Spec, map[string]string) tea.Cmd
}

var _ Dialog = (*paramForm)(nil)

// newParamForm builds the form of spec. submit runs with the spec and the
// parameter values on enter; nil submit only closes the dialog.
func newParamForm(t *theme.Theme, spec action.Spec, submit func(action.Spec, map[string]string) tea.Cmd) Dialog {
	fields := make([]paramField, 0, len(spec.Params))
	for _, p := range spec.Params {
		in := textinput.New()
		in.Prompt = ""
		in.SetValue(p.Default)
		in.CursorEnd()
		in.Width = 40
		choice := -1
		for i, c := range p.Choices {
			if c == p.Default {
				choice = i
			}
		}
		fields = append(fields, paramField{param: p, input: in, choice: choice})
	}
	if len(fields) > 0 {
		fields[0].input.Focus()
	}
	return &paramForm{t: t, spec: spec, fields: fields, submit: submit}
}

// Title is the dialog's border title.
func (d *paramForm) Title() string {
	title := d.spec.Title
	if title == "" {
		title = d.spec.ID
	}
	return title
}

// Update edits the fields; enter submits, esc is handled by the root.
func (d *paramForm) Update(msg tea.Msg) (Dialog, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		var next tea.Cmd
		if d.cur >= 0 && d.cur < len(d.fields) {
			d.fields[d.cur].input, next = d.fields[d.cur].input.Update(msg)
		}
		return d, next
	}
	switch k.String() {
	case "esc":
		return d, CloseDialog()
	case "enter":
		return d, d.accept()
	case "tab", "down":
		d.move(1)
		return d, nil
	case "shift+tab", "up":
		d.move(-1)
		return d, nil
	case "left":
		d.cycle(-1)
		return d, nil
	case "right":
		d.cycle(1)
		return d, nil
	}
	// Free text goes to the focused input; a choice field keeps its value.
	f := &d.fields[d.cur]
	if f.choice >= 0 {
		return d, nil
	}
	var next tea.Cmd
	f.input, next = f.input.Update(k)
	return d, next
}

// move focuses another field.
func (d *paramForm) move(delta int) {
	if len(d.fields) == 0 {
		return
	}
	d.fields[d.cur].input.Blur()
	d.cur = (d.cur + delta + len(d.fields)) % len(d.fields)
	d.fields[d.cur].input.Focus()
}

// cycle steps through the focused field's choices, when it has any.
func (d *paramForm) cycle(delta int) {
	if len(d.fields) == 0 {
		return
	}
	f := &d.fields[d.cur]
	if len(f.param.Choices) == 0 {
		return
	}
	if f.choice < 0 {
		f.choice = 0
	} else {
		f.choice = (f.choice + delta + len(f.param.Choices)) % len(f.param.Choices)
	}
	f.input.SetValue(f.param.Choices[f.choice])
	f.input.CursorEnd()
}

// accept validates the required parameters and submits the form.
func (d *paramForm) accept() tea.Cmd {
	values := make(map[string]string, len(d.fields))
	for _, f := range d.fields {
		v := strings.TrimSpace(f.input.Value())
		if v == "" && f.param.Required {
			name := f.param.Title
			if name == "" {
				name = f.param.Name
			}
			d.note = name + " is required"
			return nil
		}
		values[f.param.Name] = v
	}
	if d.submit == nil {
		return CloseDialog()
	}
	spec := d.spec
	// Submit alone: the root replaces this dialog with the planning spinner
	// when it handles the message (see actionsMenu.Update).
	return d.submit(spec, values)
}

// View renders one label/input pair per parameter and a footer.
func (d *paramForm) View(maxW, maxH int) string {
	if maxW < 1 || maxH < 1 {
		return ""
	}
	lines := make([]string, 0, len(d.fields)*2+2)
	for i, f := range d.fields {
		label := f.param.Title
		if label == "" {
			label = f.param.Name
		}
		if f.param.Required {
			label += " *"
		}
		if len(f.param.Choices) > 0 {
			label += " (" + strings.Join(f.param.Choices, "/") + ")"
		}
		f.input.Width = max(1, maxW-2)
		line := f.input.View()
		if i == d.cur && d.t != nil {
			label = d.t.Accent.Render(label)
		}
		lines = append(lines, render.Truncate(label, maxW))
		lines = append(lines, render.PadRight(line, maxW))
	}
	if d.note != "" {
		note := d.note
		if d.t != nil {
			note = d.t.Warn.Render(note)
		}
		lines = append(lines, render.Truncate(note, maxW))
	}
	rows := maxH - 1
	if rows < 1 {
		rows = 1
	}
	if len(lines) > rows {
		lines = lines[:rows]
	}
	for len(lines) < rows {
		lines = append(lines, "")
	}
	footer := "enter run  left/right choose  esc cancel"
	if d.t != nil {
		footer = d.t.Dim.Render(footer)
	}
	lines = append(lines, render.PadRight(footer, maxW))
	return strings.Join(lines, "\n")
}

// planningDialog is shown while the bus plans an action: a spinner, the word
// "planning…" and nothing else. esc cancels the planning context (the root
// keeps the cancel function).
type planningDialog struct {
	t     *theme.Theme
	frame int
}

var _ Dialog = (*planningDialog)(nil)

// newPlanningDialog builds the spinner.
func newPlanningDialog(t *theme.Theme) Dialog { return &planningDialog{t: t} }

// Title is the dialog's border title.
func (d *planningDialog) Title() string { return "planning" }

// Update advances the spinner.
func (d *planningDialog) Update(msg tea.Msg) (Dialog, tea.Cmd) {
	if _, ok := msg.(planningTickMsg); !ok {
		return d, nil
	}
	d.frame++
	return d, planningNext()
}

// planningNext schedules the next spinner frame.
func planningNext() tea.Cmd {
	return tea.Tick(planningTick, func(time.Time) tea.Msg { return planningTickMsg{} })
}

// View renders the spinner line, exactly maxW cells wide.
func (d *planningDialog) View(maxW, maxH int) string {
	if maxW < 1 || maxH < 1 {
		return ""
	}
	spinner := planningFrames[d.frame%len(planningFrames)]
	line := fmt.Sprintf("%s planning…", spinner)
	if d.t != nil {
		line = d.t.Dim.Render(line)
	}
	lines := []string{render.PadRight(render.Truncate(line, maxW), maxW)}
	for len(lines) < maxH-1 {
		lines = append(lines, "")
	}
	footer := "esc cancel"
	if d.t != nil {
		footer = d.t.Dim.Render(footer)
	}
	lines = append(lines, render.PadRight(footer, maxW))
	if len(lines) > maxH {
		lines = lines[:maxH]
	}
	return strings.Join(lines, "\n")
}
