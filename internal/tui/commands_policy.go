package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/bus"
)

// `:policy` and `:readonly` (SPEC §11, §16). The policy is the guard rail the
// bus applies; the read-only switch is the session-level one the RO badge and
// the bus's ReadOnly callback report. Neither command does I/O: `:policy` shows
// lines the loaded policy already holds, and `:readonly` only decides.

// readOnlyChangedMsg asks the root to set the session's read-only switch. Only
// the root mutates SessionState (BUILD-PLAN §4.8), so the command returns this
// message instead of writing the field itself.
type readOnlyChangedMsg struct{ On bool }

// RegisterPolicyCommands registers `:policy` and `:readonly` on c.
func RegisterPolicyCommands(c *Commands) error {
	if c == nil {
		return nil
	}
	if err := c.Register(policyCommand()); err != nil {
		return err
	}
	return c.Register(readOnlyCommand())
}

// policyCommand is `:policy`: the effective policy in a text view.
func policyCommand() Command {
	return Command{
		Name:  "policy",
		Usage: ":policy",
		Help:  "show the effective policy (built-ins plus policy.yaml)",
		Run: func(env *Env, args string) tea.Cmd {
			if strings.TrimSpace(args) != "" {
				return Flash(FlashError, "usage: :policy")
			}
			lines := policyLines(env)
			return func() tea.Msg { return PushViewMsg{View: newTextPage(env, "policy", lines)} }
		},
	}
}

// policyLines is the text `:policy` shows. Without a loaded policy only the
// built-ins apply, and the text says so rather than pretending the file was
// read.
func policyLines(env *Env) []string {
	if env != nil && env.Policy != nil {
		return env.Policy.Describe()
	}
	lines := []string{"no policy.yaml loaded: the built-in policy applies", ""}
	return append(lines, bus.DefaultPolicy().Describe()...)
}

// readOnlyCommand is `:readonly on|off`. Turning it off is a confirmation and
// is refused outright when policy.yaml itself says `readonly: true`, because no
// session switch may overrule the file.
func readOnlyCommand() Command {
	return Command{
		Name:  "readonly",
		Usage: ":readonly [on|off]",
		Help:  "turn the session read-only switch on or off",
		Run:   runReadOnly,
		Complete: func(_ *Env, args string) []string {
			return filterPrefix([]string{"off", "on"}, partialWord(args))
		},
	}
}

// runReadOnly dispatches `:readonly`. Nothing blocks: the confirmation is a
// dialog message and the switch itself is a message for the root.
func runReadOnly(env *Env, args string) tea.Cmd {
	switch strings.ToLower(strings.TrimSpace(args)) {
	case "":
		if sessionReadOnly(env) {
			return Flash(FlashInfo, "read-only is on: writes are disabled")
		}
		return Flash(FlashInfo, "read-only is off: writes go through the bus")
	case "on":
		return func() tea.Msg { return readOnlyChangedMsg{On: true} }
	case "off":
		if p := policyOf(env); p != nil && p.ReadOnly() {
			return Flash(FlashError, "policy.yaml sets readonly: true; edit the policy file to allow writes")
		}
		if !sessionReadOnly(env) {
			return Flash(FlashInfo, "already writable")
		}
		yes := func() tea.Msg { return readOnlyChangedMsg{On: false} }
		d := NewConfirmDialog(themeOf(env), "read-only off", "writes become possible on every target: confirm", yes)
		return func() tea.Msg { return OpenDialogMsg{Dialog: d} }
	}
	return Flash(FlashError, "usage: :readonly [on|off]")
}

// applyReadOnly switches the session's read-only mode and says which way it
// went. The root calls it; the status line's RO badge reads the same field on
// the next frame.
func (r *Root) applyReadOnly(on bool) tea.Cmd {
	if r.env != nil && r.env.Session != nil {
		r.env.Session.ReadOnly = on
	}
	if on {
		return Flash(FlashInfo, "read-only on: writes are disabled")
	}
	return Flash(FlashOK, "read-only off: writes are possible")
}

// policyOf is env's policy, or nil when none was loaded.
func policyOf(env *Env) *bus.Policy {
	if env == nil {
		return nil
	}
	return env.Policy
}

// sessionReadOnly reports the session-level switch. A policy that says
// readonly counts too, so `:readonly off` cannot claim there is nothing to do
// while writes are in fact refused.
func sessionReadOnly(env *Env) bool {
	if env != nil && env.Session != nil && env.Session.ReadOnly {
		return true
	}
	if p := policyOf(env); p != nil {
		return p.ReadOnly()
	}
	return false
}
