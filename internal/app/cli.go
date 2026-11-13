package app

import (
	"fmt"
	"path/filepath"
	"strings"

	"jumper/internal/config"
	"jumper/internal/errs"
	"jumper/internal/server"
)

// Command is one subcommand of `jumper [global flags] [command] [flags]`
// (SPEC §21.4). An invocation with no command word attaches.
type Command string

const (
	// CommandAttach attaches to a session, or creates "main".
	CommandAttach Command = "attach"
	// CommandNew creates a session, and attaches to it unless -d was given.
	CommandNew Command = "new"
	// CommandLs lists the sessions of the running server.
	CommandLs Command = "ls"
	// CommandKillSession ends one session; the server keeps running.
	CommandKillSession Command = "kill-session"
	// CommandKillServer ends the server and every session in it.
	CommandKillServer Command = "kill-server"
	// CommandServer is the session server itself: the client re-executes the
	// binary with this command, or the user runs it by hand.
	CommandServer Command = "server"
	// CommandDoctor is the preflight report (`doctor`, `doctor ssh <host>`).
	CommandDoctor Command = "doctor"
	// CommandInit writes the configuration skeleton.
	CommandInit Command = "init"
	// CommandVersion prints the version and exits.
	CommandVersion Command = "version"
	// CommandHelp prints the usage and exits.
	CommandHelp Command = "help"
)

// Invocation is a parsed command line. Parsing is pure: nothing here reads the
// environment or the filesystem, so cmd/jumper passes $JUMPER_SOCKET and the
// paths in.
type Invocation struct {
	// Command is the subcommand; CommandAttach when no command word was given.
	Command Command
	// ConfigDir is --config-dir, unexpanded. Empty means the default.
	ConfigDir string
	// Debug is --debug: write the debug log.
	Debug bool
	// Socket is -S/--socket. Empty means $JUMPER_SOCKET, then the cache dir.
	Socket string
	// Standalone is --standalone (or --demo, which implies it): one process,
	// no server.
	Standalone bool
	// Demo is --demo: the built-in fake world.
	Demo bool
	// DemoServer is the hidden --demo-server: the session server runs the
	// built-in fake world while the client attaches to it normally. --demo
	// implies standalone, which is no use for the end-to-end test; this does
	// not.
	DemoServer bool
	// ReadOnly is --readonly: every write is denied until `:readonly off`.
	ReadOnly bool
	// Fresh is --fresh: do not restore the saved layout.
	Fresh bool
	// Target is `-t name` (attach, kill-session).
	Target string
	// Session is `-s name` (new).
	Session string
	// Detached is `-d` (new): create the session without attaching.
	Detached bool
	// Foreground is `server --foreground`.
	Foreground bool
	// Args are the words after `doctor` and `init`, which parse their own flags.
	Args []string
}

// ParseCLI parses args (without the program name). Unknown commands and flags
// are *errs.Error with kind Config, which cmd/jumper turns into exit code 2.
func ParseCLI(args []string) (Invocation, error) {
	inv := Invocation{Command: CommandAttach}

	// Global flags first, up to the command word.
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--":
			i++
			if i < len(args) {
				return Invocation{}, errs.New(errs.Config, "cli.parse",
					fmt.Sprintf("unexpected argument %q", args[i]))
			}
		case isHelpFlag(a):
			return Invocation{Command: CommandHelp}, nil
		case a == "--version":
			return Invocation{Command: CommandVersion}, nil
		case isFlag(a):
			n, err := inv.parseGlobal(args, i)
			if err != nil {
				return Invocation{}, err
			}
			i = n
			continue
		default:
			c, ok := parseCommand(a)
			if !ok {
				return Invocation{}, errs.New(errs.Config, "cli.parse",
					fmt.Sprintf("unknown command %q", a))
			}
			inv.Command = c
			i++
		}
		break
	}
	rest := args[i:]

	// doctor and init own their remaining words: they have subcommands and
	// flags (`doctor ssh <host>`, `doctor conn <profile> <url> --verbose`).
	switch inv.Command {
	case CommandDoctor, CommandInit:
		inv.Args = rest
		return inv, nil
	}

	for j := 0; j < len(rest); j++ {
		if isHelpFlag(rest[j]) {
			return Invocation{Command: CommandHelp}, nil
		}
		n, err := inv.parseCommandFlag(rest, j)
		if err != nil {
			return Invocation{}, err
		}
		j = n - 1
	}

	if err := inv.validate(); err != nil {
		return Invocation{}, err
	}
	return inv, nil
}

