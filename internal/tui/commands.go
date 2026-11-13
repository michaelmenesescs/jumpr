package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/errs"
	"jumper/internal/resource"
)

// Command is a `:` command (BUILD-PLAN §4.8).
type Command struct {
	Name    string // "hosts", "split", "run"
	Aliases []string
	Usage   string // ":run [--via ssh|salt] <command>"
	Help    string // one line
	// Run returns a tea.Cmd that emits navigation or mux messages (§5). It must not block.
	Run func(env *Env, args string) tea.Cmd
	// Complete returns candidates for the argument text; may be nil.
	Complete func(env *Env, args string) []string
}

// HelpRequestedMsg asks the root to open the help dialog. The root handles it
// in task 016; the message lives here because the command registry emits it.
type HelpRequestedMsg struct{}

// KindsRequestedMsg asks the root to open a text view listing the registered
// kinds. The root handles it in task 016.
type KindsRequestedMsg struct{}

// Commands is the `:` command registry. Names and aliases are matched
// case-insensitively; the zero value is an empty registry.
type Commands struct {
	list  []Command
	index map[string]int // lower-cased name or alias -> position in list
}

// NewCommands returns an empty command registry.
func NewCommands() *Commands {
	return &Commands{index: make(map[string]int)}
}

// Register adds c. It fails with an errs.Config error on an empty name, a nil
// Run, or a name or alias that another command already claimed.
func (c *Commands) Register(cmd Command) error {
	name := strings.TrimSpace(cmd.Name)
	if name == "" {
		return errs.New(errs.Config, "tui.Commands.Register", "command has no name")
	}
	if cmd.Run == nil {
		return errs.New(errs.Config, "tui.Commands.Register",
			fmt.Sprintf("command %q has no Run function", name))
	}
	cmd.Name = name
	cmd.Aliases = append([]string(nil), cmd.Aliases...)

	if c.index == nil {
		c.index = make(map[string]int)
	}
	keys := make([]string, 0, 1+len(cmd.Aliases))
	keys = append(keys, name)
	keys = append(keys, cmd.Aliases...)
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		k := strings.ToLower(strings.TrimSpace(key))
		if k == "" {
			return errs.New(errs.Config, "tui.Commands.Register",
				fmt.Sprintf("command %q has an empty name or alias", name))
		}
		if seen[k] {
			continue // the command repeating its own name or alias is not a conflict
		}
		if _, taken := c.index[k]; taken {
			return errs.New(errs.Config, "tui.Commands.Register",
				fmt.Sprintf("%q is already a command name or alias", k))
		}
		seen[k] = true
	}

	pos := len(c.list)
	c.list = append(c.list, cmd)
	for k := range seen {
		c.index[k] = pos
	}
	return nil
}

// Lookup returns the command registered under name (case-insensitive).
func (c *Commands) Lookup(name string) (Command, bool) {
	if c == nil {
		return Command{}, false
	}
	i, ok := c.index[strings.ToLower(strings.TrimSpace(name))]
	if !ok || i < 0 || i >= len(c.list) {
		return Command{}, false
	}
	return c.list[i], true
}

// All returns every command sorted by name.
func (c *Commands) All() []Command {
	if c == nil {
		return nil
	}
	out := append([]Command(nil), c.list...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Exec runs one command line. The first word is the command name or a
// registered kind (or kind alias); the rest is the argument text. Unknown
// words and bad selectors become an error flash, never a panic.
func (c *Commands) Exec(env *Env, line string) tea.Cmd {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), ":"))
	if line == "" {
		return nil
	}
	word, args := splitFirstWord(line)
	if cmd, ok := c.Lookup(word); ok {
		return cmd.Run(env, args)
	}
	if env != nil && env.Registry != nil {
		if _, spec, ok := env.Registry.Lookup(word); ok {
			selText, hosts, qargs, err := parseQueryArgs(args)
			if err != nil {
				return Flash(FlashError, "%v", err)
			}
			sel, err := resource.ParseSelector(selText)
			if err != nil {
				return Flash(FlashError, "bad selector: %v", err)
			}
			return OpenQuery(resource.Query{Kind: spec.Name, Selector: sel, Hosts: hosts, Args: qargs})
		}
	}
	return Flash(FlashError, "unknown command: %s", word)
}

