package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/backend/linux"
	"jumper/internal/resource"
	"jumper/internal/transport"
)

// FanoutOpener builds the grouped fan-out view one `:probe <name>` run opens.
// views.NewFanout is the production implementation; it is injected here because
// package tui must not import package views.
type FanoutOpener func(env *Env, title string, cmd transport.Command, hosts []resource.HostID, normalize func(string) string) View

// RegisterProbeCommands registers the read-only `:probe` command on c. open
// builds the fan-out view; a nil open still registers the catalog listing but
// refuses to start a run.
func RegisterProbeCommands(c *Commands, open FanoutOpener) error {
	if c == nil {
		return nil
	}
	return c.Register(probeCommand(open))
}

// probeCommand is the `:probe` command: no argument lists the catalog, a name
// runs that probe on the session targets.
func probeCommand(open FanoutOpener) Command {
	return Command{
		Name:  "probe",
		Usage: ":probe [<name>]",
		Help:  "list the read-only probes or run one on the targets",
		Run: func(env *Env, args string) tea.Cmd {
			return runProbe(env, args, open)
		},
		Complete: func(_ *Env, args string) []string {
			return filterPrefix(catalogNames(), partialWord(args))
		},
	}
}

// runProbe dispatches the `:probe` argument text. Nothing here blocks: the
// catalog listing is a view push and a run hands the hosts and the command to
// the fan-out view, which starts its own work in a tea.Cmd.
func runProbe(env *Env, args string, open FanoutOpener) tea.Cmd {
	args = strings.TrimSpace(args)
	if args == "" {
		return catalogPage(env)
	}
	name, rest := splitFirstWord(args)
	if rest != "" {
		return Flash(FlashError, "usage: :probe [<name>]")
	}
	p, ok := catalogProbe(name)
	if !ok {
		return Flash(FlashError, "unknown probe %q; :probe lists them", name)
	}
	hosts := sessionHosts(env)
	if len(hosts) == 0 {
		return Flash(FlashError, "no targets; try :hosts <selector> first")
	}
	if open == nil {
		return Flash(FlashError, "no fan-out view for probes")
	}
	cmd := transport.Command{Script: p.Cmd, Sudo: p.Sudo}
	return func() tea.Msg {
		return PushViewMsg{View: open(env, "probe "+p.Name, cmd, hosts, p.Normalize)}
	}
}

// catalogPage pushes the catalog as a text view.
func catalogPage(env *Env) tea.Cmd {
	probes := linux.Catalog()
	lines := make([]string, 0, len(probes)+2)
	lines = append(lines, "read-only probes · :probe <name> runs one on the targets", "")
	for _, p := range probes {
		entry := fmt.Sprintf("  %-14s %s", p.Name, p.Help)
		if p.Sudo {
			entry += " (sudo)"
		}
		lines = append(lines, entry)
	}
	return func() tea.Msg { return PushViewMsg{View: newTextPage(env, "probes", lines)} }
}

// catalogProbe finds a catalog entry by name.
func catalogProbe(name string) (linux.Probe, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, p := range linux.Catalog() {
		if p.Name == name {
			return p, true
		}
	}
	return linux.Probe{}, false
}

// catalogNames lists the catalog names for completion.
func catalogNames() []string {
	probes := linux.Catalog()
	out := make([]string, len(probes))
	for i, p := range probes {
		out[i] = p.Name
	}
	return out
}

// sessionHosts is the session's current target set.
func sessionHosts(env *Env) []resource.HostID {
	if env == nil || env.Session == nil {
		return nil
	}
	return env.Session.Targets.Hosts
}