// parseCommandFlag consumes one flag of the command (or a global flag that
// appears after it) and returns the index of the next word to look at.
func (i *Invocation) parseCommandFlag(args []string, j int) (int, error) {
	a := args[j]
	switch {
	case matchesFlag(a, "-t", "--target"):
		if i.Command != CommandAttach && i.Command != CommandKillSession {
			return j + 1, unknownFlag(a, i.Command)
		}
		v, n, err := flagValue(args, j, a)
		if err != nil {
			return n, err
		}
		i.Target = v
		return n, nil
	case matchesFlag(a, "-s", "--session"):
		if i.Command != CommandNew {
			return j + 1, unknownFlag(a, i.Command)
		}
		v, n, err := flagValue(args, j, a)
		if err != nil {
			return n, err
		}
		i.Session = v
		return n, nil
	case matchesFlag(a, "-d", "--detached"):
		if i.Command != CommandNew {
			return j + 1, unknownFlag(a, i.Command)
		}
		i.Detached = true
		return j + 1, nil
	case matchesFlag(a, "--foreground"):
		if i.Command != CommandServer {
			return j + 1, unknownFlag(a, i.Command)
		}
		i.Foreground = true
		return j + 1, nil
	case isFlag(a):
		return i.parseGlobal(args, j)
	default:
		return j + 1, errs.New(errs.Config, "cli.parse",
			fmt.Sprintf("%s: unexpected argument %q", i.Command, a))
	}
}

// parseGlobal consumes one global flag (before or after the command word).
func (i *Invocation) parseGlobal(args []string, j int) (int, error) {
	a := args[j]
	switch {
	case matchesFlag(a, "--config-dir"):
		v, n, err := flagValue(args, j, a)
		if err != nil {
			return n, err
		}
		i.ConfigDir = v
		return n, nil
	case matchesFlag(a, "-S", "--socket"):
		v, n, err := flagValue(args, j, a)
		if err != nil {
			return n, err
		}
		i.Socket = v
		return n, nil
	case matchesFlag(a, "--debug"):
		i.Debug = true
	case matchesFlag(a, "--standalone"):
		i.Standalone = true
	case matchesFlag(a, "--demo"):
		i.Demo = true
	case matchesFlag(a, "--demo-server"):
		i.DemoServer = true
	case matchesFlag(a, "--readonly"):
		i.ReadOnly = true
	case matchesFlag(a, "--fresh"):
		i.Fresh = true
	case matchesFlag(a, "--version"):
		i.Command = CommandVersion
	default:
		return j + 1, errs.New(errs.Config, "cli.parse",
			fmt.Sprintf("unknown flag %q", a))
	}
	return j + 1, nil
}

// validate applies the rules that span several flags.
func (i *Invocation) validate() error {
	if i.Command == CommandKillSession && i.Target == "" {
		return errs.New(errs.Config, "cli.parse", "kill-session needs -t <name>")
	}
	if i.Target != "" && !server.ValidSessionName(i.Target) {
		return errs.New(errs.Config, "cli.parse", fmt.Sprintf("invalid session name %q", i.Target))
	}
	if i.Session != "" && !server.ValidSessionName(i.Session) {
		return errs.New(errs.Config, "cli.parse", fmt.Sprintf("invalid session name %q", i.Session))
	}
	// --demo runs the fake world, which only exists in-process.
	if i.Demo {
		i.Standalone = true
	}
	return nil
}

// WireCommand is the Hello.Command that carries this invocation to the server
// (BUILD-PLAN §6.2).
func (i Invocation) WireCommand() string {
	switch i.Command {
	case CommandAttach:
		return "attach"
	case CommandNew:
		return "new"
	case CommandLs:
		return "ls"
	case CommandKillSession:
		return "kill-session"
	case CommandKillServer:
		return "kill-server"
	}
	return ""
}

// SessionName is the name the command names: -t for attach and kill-session,
// -s for new. Empty means "the most recently used session" (attach) or "main".
func (i Invocation) SessionName() string {
	if i.Target != "" {
		return i.Target
	}
	return i.Session
}

// Hello builds the first frame of a connection for this invocation. env carries
// TERM, COLORTERM, SSH_AUTH_SOCK and TMUX only (BUILD-PLAN §6.2).
func (i Invocation) Hello(version string, cols, rows int, env map[string]string) server.Hello {
	return server.Hello{
		Protocol: server.ProtocolVersion,
		Version:  version,
		Command:  i.WireCommand(),
		Session:  i.SessionName(),
		Detached: i.Detached,
		Cols:     cols,
		Rows:     rows,
		Env:      env,
	}
}

// Paths resolves the directories an invocation works with: the configuration
// directory it named and the process cache directory (BUILD-PLAN §6).
func Paths(inv Invocation, home string) config.Paths {
	return config.ResolvePaths(home, inv.ConfigDir)
}

// SocketPath resolves the server socket: -S, then $JUMPER_SOCKET, then
// <cache>/jumper.sock (BUILD-PLAN §6). envSocket is passed in, never read here.
func SocketPath(inv Invocation, envSocket, home string) string {
	if inv.Socket != "" {
		return inv.Socket
	}
	if envSocket != "" {
		return envSocket
	}
	return filepath.Join(config.ResolvePaths(home, inv.ConfigDir).CacheDir, "jumper.sock")
}

