package tui

import (
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/mux"
	"jumpr/internal/resource"
)

// sshCommandMsg asks the root to open a shell pane for the host the focused
// view is about, or to prompt when the view names none. It is how `:ssh` with
// no argument differs from the prefix key, which always prompts: both arrive
// as mux.OpSSH, and an OpMsg without an argument can only mean the prefix key.
type sshCommandMsg struct{}

// RegisterSSHCommands registers the `:ssh` command. It is called once per
// registry, from the root, because only the root can add a pane.
func RegisterSSHCommands(c *Commands) error {
	if c == nil {
		return nil
	}
	return c.Register(Command{
		Name:  "ssh",
		Usage: ":ssh [host]",
		Help:  "open an SSH shell pane on a host",
		Run: func(_ *Env, args string) tea.Cmd {
			if host := strings.TrimSpace(args); host != "" {
				return opCmd(mux.OpSSH, host)
			}
			return func() tea.Msg { return sshCommandMsg{} }
		},
		Complete: func(env *Env, args string) []string {
			return filterPrefix(sshHosts(env), partialWord(args))
		},
	})
}

// sshHosts is the inventory's host list, for `:ssh` completion.
func sshHosts(env *Env) []string {
	if env == nil || env.Inventory == nil {
		return nil
	}
	hosts := env.Inventory.Hosts()
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, string(h.ID))
	}
	sort.Strings(out)
	return out
}

// sshOp handles mux.OpSSH: the prefix key prompt. With an argument the host is
// opened directly; without one the prompt defaults to the focused host.
func (r *Root) sshOp(arg string) tea.Cmd {
	if r.env == nil || r.env.Shells == nil {
		return Flash(FlashWarn, "no SSH in demo mode")
	}
	if host := strings.TrimSpace(arg); host != "" {
		return r.openSSH(host)
	}
	return r.promptSSH(string(r.subjectHost()))
}

// sshFromCommand handles `:ssh` without an argument: the focused view's host
// when it has one, the prompt otherwise.
func (r *Root) sshFromCommand() tea.Cmd {
	if r.env == nil || r.env.Shells == nil {
		return Flash(FlashWarn, "no SSH in demo mode")
	}
	if host := r.subjectHost(); host != "" {
		return r.openSSH(string(host))
	}
	return r.promptSSH("")
}

// promptSSH asks for a host, prefilled with the focused one.
func (r *Root) promptSSH(initial string) tea.Cmd {
	if r.env == nil || r.env.Shells == nil {
		return Flash(FlashWarn, "no SSH in demo mode")
	}
	r.dialog = NewPromptDialog(themeOf(r.env), "ssh", "host:", initial, func(name string) tea.Cmd {
		return opCmd(mux.OpSSH, name)
	})
	return nil
}

// openSSH resolves name through the inventory and opens a shell pane beside
// the focused one. A name the inventory cannot resolve is an error flash: a
// typo must not start a connection to something else. A read-only session
// refuses the shell unless policy allows it, and opening one is recorded
// (SPEC §11.1).
func (r *Root) openSSH(name string) tea.Cmd {
	if r.env == nil || r.env.Shells == nil {
		return Flash(FlashWarn, "no SSH in demo mode")
	}
	if r.mux == nil {
		return nil
	}
	if reason := r.shellRefusal(); reason != "" {
		return Flash(FlashWarn, "%s", reason)
	}
	id := resource.HostID(name)
	if r.env.Inventory != nil {
		h, ok := r.env.Inventory.Resolve(name)
		if !ok {
			return Flash(FlashError, "unknown host: %s", name)
		}
		id = h.ID
	}
	// The entry goes in before the pane exists: a shell that fails to connect
	// was still opened.
	entry := r.recordShellOpened(id)
	r.connectTerm = true
	_, cmd := r.mux.AddPane(mux.ContentState{Type: "term", Host: string(id)}, "h")
	r.connectTerm = false
	if entry == nil {
		return cmd
	}
	return tea.Batch(entry, cmd)
}

// subjectHost is the single host the focused view is about, if any.
func (r *Root) subjectHost() resource.HostID {
	subj := r.focusedSubject()
	if len(subj.Hosts) > 0 {
		return subj.Hosts[0]
	}
	return subj.Ref.Host
}
