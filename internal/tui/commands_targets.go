package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/inventory"
	"jumpr/internal/resource"
	"jumpr/internal/targets"
)

// TargetsViewRequestedMsg asks the root to open a text view listing the current
// target set. The root handles it; it lives here because this file emits it.
type TargetsViewRequestedMsg struct{}

// MarkedTargetsRequestMsg asks the focused table for its marked rows. The table
// answers with a TargetsChangedMsg, which the root handles as its own.
type MarkedTargetsRequestMsg struct{}

// RegisterTargetsCommands registers the `:targets` command on c. Registering it
// twice is an error from Commands.Register; callers may ignore that.
func RegisterTargetsCommands(c *Commands) error {
	if c == nil {
		return nil
	}
	return c.Register(targetsCommand())
}

// targetsCommand is the `:targets` command of requirement 4.
func targetsCommand() Command {
	return Command{
		Name:  "targets",
		Usage: ":targets [<selector>|clear|save <name>|load <name>|marked]",
		Help:  "show or change the session target set",
		Run:   runTargets,
		Complete: func(env *Env, args string) []string {
			word := partialWord(args)
			fields := strings.Fields(args)
			if len(fields) == 0 {
				return filterPrefix([]string{"clear", "load", "marked", "save"}, word)
			}
			switch fields[0] {
			case "load", "save":
				return filterPrefix(setNames(env), word)
			}
			return nil
		},
	}
}

// runTargets dispatches the `:targets` argument text. Every branch either emits
// a message that changes the session targets or reports why it did not; no
// branch blocks, so file reads and resolution happen inside the tea.Cmd.
func runTargets(env *Env, args string) tea.Cmd {
	args = strings.TrimSpace(args)
	switch args {
	case "":
		return func() tea.Msg { return TargetsViewRequestedMsg{} }
	case "clear":
		return changedTargets(targets.FromHosts("", nil))
	case "marked":
		return func() tea.Msg { return MarkedTargetsRequestMsg{} }
	}

	word, rest := splitFirstWord(args)
	switch word {
	case "save":
		return saveTargets(env, strings.TrimSpace(rest))
	case "load":
		return loadTargets(env, strings.TrimSpace(rest))
	}

	sel, err := resource.ParseSelector(args)
	if err != nil {
		return Flash(FlashError, "bad selector: %v", err)
	}
	return func() tea.Msg {
		set, err := targets.Resolve(inventoryOf(env), storeOf(env), sel, nil)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: err.Error()}
		}
		return TargetsChangedMsg{Set: set}
	}
}

// saveTargets copies the current set's hosts and returns a command that writes
// them. The hosts are read here, in the model's goroutine, so the command never
// touches mutable session state.
func saveTargets(env *Env, name string) tea.Cmd {
	if name == "" {
		return Flash(FlashError, "usage: :targets save <name>")
	}
	if env == nil || env.Sets == nil {
		return Flash(FlashError, "no target set store configured")
	}
	if env.Session == nil || len(env.Session.Targets.Hosts) == 0 {
		return Flash(FlashWarn, "no targets to save")
	}
	hosts := append([]resource.HostID(nil), env.Session.Targets.Hosts...)
	store := env.Sets
	return func() tea.Msg {
		if err := store.Save(name, hosts); err != nil {
			return FlashMsg{Level: FlashError, Text: err.Error()}
		}
		return FlashMsg{Level: FlashOK, Text: fmt.Sprintf("saved %d hosts as %s", len(hosts), name)}
	}
}

// loadTargets returns a command that reads one saved set and makes it the
// session targets.
func loadTargets(env *Env, name string) tea.Cmd {
	if name == "" {
		return Flash(FlashError, "usage: :targets load <name>")
	}
	if env == nil || env.Sets == nil {
		return Flash(FlashError, "no target set store configured")
	}
	store := env.Sets
	return func() tea.Msg {
		hosts, err := store.Load(name)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: err.Error()}
		}
		return TargetsChangedMsg{Set: targets.FromHosts("@"+name, hosts)}
	}
}

// changedTargets emits a TargetsChangedMsg for an already resolved set.
func changedTargets(set targets.Set) tea.Cmd {
	return func() tea.Msg { return TargetsChangedMsg{Set: set} }
}

// inventoryOf returns env's inventory, or nil when there is none.
func inventoryOf(env *Env) inventory.Inventory {
	if env == nil || env.Inventory == nil {
		return nil
	}
	return env.Inventory
}

// storeOf returns env's saved-set store, or nil when there is none.
func storeOf(env *Env) *targets.Store {
	if env == nil {
		return nil
	}
	return env.Sets
}

// setNames lists the saved set names for completion.
func setNames(env *Env) []string {
	if s := storeOf(env); s != nil {
		return s.List()
	}
	return nil
}
