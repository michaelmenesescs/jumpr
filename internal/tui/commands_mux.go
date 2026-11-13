package tui

import (
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/mux"
	"jumper/internal/tui/keymap"
)

// muxOpText describes one mux operation for the help dialog. The key table in
// internal/mux is the source of the keys; this map is the only place the
// descriptions live.
var muxOpText = map[string]string{
	mux.OpSplitH:        "split right",
	mux.OpSplitV:        "split down",
	mux.OpKillPane:      "kill pane",
	mux.OpZoom:          "zoom pane",
	mux.OpFocus:         "focus pane",
	mux.OpResize:        "resize pane",
	mux.OpSwap:          "swap pane",
	mux.OpLayoutNext:    "next layout",
	mux.OpNewWindow:     "new window",
	mux.OpKillWindow:    "kill window",
	mux.OpSelectWindow:  "select window",
	mux.OpNextWindow:    "next window",
	mux.OpPrevWindow:    "previous window",
	mux.OpWindowList:    "list windows",
	mux.OpRenameWindow:  "rename window",
	mux.OpRenameSession: "rename session",
	mux.OpSessionList:   "sessions",
	mux.OpSSH:           "ssh pane",
	mux.OpCopyMode:      "copy mode",
	mux.OpSync:          "synchronized input",
	mux.OpCommand:       "command prompt",
	mux.OpDetach:        "detach",
	mux.OpHelp:          "help",
}

// PrefixDisplay renders a Bubble Tea prefix key as a caret label: ctrl+a
// becomes ^A. Other keys are returned unchanged.
func PrefixDisplay(teaKey string) string {
	if rest, ok := strings.CutPrefix(teaKey, "ctrl+"); ok && len(rest) == 1 {
		return "^" + strings.ToUpper(rest)
	}
	return teaKey
}

// PrefixKey picks the session prefix key from the config values. inTmux
// selects mux.prefix_in_tmux. The result is a Bubble Tea key string ("ctrl+a").
func PrefixKey(prefix, prefixInTmux string, inTmux bool) (string, error) {
	s := prefix
	if inTmux {
		s = prefixInTmux
	}
	if strings.TrimSpace(s) == "" {
		return "", nil
	}
	return keymap.ParsePrefix(s)
}

