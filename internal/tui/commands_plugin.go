package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/plugin"
	"jumper/internal/render"
)

// `:plugins` is the plugin listing (SPEC §12): what is loaded, from which file,
// in which state, and what is wrong with the files that are not. Kinds are
// registered, so a plugin's own name is a `:` command already; this command is
// about the plugin set as a whole.

const (
	// pluginsUsage is the argument grammar shown on a parse error.
	pluginsUsage = "usage: :plugins [reload]"
)

// PluginSource is what `:plugins` reads: the loaded plugins, the problems of the
// last load and the reload itself. internal/plugin.Manager implements it; nil
// leaves the command registered but empty.
type PluginSource interface {
	Plugins() []plugin.Plugin
	Problems() []plugin.Problem
	Reload() []plugin.Problem
}

// RegisterPluginCommands registers `:plugins` on c. src is the plugin manager;
// a nil src lists nothing and refuses to reload.
func RegisterPluginCommands(c *Commands, src PluginSource) error {
	if c == nil {
		return nil
	}
	return c.Register(Command{
		Name:  "plugins",
		Usage: pluginsUsage,
		Help:  "list the YAML plugins, their state and their files",
		Run:   func(env *Env, args string) tea.Cmd { return runPlugins(env, src, args) },
		Complete: func(_ *Env, args string) []string {
			return filterPrefix([]string{"reload"}, partialWord(args))
		},
	})
}

// runPlugins dispatches `:plugins`. Nothing here blocks: the listing is a push,
// the reload runs in a tea.Cmd.
func runPlugins(env *Env, src PluginSource, args string) tea.Cmd {
	switch strings.TrimSpace(args) {
	case "":
		lines := pluginLines(src)
		return func() tea.Msg { return PushViewMsg{View: newTextPage(env, "plugins", lines)} }
	case "reload":
		if src == nil {
			return Flash(FlashError, "no plugin directory configured")
		}
		return reloadPlugins(src)
	default:
		return Flash(FlashError, "%s", pluginsUsage)
	}
}

// reloadPlugins re-reads the plugin directory off the UI goroutine and reports
// what happened: the kind list changes under the registry, the listing itself is
// rebuilt on the next `:plugins`.
func reloadPlugins(src PluginSource) tea.Cmd {
	return func() tea.Msg {
		problems := src.Reload()
		plugins := src.Plugins()
		loaded := len(plugins) - countBroken(plugins)
		if len(problems) > 0 {
			return FlashMsg{
				Level: FlashWarn,
				Text: fmt.Sprintf("plugins reloaded: %d loaded, %d %s",
					loaded, len(problems), pluralWord(len(problems), "problem")),
			}
		}
		return FlashMsg{Level: FlashOK, Text: fmt.Sprintf("plugins reloaded: %d loaded", loaded)}
	}
}

// countBroken counts the plugins whose current file is broken.
func countBroken(plugins []plugin.Plugin) int {
	n := 0
	for _, p := range plugins {
		if p.State == plugin.StateError {
			n++
		}
	}
	return n
}

// pluginLines renders the listing: a header, one block per plugin and, for a
// broken file, every problem with its file and line.
func pluginLines(src PluginSource) []string {
	if src == nil {
		return []string{"no plugin directory configured", "", "plugins live in <config-dir>/plugins/*.yaml"}
	}
	plugins := src.Plugins()
	if len(plugins) == 0 {
		return []string{"no plugins loaded", "", "plugins live in <config-dir>/plugins/*.yaml"}
	}
	const (
		nameWidth  = 22
		targetW    = 8
		stateWidth = 8
	)
	lines := []string{
		render.PadRight("NAME", nameWidth) + render.PadRight("TARGET", targetW) +
			render.PadRight("STATE", stateWidth) + "DESCRIPTION",
	}
	for _, p := range plugins {
		lines = append(lines, render.PadRight(p.Name, nameWidth)+
			render.PadRight(targetOrNone(p.Target), targetW)+
			render.PadRight(p.State, stateWidth)+
			descriptionOrNone(p.Description))
		lines = append(lines, "  file: "+p.Path)
		for _, prob := range p.Problems {
			lines = append(lines, "  "+problemLine(prob))
		}
	}
	if problems := src.Problems(); len(problems) > 0 {
		lines = append(lines, "", fmt.Sprintf("%d %s; fix the file and run :plugins reload",
			len(problems), pluralWord(len(problems), "problem")))
	}
	return lines
}

// problemLine renders one problem as "file:line: field: message", skipping the
// parts that are unknown.
func problemLine(p plugin.Problem) string {
	return p.String()
}

// targetOrNone is a dash for an empty target.
func targetOrNone(target string) string {
	if strings.TrimSpace(target) == "" {
		return "—"
	}
	return target
}

// descriptionOrNone is a dash for an empty description.
func descriptionOrNone(desc string) string {
	if strings.TrimSpace(desc) == "" {
		return "—"
	}
	return desc
}
