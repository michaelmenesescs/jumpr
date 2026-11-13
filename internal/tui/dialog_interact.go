package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/interact"
	"jumper/internal/render"
	"jumper/internal/tui/theme"
)

// Answer strings the dialogs produce and the prompter caches. They match the
// choices interact.Prompt declares for each kind (BUILD-PLAN §4.8.1).
const (
	choiceOnce   = "once"
	choiceAlways = "always"
	choiceReject = "reject"
	choiceYes    = "yes"
	choiceNo     = "no"
)

// interactDialog shows one interact.Prompt (host key, passphrase, confirm) and
// answers it exactly once. It owns no I/O: the answer leaves through the
// Reply channel of the PromptMsg that created it, from a tea.Cmd.
type interactDialog struct {
	t      *theme.Theme
	prompt interact.Prompt
	input  textinput.Model
	reply  chan<- interact.Answer
	done   bool
}

var _ Dialog = (*interactDialog)(nil)

// NewInteractDialog returns the dialog for p. reply receives the answer; it is
// the buffered channel from the PromptMsg, so the send never blocks. A secret
// prompt gets a masked text input whose value is cleared on submit.
func NewInteractDialog(t *theme.Theme, p interact.Prompt, reply chan<- interact.Answer) Dialog {
	d := &interactDialog{t: t, prompt: p, reply: reply}
	if d.masked() {
		in := textinput.New()
		in.Prompt = ""
		in.EchoMode = textinput.EchoPassword
		in.Focus()
		d.input = in
	}
	return d
}

// masked reports whether the prompt takes a secret typed by the user.
func (d *interactDialog) masked() bool {
	return d.prompt.Secret || d.prompt.Kind == interact.Passphrase
}

// Title is the dialog's border title.
func (d *interactDialog) Title() string { return promptName(d.prompt) }

// Update answers or edits the input, depending on the kind.
func (d *interactDialog) Update(msg tea.Msg) (Dialog, tea.Cmd) {
	if d.done {
		return d, nil
	}
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		if d.masked() {
			var next tea.Cmd
			d.input, next = d.input.Update(msg)
			return d, next
		}
		return d, nil
	}
	switch d.prompt.Kind {
	case interact.HostKeyUnknown:
		switch k.String() {
		case "o":
			return d.answer(interact.Answer{Choice: choiceOnce})
		case "a":
			return d.answer(interact.Answer{Choice: choiceAlways})
		case "r":
			return d.answer(interact.Answer{Choice: choiceReject})
		case "esc":
			return d.answer(interact.Answer{Choice: choiceReject, Canceled: true})
		}
	case interact.Passphrase:
		switch k.Type { //nolint:exhaustive // every other key goes to the input
		case tea.KeyEsc:
			d.clearInput()
			return d.answer(interact.Answer{Canceled: true})
		case tea.KeyEnter:
			secret := d.input.Value()
			if secret == "" {
				return d, nil
			}
			d.clearInput()
			return d.answer(interact.Answer{Text: secret})
		}
		var next tea.Cmd
		d.input, next = d.input.Update(k)
		return d, next
	case interact.Confirm:
		switch k.String() {
		case "y", "Y":
			return d.answer(interact.Answer{Choice: d.choice(0, choiceYes)})
		case "n", "N":
			return d.answer(interact.Answer{Choice: d.choice(1, choiceNo)})
		case "esc":
			return d.answer(interact.Answer{Choice: d.choice(1, choiceNo), Canceled: true})
		}
	}
	return d, nil
}

// clearInput drops the typed secret from the dialog.
func (d *interactDialog) clearInput() {
	d.input.SetValue("")
	d.input.Blur()
}

// choice returns the i-th declared choice, or fallback when the prompt does not
// declare it.
func (d *interactDialog) choice(i int, fallback string) string {
	if i < len(d.prompt.Choices) && d.prompt.Choices[i] != "" {
		return d.prompt.Choices[i]
	}
	return fallback
}

// answer records the answer and returns the command that sends it. The
// command yields CloseDialogMsg, which clears the prompt in the root; there
// must be exactly one such message, or the dialog underneath would close too.
func (d *interactDialog) answer(a interact.Answer) (Dialog, tea.Cmd) {
	d.done = true
	return d, replyCmd(d.reply, a)
}

// replyCmd sends a on ch and then closes the dialog. ch is buffered by one, so
// this cannot block even when the asker has given up already.
func replyCmd(ch chan<- interact.Answer, a interact.Answer) tea.Cmd {
	return func() tea.Msg {
		if ch != nil {
			select {
			case ch <- a:
			default:
			}
		}
		return CloseDialogMsg{}
	}
}

// View renders the body, the input for a secret and the key footer, exactly
// maxW cells wide.
func (d *interactDialog) View(maxW, maxH int) string {
	if maxW < 1 || maxH < 1 {
		return ""
	}
	lines := make([]string, 0, maxH)
	if d.prompt.Body != "" {
		for _, l := range strings.Split(d.prompt.Body, "\n") {
			lines = append(lines, render.Truncate(l, maxW))
		}
	}
	if d.masked() {
		d.input.Width = max(1, maxW-2)
		lines = append(lines, d.input.View())
	}
	if len(lines) > maxH-1 {
		lines = lines[:max(0, maxH-1)]
	}
	for len(lines) < maxH-1 {
		lines = append(lines, "")
	}
	footer := d.footer()
	if d.t != nil {
		footer = d.t.Dim.Render(footer)
	}
	lines = append(lines, footer)
	if len(lines) > maxH {
		lines = lines[:maxH]
	}
	for i := range lines {
		lines[i] = render.PadRight(lines[i], maxW)
	}
	return strings.Join(lines, "\n")
}

// footer is the key line under the body.
func (d *interactDialog) footer() string {
	switch d.prompt.Kind {
	case interact.HostKeyUnknown:
		return "[o] once  [a] always (add to known_hosts)  [r] reject"
	case interact.Passphrase:
		return "enter submit  esc cancel"
	case interact.Confirm:
		return "y/n"
	}
	return "esc cancel"
}