// MaxSocketPath is the longest usable unix socket path: macOS `sun_path` is 104
// bytes including the terminator (BUILD-PLAN §6).
const MaxSocketPath = 103

// CheckSocketPath rejects a socket path the operating system cannot bind.
func CheckSocketPath(path string) error {
	if path == "" {
		return errs.New(errs.Config, "cli.socket", "empty socket path")
	}
	if len(path) > MaxSocketPath {
		return errs.New(errs.Config, "cli.socket",
			fmt.Sprintf("socket path is %d bytes; the limit is %d", len(path), MaxSocketPath))
	}
	return nil
}

// FormatSessionLine renders one `jumper ls` line, as tmux does:
//
//	main: 2 windows (created Nov 12 14:02) (attached)
func FormatSessionLine(s server.SessionInfo) string {
	unit := "windows"
	if s.Windows == 1 {
		unit = "window"
	}
	line := fmt.Sprintf("%s: %d %s (created %s)",
		s.Name, s.Windows, unit, s.Created.Local().Format("Jan _2 15:04"))
	if s.Attached {
		line += " (attached)"
	}
	return line
}

// Usage is the text `-h`, `--help` and a bad command line print.
func Usage() string {
	return "usage: jumper [global flags] [command] [flags]\n" +
		"\n" +
		"commands:\n" +
		"  (none)                  attach to the last session, or create \"main\"\n" +
		"  attach [-t name]        attach to a session\n" +
		"  new [-s name] [-d]      create a session (-d: do not attach)\n" +
		"  ls                      list the sessions of the running server\n" +
		"  kill-session -t name    end one session\n" +
		"  kill-server             end the server and every session\n" +
		"  server [--foreground]   run the session server in this terminal\n" +
		"  doctor [ssh <host> | conn <profile> <url>] [--verbose]\n" +
		"                          check the configuration, SSH, agent and backends\n" +
		"  init [--config-dir DIR] write the configuration skeleton\n" +
		"  version                 print the version\n" +
		"\n" +
		"global flags:\n" +
		"  --config-dir DIR        configuration directory (default ~/.config/jumper)\n" +
		"  --debug                 write the debug log\n" +
		"  -S, --socket PATH       server socket (default ~/.cache/jumper/jumper.sock)\n" +
		"  --standalone            one process, no session server\n" +
		"  --demo                  run the built-in fake world (implies --standalone)\n" +
		"  --readonly              start read-only: every write is denied\n" +
		"  --fresh                 do not restore the saved layout\n" +
		"  --version               print the version\n"
}

// parseCommand maps a command word to its Command.
func parseCommand(s string) (Command, bool) {
	switch s {
	case "attach":
		return CommandAttach, true
	case "new":
		return CommandNew, true
	case "ls":
		return CommandLs, true
	case "kill-session":
		return CommandKillSession, true
	case "kill-server":
		return CommandKillServer, true
	case "server":
		return CommandServer, true
	case "doctor":
		return CommandDoctor, true
	case "init":
		return CommandInit, true
	case "version":
		return CommandVersion, true
	}
	return "", false
}

// AllCommands is every command word the CLI accepts, in usage order.
func AllCommands() []Command {
	return []Command{
		CommandAttach, CommandNew, CommandLs, CommandKillSession, CommandKillServer,
		CommandServer, CommandDoctor, CommandInit, CommandVersion,
	}
}

// isFlag reports whether a word is an option rather than a command or an
// argument. A bare "-" is an argument.
func isFlag(a string) bool {
	return len(a) > 1 && a[0] == '-'
}

// isHelpFlag reports the two spellings of help.
func isHelpFlag(a string) bool {
	return a == "-h" || a == "--help"
}

// matchesFlag reports whether a is one of the spellings, with or without a
// =value suffix.
func matchesFlag(a string, flags ...string) bool {
	for _, f := range flags {
		if a == f || strings.HasPrefix(a, f+"=") {
			return true
		}
	}
	return false
}

// flagValue reads the value of a flag: after `=` in the same word, or from the
// next word. It returns the index of the next word to look at.
func flagValue(args []string, j int, flag string) (string, int, error) {
	a := args[j]
	if i := strings.IndexByte(a, '='); i >= 0 {
		return a[i+1:], j + 1, nil
	}
	if j+1 >= len(args) {
		return "", j + 1, errs.New(errs.Config, "cli.parse", fmt.Sprintf("%s needs a value", flag))
	}
	return args[j+1], j + 2, nil
}

// unknownFlag is the error for a flag the command does not take.
func unknownFlag(flag string, c Command) error {
	return errs.New(errs.Config, "cli.parse",
		fmt.Sprintf("%s: unknown flag %q", c, flag))
}