// parseQueryArgs splits a kind command's argument text into the selector text
// and the --host a,b / --arg k=v flags. Everything that is not a flag is
// selector text. A flag without a usable value is an errs.Config error.
func parseQueryArgs(args string) (sel string, hosts []resource.HostID, qargs map[string]string, err error) {
	toks := strings.Fields(args)
	var selParts []string
	for i := 0; i < len(toks); i++ {
		switch toks[i] {
		case "--host":
			if i+1 >= len(toks) {
				return "", nil, nil, errs.New(errs.Config, "tui.Commands.Exec", "--host needs a comma-separated host list")
			}
			i++
			for _, h := range strings.Split(toks[i], ",") {
				if h = strings.TrimSpace(h); h != "" {
					hosts = append(hosts, resource.HostID(h))
				}
			}
		case "--arg":
			if i+1 >= len(toks) {
				return "", nil, nil, errs.New(errs.Config, "tui.Commands.Exec", "--arg needs k=v")
			}
			i++
			k, v, ok := strings.Cut(toks[i], "=")
			if k = strings.TrimSpace(k); !ok || k == "" {
				return "", nil, nil, errs.New(errs.Config, "tui.Commands.Exec",
					fmt.Sprintf("--arg needs k=v, got %q", toks[i]))
			}
			if qargs == nil {
				qargs = make(map[string]string)
			}
			qargs[k] = v
		default:
			selParts = append(selParts, toks[i])
		}
	}
	return strings.Join(selParts, " "), hosts, qargs, nil
}

// splitFirstWord splits line at the first run of spaces, trimming the rest.
func splitFirstWord(line string) (word, rest string) {
	line = strings.TrimSpace(line)
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		return line[:i], strings.TrimSpace(line[i+1:])
	}
	return line, ""
}

// RegisterBuiltins registers the commands jumper itself provides: quit, help,
// kinds and time. `window` is deliberately absent — the multiplexer owns that
// name (task 022).
func RegisterBuiltins(c *Commands) {
	if c == nil {
		return
	}
	// Registration cannot fail on a fresh registry; the names and aliases below
	// are distinct and every Run is set.
	_ = c.Register(Command{
		Name:    "q",
		Aliases: []string{"quit"},
		Usage:   ":q",
		Help:    "quit jumper",
		Run: func(*Env, string) tea.Cmd {
			return tea.Quit
		},
	})
	_ = c.Register(Command{
		Name:  "help",
		Usage: ":help",
		Help:  "show keys and commands",
		Run: func(*Env, string) tea.Cmd {
			return func() tea.Msg { return HelpRequestedMsg{} }
		},
	})
	_ = c.Register(Command{
		Name:  "kinds",
		Usage: ":kinds",
		Help:  "list the resource kind names",
		Run: func(*Env, string) tea.Cmd {
			return func() tea.Msg { return KindsRequestedMsg{} }
		},
	})
	_ = c.Register(Command{
		Name:    "time",
		Aliases: []string{"t"},
		Usage:   ":time <window>",
		Help:    "set the session time window, e.g. :time -1h",
		Run: func(env *Env, args string) tea.Cmd {
			now := time.Now
			if env != nil && env.Now != nil {
				now = env.Now
			}
			if strings.TrimSpace(args) == "" {
				return Flash(FlashError, "usage: :time <window>")
			}
			w, err := resource.ParseTimeWindow(args, now())
			if err != nil {
				return Flash(FlashError, "bad time window: %v", err)
			}
			return func() tea.Msg { return WindowChangedMsg{Window: w} }
		},
	})
}
