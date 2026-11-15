package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend/linux"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
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
		Usage: ":probe [--via ssh|salt] [<name>]",
		Help:  "list the read-only probes or run one on the targets",
		Run: func(env *Env, args string) tea.Cmd {
			return runProbe(env, args, open)
		},
		Complete: func(_ *Env, args string) []string {
			return filterPrefix(catalogNames(), partialWord(args))
		},
	}
}

// probeUsage is the argument grammar shown on a parse error.
const probeUsage = "usage: :probe [--via ssh|salt] [<name>]"

// parseProbeArgs splits the leading --via flag from the probe name.
func parseProbeArgs(args string) (via, name string, err error) {
	rest := strings.TrimSpace(args)
	for strings.HasPrefix(rest, "--via") {
		flag, tail := splitFirstWord(rest)
		var value string
		if flag == "--via" {
			if tail == "" {
				return "", "", fmt.Errorf("--via needs ssh or salt; %s", probeUsage)
			}
			value, tail = splitFirstWord(tail)
		} else {
			if !strings.HasPrefix(flag, "--via=") {
				break
			}
			value = strings.TrimPrefix(flag, "--via=")
		}
		switch value {
		case "ssh", saltBackendName:
			via = value
		default:
			return "", "", fmt.Errorf("unknown transport %q; %s", value, probeUsage)
		}
		rest = strings.TrimSpace(tail)
	}
	return via, rest, nil
}

// runProbe dispatches the `:probe` argument text. Nothing here blocks: the
// catalog listing is a view push and a run hands the hosts and the command to
// the fan-out view, which starts its own work in a tea.Cmd.
func runProbe(env *Env, args string, open FanoutOpener) tea.Cmd {
	via, rest, err := parseProbeArgs(args)
	if err != nil {
		return Flash(FlashError, "%v", err)
	}
	args = rest
	if args == "" {
		if via != "" {
			return Flash(FlashError, "%s", probeUsage)
		}
		return catalogPage(env)
	}
	name, extra := splitFirstWord(args)
	if extra != "" {
		return Flash(FlashError, "%s", probeUsage)
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
	viewEnv := env
	var warn tea.Cmd
	if via != "" {
		ex := env.Execs[via]
		if ex == nil {
			return Flash(FlashError, "no transport %q configured", via)
		}
		// The fan-out runs through Env.Exec, so the chosen transport is a copy
		// of the environment with that executor swapped in.
		copied := *env
		copied.Exec = ex
		viewEnv = &copied
		if via == saltBackendName && noteSaltUse(env) {
			warn = Flash(FlashWarn, "%s", saltJobCacheWarning)
		}
	}
	push := func() tea.Msg {
		return PushViewMsg{View: open(viewEnv, "probe "+p.Name, cmd, hosts, p.Normalize)}
	}
	if warn == nil {
		return push
	}
	return tea.Batch(warn, push)
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