// MuxHelpSection builds the "Panes and windows" help section from the mux key
// table. display is the prefix as shown to the user, e.g. "^A".
func MuxHelpSection(display string) HelpSection {
	table := mux.DefaultKeyTable()
	keys := make([]string, 0, len(table)+1)
	for k := range table {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	hints := make([]KeyHint, 0, len(keys)+1)
	hints = append(hints, KeyHint{Key: display + " " + display, Text: "send the prefix"})
	for _, k := range keys {
		hints = append(hints, KeyHint{Key: display + " " + k, Text: opText(table[k])})
	}
	return HelpSection{Title: "Panes and windows", Hints: hints}
}

// opText describes op, including its argument where it has one.
func opText(op mux.OpMsg) string {
	text := muxOpText[op.Op]
	if op.Arg == "" {
		return text
	}
	return text + " " + op.Arg
}

// opCmd returns a command that asks the mux to run one operation.
func opCmd(op, arg string) tea.Cmd {
	return func() tea.Msg { return mux.OpMsg{Op: op, Arg: arg} }
}

// repeatOp returns a command that emits op n times, for operations the mux
// applies in fixed steps.
func repeatOp(op, arg string, n int) tea.Cmd {
	if n <= 0 {
		return nil
	}
	cmds := make([]tea.Cmd, 0, n)
	for i := 0; i < n; i++ {
		cmds = append(cmds, opCmd(op, arg))
	}
	if len(cmds) == 1 {
		return cmds[0]
	}
	return tea.Batch(cmds...)
}

// dirCompletions are the arguments the focus and resize commands accept.
// filterPrefix keeps the candidates that start with prefix, preserving order.
// An empty prefix matches everything (tab on an empty word lists them all).
func filterPrefix(cands []string, prefix string) []string {
	if prefix == "" {
		return cands
	}
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func dirCompletions(extra ...string) []string {
	return append([]string{"left", "right", "up", "down"}, extra...)
}

// RegisterMuxCommands registers the multiplexer's `:` commands on c. Every one
// plans a mux.OpMsg; the mux model executes it.
func RegisterMuxCommands(c *Commands) error {
	if c == nil {
		return nil
	}
	for _, cmd := range muxCommands() {
		if err := c.Register(cmd); err != nil {
			return err
		}
	}
	return nil
}

// muxCommands is the command table of requirement 5.
func muxCommands() []Command {
	return []Command{
		{
			Name:     "split",
			Aliases:  []string{"split-h"},
			Usage:    ":split",
			Help:     "split the focused pane, new pane to the right",
			Run:      func(*Env, string) tea.Cmd { return opCmd(mux.OpSplitH, "") },
			Complete: func(*Env, string) []string { return nil },
		},
		{
			Name:     "vsplit",
			Aliases:  []string{"split-v"},
			Usage:    ":vsplit",
			Help:     "split the focused pane, new pane below",
			Run:      func(*Env, string) tea.Cmd { return opCmd(mux.OpSplitV, "") },
			Complete: func(*Env, string) []string { return nil },
		},
		{
			Name:     "kill-pane",
			Usage:    ":kill-pane",
			Help:     "close the focused pane (asks first if it is busy)",
			Run:      func(*Env, string) tea.Cmd { return opCmd(mux.OpKillPane, "") },
			Complete: func(*Env, string) []string { return nil },
		},
		{
			Name:     "zoom",
			Aliases:  []string{"z"},
			Usage:    ":zoom",
			Help:     "zoom or unzoom the focused pane",
			Run:      func(*Env, string) tea.Cmd { return opCmd(mux.OpZoom, "") },
			Complete: func(*Env, string) []string { return nil },
		},
		{
			Name:  "focus",
			Usage: ":focus <left|right|up|down|next|last>",
			Help:  "move the focus to another pane",
			Run: func(_ *Env, args string) tea.Cmd {
				dir := strings.TrimSpace(args)
				for _, d := range dirCompletions("next", "last") {
					if dir == d {
						return opCmd(mux.OpFocus, dir)
					}
				}
				return Flash(FlashError, "usage: :focus <left|right|up|down|next|last>")
			},
			Complete: func(_ *Env, args string) []string {
				return filterPrefix(dirCompletions("next", "last"), partialWord(args))
			},
		},
		{
			Name:  "resize",
			Usage: ":resize <left|right|up|down> [n]",
			Help:  "move a pane divider n cells (default 2)",
			Run: func(_ *Env, args string) tea.Cmd {
				fields := strings.Fields(args)
				if len(fields) == 0 || len(fields) > 2 {
					return Flash(FlashError, "usage: :resize <left|right|up|down> [n]")
				}
				dir := fields[0]
				ok := false
				for _, d := range dirCompletions() {
					if dir == d {
						ok = true
						break
					}
				}
				if !ok {
					return Flash(FlashError, "usage: :resize <left|right|up|down> [n]")
				}
				cells := 2
				if len(fields) == 2 {
					n, err := strconv.Atoi(fields[1])
					if err != nil || n <= 0 {
						return Flash(FlashError, "resize: %q is not a positive cell count", fields[1])
					}
					cells = n
				}
				// The mux moves a divider two cells per operation.
				return repeatOp(mux.OpResize, dir, max(1, (cells+1)/2))
			},
			Complete: func(_ *Env, args string) []string {
				if len(strings.Fields(args)) > 1 {
					return nil
				}
				return filterPrefix(dirCompletions(), partialWord(args))
			},
		},
		{
			Name:  "layout",
			Usage: ":layout [even-h|even-v|main-left|next]",
			Help:  "cycle the window layout",
			Run: func(_ *Env, args string) tea.Cmd {
				switch strings.TrimSpace(args) {
				case "", "next", "even-h":
					return opCmd(mux.OpLayoutNext, "")
				case "even-v":
					return repeatOp(mux.OpLayoutNext, "", 2)
				case "main-left":
					return repeatOp(mux.OpLayoutNext, "", 3)
				}
				return Flash(FlashError, "usage: :layout [even-h|even-v|main-left|next]")
			},
			Complete: func(_ *Env, args string) []string {
				return filterPrefix([]string{"even-h", "even-v", "main-left", "next"}, partialWord(args))
			},
		},
		{
			Name:  "window",
			Usage: ":window [new|kill|next|prev|<n>]",
			Help:  "create, close or select a window",
			Run: func(_ *Env, args string) tea.Cmd {
				switch arg := strings.TrimSpace(args); arg {
				case "new":
					return opCmd(mux.OpNewWindow, "")
				case "kill":
					return opCmd(mux.OpKillWindow, "")
				case "next":
					return opCmd(mux.OpNextWindow, "")
				case "prev":
					return opCmd(mux.OpPrevWindow, "")
				case "":
					return Flash(FlashError, "usage: :window [new|kill|next|prev|<n>]")
				default:
					n, err := strconv.Atoi(arg)
					if err != nil || n < 0 {
						return Flash(FlashError, "window: %q is not a window number", arg)
					}
					return opCmd(mux.OpSelectWindow, strconv.Itoa(n))
				}
			},
			Complete: func(_ *Env, args string) []string {
				return filterPrefix([]string{"new", "kill", "next", "prev"}, partialWord(args))
			},
		},
		{
			Name:  "rename",
			Usage: ":rename [name]",
			Help:  "rename the active window",
			Run: func(_ *Env, args string) tea.Cmd {
				return opCmd(mux.OpRenameWindow, strings.TrimSpace(args))
			},
			Complete: func(*Env, string) []string { return nil },
		},
		{
			Name:  "rename-session",
			Usage: ":rename-session [name]",
			Help:  "rename the session",
			Run: func(_ *Env, args string) tea.Cmd {
				return opCmd(mux.OpRenameSession, strings.TrimSpace(args))
			},
			Complete: func(*Env, string) []string { return nil },
		},
		{
			Name:     "detach",
			Aliases:  []string{"d"},
			Usage:    ":detach",
			Help:     "detach from the session (needs the session server, M5)",
			Run:      func(*Env, string) tea.Cmd { return opCmd(mux.OpDetach, "") },
			Complete: func(*Env, string) []string { return nil },
		},
		{
			Name:     "copy",
			Aliases:  []string{"copy-mode"},
			Usage:    ":copy",
			Help:     "enter copy mode (coming with terminal panes)",
			Run:      func(*Env, string) tea.Cmd { return opCmd(mux.OpCopyMode, "") },
			Complete: func(*Env, string) []string { return nil },
		},
		{
			Name:  "sync",
			Usage: ":sync [raw|off]",
			Help:  "broadcast a line to the panes, or toggle raw synchronized input",
			Run: func(_ *Env, args string) tea.Cmd {
				return opCmd(mux.OpSync, strings.TrimSpace(args))
			},
			Complete: func(_ *Env, args string) []string {
				return filterPrefix([]string{"raw", "off"}, partialWord(args))
			},
		},
	}
}
